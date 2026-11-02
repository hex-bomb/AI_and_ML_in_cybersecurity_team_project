from app.rag import build_citations, parse_answer


def hits() -> list[dict]:
    return [
        {
            "chunk_id": "c1",
            "document_id": "d1",
            "version_id": "v1",
            "title": "Гарантия",
            "text": "24 месяца",
            "locator": {
                "block_ids": [],
                "section_path": ["Сервис"],
                "page_start": None,
                "page_end": None,
            },
        },
        {
            "chunk_id": "c2",
            "document_id": "d2",
            "version_id": "v2",
            "title": "Лизинг",
            "text": "от 12 месяцев",
            "locator": {
                "block_ids": [],
                "section_path": ["Финансы"],
                "page_start": None,
                "page_end": None,
            },
        },
    ]


def test_parse_json_contract() -> None:
    text, numbers = parse_answer('{"answer": "24 месяца [1]", "citations": [1]}')
    assert text == "24 месяца [1]"
    assert numbers == (1,)


def test_parse_json_inside_fence() -> None:
    text, numbers = parse_answer('```json\n{"answer": "ok", "citations": [2]}\n```')
    assert text == "ok"
    assert numbers == (2,)


def test_parse_plain_text_falls_back_to_brackets() -> None:
    text, numbers = parse_answer("Гарантия 24 месяца [1], лизинг от 12 [2].")
    assert text.startswith("Гарантия")
    assert numbers == (1, 2)


def test_citations_are_clamped_to_given_hits() -> None:
    assert build_citations((1, 7, 0, -1), hits()) == [build_citations((1,), hits())[0]]


def test_citations_dedupe_and_keep_order() -> None:
    numbers = [2, 1, 2]
    result = build_citations(numbers, hits())
    assert [item["number"] for item in result] == [2, 1]
    assert result[0]["download_url"].endswith("/api/v1/documents/d2/source")


def test_empty_hits_give_no_citations() -> None:
    assert build_citations((1, 2), []) == []
