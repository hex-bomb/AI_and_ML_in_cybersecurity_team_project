from collections.abc import Callable, Sequence
from typing import Any

from app.errors import AppError, Unavailable
from app.ports import Completer
from app.rag import SYSTEM, build_citations, build_prompt, parse_answer

CitationBuilder = Callable[[Sequence[int], Sequence[dict[str, Any]]], list[dict[str, Any]]]


async def answer_from_hits(
    completer: Completer,
    question: str,
    history: Sequence[str],
    hits: Sequence[dict[str, Any]],
    citations: CitationBuilder = build_citations,
    page: str = "",
) -> dict[str, Any]:
    """Сгенерировать ответ и цитаты по готовой выдаче.

    Оба эндпоинта отвечают одинаково, различается только построение выдачи и
    вид цитат (у файла ссылки ведут не в хранилище документов).
    """
    try:
        raw = await completer.complete(SYSTEM, build_prompt(question, history, hits, page))
    except Unavailable as exc:
        raise AppError.llm_unavailable() from exc

    text, numbers = parse_answer(raw)
    if not text:
        raise AppError.bad_output()

    # Модель любит не указывать номера — тогда ссылаемся на первые фрагменты.
    return {
        "status": "ok",
        "answer": text,
        "citations": citations(numbers or tuple(range(1, min(len(hits), 3) + 1)), hits),
    }
