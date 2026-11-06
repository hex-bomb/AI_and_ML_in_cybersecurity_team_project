-- Проверка инвариантов vektor_docs на живой схеме.
--   psql -v ON_ERROR_STOP=1 -f infra/scripts/schema/docs-invariants.sql
-- Скрипт вставляет данные внутри транзакции и откатывает её.
-- Нарушенный инвариант печатает "invariant broken" и роняет psql.

BEGIN;

DO $$
DECLARE
    blob   uuid := gen_random_uuid();
    doc    uuid := gen_random_uuid();
    ver    uuid := gen_random_uuid();
    gen    uuid := gen_random_uuid();
BEGIN
    INSERT INTO blobs (id, storage_key, size_bytes, sha256, content_type)
    VALUES (blob, 'blobs/' || blob, 1024, repeat('a', 64), 'text/plain');

    INSERT INTO documents (id, external_id, title)
    VALUES (doc, 'invariant-' || doc, 'Проверка инвариантов');

    INSERT INTO document_versions (id, document_id, blob_id, content_sha256, version_label)
    VALUES (ver, doc, blob, repeat('b', 64), 'v1');

    -- 1. Необработанную версию публиковать нельзя.
    BEGIN
        UPDATE documents SET published_version_id = ver WHERE id = doc;
        RAISE EXCEPTION 'invariant broken: published a pending version';
    EXCEPTION WHEN raise_exception THEN
        IF SQLERRM LIKE 'invariant broken%' THEN RAISE; END IF;
        RAISE NOTICE 'ok: публикация требует processing = ready';
    END;

    -- 2. Версия без указателя не может называться опубликованной.
    BEGIN
        UPDATE documents SET publication = 'published' WHERE id = doc;
        RAISE EXCEPTION 'invariant broken: published without a version';
    EXCEPTION WHEN check_violation THEN
        RAISE NOTICE 'ok: publication = published требует published_version_id';
    END;

    -- 3. Готовая версия публикуется, состояние выставляется триггером.
    UPDATE document_versions
       SET processing = 'ready', ready_at = now()
     WHERE id = ver;

    UPDATE documents SET published_version_id = ver WHERE id = doc;

    IF (SELECT publication FROM documents WHERE id = doc) <> 'published' THEN
        RAISE EXCEPTION 'invariant broken: publication was not advanced';
    END IF;
    RAISE NOTICE 'ok: готовая версия публикуется, publication = published';

    -- 4. Опубликованную версию нельзя удалить.
    BEGIN
        DELETE FROM document_versions WHERE id = ver;
        RAISE EXCEPTION 'invariant broken: deleted a published version';
    EXCEPTION WHEN raise_exception THEN
        IF SQLERRM LIKE 'invariant broken%' THEN RAISE; END IF;
        RAISE NOTICE 'ok: опубликованная версия не удаляется';
    END;

    -- 5. На версию не больше одной сборки индекса.
    INSERT INTO index_generations (id, version_id, generation, embedding_model, embedding_dim, state)
    VALUES (gen, ver, 1, 'test-model', 768, 'building');

    BEGIN
        INSERT INTO index_generations (id, version_id, generation, embedding_model, embedding_dim, state)
        VALUES (gen_random_uuid(), ver, 2, 'test-model', 768, 'building');
        RAISE EXCEPTION 'invariant broken: two building generations';
    EXCEPTION WHEN unique_violation THEN
        RAISE NOTICE 'ok: одна сборка индекса на версию';
    END;

    -- 6. Журнал неизменяем.
    INSERT INTO audit_events (actor_kind, action, resource_type, resource_id, outcome)
    VALUES ('system', 'document.uploaded', 'document', doc::text, 'ok');

    BEGIN
        UPDATE audit_events SET outcome = 'failed' WHERE resource_id = doc::text;
        RAISE EXCEPTION 'invariant broken: audit row was updated';
    EXCEPTION WHEN raise_exception THEN
        IF SQLERRM LIKE 'invariant broken%' THEN RAISE; END IF;
        RAISE NOTICE 'ok: audit_events не обновляется';
    END;

    BEGIN
        DELETE FROM audit_events WHERE resource_id = doc::text;
        RAISE EXCEPTION 'invariant broken: audit row was deleted';
    EXCEPTION WHEN raise_exception THEN
        IF SQLERRM LIKE 'invariant broken%' THEN RAISE; END IF;
        RAISE NOTICE 'ok: audit_events не удаляется';
    END;

    -- 7. Одинаковый грант не дублируется, в том числе с NULL.
    INSERT INTO access_grants (document_id, group_key, channel)
    VALUES (doc, NULL, 'internal');

    BEGIN
        INSERT INTO access_grants (document_id, group_key, channel)
        VALUES (doc, NULL, 'internal');
        RAISE EXCEPTION 'invariant broken: duplicate grant accepted';
    EXCEPTION WHEN unique_violation THEN
        RAISE NOTICE 'ok: дубликат гранта отклонён';
    END;
END;
$$;

ROLLBACK;
