"""Хранилище диалогов: пул соединений, миграции и SQL.

Файл — единственное место, где сервис знает про Postgres. Сценарии работают
с протоколом ``Dialogs`` из :mod:`app.ports`, поэтому их можно проверять без
базы.
"""

import hashlib
import logging
import pathlib
import uuid
from collections.abc import Sequence
from typing import Any

import psycopg
from psycopg.rows import dict_row
from psycopg.types.json import Jsonb
from psycopg_pool import AsyncConnectionPool

from app.ports import Started

log = logging.getLogger("answer.db")

MIGRATIONS = pathlib.Path(__file__).resolve().parents[1] / "migrations"

# Ключ advisory lock: миграции накатывает один процесс, даже если реплик
# несколько. Число произвольное, важно только постоянство.
MIGRATION_LOCK = 718_023

# Сколько реплик истории уходит в промпт.
HISTORY_TURNS = 8

# Срок жизни записи идемпотентности: повтор запроса после этого считается
# новым запросом.
IDEMPOTENCY_TTL_SECONDS = 86_400


class ConversationNotFound(Exception):
    """Диалога нет или он принадлежит другому пользователю."""


class TurnInProgress(Exception):
    """В диалоге уже идёт генерация: второй ответ сломал бы порядок реплик."""


class IdempotencyConflict(Exception):
    """Тот же ``client_message_id`` пришёл с другим текстом вопроса."""


def _idempotency_key(conversation_id: uuid.UUID, client_message_id: str) -> str:
    return f"{conversation_id}:{client_message_id}"[:200]


def _uuid_or_none(value: Any) -> uuid.UUID | None:
    try:
        return uuid.UUID(str(value))
    except (TypeError, ValueError):
        return None


