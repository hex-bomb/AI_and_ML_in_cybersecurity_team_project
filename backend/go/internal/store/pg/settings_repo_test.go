package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// Тесты настроек. Каталог ключей — docs/settings.md; здесь проверяется
// только чтение уровней и наследование по базе знаний (ТЗ раздел 9).

// TestLevelsSeparatesKBAndTenant — порт settings.py:117-129.
// Уровни возвращаются раздельно: склейка в адаптере потребовала бы второго
func TestLevelsSeparatesKBAndTenant(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewSettingsRepository()

	insertSetting(t, ctx, &tenantID, nil, "search.top_k", 10)
	insertSetting(t, ctx, &tenantID, nil, "search.embedding_model", "local-hash")
	insertSetting(t, ctx, &tenantID, &kbID, "search.top_k", 25)

	kbLevel, tenantLevel, err := repo.Levels(ctx, testDB, tenantID.String(), kbID.String())
	if err != nil {
		t.Fatalf("Levels: %v", err)
	}

	if got := kbLevel["search.top_k"]; got != int64(25) {
		t.Errorf("уровень KB search.top_k = %#v, ожидалось int64(25)", got)
	}
	if got := tenantLevel["search.top_k"]; got != int64(10) {
		t.Errorf("уровень tenant search.top_k = %#v, ожидалось int64(10)", got)
	}
	if _, exists := kbLevel["search.embedding_model"]; exists {
		t.Error("ключ уровня tenant попал в уровень KB")
	}
	if tenantLevel["search.embedding_model"] != "local-hash" {
		t.Errorf("уровень tenant search.embedding_model = %#v", tenantLevel["search.embedding_model"])
	}
}

// TestLevelsPreservesIntegerType — порт docs/settings.md, каталог
// search.embedding_dimension.
func TestLevelsPreservesIntegerType(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewSettingsRepository()

	insertSetting(t, ctx, &tenantID, nil, "search.embedding_dimension", 384)
	insertSetting(t, ctx, &tenantID, nil, "chunking.overlap_tokens", 100)
	insertSetting(t, ctx, &tenantID, nil, "search.embedding_normalize", true)
	insertSetting(t, ctx, &tenantID, nil, "tools.enabled", []string{"kb_search"})
	insertSettingRaw(t, ctx, &tenantID, nil, "answer.max_context_tokens", "0.5")

	kbLevel, tenantLevel, err := repo.Levels(ctx, testDB, tenantID.String(), kbID.String())
	if err != nil {
		t.Fatalf("Levels: %v", err)
	}

	if got := tenantLevel["search.embedding_dimension"]; got != int64(384) {
		t.Errorf("embedding_dimension = %#v (%T), ожидалось int64(384)", got, got)
	}
	if got := tenantLevel["chunking.overlap_tokens"]; got != int64(100) {
		t.Errorf("overlap_tokens = %#v (%T), ожидалось int64(100)", got, got)
	}
	if got := tenantLevel["search.embedding_normalize"]; got != true {
		t.Errorf("embedding_normalize = %#v (%T), ожидалось true", got, got)
	}
	tools, ok := tenantLevel["tools.enabled"].([]any)
	if !ok || len(tools) != 1 || tools[0] != "kb_search" {
		t.Errorf("tools.enabled = %#v, ожидался [kb_search]", tenantLevel["tools.enabled"])
	}
	// Дробное число остаётся float64: Python json.loads тоже различает
	// 384 и 384.0, и каталог отвергает дробное там, где нужно целое.
	if got := tenantLevel["answer.max_context_tokens"]; got != 0.5 {
		t.Errorf("max_context_tokens = %#v (%T), ожидалось float64(0.5)", got, got)
	}
	if len(kbLevel) != 0 {
		t.Errorf("уровень KB не должен быть пустым? %+v", kbLevel)
	}
}

// TestLevelsIgnoresForeignTenantAndKB — проверка без Python-теста.
// Настройки других tenant'ов и других баз не читаются никогда (docs/settings.md).
func TestLevelsIgnoresForeignTenantAndKB(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewSettingsRepository()

	foreignTenant := mustUUID(t, "99999999-9999-4999-8999-999999999999")
	insertTenant(t, ctx, foreignTenant)
	insertSetting(t, ctx, &tenantID, nil, "search.top_k", 10)
	insertSetting(t, ctx, &tenantID, &kbID, "chunking.max_tokens", 600)
	insertSecondKB(t, ctx)
	insertSetting(t, ctx, &tenantID, &otherKBID, "chunking.max_tokens", 999)
	insertSetting(t, ctx, &foreignTenant, nil, "search.top_k", 99)

	kbLevel, tenantLevel, err := repo.Levels(ctx, testDB, tenantID.String(), kbID.String())
	if err != nil {
		t.Fatalf("Levels: %v", err)
	}
	if tenantLevel["search.top_k"] != int64(10) {
		t.Errorf("верхний уровень искажён: %+v", tenantLevel)
	}
	if kbLevel["chunking.max_tokens"] != int64(600) {
		t.Errorf("настройка другой базы попала в результат: %+v", kbLevel)
	}
}

