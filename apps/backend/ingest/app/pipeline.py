"""Чистое ядро ингеста: нарезка текста, идентификаторы, маппинг в документ ES."""

import hashlib
import json


def chunk_id(document_id: str, version_id: str, position: int, text: str) -> str:
    """Детерминированный id фрагмента: повторная индексация идемпотентна."""
    digest = hashlib.sha256()
    for part in (document_id, version_id, str(position), text):
        digest.update(part.encode("utf-8"))
        digest.update(b"\x00")
    return f"sha256:{digest.hexdigest()}"


def index_document(
    event: dict,
    text: str,
    vector: list[float] | None,
    position: int,
    index: str,
) -> dict:
    """Собрать документ для bulk-записи в Elasticsearch."""
    identifier = chunk_id(event["document_id"], event["version_id"], position, text)
    document = {
        "chunk_id": identifier,
        "document_id": event["document_id"],
        "version_id": event["version_id"],
        "kb_id": event["kb_id"],
        "title": event.get("title") or "",
        "text": text,
        "section_path": [],
        "audience": event["audience"],
        "page_start": None,
        "page_end": None,
    }
    if vector is not None:
        document["embedding"] = vector
    return {"operation": {"index": {"_index": index, "_id": identifier}}, "document": document}


def bulk_body(operations: list[tuple[dict, dict]]) -> str:
    """Собрать NDJSON для _bulk: пара (операция, документ) на строку."""
    lines: list[str] = []
    for operation, document in operations:
        lines.append(json.dumps(operation))
        lines.append(json.dumps(document))
    return "\n".join(lines) + "\n"


def stale_query(kb_id: str, document_id: str, keep_ids: list[str]) -> dict:
    """Запрос удаления устаревших фрагментов: не трогает только что записанные."""
    return {
        "query": {
            "bool": {
                "filter": [
                    {"term": {"kb_id": kb_id}},
                    {"term": {"document_id": document_id}},
                ],
                "must_not": [{"terms": {"chunk_id": keep_ids}}],
            }
        }
    }
