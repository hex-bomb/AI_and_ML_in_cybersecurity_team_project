"""Разбор контекста страницы: границы полей и безопасный адрес."""

from app.page_context import (
    MAX_SELECTION,
    MAX_TITLE,
    MAX_URL,
    parse,
    prompt_block,
    query_hint,
)


def test_rejects_non_object() -> None:
    assert parse(None) is None
    assert parse("url") is None
    assert parse([1, 2]) is None


def test_keeps_only_http_urls() -> None:
    assert parse({"url": "javascript:alert(1)"}) is None
    assert parse({"url": "file:///etc/passwd"}) is None
    assert parse({"url": "https://shop.example/help/returns"}) == {
        "url": "https://shop.example/help/returns"
    }


def test_strips_query_and_fragment() -> None:
    # В адресе могут быть токены и персональные данные — они не нужны.
    context = parse({"url": "https://shop.example/help?token=secret#top"})

    assert context == {"url": "https://shop.example/help"}


def test_truncates_long_fields() -> None:
    context = parse({"title": "я" * (MAX_TITLE + 50), "url": "https://e.example/" + "a" * MAX_URL})

    assert len(context["title"]) == MAX_TITLE
    assert len(context["url"]) <= MAX_URL


def test_selection_is_bounded_and_collapsed() -> None:
    context = parse(
        {
            "selection": {
                "text": "  Возврат\n возможен  " + "х" * MAX_SELECTION,
                "prefix": "a" * 500,
                "suffix": "хвост",
            }
        }
    )

    selection = context["selection"]
    assert len(selection["text"]) == MAX_SELECTION
    assert "\n" not in selection["text"]
    assert len(selection["prefix"]) <= 200
    assert selection["suffix"] == "хвост"


def test_empty_selection_is_dropped() -> None:
    assert parse({"selection": {"text": "   "}}) is None


def test_unknown_fields_are_dropped() -> None:
    context = parse({"title": "Страница", "documents": ["внутренний-документ"]})

    assert context == {"title": "Страница"}


def test_query_hint_uses_selection() -> None:
    context = parse({"selection": {"text": "Возврат возможен в течение 14 дней"}})

    assert query_hint(context) == "Возврат возможен в течение 14 дней"
    assert query_hint(None) == ""


def test_prompt_block_lists_page_and_selection() -> None:
    context = parse(
        {
            "url": "https://shop.example/help/returns",
            "title": "Возврат товара",
            "selection": {"text": "Возврат возможен в течение 14 дней"},
        }
    )

    block = prompt_block(context)

    assert "Страница: Возврат товара" in block
    assert "Адрес: https://shop.example/help/returns" in block
    assert 'Пользователь выделил: "Возврат возможен в течение 14 дней"' in block
    assert prompt_block(None) == ""
