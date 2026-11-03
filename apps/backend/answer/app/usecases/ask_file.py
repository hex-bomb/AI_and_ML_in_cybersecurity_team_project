from dataclasses import dataclass
from typing import Any

from app.errors import AppError, BadRequest, Unavailable
from app.guard import blocked_reason
from app.ports import Completer, Embedder, TextExtractor
from app.rag import build_file_citations, pseudo_hits, rank, slices
from app.usecases.common import answer_from_hits


@dataclass(frozen=True)
class FileQuestion:
    filename: str
    content: bytes
    question: str


class AskAboutFile:
    """guard → разбор файла → векторы → ранжирование → генерация."""

    def __init__(
        self,
        extractor: TextExtractor,
        embedder: Embedder,
        completer: Completer,
        top_k: int = 4,
    ) -> None:
        self._extractor = extractor
        self._embedder = embedder
        self._completer = completer
        self._top_k = top_k

    async def __call__(self, ask: FileQuestion) -> dict[str, Any]:
        reason = blocked_reason(ask.question)
        if reason:
            raise AppError.forbidden("Запрос отклонён политикой безопасности", {"reason": reason})

        try:
            content = await self._extractor.extract(ask.filename, ask.content)
            chunks = slices(content)
            vectors = await self._embedder.embed(chunks, "passage")
            query = (await self._embedder.embed([ask.question], "query"))[0]
        except BadRequest as exc:
            raise AppError.bad_request("Файл не удалось разобрать") from exc
        except Unavailable as exc:
            raise AppError.service_unavailable() from exc

        hits = pseudo_hits(ask.filename, rank(chunks, vectors, query, self._top_k))
        return await answer_from_hits(self._completer, ask.question, (), hits, build_file_citations)
