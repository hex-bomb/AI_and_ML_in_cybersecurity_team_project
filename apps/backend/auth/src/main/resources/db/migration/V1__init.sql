CREATE TABLE auth_users (
    id            uuid         PRIMARY KEY,
    tenant_id     uuid         NOT NULL,
    login         varchar(100) NOT NULL UNIQUE,
    name          varchar(300) NOT NULL,
    role          varchar(20)  NOT NULL,
    password_hash text         NOT NULL,
    created_at    timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT ck_auth_users_role CHECK (role IN ('user', 'employee'))
);
