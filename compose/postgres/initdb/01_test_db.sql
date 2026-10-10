-- Тестовая база для интеграционных тестов Go (см. internal/infrastructure/postgres/schema_test.go).
-- Основная база — kb; тесты работают в отдельной, чтобы можно было
-- пересоздавать схему с нуля при каждом прогоне.
CREATE DATABASE kb_go_test OWNER kb;
