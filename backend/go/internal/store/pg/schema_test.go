package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Тесты пакета — против живого PostgreSQL (стенд compose.yaml; адрес —
// KB_GO_TEST_DATABASE_URL). Без БД и в -short — пропуск, не падение.

// defaultTestDSN — адрес стенда из compose.yaml и backend/.env.example.
const defaultTestDSN = "postgres://kb:dev@127.0.0.1:55432/kb_go_test?sslmode=disable"

// testSchemaSQL — копия миграции для подъёма без Python (при смене миграции
// обновить здесь; порядок таблиц повторяет миграцию из-за циклических ключей).
const testSchemaSQL = `
CREATE TABLE IF NOT EXISTS tenants (
    id   uuid PRIMARY KEY,
    name varchar(300) NOT NULL
);

CREATE TABLE IF NOT EXISTS knowledge_bases (
    id                   uuid PRIMARY KEY,
    tenant_id            uuid NOT NULL REFERENCES tenants(id),
    name                 varchar(300) NOT NULL,
    allowed_user_ids     jsonb NOT NULL DEFAULT '[]',
    allowed_group_ids    jsonb NOT NULL DEFAULT '[]',
    embedding_profile_id text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_kbs_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT ck_kbs_users_array  CHECK (jsonb_typeof(allowed_user_ids)  = 'array'),
    CONSTRAINT ck_kbs_groups_array CHECK (jsonb_typeof(allowed_group_ids) = 'array')
);

CREATE TABLE IF NOT EXISTS documents (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL,
    kb_id             uuid NOT NULL,
    external_id       varchar(300),
    acl_override      jsonb,
    active_version_id uuid,
    latest_version_id uuid,
    deleted_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_documents_tenant_id   UNIQUE (tenant_id, id),
    CONSTRAINT uq_document_external_id  UNIQUE (kb_id, external_id),
    CONSTRAINT fk_documents_kb FOREIGN KEY (tenant_id, kb_id)
        REFERENCES knowledge_bases (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS document_versions (
    id                  uuid PRIMARY KEY,
    tenant_id           uuid NOT NULL,
    document_id         uuid NOT NULL,
    filename            text NOT NULL,
    title               varchar(300) NOT NULL,
    source_url          text,
    metadata            jsonb NOT NULL,
    content_sha256      varchar(64) NOT NULL,
    original_key        text NOT NULL,
    parsed_key          text,
    status              text NOT NULL,
    failed_stage        text,
    error_code          text,
    error_message       text,
    embedding_profile_id text,
    indexing_config     jsonb,
    chunk_count         integer NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_versions_tenant_document_id UNIQUE (tenant_id, document_id, id),
    CONSTRAINT uq_versions_tenant_id          UNIQUE (tenant_id, id),
    CONSTRAINT ck_version_status    CHECK (status IN ('uploaded','parsing','parsed','indexing','ready','failed')),
    CONSTRAINT ck_version_chunk_count CHECK (chunk_count >= 0),
    CONSTRAINT fk_versions_document FOREIGN KEY (tenant_id, document_id)
        REFERENCES documents (tenant_id, id)
);

ALTER TABLE documents
    DROP CONSTRAINT IF EXISTS fk_document_active_version;
ALTER TABLE documents
    ADD CONSTRAINT fk_document_active_version
    FOREIGN KEY (tenant_id, id, active_version_id)
    REFERENCES document_versions (tenant_id, document_id, id);
ALTER TABLE documents
    DROP CONSTRAINT IF EXISTS fk_document_latest_version;
ALTER TABLE documents
    ADD CONSTRAINT fk_document_latest_version
    FOREIGN KEY (tenant_id, id, latest_version_id)
    REFERENCES document_versions (tenant_id, document_id, id);

CREATE INDEX IF NOT EXISTS ix_documents_listing
    ON documents (tenant_id, kb_id, created_at, id);

CREATE TABLE IF NOT EXISTS jobs (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL REFERENCES tenants(id),
    document_id       uuid NOT NULL,
    version_id        uuid,
    kind              text NOT NULL,
    stage             text NOT NULL,
    status            text NOT NULL,
    attempt           integer NOT NULL DEFAULT 0,
    next_attempt_at   timestamptz,
    lease_token       text,
    lease_expires_at  timestamptz,
    worker_id         text,
    processed_chunks  integer NOT NULL DEFAULT 0,
    total_chunks      integer,
    error_code        text,
    error_message     text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ck_job_kind   CHECK (kind   IN ('ingest','delete','cleanup_version')),
    CONSTRAINT ck_job_stage  CHECK (stage  IN ('convert','index','delete','cleanup')),
    CONSTRAINT ck_job_status CHECK (status IN ('queued','running','succeeded','failed','cancelled')),
    CONSTRAINT ck_job_attempt CHECK (attempt >= 0),
    CONSTRAINT ck_job_processed_chunks CHECK (processed_chunks >= 0),
    CONSTRAINT fk_jobs_document FOREIGN KEY (tenant_id, document_id)
        REFERENCES documents (tenant_id, id),
    CONSTRAINT fk_jobs_version FOREIGN KEY (tenant_id, document_id, version_id)
        REFERENCES document_versions (tenant_id, document_id, id)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_jobs_active_ingest
    ON jobs (document_id) WHERE kind = 'ingest' AND status IN ('queued','running');
CREATE UNIQUE INDEX IF NOT EXISTS uq_jobs_cleanup_version
    ON jobs (tenant_id, document_id, version_id) WHERE kind = 'cleanup_version';
CREATE INDEX IF NOT EXISTS ix_jobs_claim ON jobs (status, stage, next_attempt_at);
CREATE INDEX IF NOT EXISTS ix_jobs_lease ON jobs (lease_expires_at) WHERE status = 'running';

CREATE TABLE IF NOT EXISTS settings (
    id         uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL REFERENCES tenants(id),
    kb_id      uuid,
    key        text NOT NULL,
    value_type text NOT NULL,
    subtype    text NOT NULL,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fk_settings_kb FOREIGN KEY (tenant_id, kb_id)
        REFERENCES knowledge_bases (tenant_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_settings_tenant ON settings (tenant_id, key) WHERE kb_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_settings_kb     ON settings (tenant_id, kb_id, key) WHERE kb_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS api_idempotency (
    id              uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES tenants(id),
    user_id         varchar(300) NOT NULL,
    method          varchar(16) NOT NULL,
    path            varchar(1000) NOT NULL,
    key             varchar(128) NOT NULL,
    request_hash    varchar(64) NOT NULL,
    response_status integer NOT NULL,
    response_body   jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    CONSTRAINT ck_api_idempotency_key_length CHECK (char_length(key) BETWEEN 1 AND 128),
    CONSTRAINT uq_api_idempotency_key UNIQUE (tenant_id, user_id, method, path, key)
);
CREATE INDEX IF NOT EXISTS ix_api_idempotency_expires ON api_idempotency (expires_at);
`

// testTables — таблицы, которые очищаются перед каждым тестом.
// CASCADE обязателен: внешние ключи между documents и document_versions
const testTables = "api_idempotency, jobs, settings, document_versions, documents, knowledge_bases, tenants"

// testDB — общий пул на весь пакет тестов.
var testDB *DB

// TestMain поднимает пул один раз на пакет.
// БД может не быть — тогда тесты пропускаются с объяснением, а не падают:
func TestMain(m *testing.M) {
	short := isShortMode()
	if !short {
		if err := prepareTestDatabase(); err != nil {
			printDatabaseHint(err)
			os.Exit(m.Run())
		}
	}
	code := m.Run()
	if testDB != nil {
		testDB.Close()
	}
	os.Exit(code)
}

// printDatabaseHint объясняет, что делать, если базы нет.
func printDatabaseHint(err error) {
	println("PostgreSQL для интеграционных тестов недоступен:", err.Error())
	println("Подними стенд: docker start kbpg")
	println("и пересоздай базу: docker exec kbpg psql -U kb -d postgres -c \"CREATE DATABASE kb_go_test OWNER kb\";")
	println("Адрес переопределяется переменной KB_GO_TEST_DATABASE_URL.")
}

// prepareTestDatabase подключается и создаёт схему при первом запуске.
// Схема накатывается всегда с IF NOT EXISTS и безусловным добавлением
func prepareTestDatabase() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dsn := testDSN()
	db, err := New(ctx, Config{URL: dsn, MaxConns: 4})
	if err != nil {
		return err
	}
	testDB = db

	if _, err := db.Exec(ctx, testSchemaSQL); err != nil {
		testDB.Close()
		testDB = nil
		return err
	}
	resetTables(ctx)
	return nil
}

