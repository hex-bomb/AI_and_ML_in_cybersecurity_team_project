CREATE TABLE documents (
    id          uuid PRIMARY KEY,
    kb_id       varchar(64)  NOT NULL,
    external_id varchar(200) NOT NULL,
    title       varchar(300) NOT NULL,
    audience    varchar(20)  NOT NULL,
    section     varchar(200) NOT NULL,
    created_at  timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT uq_documents_external UNIQUE (kb_id, external_id),
    CONSTRAINT ck_documents_audience CHECK (audience IN ('public', 'employee'))
);

CREATE TABLE document_versions (
    id             uuid PRIMARY KEY,
    document_id     uuid        NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    content_sha256  char(64)    NOT NULL,
    version_label   varchar(50) NOT NULL,
    full_text       text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_versions_hash UNIQUE (document_id, content_sha256)
);

CREATE INDEX idx_versions_document ON document_versions (document_id, created_at DESC);