class Dialogs:
    """Репозиторий диалогов. Один экземпляр на процесс."""

    def __init__(self, dsn: str, max_size: int = 10) -> None:
        self._pool = AsyncConnectionPool(dsn, min_size=1, max_size=max_size, open=False)

    async def start(self, stale_seconds: int) -> None:
        """Поднять пул, накатить миграции и закрыть брошенные генерации."""
        await self._pool.open(wait=True, timeout=15)
        await self._migrate()
        await self._reap_stale(stale_seconds)

    async def close(self) -> None:
        await self._pool.close()

    async def ping(self) -> None:
        async with self._pool.connection() as conn:
            await conn.execute("select 1")

    async def begin(
        self,
        *,
        conversation_id: uuid.UUID | None,
        subject: str,
        channel: str,
        client_message_id: str,
        question: str,
        request_id: str,
    ) -> Started:
        """Зарегистрировать реплику и вернуть контекст для генерации.

        Блокировка строки диалога сериализует одновременные запросы: порядок
        реплик и одна активная генерация держатся без отдельных блокировок в
        приложении.
        """
        async with (
            self._pool.connection() as conn,
            conn.cursor(row_factory=dict_row) as cur,
        ):
            if conversation_id is None:
                conversation_id = uuid.uuid4()
                await cur.execute(
                    "insert into conversations (id, channel, owner_subject) values (%s, %s, %s)",
                    (conversation_id, channel, subject),
                )
            else:
                await cur.execute(
                    "select owner_subject, channel from conversations where id = %s for update",
                    (conversation_id,),
                )
                row = await cur.fetchone()
                if row is None or row["owner_subject"] != subject or row["channel"] != channel:
                    raise ConversationNotFound(str(conversation_id))

            await cur.execute(
                "select id, state, question from turns"
                " where conversation_id = %s and client_message_id = %s",
                (conversation_id, client_message_id),
            )
            existing = await cur.fetchone()

            if existing is not None:
                if existing["question"] != question:
                    raise IdempotencyConflict(str(existing["id"]))
                if existing["state"] in ("accepted", "running"):
                    raise TurnInProgress(str(existing["id"]))
                if existing["state"] == "completed":
                    await cur.execute(
                        "select response_body from idempotency_records"
                        " where scope = 'answer' and idempotency_key = %s",
                        (_idempotency_key(conversation_id, client_message_id),),
                    )
                    record = await cur.fetchone()
                    replay = record["response_body"] if record else None
                    return Started(conversation_id, existing["id"], (), replay)
                # failed/interrupted/cancelled: повтор того же сообщения
                # переиспользует реплику, а не создаёт вторую.
                turn_id = existing["id"]
                await cur.execute(
                    "update turns set state = 'running', error_code = null,"
                    " request_id = %s where id = %s",
                    (request_id, turn_id),
                )
            else:
                await cur.execute(
                    "select 1 from turns where conversation_id = %s"
                    " and state in ('accepted', 'running')",
                    (conversation_id,),
                )
                if await cur.fetchone() is not None:
                    raise TurnInProgress(str(conversation_id))
                await cur.execute(
                    "select coalesce(max(seq), 0) + 1 as seq from turns where conversation_id = %s",
                    (conversation_id,),
                )
                seq = (await cur.fetchone())["seq"]
                turn_id = uuid.uuid4()
                await cur.execute(
                    "insert into turns (id, conversation_id, seq, client_message_id,"
                    " question, state, request_id)"
                    " values (%s, %s, %s, %s, %s, 'running', %s)",
                    (
                        turn_id,
                        conversation_id,
                        seq,
                        client_message_id,
                        question,
                        request_id,
                    ),
                )

            await cur.execute(
                "select question, answer from turns"
                " where conversation_id = %s and state = 'completed' and id <> %s"
                " order by seq desc limit %s",
                (conversation_id, turn_id, HISTORY_TURNS),
            )
            previous = await cur.fetchall()

        history = tuple(
            line
            for row in reversed(previous)
            for line in (f"user: {row['question']}", f"assistant: {row['answer'] or ''}")
        )
        return Started(conversation_id, turn_id, history)

    async def finish(
        self,
        *,
        turn_id: uuid.UUID,
        conversation_id: uuid.UUID,
        client_message_id: str,
        question: str,
        payload: dict[str, Any],
        citations: Sequence[dict[str, Any]],
    ) -> None:
        """Записать ответ, цитаты и запись идемпотентности одной транзакцией."""
        async with (
            self._pool.connection() as conn,
            conn.cursor() as cur,
        ):
            await cur.execute(
                "update turns set state = 'completed', answer = %s,"
                " completed_at = now() where id = %s",
                (payload["answer"], turn_id),
            )
            await cur.execute(
                "update conversations set last_turn_at = now() where id = %s",
                (conversation_id,),
            )
            for row in citations:
                await cur.execute(
                    "insert into citations (turn_id, ordinal, document_id, version_id,"
                    " chunk_id, page_start, page_end, section_path)"
                    " values (%s, %s, %s, %s, %s, %s, %s, %s)",
                    (
                        turn_id,
                        row["ordinal"],
                        row["document_id"],
                        row["version_id"],
                        row["chunk_id"],
                        row["page_start"],
                        row["page_end"],
                        row["section_path"],
                    ),
                )
            await cur.execute(
                "insert into idempotency_records (scope, idempotency_key, request_hash,"
                " status, response_status, response_body, expires_at)"
                " values ('answer', %s, %s, 'completed', 200, %s,"
                " now() + make_interval(secs => %s))"
                " on conflict (scope, idempotency_key) do update set"
                " status = excluded.status,"
                " response_body = excluded.response_body,"
                " response_status = excluded.response_status,"
                " expires_at = excluded.expires_at",
                (
                    _idempotency_key(conversation_id, client_message_id),
                    hashlib.sha256(question.encode()).hexdigest(),
                    Jsonb(payload),
                    IDEMPOTENCY_TTL_SECONDS,
                ),
            )

    async def fail(self, turn_id: uuid.UUID, code: str) -> None:
        """Отметить реплику сбойной: повтор с тем же ключом снова её возьмёт."""
        async with self._pool.connection() as conn:
            await conn.execute(
                "update turns set state = 'failed', error_code = %s"
                " where id = %s and state = 'running'",
                (code, turn_id),
            )

    async def _reap_stale(self, stale_seconds: int) -> None:
        """Падение процесса не должно оставлять диалог вечно занятым."""
        async with self._pool.connection() as conn:
            cursor = await conn.execute(
                "update turns set state = 'interrupted'"
                " where state in ('accepted', 'running')"
                " and created_at < now() - make_interval(secs => %s)"
                " returning id",
                (stale_seconds,),
            )
            stale = await cursor.fetchall()
        if stale:
            log.warning("interrupted turns after restart: %d", len(stale))

    async def _migrate(self) -> None:
        """Накатить неприменённые файлы из ``migrations/`` по возрастанию имени."""
        async with await psycopg.AsyncConnection.connect(
            self._pool.conninfo, autocommit=True
        ) as lock:
            await lock.execute("select pg_advisory_lock(%s)", (MIGRATION_LOCK,))
            try:
                async with (
                    self._pool.connection() as conn,
                    conn.cursor() as cur,
                ):
                    await cur.execute(
                        "create table if not exists schema_migrations ("
                        " version text primary key,"
                        " applied_at timestamptz not null default now())"
                    )
                    await cur.execute("select version from schema_migrations")
                    applied = {row[0] for row in await cur.fetchall()}
                    for path in sorted(MIGRATIONS.glob("*.sql")):
                        if path.stem in applied:
                            continue
                        await cur.execute(path.read_text(encoding="utf-8"))
                        await cur.execute(
                            "insert into schema_migrations (version) values (%s)",
                            (path.stem,),
                        )
                        log.info("applied migration %s", path.stem)
            finally:
                await lock.execute("select pg_advisory_unlock(%s)", (MIGRATION_LOCK,))


def citation_rows(
    citations: Sequence[dict[str, Any]], hits: Sequence[dict[str, Any]]
) -> list[dict[str, Any]]:
    """Перевести цитаты ответа в строки таблицы ``citations``.

    Номер цитаты соответствует позиции в выдаче. Документ хранится внутренним
    идентификатором (``document_ref``), а не внешним ключом: по внешнему ключу
    нет ссылочной целостности. Цитата без него пропускается — ответ важнее
    статистики.
    """
    rows: list[dict[str, Any]] = []
    for citation in citations:
        number = citation["number"]
        if not 1 <= number <= len(hits):
            continue
        hit = hits[number - 1]
        document_id = _uuid_or_none(hit.get("document_ref"))
        version_id = _uuid_or_none(citation.get("version_id"))
        if document_id is None or version_id is None:
            continue
        locator = citation.get("locator") or {}
        rows.append(
            {
                "ordinal": number,
                "document_id": document_id,
                "version_id": version_id,
                "chunk_id": citation["chunk_id"][:200],
                "page_start": locator.get("page_start"),
                "page_end": locator.get("page_end"),
                "section_path": locator.get("section_path") or [],
            }
        )
    return rows