// testDSN отдаёт адрес тестовой базы.
func testDSN() string {
	if custom := os.Getenv("KB_GO_TEST_DATABASE_URL"); custom != "" {
		return custom
	}
	return defaultTestDSN
}

// isShortMode сообщает, запущены ли тесты в режиме -short.
// testing.Short() доступен только внутри теста, поэтому режим читается из
func isShortMode() bool {
	for _, arg := range os.Args[1:] {
		if arg == "-test.short" || arg == "--test.short" {
			return true
		}
		if arg == "-test.short=true" {
			return true
		}
	}
	return false
}

// resetTables очищает все таблицы.
// TRUNCATE, а не DELETE: DELETE вызывает триггеры и вакансирует идентификаторы
func resetTables(ctx context.Context) {
	if testDB == nil {
		return
	}
	if _, err := testDB.Exec(ctx, "TRUNCATE "+testTables+" CASCADE"); err != nil {
		panic("очистка тестовой базы: " + err.Error())
	}
}

// requireDB подготавливает тест к работе с базой: пропускает его в режиме
// -short или когда база недоступна, иначе очищает таблицы.
func requireDB(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("интеграционный тест: нужен PostgreSQL (запустите без -short)")
	}
	if testDB == nil {
		t.Skip("PostgreSQL для тестов недоступен: поднимите контейнер kbpg")
	}
	ctx := testContext(t)
	resetTables(ctx)
}

