"""Разбор контекста страницы, который присылает виджет.

Контекст недоверенный: он пришёл из браузера, где живёт произвольная разметка.
Поэтому поля обрезаются до жёстких пределов, адрес принимается только http(s)
и без query/fragment, а доступ к документам от контекста не зависит — он влияет
только на формулировку поискового запроса.
"""

from typing import Any
from urllib.parse import urlsplit, urlunsplit

MAX_URL = 500
MAX_TITLE = 300
MAX_KEY = 100
MAX_SELECTION = 1000
MAX_NEIGHBOUR = 200


def _text(value: Any, limit: int) -> str:
    return " ".join(str(value).split())[:limit] if value else ""


def _url(value: Any) -> str:
    """Оставить схему, хост и путь: параметры и якорь к делу не относятся."""
    raw = _text(value, MAX_URL)
    if not raw:
        return ""
    parts = urlsplit(raw)
    if parts.scheme not in ("http", "https") or not parts.netloc:
        return ""
    return urlunsplit((parts.scheme, parts.netloc, parts.path, "", ""))[:MAX_URL]


def parse(raw: Any) -> dict[str, Any] | None:
    """Привести контекст к известной форме или вернуть None.

    Пустой контекст не сохраняем: иначе в истории останется запись о том,
    что страница была, хотя виджет её не присылал.
    """
    if not isinstance(raw, dict):
        return None

    selection = raw.get("selection")
    selection_data = None
    if isinstance(selection, dict):
        text = _text(selection.get("text"), MAX_SELECTION)
        if text:
            selection_data = {"text": text}
            for side in ("prefix", "suffix"):
                neighbour = _text(selection.get(side), MAX_NEIGHBOUR)
                if neighbour:
                    selection_data[side] = neighbour

    context: dict[str, Any] = {}
    for key, value in (
        ("url", _url(raw.get("url"))),
        ("title", _text(raw.get("title"), MAX_TITLE)),
        ("content_key", _text(raw.get("content_key"), MAX_KEY)),
        ("version_hint", _text(raw.get("version_hint"), MAX_KEY)),
        ("section_key", _text(raw.get("section_key"), MAX_KEY)),
    ):
        if value:
            context[key] = value
    if selection_data:
        context["selection"] = selection_data

    return context or None


def query_hint(context: dict[str, Any] | None) -> str:
    """Выделенный текст для поиска: короткий вопрос без него теряет тему."""
    if not context:
        return ""
    selection = context.get("selection") or {}
    return str(selection.get("text") or "")


def prompt_block(context: dict[str, Any] | None) -> str:
    """Строки о странице для промпта. Данные, а не инструкции."""
    if not context:
        return ""
    lines = []
    if context.get("title"):
        lines.append(f"Страница: {context['title']}")
    if context.get("url"):
        lines.append(f"Адрес: {context['url']}")
    selection = context.get("selection") or {}
    if selection.get("text"):
        lines.append(f'Пользователь выделил: "{selection["text"]}"')
    return "\n".join(lines)
