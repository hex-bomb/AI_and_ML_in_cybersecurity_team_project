from collections.abc import Sequence
from dataclasses import dataclass
from typing import Any, Protocol
from uuid import UUID


class Retriever(Protocol):
    async def search(self, query: str, top_k: int, token: str) -> list[dict[str, Any]]: ...


class Completer(Protocol):
    async def complete(self, system: str, user: str) -> str: ...


class Embedder(Protocol):
    async def embed(self, texts: list[str], kind: str) -> list[list[float] | None]: ...


class TextExtractor(Protocol):
    async def extract(self, filename: str, content: bytes) -> str: ...


@dataclass(frozen=True)
class Started:
    """Реплика зарегистрирована и готова к генерации.

    ``replay`` заполнен, если это повтор запроса с тем же ``client_message_id``:
    тогда ответ берётся из записи идемпотентности, а модель не вызывается.
    """

    conversation_id: UUID
    turn_id: UUID
    history: tuple[str, ...]
    replay: dict[str, Any] | None = None


class Dialogs(Protocol):
    """Хранилище реплик: единственный путь записи ответа в историю."""

    async def begin(
        self,
        *,
        conversation_id: UUID | None,
        subject: str,
        channel: str,
        client_message_id: str,
        question: str,
        request_id: str,
    ) -> Started: ...

    async def finish(
        self,
        *,
        turn_id: UUID,
        conversation_id: UUID,
        client_message_id: str,
        question: str,
        payload: dict[str, Any],
        citations: Sequence[dict[str, Any]],
    ) -> None: ...

    async def fail(self, turn_id: UUID, code: str) -> None: ...

    async def ping(self) -> None: ...