// testContext возвращает контекст с разумным таймаутом.
// Таймаут обязателен: зависший запрос к недоступной базе иначе висит до
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ── Данные для сидов ─────────────────────────────────────────────────

// Константы идентификаторов повторяют test_jobs.py/test_infrastructure.py:
// один и тот же эталонный набор строк позволяет сопоставлять тесты Go и
var (
	tenantID    = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	kbID        = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	otherKBID   = uuid.MustParse("22222222-2222-4222-8222-222222222223")
	otherTenant = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	docID       = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	otherDoc    = uuid.MustParse("33333333-3333-4333-8333-333333333334")
	versionID   = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	secondVer   = uuid.MustParse("44444444-4444-4444-8444-444444444445")
	otherVer    = uuid.MustParse("44444444-4444-4444-8444-444444444446")
	jobID       = uuid.MustParse("66666666-6666-4666-8666-666666666666")

	// profileID — фиктивный профиль эмбеддинга. Настоящий считается
	// domain.IndexingConfig.ProfileID(), но здесь важна только стабильность
	profileID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// seedOptions задаёт, какие строки создаёт seedChain.
type seedOptions struct {
	// kbUsers и kbGroups — ACL базы знаний.
	kbUsers  []string
	kbGroups []string
	// stage и status задания по умолчанию: convert/queued.
	stage  string
	status string
	// sourceURL версии; nil — колонка остаётся NULL.
	sourceURL *string
	// embeddingProfile базы; nil — профиль ещё не зафиксирован.
	embeddingProfile *string
	// parsedKey версии; nil — parsed_key остаётся NULL.
	parsedKey *string
	// externalID документа; nil — внешнего идентификатора нет.
	externalID *string
}

// seedChain создаёт цепочку tenant -> KB -> document -> version -> job.
// Порядок вставок фиксирован внешними ключами: версия ссылается на
func seedChain(t *testing.T, ctx context.Context, opts seedOptions) {
	t.Helper()

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		steps := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO tenants (id, name) VALUES ($1, 'local')`, []any{tenantID}},
			{
				`INSERT INTO knowledge_bases (id, tenant_id, name, allowed_user_ids, allowed_group_ids,
				                             embedding_profile_id)
				 VALUES ($1, $2, 'kb', $3::jsonb, $4::jsonb, $5)`,
				[]any{kbID, tenantID, jsonArray(opts.kbUsers), jsonArray(opts.kbGroups), opts.embeddingProfile},
			},
			{
				`INSERT INTO documents (id, tenant_id, kb_id, external_id) VALUES ($1, $2, $3, $4)`,
				[]any{docID, tenantID, kbID, opts.externalID},
			},
			{
				`INSERT INTO document_versions (id, tenant_id, document_id, filename, title, source_url,
				                               metadata, content_sha256, original_key, parsed_key, status)
				 VALUES ($1, $2, $3, 'guide.pdf', 'Инструкция', $4, '{}'::jsonb, $5, $6, $7, 'uploaded')`,
				[]any{versionID, tenantID, docID, opts.sourceURL, repeatA64(), versionID.String() + "/original.bin", opts.parsedKey},
			},
			{
				`UPDATE documents SET latest_version_id = $1 WHERE id = $2`,
				[]any{versionID, docID},
			},
		}
		for _, step := range steps {
			if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("подготовка данных: %v", err)
	}

	if err := insertJob(t, ctx, jobID, defaultString(opts.stage, domain.StageConvert),
		defaultString(opts.status, domain.JobStatusQueued)); err != nil {
		t.Fatalf("создание задания: %v", err)
	}
}

// defaultString подставляет значение по умолчанию для пустой строки.
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// insertJob создаёт ingest-задание для версии.
// Стадия и статус задаются явно, потому что почти каждый тест очереди
func insertJob(t *testing.T, ctx context.Context, id uuid.UUID, stage, status string) error {
	t.Helper()
	_, err := testDB.Exec(ctx, `
		INSERT INTO jobs (id, tenant_id, document_id, version_id, kind, stage, status, attempt)
		VALUES ($1, $2, $3, $4, 'ingest', $5, $6, 0)`,
		id, tenantID, docID, versionID, stage, status)
	return err
}

// repeatA64 даёт 64 символа «a» — валидный content_sha256.
func repeatA64() string {
	const pattern = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return pattern
}

// seedTenant создаёт только tenant.
// Нужна тестам, которые сами создают базы знаний: seedChain заодно создаёт
func seedTenant(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := testDB.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, 'local')`, tenantID); err != nil {
		t.Fatalf("создание tenant: %v", err)
	}
}

