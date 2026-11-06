import uuid
from dataclasses import dataclass
from typing import Any
from uuid import UUID

from app.db import ConversationNotFound, IdempotencyConflict, TurnInProgress, citation_rows
from app.errors import AppError, Unavailable
from app.guard import blocked_reason
from app.page_context import prompt_block, query_hint
from app.ports import Binder, Completer, Dialogs, Retriever
from app.rag import NO_EVIDENCE
from app.usecases.common import answer_from_hits


@dataclass(frozen=True)
class Question:
    """Вход сценария: вопрос, сессия и место в диалоге.

    ``conversation_id`` пустой — новый диалог; ``client_message_id`` — ключ
    намерения клиента, по нему повтор не создаёт вторую реплику.
    """

    text: str
    token: str
    subject: str = ""
    channel: str = "public"
    conversation_id: UUID | None = None
    client_message_id: str | None = None
    request_id: str = ""
    page_context: dict[str, Any] | None = None


class AskQuestion:
    """guard → поиск → генерация → запись реплики.

    Порядок и обработка отказов живут здесь, а не в обработчике запроса:
    новый транспорт (HTTP, очередь, CLI) не должен их повторять. История
    берётся из хранилища, а не из тела запроса: клиент не может подсунуть
    модели чужой контекст.
    """

    def __init__(
        self,
        retriever: Retriever,
        completer: Completer,
        dialogs: Dialogs,
        top_k: int = 4,
        binder: Binder | None = None,
    ) -> None:
        self._retriever = retriever
        self._completer = completer
        self._dialogs = dialogs
        self._top_k = top_k
        self._binder = binder

    async def __call__(self, question: Question) -> dict[str, Any]:
        client_message_id = question.client_message_id or f"srv-{uuid.uuid4().hex}"
        # Привязка разрешается до записи реплики: в истории остаётся то, на что
        # сервер действительно опирался, а не ключ клиента.
        page_context = await self._bind(question.page_context)
        try:
            started = await self._dialogs.begin(
                conversation_id=question.conversation_id,
                subject=question.subject,
                channel=question.channel,
                client_message_id=client_message_id,
                question=question.text,
                request_id=question.request_id,
                page_context=page_context,
            )
        except ConversationNotFound as exc:
            raise AppError.not_found("CONVERSATION_NOT_FOUND", "Диалог не найден") from exc
        except TurnInProgress as exc:
            raise AppError.conflict(
                "TURN_IN_PROGRESS", "Предыдущий ответ в этом диалоге ещё не готов"
            ) from exc
        except IdempotencyConflict as exc:
            raise AppError.conflict(
                "IDEMPOTENCY_CONFLICT",
                "Этот client_message_id уже использован с другим вопросом",
            ) from exc

        if started.replay is not None:
            return started.replay

        try:
            result, hits = await self._answer(
                question.text,
                started.history,
                question.token,
                page_context,
            )
        except AppError as exc:
            await self._dialogs.fail(started.turn_id, exc.code)
            raise

        payload = {
            **result,
            "conversation_id": str(started.conversation_id),
            "turn_id": str(started.turn_id),
        }
        await self._dialogs.finish(
            turn_id=started.turn_id,
            conversation_id=started.conversation_id,
            client_message_id=client_message_id,
            question=question.text,
            payload=payload,
            citations=citation_rows(result["citations"], hits),
        )
        return payload

    async def _bind(self, page_context: dict[str, Any] | None) -> dict[str, Any] | None:
        """Разрешить content_key серверной привязкой.

        Клиент присылает ключ, а не документ: какой документ за ним стоит,
        решает сервер. Нет привязки — контекст остаётся без неё, вопрос всё
        равно обрабатывается.
        """
        if not page_context or self._binder is None:
            return page_context
        content_key = page_context.get("content_key")
        if not content_key:
            return page_context

        binding = await self._binder.binding(str(content_key))
        if not binding or not binding.get("document_ref"):
            return page_context
        return {
            **page_context,
            "binding": {
                "document_ref": binding["document_ref"],
                "external_id": binding.get("external_id", ""),
                "section_key": binding.get("section_key", ""),
            },
        }

    async def _answer(
        self,
        text: str,
        history: tuple[str, ...],
        token: str,
        page_context: dict[str, Any] | None,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        reason = blocked_reason(text)
        if reason:
            raise AppError.forbidden("Запрос отклонён политикой безопасности", {"reason": reason})

        # Выделение помогает короткому вопросу: «А для товара со скидкой?» без
        # него не находит нужный раздел. Права от этого не меняются.
        hint = query_hint(page_context)
        query = f"{hint} {text}" if hint else text
        focus = str((page_context or {}).get("binding", {}).get("document_ref", ""))

        try:
            hits = await self._retriever.search(query, self._top_k, token, focus)
        except Unavailable as exc:
            raise AppError.search_unavailable() from exc

        if not hits:
            return {"status": "no_evidence", "answer": NO_EVIDENCE, "citations": []}, []

        result = await answer_from_hits(
            self._completer, text, history, hits, page=prompt_block(page_context)
        )
        return result, hits
