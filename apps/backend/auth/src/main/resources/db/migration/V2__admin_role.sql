-- Сервисный аккаунт admin (dev): расширяем допустимые роли.
-- Сиды с профилем dev создают пользователя с ролью admin.
ALTER TABLE auth_users DROP CONSTRAINT ck_auth_users_role;
ALTER TABLE auth_users
    ADD CONSTRAINT ck_auth_users_role CHECK (role IN ('user', 'employee', 'admin'));
