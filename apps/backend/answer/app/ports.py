from typing import Any, Protocol


class Retriever(Protocol):
    async def search(self, query: str, top_k: int, token: str) -> list[dict[str, Any]]: ...


class Completer(Protocol):
    async def complete(self, system: str, user: str) -> str: ...


class Embedder(Protocol):
    async def embed(self, texts: list[str], kind: str) -> list[list[float] | None]: ...


class TextExtractor(Protocol):
    async def extract(self, filename: str, content: bytes) -> str: ...
