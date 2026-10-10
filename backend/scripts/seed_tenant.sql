-- Локальный tenant для стенда (ТЗ 2.2, раздел 9).
--
-- ВАЖНО: это НЕ миграция и не часть приложения. Миграция 0001 создаёт только
-- структуру; рабочие tenants, ACL и настройки не засеваются миграциями.
-- В рабочем окружении tenant приходит из доверенного JWT-издателя, и этот
-- файл не нужен.
--
-- Локальной сборке tenant нужен для того, чтобы POST /kbs прошёл внешний ключ
-- knowledge_bases.tenant_id -> tenants.id: без записи приложение возвращает
-- 500 INTERNAL_ERROR. Идентификатор совпадает с LOCAL_TENANT_ID из
-- app/contracts/models.py, то есть с tenant, под которым работает
-- локальный Principal.
--
-- Запуск (идемпотентно, повторный запуск безопасен):
--   psql "$DATABASE_URL_SYNC" -f scripts/seed_tenant.sql
--   docker exec -i <postgres> psql -U kb -d kb -f - < scripts/seed_tenant.sql

INSERT INTO tenants (id, name)
VALUES ('11111111-1111-4111-8111-111111111111', 'local')
ON CONFLICT (id) DO NOTHING;

-- Проверка:
--   SELECT id, name FROM tenants;