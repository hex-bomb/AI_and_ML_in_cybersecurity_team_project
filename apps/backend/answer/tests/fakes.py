"""Фейки адаптеров: тесты сценариев без сети и без моков httpx."""

import uuid
from typing import Any

from app.ports import Started


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
        self.queries: list[str] = []
        self.focus = ""

    async def search(self, query: str, top_k: int, token: str, focus: str = ""):
        self.calls += 1
        self.queries.append(query)
        self.focus = focus
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


class FakeDialogs:
    """Хранилище реплик в памяти: проверяет сценарий, а не SQL."""

    def __init__(
        self,
        history: tuple[str, ...] = (),
        replay: dict[str, Any] | None = None,
        error: Exception | None = None,
    ) -> None:
        self.history = history
        self.replay = replay
        self.error = error
        self.finished: list[dict[str, Any]] = []
        self.failed: list[tuple[Any, str]] = []
        self.conversation_id: uuid.UUID | None = None
        self.turn_id: uuid.UUID = uuid.uuid4()
        self.pings = 0

    async def begin(
        self,
        *,
        conversation_id,
        subject,
        channel,
        client_message_id,
        question,
        request_id,
        page_context=None,
    ) -> Started:
        if self.error:
            raise self.error
        self.conversation_id = conversation_id or uuid.uuid4()
        self.page_context = page_context
        self.channel = channel
        return Started(
            conversation_id=self.conversation_id,
            turn_id=self.turn_id,
            history=self.history,
            replay=self.replay,
        )

    async def finish(self, **kwargs) -> None:
        self.finished.append(kwargs)

    async def fail(self, turn_id, code) -> None:
        self.failed.append((turn_id, code))

    async def ping(self) -> None:
        self.pings += 1


class FakeBinder:
    """Привязка страницы в памяти: проверяет сценарий, а не HTTP."""

    def __init__(self, binding: dict[str, Any] | None = None) -> None:
        self.binding_value = binding
        self.keys: list[str] = []

    async def binding(self, content_key: str) -> dict[str, Any] | None:
        self.keys.append(content_key)
        return self.binding_value
