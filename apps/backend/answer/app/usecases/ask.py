from dataclasses import dataclass
from typing import Any

from app.errors import AppError, Unavailable
from app.guard import blocked_reason
from app.ports import Completer, Retriever
from app.rag import NO_EVIDENCE
from app.usecases.common import answer_from_hits


@dataclass(frozen=True)
class Question:
    """Вход сценария: вопрос, история диалога и сессия для вызовов соседей."""

    text: str
    history: tuple[str, ...]
    token: str


class AskQuestion:
    """guard → поиск → генерация.

    Порядок и обработка отказов живут здесь, а не в обработчике запроса:
    новый транспорт (HTTP, очередь, CLI) не должен их повторять.
    """

    def __init__(
        self,
        retriever: Retriever,
        completer: Completer,
        top_k: int = 4,
    ) -> None:
        self._retriever = retriever
        self._completer = completer
        self._top_k = top_k

    async def __call__(self, question: Question) -> dict[str, Any]:
        reason = blocked_reason(question.text)
        if reason:
            raise AppError.forbidden("Запрос отклонён политикой безопасности", {"reason": reason})

        try:
            hits = await self._retriever.search(question.text, self._top_k, question.token)
        except Unavailable as exc:
            raise AppError.search_unavailable() from exc

        if not hits:
            return {"status": "no_evidence", "answer": NO_EVIDENCE, "citations": []}

        return await answer_from_hits(self._completer, question.text, question.history, hits)
