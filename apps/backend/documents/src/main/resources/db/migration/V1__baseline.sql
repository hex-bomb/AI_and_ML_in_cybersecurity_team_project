-- Baseline: одна организация, одно пространство знаний.
-- kb_id и audience убраны: доступ описывается grants, видимость — публикацией.

CREATE TABLE blobs (
    id           uuid         PRIMARY KEY,
    storage_key  varchar(300) NOT NULL UNIQUE,
    size_bytes   bigint       NOT NULL,
    sha256       char(64)     NOT NULL,
    content_type varchar(150) NOT NULL,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT ck_blobs_size CHECK (size_bytes > 0)
);

CREATE TABLE documents (
    id                   uuid         PRIMARY KEY,
    external_id          varchar(200) NOT NULL UNIQUE,
    title                varchar(300) NOT NULL,
    section              varchar(200) NOT NULL DEFAULT '',
    lifecycle            varchar(20)  NOT NULL DEFAULT 'active',
    publication          varchar(20)  NOT NULL DEFAULT 'draft',
    published_version_id uuid         NULL,
    created_at           timestamptz  NOT NULL DEFAULT now(),
    updated_at           timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT ck_documents_lifecycle CHECK (lifecycle IN ('active', 'deleting', 'deleted')),
    CONSTRAINT ck_documents_publication CHECK (publication IN ('draft', 'published', 'withdrawn')),
    CONSTRAINT ck_documents_published
        CHECK (publication <> 'published' OR published_version_id IS NOT NULL)
);

CREATE TABLE document_versions (
    id               uuid        PRIMARY KEY,
    document_id      uuid        NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    blob_id          uuid        NOT NULL REFERENCES blobs (id),
    text_blob_id     uuid        NOT NULL REFERENCES blobs (id),
    content_sha256   char(64)    NOT NULL,
    version_label    varchar(50) NOT NULL,
    processing       varchar(20) NOT NULL DEFAULT 'pending',
    index_generation integer     NOT NULL DEFAULT 0,
    failure_reason   text        NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    ready_at         timestamptz NULL,
    CONSTRAINT uq_versions_hash UNIQUE (document_id, content_sha256),
    CONSTRAINT ck_versions_processing CHECK (processing IN ('pending', 'running', 'ready', 'failed')),
    CONSTRAINT ck_versions_generation CHECK (index_generation >= 0),
    CONSTRAINT ck_versions_ready_at CHECK (processing <> 'ready' OR ready_at IS NOT NULL)
);

-- Ссылка добавлена после обеих таблиц: указатель и версия ссылаются друг на друга.
ALTER TABLE documents
    ADD CONSTRAINT fk_documents_published_version
    FOREIGN KEY (published_version_id) REFERENCES document_versions (id) ON DELETE SET NULL;

CREATE INDEX idx_versions_document ON document_versions (document_id, created_at DESC);
CREATE INDEX idx_documents_publication ON documents (publication, created_at DESC);

CREATE TABLE extraction_runs (
    id             uuid        PRIMARY KEY,
    version_id     uuid        NOT NULL REFERENCES document_versions (id) ON DELETE CASCADE,
    parser         varchar(50) NOT NULL,
    parser_version varchar(50) NOT NULL,
    text_chars     integer     NOT NULL,
    quality        varchar(20) NOT NULL,
    warnings       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ck_extraction_quality CHECK (quality IN ('ok', 'low_text', 'empty')),
    CONSTRAINT ck_extraction_chars CHECK (text_chars >= 0)
);

CREATE INDEX idx_extraction_version ON extraction_runs (version_id, created_at DESC);

CREATE TABLE index_generations (
    id              uuid         PRIMARY KEY,
    version_id      uuid         NOT NULL REFERENCES document_versions (id) ON DELETE CASCADE,
    generation      integer      NOT NULL,
    embedding_model varchar(100) NULL,
    embedding_dim   integer      NULL,
    state           varchar(20)  NOT NULL,
    chunk_count     integer      NOT NULL DEFAULT 0,
    started_at      timestamptz  NOT NULL DEFAULT now(),
    finished_at     timestamptz  NULL,
    CONSTRAINT uq_generations UNIQUE (version_id, generation),
    CONSTRAINT ck_generations_state CHECK (state IN ('building', 'ready', 'failed')),
    CONSTRAINT ck_generations_dim CHECK (embedding_dim IS NULL OR embedding_dim > 0),
    CONSTRAINT ck_generations_ready CHECK (
        state <> 'ready' OR (embedding_model IS NOT NULL AND embedding_dim IS NOT NULL)
    )
);

-- Одна сборка на версию: две параллельные сборки дали бы разные chunk_id.
CREATE UNIQUE INDEX uq_generations_building
    ON index_generations (version_id) WHERE state = 'building';

