import json
import re
from collections.abc import Sequence
from pathlib import PurePath
from typing import Any

NO_EVIDENCE = "В доступной базе знаний не найдено подтверждающих материалов."

SLICE_CHARS = 1000

SYSTEM = """Ты — ассистент компании ВЕКТОР.
Отвечай только по фрагментам базы знаний, переданным в сообщении пользователя.
Не выдумывай факты, цены, сроки и условия, которых нет во фрагментах.
Если ответа нет — скажи, что подтверждающих материалов нет.
Ссылайся на фрагменты номерами в квадратных скобках: [1], [2].
Верни строго один JSON-объект без markdown: {"answer": "текст", "citations": [1, 2]}"""

_JSON = re.compile(r"\{.*\}", re.DOTALL)
_NUMBER = re.compile(r"\[(\d{1,2})\]")


def build_context(hits: Sequence[dict[str, Any]]) -> str:
    """Склеить фрагменты в нумерованный контекст.

    Parameters
    ----------
    hits : Sequence[dict]
        Результат retrieval; порядок задаёт номера цитат.

    Returns
    -------
    str
        Блоки вида ``[1] Заголовок — раздел`` и текст фрагмента.
    """
    blocks = []
    for number, hit in enumerate(hits, start=1):
        path = " / ".join((hit.get("locator") or {}).get("section_path") or [])
        header = f"[{number}] {hit['title']}" + (f" — {path}" if path else "")
        blocks.append(f"{header}\n{hit['text']}")
    return "\n\n".join(blocks)


def build_prompt(question: str, history: Sequence[str], hits: Sequence[dict[str, Any]]) -> str:
    history_text = "\n".join(history) if history else "(нет)"
    return (
        f"История диалога:\n{history_text}\n\n"
        f"Фрагменты базы знаний:\n\n{build_context(hits)}\n\n"
        f"Вопрос пользователя: {question}"
    )


def parse_answer(raw: str) -> tuple[str, tuple[int, ...]]:
    """Достать текст и номера фрагментов из ответа модели.

    Parameters
    ----------
    raw : str
        Сырой текст модели: ожидается JSON, допускается обычный текст.

    Returns
    -------
    tuple
        ``(answer, numbers)``; numbers пустые, если модель не сослалась.
    """
    text = raw.strip()
    match = _JSON.search(text)
    if match:
        try:
            payload = json.loads(match.group(0))
        except json.JSONDecodeError:
            payload = None
        if isinstance(payload, dict) and str(payload.get("answer") or "").strip():
            numbers = tuple(n for n in payload.get("citations") or [] if isinstance(n, int))
            return str(payload["answer"]).strip(), numbers
    return text, tuple(dict.fromkeys(int(n) for n in _NUMBER.findall(text)))


def build_citations(numbers: Sequence[int], hits: Sequence[dict[str, Any]]) -> list[dict[str, Any]]:
    """Оставить только существующие номера: модель не может сослаться вне выдачи."""
    out: list[dict[str, Any]] = []
    seen: set[int] = set()
    for number in numbers:
        if number in seen or not 1 <= number <= len(hits):
            continue
        seen.add(number)
        hit = hits[number - 1]
        out.append(
            {
                "number": number,
                "chunk_id": hit["chunk_id"],
                "document_id": hit["document_id"],
                "version_id": hit["version_id"],
                "title": hit["title"],
                "quote": hit["text"],
                "locator": hit["locator"],
                "source_url": None,
                "download_url": f"/api/v1/documents/{hit['document_id']}/source",
            }
        )
    return out


def build_file_citations(
    numbers: Sequence[int], hits: Sequence[dict[str, Any]]
) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    seen: set[int] = set()
    for number in numbers:
        if number in seen or not 1 <= number <= len(hits):
            continue
        seen.add(number)
        hit = hits[number - 1]
        out.append(
            {
                "number": number,
                "filename": hit["title"],
                "quote": hit["text"],
                "locator": hit["locator"],
            }
        )
    return out


def slices(text: str, slice_chars: int = SLICE_CHARS) -> list[str]:
    """Нарезать текст файла на окна по абзацам.

    Абзац длиннее окна режется на части, а не теряет хвост.

    Parameters
    ----------
    text : str
        Извлечённый текст файла.
    slice_chars : int
        Максимальная длина окна.

    Returns
    -------
    list[str]
        Окна; при пустом тексте — одно окно с обрезанным исходником.
    """
    out: list[str] = []
    current = ""
    for paragraph in text.split("\n\n"):
        paragraph = paragraph.strip()
        if not paragraph:
            continue
        while len(paragraph) > slice_chars:
            if current:
                out.append(current)
                current = ""
            out.append(paragraph[:slice_chars])
            paragraph = paragraph[slice_chars:]
        if len(current) + len(paragraph) + 2 <= slice_chars:
            current = f"{current}\n\n{paragraph}" if current else paragraph
        else:
            if current:
                out.append(current)
            current = paragraph
    if current:
        out.append(current)
    return out or [text[:slice_chars]]


def cosine(left: list[float], right: list[float]) -> float:
    """Косинус через скалярное произведение: embedding отдаёт векторы единичной длины."""
    return sum(a * b for a, b in zip(left, right, strict=True))


def rank(
    chunks: list[str],
    vectors: list[list[float] | None],
    query: list[float] | None,
    top_k: int,
) -> list[str]:
    """Отобрать top_k фрагментов по близости к запросу.

    Если вектора недоступны (деградация embedding) — возвращаем первые top_k.
    """
    if query is None or any(vector is None for vector in vectors):
        return chunks[:top_k]
    scored = sorted(
        (
            (cosine(query, vector), index)
            for index, vector in enumerate(vectors)
            if vector is not None
        ),
        reverse=True,
    )
    return [chunks[index] for _, index in scored[:top_k]]


def pseudo_hits(filename: str, texts: list[str]) -> list[dict[str, Any]]:
    """Собрать фиктивные hits для файла: он живёт только внутри запроса."""
    name = PurePath(filename or "document").name
    return [
        {
            "chunk_id": f"upload:{index}",
            "document_id": "upload",
            "version_id": "upload",
            "title": name,
            "text": text,
            "locator": {
                "block_ids": [],
                "section_path": ["Файл"],
                "page_start": None,
                "page_end": None,
            },
        }
        for index, text in enumerate(texts)
    ]
