"""Ингест: Kafka -> текст из documents -> фрагменты -> эмбеддинги -> индекс ES."""

import asyncio
import json
import logging
import os
from contextlib import asynccontextmanager, suppress

import httpx
from aiokafka import AIOKafkaConsumer
from fastapi import FastAPI
from fastapi.responses import JSONResponse
from prometheus_client import Counter, make_asgi_app

from app.pipeline import (
    access_query,
    bulk_body,
    index_document,
    published_query,
    split,
    stale_query,
    superseded_query,
    update_body,
    withdrawn_query,
)

KAFKA = os.environ.get("KAFKA_BOOTSTRAP", "localhost:9092")
TOPIC = os.environ.get("KAFKA_TOPIC", "documents")
GROUP = os.environ.get("KAFKA_GROUP", "ingest")
ES_URL = os.environ.get("ES_URL", "http://localhost:9200").rstrip("/")
ES_INDEX = os.environ.get("ES_INDEX", "vektor-chunks-v2")
DOCUMENTS_URL = os.environ.get("DOCUMENTS_URL", "http://localhost:8080").rstrip("/")
# Общий секрет для /internal/* эндпоинтов documents.
INTERNAL_TOKEN = os.environ.get("DOCUMENTS_INTERNAL_TOKEN", "")
EMBEDDING_URL = os.environ.get("EMBEDDING_URL", "http://localhost:8000").rstrip("/")
# Идентификатор модели попадает в index_generations: при смене модели нужен
# новый индекс, смешивать векторы разных моделей нельзя.
EMBEDDING_MODEL = os.environ.get("EMBEDDING_MODEL", "onnx-local")
CHUNK_CHARS = int(os.environ.get("CHUNK_CHARS", "1200"))
CHUNK_OVERLAP = int(os.environ.get("CHUNK_OVERLAP", "200"))
DIMS = int(os.environ.get("EMBEDDING_DIMS", "384"))
REFRESH = os.environ.get("ES_REFRESH", "false")

DOCUMENTS = Counter("ingest_documents_total", "Documents processed", ["status"])
CHUNKS = Counter("ingest_chunks_total", "Chunks written")

log = logging.getLogger("ingest")

# Сколько раз читаем сообщение до того, как сдадим его как безнадёжное.
MAX_ATTEMPTS = int(os.environ.get("INGEST_MAX_ATTEMPTS", "3"))
RETRY_BASE_SECONDS = float(os.environ.get("INGEST_RETRY_BASE_SECONDS", "2"))

# Состояние конвейера для healthz: пока задача жива — ok, если умерла — degraded.
_pipeline_task: asyncio.Task | None = None


async def ensure_index(client: httpx.AsyncClient) -> None:
    if (await client.head(f"{ES_URL}/{ES_INDEX}")).status_code == 200:
        return
    body = {
        "settings": {"number_of_shards": 1, "number_of_replicas": 0},
        "mappings": {
            "properties": {
                "chunk_id": {"type": "keyword"},
                "document_id": {"type": "keyword"},
                "document_ref": {"type": "keyword"},
                "version_id": {"type": "keyword"},
                "title": {"type": "text", "analyzer": "russian"},
                "text": {"type": "text", "analyzer": "russian"},
                "section_path": {"type": "keyword"},
                "published": {"type": "boolean"},
                "channels": {"type": "keyword"},
                "group_keys": {"type": "keyword"},
                "open_to_all": {"type": "boolean"},
                "page_start": {"type": "integer"},
                "page_end": {"type": "integer"},
                "embedding": {
                    "type": "dense_vector",
                    "dims": DIMS,
                    "index": True,
                    "similarity": "cosine",
                },
            }
        },
    }
    await client.put(f"{ES_URL}/{ES_INDEX}", json=body)


def internal_headers() -> dict[str, str]:
    return {"X-Internal-Token": INTERNAL_TOKEN} if INTERNAL_TOKEN else {}


