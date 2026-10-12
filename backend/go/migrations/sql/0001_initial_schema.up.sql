-- Первичная схема: 7 таблиц (ТЗ 5.3).
--
-- SQL снят с Alembic-миграции 0001 командой `alembic upgrade head --sql`, а не
-- переписан руками: так схема гарантированно совпадает с той, что применялась
-- на стендах. Alembic вместе с Python удалён, версионированием занимается
-- golang-migrate.
--
-- Отличается от исходной миграции одной строкой: таблицу alembic_version
-- создаёт golang-migrate, поэтому её объявления здесь нет.


CREATE TABLE tenants (
    id UUID NOT NULL, 
    name VARCHAR(300) NOT NULL, 
    PRIMARY KEY (id)
);

CREATE TABLE knowledge_bases (
    id UUID NOT NULL, 
    tenant_id UUID NOT NULL, 
    name VARCHAR(300) NOT NULL, 
    allowed_user_ids JSONB DEFAULT '[]' NOT NULL, 
    allowed_group_ids JSONB DEFAULT '[]' NOT NULL, 
    embedding_profile_id VARCHAR, 
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL, 
    PRIMARY KEY (id), 
    CONSTRAINT ck_kbs_groups_array CHECK (jsonb_typeof(allowed_group_ids) = 'array'), 
    CONSTRAINT ck_kbs_users_array CHECK (jsonb_typeof(allowed_user_ids) = 'array'), 
    FOREIGN KEY(tenant_id) REFERENCES tenants (id), 
    CONSTRAINT uq_kbs_tenant_id UNIQUE (tenant_id, id)
);

CREATE TABLE documents (
    id UUID NOT NULL, 
    tenant_id UUID NOT NULL, 
    kb_id UUID NOT NULL, 
    external_id VARCHAR(300), 
    acl_override JSONB, 
    active_version_id UUID, 
    latest_version_id UUID, 
    deleted_at TIMESTAMP WITH TIME ZONE, 
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL, 
    PRIMARY KEY (id), 
    FOREIGN KEY(tenant_id, kb_id) REFERENCES knowledge_bases (tenant_id, id), 
    CONSTRAINT uq_document_external_id UNIQUE (kb_id, external_id), 
    CONSTRAINT uq_documents_tenant_id UNIQUE (tenant_id, id)
);

CREATE INDEX ix_documents_listing ON documents (tenant_id, kb_id, created_at, id);

CREATE TABLE document_versions (
    id UUID NOT NULL, 
    tenant_id UUID NOT NULL, 
    document_id UUID NOT NULL, 
    filename VARCHAR NOT NULL, 
    title VARCHAR(300) NOT NULL, 
    source_url VARCHAR, 
    metadata JSONB NOT NULL, 
    content_sha256 VARCHAR(64) NOT NULL, 
    original_key VARCHAR NOT NULL, 
    parsed_key VARCHAR, 
    status VARCHAR NOT NULL, 
    failed_stage VARCHAR, 
    error_code VARCHAR, 
    error_message VARCHAR, 
    embedding_profile_id VARCHAR, 
    indexing_config JSONB, 
    chunk_count INTEGER DEFAULT '0' NOT NULL, 
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL, 
    PRIMARY KEY (id), 
    CONSTRAINT ck_version_status CHECK (status IN ('uploaded','parsing','parsed','indexing','ready','failed')), 
    CONSTRAINT ck_version_chunk_count CHECK (chunk_count >= 0), 
    FOREIGN KEY(tenant_id, document_id) REFERENCES documents (tenant_id, id), 
    CONSTRAINT uq_versions_tenant_document_id UNIQUE (tenant_id, document_id, id), 
    CONSTRAINT uq_versions_tenant_id UNIQUE (tenant_id, id)
);

ALTER TABLE documents ADD CONSTRAINT fk_document_active_version FOREIGN KEY(tenant_id, id, active_version_id) REFERENCES document_versions (tenant_id, document_id, id);

ALTER TABLE documents ADD CONSTRAINT fk_document_latest_version FOREIGN KEY(tenant_id, id, latest_version_id) REFERENCES document_versions (tenant_id, document_id, id);

