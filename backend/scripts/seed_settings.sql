-- Настройки базы знаний (ТЗ 2.2, раздел 9).
--
-- ВАЖНО: настройки заводит и изменяет системный аналитик. Приложение их не
-- создаёт и не изменяет — ни при старте, ни в миграциях, ни по запросу.
-- Миграции создают только структуру таблицы, и после `alembic upgrade head`
-- таблица settings пуста (ТЗ 5.3). Этот скрипт — единственный способ внести
-- значения в локальной БД; выполняется вручную оператором.
--
-- Ключи и ограничения — каталог ТЗ раздел 9. Приоритет чтения:
-- настройка конкретной KB -> настройка tenant (kb_id IS NULL).
--
-- Значение search.embedding_dimension = 384 под локальный детерминированный
-- эмбеддер app/indexing/embeddings.py. При выборе реальной модели аналитик
-- указывает её фактическую размерность и меняет revision.
--
-- Запуск:
--   psql "$DATABASE_URL_SYNC" -f scripts/seed_settings.sql

BEGIN;

-- ── Уровень tenant: обязательные ключи индексации и поиска ──────────────
INSERT INTO settings (id, tenant_id, kb_id, key, value_type, subtype, value, updated_at)
VALUES
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'search.embedding_provider_id', 'string', 'model', '"primary"', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'search.embedding_model', 'string', 'model', '"local-hash"', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'search.embedding_revision', 'string', 'model', '"pilot-1"', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'search.embedding_dimension', 'integer', 'model', '384', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'search.embedding_normalize', 'boolean', 'model', 'true', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'search.embedding_distance', 'string', 'model', '"cosine"', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'chunking.tokenizer_id', 'string', 'ingestion', '"whitespace-approx"', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'chunking.max_tokens', 'integer', 'ingestion', '600', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'chunking.overlap_tokens', 'integer', 'ingestion', '100', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'search.top_k', 'integer', 'retrieval', '10', now())
ON CONFLICT DO NOTHING;

-- ── Уровень tenant: ключи генерации ответа (ТЗ 7.5) ───────────────────
INSERT INTO settings (id, tenant_id, kb_id, key, value_type, subtype, value, updated_at)
VALUES
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'answer.provider_id', 'string', 'model', '"local-ollama"', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'answer.model', 'string', 'model', '"qwen3:4b"', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'answer.tokenizer_id', 'string', 'generation', '"ollama-num_predict"', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'answer.max_context_tokens', 'integer', 'generation', '6000', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'answer.max_output_tokens', 'integer', 'generation', '1000', now()),
  (gen_random_uuid(), '11111111-1111-4111-8111-111111111111', NULL,
   'tools.enabled', 'array[string]', 'tools', '["kb_search"]', now())
ON CONFLICT DO NOTHING;

COMMIT;

-- Проверка:
--   SELECT key, value FROM settings ORDER BY key;