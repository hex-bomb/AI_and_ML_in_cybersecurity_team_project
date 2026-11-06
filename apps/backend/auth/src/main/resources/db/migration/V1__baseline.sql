-- Baseline после перехода на одну организацию: tenant_id убран.
-- Ключ группы стабилен и используется другими сервисами (documents хранит его
-- строкой), поэтому формат ограничен.

CREATE TABLE users (
    id            uuid         PRIMARY KEY,
    login         varchar(100) NOT NULL UNIQUE,
    name          varchar(300) NOT NULL,
    role          varchar(20)  NOT NULL,
    password_hash text         NOT NULL,
    is_active     boolean      NOT NULL DEFAULT true,
    created_at    timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT ck_users_role CHECK (role IN ('user', 'employee', 'admin'))
);

CREATE TABLE groups (
    id         uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    key        varchar(64)  NOT NULL UNIQUE,
    name       varchar(200) NOT NULL,
    created_at timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT ck_groups_key CHECK (key ~ '^[a-z0-9][a-z0-9._-]{1,63}$')
);

CREATE TABLE memberships (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    group_id   uuid        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, group_id)
);

CREATE INDEX idx_memberships_group ON memberships (group_id);
