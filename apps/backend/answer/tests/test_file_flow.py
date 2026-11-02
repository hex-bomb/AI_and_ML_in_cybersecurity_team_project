from app.rag import cosine, pseudo_hits, rank, slices


def test_slices_keeps_short_paragraphs_together() -> None:
    text = "Абзац раз.\n\nАбзац два."
    assert slices(text, 1000) == ["Абзац раз.\n\nАбзац два."]


def test_slices_cuts_long_paragraph_without_losing_tail() -> None:
    # Раньше хвост такого абзаца терялся молча.
    text = "а" * 2500
    parts = slices(text, 1000)
    assert len(parts) == 3
    assert "".join(parts) == text


def test_slices_empty_text_returns_single_window() -> None:
    assert slices("", 1000) == [""]


def test_cosine_is_dot_product() -> None:
    assert cosine([1.0, 0.0], [1.0, 0.0]) == 1.0
    assert cosine([1.0, 0.0], [0.0, 1.0]) == 0.0


def test_rank_orders_by_query_similarity() -> None:
    chunks = ["a", "b", "c"]
    vectors = [[1.0, 0.0], [0.0, 1.0], [0.7, 0.7]]
    query = [1.0, 0.0]
    assert rank(chunks, vectors, query, 2) == ["a", "c"]


def test_rank_falls_back_to_order_when_query_missing() -> None:
    chunks = ["a", "b", "c"]
    vectors = [[1.0, 0.0], None, [0.0, 1.0]]
    # Есть None-вектор — деградация: первые top_k в исходном порядке.
    assert rank(chunks, vectors, None, 2) == ["a", "b"]
    assert rank(chunks, vectors, [1.0, 0.0], 2) == ["a", "b"]


def test_pseudo_hits_uses_basename_only() -> None:
    hits = pseudo_hits("../../etc/passwd", ["текст"])
    assert hits[0]["title"] == "passwd"
    assert hits[0]["document_id"] == "upload"
    assert hits[0]["locator"]["section_path"] == ["Файл"]