// TestLevelsWithoutKBReturnsOnlyTenantLevel — проверка без Python-теста.
func TestLevelsWithoutKBReturnsOnlyTenantLevel(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewSettingsRepository()

	insertSetting(t, ctx, &tenantID, nil, "search.top_k", 10)

	kbLevel, tenantLevel, err := repo.Levels(ctx, testDB, tenantID.String(), "")
	if err != nil {
		t.Fatalf("Levels: %v", err)
	}
	if len(kbLevel) != 0 {
		t.Errorf("уровень KB непуст без kb_id: %+v", kbLevel)
	}
	if tenantLevel["search.top_k"] != int64(10) {
		t.Errorf("верхний уровень: %+v", tenantLevel)
	}
}

// TestLevelsOnEmptyTableReturnsEmptyMaps — проверка без Python-теста.
// После alembic upgrade head таблица настроек пуста (docs/settings.md), и
func TestLevelsOnEmptyTableReturnsEmptyMaps(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := NewSettingsRepository()

	kbLevel, tenantLevel, err := repo.Levels(ctx, testDB, tenantID.String(), kbID.String())
	if err != nil {
		t.Fatalf("Levels: %v", err)
	}
	if kbLevel == nil || tenantLevel == nil {
		t.Fatalf("ожидались пустые карты, а не nil: %+v / %+v", kbLevel, tenantLevel)
	}
	if len(kbLevel) != 0 || len(tenantLevel) != 0 {
		t.Fatalf("ожидались пустые карты: %+v / %+v", kbLevel, tenantLevel)
	}
}

// TestLevelsRejectsMalformedTenantID — проверка без Python-теста.
func TestLevelsRejectsMalformedTenantID(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	repo := NewSettingsRepository()

	if _, _, err := repo.Levels(ctx, testDB, "не-uuid", kbID.String()); err == nil {
		t.Error("Levels принял неверный tenant_id")
	}
}

// ── helpers ────────────────────────────────────────────────────────

// insertTenant создаёт tenant с произвольным идентификатором.
func insertTenant(t *testing.T, ctx context.Context, id uuid.UUID) {
	t.Helper()
	if _, err := testDB.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, 'foreign')`, id); err != nil {
		t.Fatalf("создание tenant: %v", err)
	}
}

// insertSecondKB создаёт ещё одну базу знаний того же tenant'а.
func insertSecondKB(t *testing.T, ctx context.Context) {
	t.Helper()
	_, err := testDB.Exec(ctx, `
		INSERT INTO knowledge_bases (id, tenant_id, name) VALUES ($1, $2, 'kb-2')`,
		otherKBID, tenantID)
	if err != nil {
		t.Fatalf("создание второй базы знаний: %v", err)
	}
}

// insertSetting вносит настройку заданного уровня со значением any.
func insertSetting(t *testing.T, ctx context.Context, tenant, kb *uuid.UUID, key string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("кодирование значения настройки: %v", err)
	}
	insertSettingRaw(t, ctx, tenant, kb, key, string(payload))
}

// insertSettingRaw вносит настройку с готовым JSON-текстом значения.
func insertSettingRaw(
	t *testing.T, ctx context.Context, tenant, kb *uuid.UUID, key, rawValue string,
) {
	t.Helper()
	_, err := testDB.Exec(ctx, `
		INSERT INTO settings (id, tenant_id, kb_id, key, value_type, subtype, value)
		VALUES (gen_random_uuid(), $1, $2, $3, 'test', 'test', $4::jsonb)`,
		tenant, kb, key, rawValue)
	if err != nil {
		t.Fatalf("внесение настройки %s: %v", key, err)
	}
}

// mustUUID разбирает UUID или роняет тест.
func mustUUID(t *testing.T, value string) uuid.UUID {
	t.Helper()
	parsed, err := uuid.Parse(value)
	if err != nil {
		t.Fatalf("неверный UUID в тесте: %v", err)
	}
	return parsed
}
