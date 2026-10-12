-- Откат 0002. Ничего из 0001 не удаляется: 0001 нужен приложению целиком, и
-- падение на нём означало бы не откат этой миграции, а потерю базы.
--
-- Объекты снимаются в обратном порядке появления и с CASCADE там, где
-- зависимость не выражена внешним ключом, а существует только в коде.
-- CASCADE на представлении connector_freshness идёт первым: представление
-- зависит от connectors, и без этого DROP TABLE connectors упадёт.

DROP VIEW IF EXISTS connector_freshness;

-- Колонки document_versions возвращаются к состоянию 0001. Порядок важен:
-- ограничение держит внешний ключ на саму таблицу и снимается первым,
-- индекс по version_number идёт следом за колонкой, а не перед ней.
ALTER TABLE document_versions DROP CONSTRAINT IF EXISTS fk_versions_parent_version;

DROP INDEX IF EXISTS uq_versions_tenant_document_number;

ALTER TABLE document_versions DROP CONSTRAINT IF EXISTS ck_versions_number_positive;

ALTER TABLE document_versions DROP COLUMN IF EXISTS version_number;

ALTER TABLE document_versions DROP COLUMN IF EXISTS snapshot_key;

ALTER TABLE document_versions DROP COLUMN IF EXISTS message;

ALTER TABLE document_versions DROP COLUMN IF EXISTS author;

ALTER TABLE document_versions DROP COLUMN IF EXISTS parent_version_id;

DROP TABLE IF EXISTS connectors;

DROP TABLE IF EXISTS doc_edges;

-- Права §6 снимаются от листьев к корню: membership-таблицы ссылаются на
-- группы, а группы — на пользователей и контейнеры.
DROP TABLE IF EXISTS acl_permissions;

DROP TABLE IF EXISTS acl_resource_group_members;

DROP TABLE IF EXISTS acl_resource_groups;

DROP TABLE IF EXISTS acl_containers;

DROP TABLE IF EXISTS acl_global_group_members;

DROP TABLE IF EXISTS acl_global_groups;

DROP TABLE IF EXISTS acl_users;

-- Lake. Триггер идёт раньше таблицы: он привязан к ней, и снятие триггера
-- после DROP TABLE уже невозможно (объект не существует).
DROP TRIGGER IF EXISTS trg_raw_events_append_only ON raw_events;

DROP TABLE IF EXISTS raw_events;

DROP FUNCTION IF EXISTS raw_events_append_only();

DROP TABLE IF EXISTS dead_letter_events;

DROP TABLE IF EXISTS raw_event_ids;
