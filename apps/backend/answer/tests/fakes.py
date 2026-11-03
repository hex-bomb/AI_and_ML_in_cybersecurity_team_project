"""Фейки адаптеров: тесты сценариев без сети и без моков httpx."""

from typing import Any


def hit(number: int = 1) -> dict[str, Any]:
    return {
        "chunk_id": f"chunk-{number}",
        "document_id": f"doc-{number}",
        "version_id": "v1",
        "title": f"Документ {number}",
        "text": "фрагмент",
        "locator": {
            "block_ids": [],
            "section_path": ["Раздел"],
            "page_start": None,
            "page_end": None,
        },
    }


class FakeRetriever:
    def __init__(
        self, hits: list[dict[str, Any]] | None = None, error: Exception | None = None
    ) -> None:
        self.hits = hits or []
        self.error = error
        self.calls = 0

    async def search(self, kb_id: str, query: str, top_k: int, token: str):
        self.calls += 1
        if self.error:
            raise self.error
        return self.hits


class FakeCompleter:
    def __init__(
        self,
        reply: str = '{"answer": "ответ", "citations": [1]}',
        error: Exception | None = None,
    ) -> None:
        self.reply = reply
        self.error = error
        self.prompts: list[str] = []

    async def complete(self, system: str, user: str) -> str:
        self.prompts.append(user)
        if self.error:
            raise self.error
        return self.reply


class FakeEmbedder:
    def __init__(self, error: Exception | None = None) -> None:
        self.error = error
        self.calls = 0

    async def embed(self, texts: list[str], kind: str):
        self.calls += 1
        if self.error:
            raise self.error
        return [[1.0, 0.0] for _ in texts]


class FakeExtractor:
    def __init__(self, text: str = "текст файла", error: Exception | None = None) -> None:
        self.text = text
        self.error = error
        self.calls = 0

    async def extract(self, filename: str, content: bytes) -> str:
        self.calls += 1
        if self.error:
            raise self.error
        return self.text
