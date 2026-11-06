-- Baseline базы диалогов. Сервис answer до этого был stateless; теперь он
-- владеет историей, цитатами и продуктовыми событиями.

CREATE TABLE conversations (
    id              uuid         PRIMARY KEY,
    channel         varchar(20)  NOT NULL,
    owner_subject   varchar(100) NOT NULL,
    installation_id uuid         NULL,
    status          varchar(20)  NOT NULL DEFAULT 'active',
    created_at      timestamptz  NOT NULL DEFAULT now(),
    last_turn_at    timestamptz  NULL,
    CONSTRAINT ck_conversations_channel CHECK (channel IN ('internal', 'public')),
    CONSTRAINT ck_conversations_status CHECK (status IN ('active', 'closed'))
);

CREATE INDEX idx_conversations_owner ON conversations (owner_subject, last_turn_at DESC);

CREATE TABLE turns (
    id                uuid         PRIMARY KEY,
    conversation_id   uuid         NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq               bigint       NOT NULL,
    client_message_id varchar(100) NOT NULL,
    question          text         NOT NULL,
    page_context      jsonb        NULL,
    state             varchar(20)  NOT NULL,
    answer            text         NULL,
    request_id        varchar(100) NULL,
    error_code        varchar(50)  NULL,
    guard_mode        varchar(20)  NULL,
    policy_version    varchar(50)  NULL,
    created_at        timestamptz  NOT NULL DEFAULT now(),
    completed_at      timestamptz  NULL,
    CONSTRAINT uq_turns_client_message UNIQUE (conversation_id, client_message_id),
    CONSTRAINT uq_turns_seq UNIQUE (conversation_id, seq),
    CONSTRAINT ck_turns_state
        CHECK (state IN ('accepted', 'running', 'completed', 'failed', 'cancelled', 'interrupted')),
    CONSTRAINT ck_turns_completed CHECK (state <> 'completed' OR completed_at IS NOT NULL)
);

-- Одна активная генерация на диалог: порядок реплик и повтор запроса остаются
-- предсказуемыми без блокировок в приложении.
CREATE UNIQUE INDEX uq_turns_active
    ON turns (conversation_id) WHERE state IN ('accepted', 'running');

CREATE TABLE citations (
    turn_id      uuid         NOT NULL REFERENCES turns (id) ON DELETE CASCADE,
    ordinal      integer      NOT NULL,
    document_id  uuid         NOT NULL,
    version_id   uuid         NOT NULL,
    chunk_id     varchar(200) NOT NULL,
    page_start   integer      NULL,
    page_end     integer      NULL,
    section_path text[]       NULL,
    PRIMARY KEY (turn_id, ordinal),
    CONSTRAINT ck_citations_ordinal CHECK (ordinal > 0),
    CONSTRAINT ck_citations_pages CHECK (
        page_start IS NULL OR page_end IS NULL OR page_end >= page_start
    )
);

CREATE TABLE product_events (
    id              bigserial   PRIMARY KEY,
    type            varchar(60) NOT NULL,
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    conversation_id uuid        NULL,
    turn_id         uuid        NULL,
    document_id     uuid        NULL,
    payload         jsonb       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX idx_product_events_type ON product_events (type, occurred_at DESC);
CREATE INDEX idx_product_events_document ON product_events (document_id, occurred_at DESC)
    WHERE document_id IS NOT NULL;

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
