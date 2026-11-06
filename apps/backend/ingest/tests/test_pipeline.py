import json

from app.pipeline import (
    access_query,
    bulk_body,
    chunk_id,
    index_document,
    published_query,
    split,
    stale_query,
    superseded_query,
    update_body,
    withdrawn_query,
)


def event() -> dict:
    return {
        "document_id": "8f14e45f-ceea-467a-a1b2-3c4d5e6f7a8b",
        "external_id": "d1",
        "version_id": "v1",
        "generation": 1,
        "title": "T",
    }


def test_chunk_id_is_deterministic() -> None:
    a = chunk_id("d1", "v1", 0, "текст")
    b = chunk_id("d1", "v1", 0, "текст")
    assert a == b
    assert a.startswith("sha256:")


def test_chunk_id_changes_with_position_and_text() -> None:
    base = chunk_id("d1", "v1", 0, "текст")
    assert chunk_id("d1", "v1", 1, "текст") != base
    assert chunk_id("d1", "v1", 0, "другой") != base
    assert chunk_id("d2", "v1", 0, "текст") != base


def test_index_document_embeds_vector_only_when_present() -> None:
    with_vector = index_document(event(), "текст", [0.1, 0.2], 0, "idx")
    assert with_vector["document"]["embedding"] == [0.1, 0.2]
    assert with_vector["operation"]["index"]["_id"] == with_vector["document"]["chunk_id"]

    without = index_document(event(), "текст", None, 0, "idx")
    assert "embedding" not in without["document"]


def test_indexed_chunk_is_not_searchable_until_published() -> None:
    document = index_document(event(), "текст", None, 0, "idx")["document"]
    assert document["published"] is False
    assert document["channels"] == []
    # Публичный ключ и внутренняя ссылка — разные поля.
    assert document["document_id"] == "d1"
    assert document["document_ref"] == event()["document_id"]


def test_bulk_body_is_valid_ndjson() -> None:
    ops = [
        index_document(event(), "a", None, 0, "idx"),
        index_document(event(), "b", None, 1, "idx"),
    ]
    body = bulk_body([(op["operation"], op["document"]) for op in ops])
    lines = body.strip().split("\n")
    assert len(lines) == 4  # две пары операция+документ
    # Чередование: action, source, action, source.
    assert "index" in json.loads(lines[0])
    assert json.loads(lines[1])["text"] == "a"
    assert body.endswith("\n")


def test_stale_query_keeps_new_ids() -> None:
    query = stale_query("ref", "v1", ["keep1", "keep2"])
    bool_ = query["query"]["bool"]
    assert {"term": {"document_ref": "ref"}} in bool_["filter"]
    assert {"term": {"version_id": "v1"}} in bool_["filter"]
    assert bool_["must_not"] == [{"terms": {"chunk_id": ["keep1", "keep2"]}}]


def test_published_query_opens_only_listed_channels() -> None:
    query = published_query("ref", "v1", ["internal"], ["legal"])
    doc = query["doc"]
    assert doc["published"] is True
    assert doc["channels"] == ["internal"]
    assert doc["open_to_all"] is False

    # Грант без группы видит весь канал.
    assert published_query("ref", "v1", ["public"], [])["doc"]["open_to_all"] is True


def test_superseded_and_withdrawn_hide_document() -> None:
    superseded = superseded_query("ref", "v2")
    assert superseded["doc"] == {"published": False}
    assert {"term": {"version_id": "v2"}} in superseded["query"]["bool"]["must_not"]

    assert withdrawn_query("ref")["doc"] == {"published": False}


def test_access_query_updates_published_chunks() -> None:
    query = access_query("ref", ["public", "internal"], [])
    assert query["doc"] == {
        "channels": ["public", "internal"],
        "group_keys": [],
        "open_to_all": True,
    }


def test_split_keeps_text_shorter_than_window() -> None:
    assert split("Короткий текст.", 1000, 200) == ["Короткий текст."]


def test_split_hard_cuts_sentence_longer_than_window() -> None:
    text = "а" * 2500
    chunks = split(text, 1000, 200)

    assert chunks[0] == text[:1000]
    assert all(len(chunk) <= 1000 for chunk in chunks)
    assert text[-1000:] in "".join(chunks), "хвост текста потерян"


def test_split_respects_sentence_boundaries() -> None:
    text = "Первое предложение. Второе предложение."
    chunks = split(text, 1000, 200)
    assert chunks == [text]


def test_update_body_uses_script_not_doc() -> None:
    # _update_by_query не принимает поле doc: нужен скрипт, иначе 400.
    body = update_body(published_query("ref", "v1", ["public"], []))

    assert "doc" not in body
    assert body["script"]["params"]["doc"]["published"] is True
    assert body["script"]["params"]["doc"]["channels"] == ["public"]


def test_update_body_keeps_query() -> None:
    query = withdrawn_query("ref")

    assert update_body(query)["query"] == query["query"]
