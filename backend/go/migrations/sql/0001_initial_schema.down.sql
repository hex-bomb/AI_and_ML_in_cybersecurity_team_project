-- Откат полностью сносит схему. Восстановление данных не предусмотрено:
-- dev-стенд, и в рабочем окружении откат 0001 означает пересоздание базы.

DROP TABLE IF EXISTS api_idempotency;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS document_versions;
DROP TABLE IF EXISTS documents;
DROP TABLE IF EXISTS knowledge_bases;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS tenants;
