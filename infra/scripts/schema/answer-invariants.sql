-- Проверка инвариантов vektor_answer на живой схеме.
--   psql -v ON_ERROR_STOP=1 -f infra/scripts/schema/answer-invariants.sql

BEGIN;

DO $$
DECLARE
    conv uuid := gen_random_uuid();
BEGIN
    INSERT INTO conversations (id, channel, owner_subject)
    VALUES (conv, 'internal', 'user-1');

    INSERT INTO turns (id, conversation_id, seq, client_message_id, question, state)
    VALUES (gen_random_uuid(), conv, 1, 'msg-1', 'Первый вопрос', 'running');

    -- 1. Вторая активная генерация в том же диалоге запрещена.
    BEGIN
        INSERT INTO turns (id, conversation_id, seq, client_message_id, question, state)
        VALUES (gen_random_uuid(), conv, 2, 'msg-2', 'Второй вопрос', 'accepted');
        RAISE EXCEPTION 'invariant broken: two active generations';
    EXCEPTION WHEN unique_violation THEN
        RAISE NOTICE 'ok: одна активная генерация на диалог';
    END;

    -- 2. Повтор сообщения с тем же client_message_id не создаёт вторую реплику.
    BEGIN
        INSERT INTO turns (id, conversation_id, seq, client_message_id, question, state, completed_at)
        VALUES (gen_random_uuid(), conv, 2, 'msg-1', 'Повтор', 'completed', now());
        RAISE EXCEPTION 'invariant broken: duplicate client_message_id accepted';
    EXCEPTION WHEN unique_violation THEN
        RAISE NOTICE 'ok: client_message_id уникален в диалоге';
    END;

    -- 3. Завершённая реплика обязана иметь completed_at.
    BEGIN
        INSERT INTO turns (id, conversation_id, seq, client_message_id, question, state)
        VALUES (gen_random_uuid(), conv, 3, 'msg-3', 'Без времени', 'completed');
        RAISE EXCEPTION 'invariant broken: completed turn without completed_at';
    EXCEPTION WHEN check_violation THEN
        RAISE NOTICE 'ok: completed требует completed_at';
    END;
END;
$$;

ROLLBACK;