// readDocumentACL читает acl_override документа.
// Ключи достаются через оператор -> : documents.acl_override — объект с
func readDocumentACL(t *testing.T, ctx context.Context, id uuid.UUID) domain.ACL {
	t.Helper()
	var users, groups []string
	err := testDB.QueryRow(ctx, `
		SELECT COALESCE(acl_override->'allowed_user_ids', '[]'::jsonb),
		       COALESCE(acl_override->'allowed_group_ids', '[]'::jsonb)
		FROM documents WHERE id = $1`, id,
	).Scan(&users, &groups)
	if err != nil {
		t.Fatalf("чтение acl_override: %v", err)
	}
	return domain.ACL{Users: users, Groups: groups}
}

// jsonArray кодирует срез строк как JSON-массив для jsonb-колонки.
func jsonArray(values []string) []byte {
	if values == nil {
		return []byte("[]")
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		// Срез строк всегда сериализуется; ошибка здесь означила бы, что в
		// тест попала испорченная константа, и падать надо сразу.
		panic("кодирование тестового массива: " + err.Error())
	}
	return encoded
}

// execRaw выполняет утверждение напрямую.
// Нужен тестам, которые проверяют состояние, недоступное через порты:
func execRaw(t *testing.T, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := testDB.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("подготовка состояния (%s): %v", sql, err)
	}
}