-- document_id IS NULL — доступ ко всем документам, group_key IS NULL — ко всем
-- пользователям канала. Пустое сочетание и есть «вся внутренняя база».
CREATE TABLE access_grants (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id uuid        NULL REFERENCES documents (id) ON DELETE CASCADE,
    group_key   varchar(64) NULL,
    channel     varchar(20) NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ck_grants_channel CHECK (channel IN ('internal', 'public')),
    CONSTRAINT ck_grants_group_key CHECK (group_key ~ '^[a-z0-9][a-z0-9._-]{1,63}$'),
    CONSTRAINT uq_grants UNIQUE NULLS NOT DISTINCT (document_id, group_key, channel)
);

CREATE TABLE widget_installations (
    id              uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    key             varchar(64)  NOT NULL UNIQUE,
    name            varchar(200) NOT NULL,
    channel         varchar(20)  NOT NULL,
    allowed_origins text[]       NOT NULL,
    is_active       boolean      NOT NULL DEFAULT true,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT ck_installations_channel CHECK (channel IN ('internal', 'public')),
    CONSTRAINT ck_installations_origins CHECK (cardinality(allowed_origins) > 0)
);

CREATE TABLE page_bindings (
    id              uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id uuid         NOT NULL REFERENCES widget_installations (id) ON DELETE CASCADE,
    content_key     varchar(100) NOT NULL,
    document_id     uuid         NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    section_key     varchar(100) NULL,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT uq_binding UNIQUE (installation_id, content_key)
);

CREATE INDEX idx_bindings_document ON page_bindings (document_id);

CREATE TABLE outbox_events (
    id             bigserial   PRIMARY KEY,
    aggregate_type varchar(40) NOT NULL,
    aggregate_id   uuid        NOT NULL,
    event_type     varchar(60) NOT NULL,
    payload        jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz NULL,
    attempts       integer     NOT NULL DEFAULT 0,
    last_error     text        NULL
);

CREATE INDEX idx_outbox_pending ON outbox_events (id) WHERE published_at IS NULL;

CREATE TABLE idempotency_records (
    scope           varchar(50)  NOT NULL,
    idempotency_key varchar(200) NOT NULL,
    request_hash    char(64)     NOT NULL,
    status          varchar(20)  NOT NULL,
    response_status integer      NULL,
    response_body   jsonb        NULL,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    expires_at      timestamptz  NOT NULL,
    PRIMARY KEY (scope, idempotency_key),
    CONSTRAINT ck_idempotency_status CHECK (status IN ('in_progress', 'completed'))
);

CREATE INDEX idx_idempotency_expiry ON idempotency_records (expires_at);

CREATE TABLE audit_events (
    id             bigserial   PRIMARY KEY,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    actor_user_id  uuid        NULL,
    actor_kind     varchar(20) NOT NULL,
    action         varchar(60) NOT NULL,
    resource_type  varchar(40) NOT NULL,
    resource_id    varchar(100) NOT NULL,
    outcome        varchar(20) NOT NULL,
    request_id     varchar(100) NULL,
    trace_id       varchar(100) NULL,
    policy_version varchar(50) NULL,
    details        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT ck_audit_actor CHECK (actor_kind IN ('user', 'system', 'anonymous')),
    CONSTRAINT ck_audit_outcome CHECK (outcome IN ('ok', 'denied', 'failed'))
);

CREATE INDEX idx_audit_occurred ON audit_events (occurred_at DESC);

-- Публикация проверяется в БД, а не только в сервисе: прямой UPDATE не должен
-- сделать поисковым необработанную версию.
CREATE OR REPLACE FUNCTION enforce_publication() RETURNS trigger AS $$
DECLARE
    state varchar(20);
    owner uuid;
BEGIN
    IF NEW.published_version_id IS NULL THEN
        RETURN NEW;
    END IF;

    SELECT dv.processing, dv.document_id INTO state, owner
      FROM document_versions dv
     WHERE dv.id = NEW.published_version_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'published version % does not exist', NEW.published_version_id;
    END IF;
    IF owner <> NEW.id THEN
        RAISE EXCEPTION 'version % belongs to document %', NEW.published_version_id, owner;
    END IF;
    IF state <> 'ready' THEN
        RAISE EXCEPTION 'version % is not ready (processing=%)', NEW.published_version_id, state;
    END IF;
    IF NEW.lifecycle <> 'active' THEN
        RAISE EXCEPTION 'cannot publish document in lifecycle=%', NEW.lifecycle;
    END IF;

    NEW.publication := 'published';
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_enforce_publication
    BEFORE INSERT OR UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION enforce_publication();

CREATE OR REPLACE FUNCTION touch_document() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_documents_touch
    BEFORE UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION touch_document();

-- Опубликованную версию нельзя удалить: сначала документ выводят из публикации.
CREATE OR REPLACE FUNCTION forbid_published_delete() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM documents WHERE published_version_id = OLD.id) THEN
        RAISE EXCEPTION 'version % is published; withdraw it first', OLD.id;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_forbid_published_delete
    BEFORE DELETE ON document_versions
    FOR EACH ROW EXECUTE FUNCTION forbid_published_delete();

-- Журнал неизменяем: ни UPDATE, ни DELETE не проходят даже с правами владельца.
CREATE OR REPLACE FUNCTION forbid_audit_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_audit_append_only
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION forbid_audit_mutation();
