"""Чистое ядро ингеста: нарезка текста, идентификаторы, тела запросов ES."""

import hashlib
import json
import re

_SENTENCE = re.compile(r"(?<=[.!?…])\s+")


def split(text: str, chunk_chars: int = 1200, overlap: int = 200) -> list[str]:
    """Нарезать текст на перекрывающиеся окна по предложениям.

    Предложение длиннее окна (таблица, лог, текст без пунктуации) режется
    жёстко: иначе окно превышает лимит токенизатора и хвост молча теряется.

    Parameters
    ----------
    text : str
        Полный текст версии документа.
    chunk_chars : int
        Максимальная длина окна.
    overlap : int
        Перекрытие соседних окон в символах.

    Returns
    -------
    list[str]
        Окна длиной не больше ``chunk_chars``.
    """
    chunks: list[str] = []
    current = ""
    for sentence in (part.strip() for part in _SENTENCE.split(text)):
        if not sentence:
            continue
        if len(sentence) > chunk_chars:
            if current:
                chunks.append(current)
                current = ""
            step = max(1, chunk_chars - overlap)
            for start in range(0, len(sentence), step):
                chunks.append(sentence[start : start + chunk_chars])
                if start + chunk_chars >= len(sentence):
                    break
            continue
        if len(current) + len(sentence) + 1 <= chunk_chars:
            current = f"{current} {sentence}".strip()
            continue
        if current:
            chunks.append(current)
        tail = current[-overlap:] if overlap and current else ""
        current = f"{tail} {sentence}".strip()
    if current:
        chunks.append(current)
    return chunks or [text[:chunk_chars]]


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
    """Собрать документ для bulk-записи в Elasticsearch.

    ``document_id`` — публичный ключ (external_id), он уходит в ссылки и цитаты.
    ``document_ref`` — внутренний uuid, по нему ингест обновляет и удаляет
    фрагменты, не зная публичных идентификаторов.
    """
    identifier = chunk_id(event["external_id"], event["version_id"], position, text)
    document = {
        "chunk_id": identifier,
        "document_id": event["external_id"],
        "document_ref": event["document_id"],
        "version_id": event["version_id"],
        "title": event.get("title") or "",
        "text": text,
        "section_path": [],
        "page_start": None,
        "page_end": None,
        # До публикации фрагмент не виден поиску: visibility выставляет событие.
        "published": False,
        "channels": [],
        "group_keys": [],
        "open_to_all": False,
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


def stale_query(document_ref: str, version_id: str, keep_ids: list[str]) -> dict:
    """Остатки прошлого прохода по той же версии: их не должно остаться."""
    return {
        "query": {
            "bool": {
                "filter": [
                    {"term": {"document_ref": document_ref}},
                    {"term": {"version_id": version_id}},
                ],
                "must_not": [{"terms": {"chunk_id": keep_ids}}],
            }
        }
    }


def published_query(
    document_ref: str,
    version_id: str,
    channels: list[str],
    group_keys: list[str],
) -> dict:
    """Публикация: чанки этой версии становятся видимыми в указанных каналах."""
    return {
        "query": {
            "bool": {
                "filter": [
                    {"term": {"document_ref": document_ref}},
                    {"term": {"version_id": version_id}},
                ]
            }
        },
        "doc": {
            "published": True,
            "channels": channels,
            "group_keys": group_keys,
            "open_to_all": not group_keys,
        },
    }


def superseded_query(document_ref: str, version_id: str) -> dict:
    """Предыдущие версии остаются в индексе, но поиск их больше не видит."""
    return {
        "query": {
            "bool": {
                "filter": [{"term": {"document_ref": document_ref}}],
                "must_not": [{"term": {"version_id": version_id}}],
            }
        },
        "doc": {"published": False},
    }


def access_query(document_ref: str, channels: list[str], group_keys: list[str]) -> dict:
    """Смена грантов у опубликованного документа."""
    return {
        "query": {"bool": {"filter": [{"term": {"document_ref": document_ref}}]}},
        "doc": {
            "channels": channels,
            "group_keys": group_keys,
            "open_to_all": not group_keys,
        },
    }


def withdrawn_query(document_ref: str) -> dict:
    return {
        "query": {"bool": {"filter": [{"term": {"document_ref": document_ref}}]}},
        "doc": {"published": False},
    }