CREATE TABLE jobs (
    id UUID NOT NULL, 
    tenant_id UUID NOT NULL, 
    document_id UUID NOT NULL, 
    version_id UUID, 
    kind VARCHAR NOT NULL, 
    stage VARCHAR NOT NULL, 
    status VARCHAR NOT NULL, 
    attempt INTEGER DEFAULT '0' NOT NULL, 
    next_attempt_at TIMESTAMP WITH TIME ZONE, 
    lease_token VARCHAR, 
    lease_expires_at TIMESTAMP WITH TIME ZONE, 
    worker_id VARCHAR, 
    processed_chunks INTEGER DEFAULT '0' NOT NULL, 
    total_chunks INTEGER, 
    error_code VARCHAR, 
    error_message VARCHAR, 
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL, 
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL, 
    PRIMARY KEY (id), 
    CONSTRAINT ck_job_kind CHECK (kind IN ('ingest','delete','cleanup_version')), 
    CONSTRAINT ck_job_stage CHECK (stage IN ('convert','index','delete','cleanup')), 
    CONSTRAINT ck_job_status CHECK (status IN ('queued','running','succeeded','failed','cancelled')), 
    CONSTRAINT ck_job_attempt CHECK (attempt >= 0), 
    CONSTRAINT ck_job_processed_chunks CHECK (processed_chunks >= 0), 
    FOREIGN KEY(tenant_id) REFERENCES tenants (id), 
    FOREIGN KEY(tenant_id, document_id) REFERENCES documents (tenant_id, id), 
    FOREIGN KEY(tenant_id, document_id, version_id) REFERENCES document_versions (tenant_id, document_id, id)
);

CREATE UNIQUE INDEX uq_jobs_active_ingest ON jobs (document_id) WHERE kind = 'ingest' AND status IN ('queued','running');

CREATE UNIQUE INDEX uq_jobs_cleanup_version ON jobs (tenant_id, document_id, version_id) WHERE kind = 'cleanup_version';

CREATE INDEX ix_jobs_claim ON jobs (status, stage, next_attempt_at);

CREATE INDEX ix_jobs_lease ON jobs (lease_expires_at) WHERE status = 'running';

CREATE TABLE settings (
    id UUID NOT NULL, 
    tenant_id UUID NOT NULL, 
    kb_id UUID, 
    key VARCHAR NOT NULL, 
    value_type VARCHAR NOT NULL, 
    subtype VARCHAR NOT NULL, 
    value JSONB NOT NULL, 
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL, 
    PRIMARY KEY (id), 
    FOREIGN KEY(tenant_id) REFERENCES tenants (id), 
    FOREIGN KEY(tenant_id, kb_id) REFERENCES knowledge_bases (tenant_id, id)
);

CREATE UNIQUE INDEX uq_settings_kb ON settings (tenant_id, kb_id, key) WHERE kb_id IS NOT NULL;

CREATE UNIQUE INDEX uq_settings_tenant ON settings (tenant_id, key) WHERE kb_id IS NULL;

CREATE TABLE api_idempotency (
    id UUID NOT NULL, 
    tenant_id UUID NOT NULL, 
    user_id VARCHAR(300) NOT NULL, 
    method VARCHAR(16) NOT NULL, 
    path VARCHAR(1000) NOT NULL, 
    key VARCHAR(128) NOT NULL, 
    request_hash VARCHAR(64) NOT NULL, 
    response_status INTEGER NOT NULL, 
    response_body JSONB NOT NULL, 
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL, 
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL, 
    PRIMARY KEY (id), 
    CONSTRAINT ck_api_idempotency_key_length CHECK (char_length(key) BETWEEN 1 AND 128), 
    FOREIGN KEY(tenant_id) REFERENCES tenants (id), 
    CONSTRAINT uq_api_idempotency_key UNIQUE (tenant_id, user_id, method, path, key)
);

CREATE INDEX ix_api_idempotency_expires ON api_idempotency (expires_at);