async def embed(client: httpx.AsyncClient, texts: list[str]) -> list[list[float] | None]:
    response = await client.post(f"{EMBEDDING_URL}/embed", json={"texts": texts, "kind": "passage"})
    response.raise_for_status()
    return response.json()["vectors"]


async def replace(
    client: httpx.AsyncClient,
    event: dict,
    chunks: list[str],
    vectors: list[list[float] | None],
) -> int:
    # Сначала записываем все новые фрагменты, потом удаляем остатки прошлого
    # прохода: окна «документа нет» не возникает.
    operations = [
        index_document(event, text, vector, position, ES_INDEX)
        for position, (text, vector) in enumerate(zip(chunks, vectors, strict=True))
    ]
    ids = [op["document"]["chunk_id"] for op in operations]

    if operations:
        pairs = [(op["operation"], op["document"]) for op in operations]
        response = await client.post(
            f"{ES_URL}/_bulk?refresh={REFRESH}",
            content=bulk_body(pairs),
            headers={"Content-Type": "application/x-ndjson"},
        )
        payload = response.json()
        if payload.get("errors"):
            raise RuntimeError("bulk отклонён")

    await client.post(
        f"{ES_URL}/{ES_INDEX}/_delete_by_query?conflicts=proceed&refresh={REFRESH}",
        json=stale_query(event["document_id"], event["version_id"], ids),
    )
    return len(ids)


async def report_ready(
    client: httpx.AsyncClient,
    event: dict,
    chunk_count: int,
    vectors: list[list[float] | None],
) -> None:
    """Сообщить documents, что генерация проиндексирована.

    Сервис сравнит номер генерации: отчёт устаревшего прохода он отклонит,
    и версия не вернётся в прежнее состояние.
    """
    dims = next((len(v) for v in vectors if v), DIMS)
    response = await client.post(
        f"{DOCUMENTS_URL}/internal/versions/{event['version_id']}/index",
        headers=internal_headers(),
        json={
            "generation": event.get("generation", 1),
            "embedding_model": EMBEDDING_MODEL,
            "embedding_dim": dims,
            "chunk_count": chunk_count,
        },
    )
    if response.status_code != 200:
        raise RuntimeError(f"documents index report {response.status_code}")
    if not response.json().get("accepted", False):
        log.warning(
            "index report rejected as stale version_id=%s generation=%s",
            event["version_id"],
            event.get("generation"),
        )


async def apply(client: httpx.AsyncClient, body: dict) -> None:
    """Применить частичное обновление к отобранным чанкам.

    ``_update_by_query`` не принимает поле ``doc``: его нужно передать через
    скрипт. Статус проверяется явно — иначе 400 от Elasticsearch проходит
    молча и публикация «удаётся», ничего не меняя.
    """
    response = await client.post(
        f"{ES_URL}/{ES_INDEX}/_update_by_query?conflicts=proceed&refresh={REFRESH}",
        json=update_body(body),
    )
    if response.status_code != 200:
        raise RuntimeError(f"update_by_query {response.status_code}: {response.text[:200]}")
    payload = response.json()
    if payload.get("failures"):
        raise RuntimeError(f"update_by_query: {payload['failures']}")


async def index_version(client: httpx.AsyncClient, event: dict) -> None:
    response = await client.get(
        f"{DOCUMENTS_URL}/internal/documents/{event['external_id']}"
        f"/versions/{event['version_id']}/text",
        headers=internal_headers(),
    )
    if response.status_code != 200:
        raise RuntimeError(f"documents {response.status_code}")

    chunks = split(response.text, CHUNK_CHARS, CHUNK_OVERLAP)
    vectors = await embed(client, chunks)
    written = await replace(client, event, chunks, vectors)
    await report_ready(client, event, written, vectors)

    CHUNKS.inc(written)
    DOCUMENTS.labels("ok").inc()
    log.info(
        "indexed document_id=%s version_id=%s chunks=%d",
        event["external_id"],
        event["version_id"],
        written,
    )


