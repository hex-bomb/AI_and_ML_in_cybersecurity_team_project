import json

from app.pipeline import bulk_body, chunk_id, index_document, split, stale_query


def event() -> dict:
    return {
        "document_id": "d1",
        "version_id": "v1",
        "kb_id": "kb1",
        "audience": "public",
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
    query = stale_query("kb1", "d1", ["keep1", "keep2"])
    bool_ = query["query"]["bool"]
    assert {"term": {"kb_id": "kb1"}} in bool_["filter"]
    assert {"term": {"document_id": "d1"}} in bool_["filter"]
    assert bool_["must_not"] == [{"terms": {"chunk_id": ["keep1", "keep2"]}}]


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