async def publish(client: httpx.AsyncClient, event: dict) -> None:
    channels = event.get("channels") or []
    groups = event.get("group_keys") or []
    await apply(
        client,
        published_query(event["document_id"], event["version_id"], channels, groups),
    )
    await apply(client, superseded_query(event["document_id"], event["version_id"]))
    DOCUMENTS.labels("ok").inc()
    log.info("published document_id=%s version_id=%s", event["external_id"], event["version_id"])


async def change_access(client: httpx.AsyncClient, event: dict) -> None:
    await apply(
        client,
        access_query(
            event["document_id"],
            event.get("channels") or [],
            event.get("group_keys") or [],
        ),
    )
    log.info("access changed document_id=%s", event.get("external_id"))


async def withdraw(client: httpx.AsyncClient, event: dict) -> None:
    await apply(client, withdrawn_query(event["document_id"]))
    log.info("withdrawn document_id=%s", event.get("external_id"))


HANDLERS = {
    "document.version.created": index_version,
    "document.published": publish,
    "document.access.changed": change_access,
    "document.withdrawn": withdraw,
}


async def handle(client: httpx.AsyncClient, raw: bytes) -> None:
    event = json.loads(raw)
    handler = HANDLERS.get(event.get("event", ""))
    if handler is None:
        # Неизвестное событие — не повод ретраить его три раза.
        log.warning("skip unknown event %r", event.get("event"))
        return
    await handler(client, event)


async def run() -> None:
    async with httpx.AsyncClient(timeout=httpx.Timeout(60.0)) as client:
        await ensure_index(client)
        consumer = AIOKafkaConsumer(
            TOPIC,
            bootstrap_servers=KAFKA,
            group_id=GROUP,
            auto_offset_reset="earliest",
            enable_auto_commit=False,
        )
        await consumer.start()
        try:
            async for message in consumer:
                # Ретраи перед commit: транзиентный сбой documents/embedding не должен
                # навсегда выбрасывать документ из индекса. Commit — только после успеха
                # или исчерпания попыток (тогда сообщение считается безнадёжным).
                for attempt in range(1, MAX_ATTEMPTS + 1):
                    try:
                        await handle(client, message.value)
                        break
                    except Exception as exc:
                        DOCUMENTS.labels("error").inc()
                        if attempt == MAX_ATTEMPTS:
                            log.error("ingest dropped after %d attempts: %s", MAX_ATTEMPTS, exc)
                        else:
                            delay = RETRY_BASE_SECONDS * (2 ** (attempt - 1))
                            log.warning(
                                "ingest attempt %d/%d failed, retry in %.1fs: %s",
                                attempt,
                                MAX_ATTEMPTS,
                                delay,
                                exc,
                            )
                            await asyncio.sleep(delay)
                await consumer.commit()
        finally:
            await consumer.stop()


@asynccontextmanager
async def lifespan(_: FastAPI):
    global _pipeline_task
    _pipeline_task = asyncio.create_task(run())
    try:
        yield
    finally:
        _pipeline_task.cancel()
        with suppress(asyncio.CancelledError):
            await _pipeline_task


app = FastAPI(title="vektor-ingest", lifespan=lifespan)
app.mount("/metrics", make_asgi_app())


@app.get("/healthz")
async def healthz() -> JSONResponse:
    """Жив ли конвейер, а не просто процесс.

    Если фоновая задача упала (Kafka/ES недоступны при старте), процесс жив,
    но индексация не идёт — такие pod'ы должны уходить из endpoints.
    """
    task = _pipeline_task
    if task is None or task.done():
        detail = "конвейер не запущен"
        if task is not None and task.done() and not task.cancelled():
            detail = f"конвейер умер: {task.exception()!r}"
        return JSONResponse(
            status_code=503,
            content={"status": "degraded", "reason": detail},
        )
    return JSONResponse(status_code=200, content={"status": "ok"})
