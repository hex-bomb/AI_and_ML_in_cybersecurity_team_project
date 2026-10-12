package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Тесты каталога и разрешения настроек работают на подставном репозитории:
// проверять правила приоритета и валидации нужно на всех сочетаниях уровней, а
// не на тех, что случайно оказались в тестовой базе. Интеграционный тест с
// живым PostgreSQL — settings_db_test.go.

// fakeRepo — репозиторий настроек из двух карт.
//
// Карты отдаются как есть: адаптер отвечает за JSON-разбор и целочисленные
// типы, а пакет settings — за приоритет уровней и проверку значений.
type fakeRepo struct {
	kbLevel     map[string]any
	tenantLevel map[string]any
	err         error
	calls       int
}

// Levels реализует ports.SettingsRepository.
func (f *fakeRepo) Levels(
	_ context.Context, _ ports.Queryer, _, _ string,
) (map[string]any, map[string]any, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.kbLevel, f.tenantLevel, nil
}

func newFakeRepo(tenantLevel, kbLevel map[string]any) *fakeRepo {
	return &fakeRepo{tenantLevel: tenantLevel, kbLevel: kbLevel}
}

// baselineKeys — значения локального стенда (docs/settings.md, «Значения
// локального стенда»). Числа — int64: jsonb отдаёт целые как int64, и
// подставлять int32 проверяло бы не тот тип, который приходит из базы.
func baselineKeys() map[string]any {
	return map[string]any{
		EmbeddingProviderID.Name():    "primary",
		EmbeddingModel.Name():         "local-hash",
		EmbeddingRevision.Name():      "pilot-1",
		EmbeddingDimension.Name():     int64(384),
		EmbeddingNormalize.Name():     true,
		EmbeddingDistance.Name():      "cosine",
		ChunkingTokenizerID.Name():    "whitespace-approx",
		ChunkingMaxTokens.Name():      int64(600),
		ChunkingOverlapTokens.Name():  int64(100),
		SearchTopK.Name():             int64(10),
		AnswerProviderID.Name():       "local-deterministic",
		AnswerModel.Name():            "stub-1",
		AnswerTokenizerID.Name():      "whitespace-approx",
		AnswerMaxContextTokens.Name(): int64(6000),
		AnswerMaxOutputTokens.Name():  int64(1000),
		ToolsEnabled.Name():           []any{"kb_search"},
	}
}

// withKey возвращает копию карты с одним изменённым значением.
func withKey(base map[string]any, key Key, value any) map[string]any {
	out := make(map[string]any, len(base)+1)
	for name, existing := range base {
		out[name] = existing
	}
	out[key.Name()] = value
	return out
}

func newService(tenantLevel, kbLevel map[string]any) (*Service, *fakeRepo) {
	repo := newFakeRepo(tenantLevel, kbLevel)
	return New(repo), repo
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// ── Каталог ────────────────────────────────────────────────────────

// TestCatalogMatchesDocsSettings фиксирует каталог ключей.
//
// Состав, тип, подтип и обязательность взяты из docs/settings.md. Тест падает
// при добавлении, удалении или переименовании ключа: это единственное место,
// где каталог Go сверяется с эталоном, и без него новый ключ появился бы в
// коде, но не в документации для аналитика.
func TestCatalogMatchesDocsSettings(t *testing.T) {
	want := []struct {
		key       Key
		valueType ValueType
		subtype   string
	}{
		{EmbeddingProviderID, TypeString, SubtypeModel},
		{EmbeddingModel, TypeString, SubtypeModel},
		{EmbeddingRevision, TypeString, SubtypeModel},
		{EmbeddingDimension, TypeInteger, SubtypeModel},
		{EmbeddingNormalize, TypeBoolean, SubtypeModel},
		{EmbeddingDistance, TypeString, SubtypeModel},
		{ChunkingTokenizerID, TypeString, SubtypeIngestion},
		{ChunkingMaxTokens, TypeInteger, SubtypeIngestion},
		{ChunkingOverlapTokens, TypeInteger, SubtypeIngestion},
		{SearchTopK, TypeInteger, SubtypeRetrieval},
		{AnswerProviderID, TypeString, SubtypeModel},
		{AnswerModel, TypeString, SubtypeModel},
		{AnswerTokenizerID, TypeString, SubtypeGeneration},
		{AnswerMaxContextTokens, TypeInteger, SubtypeGeneration},
		{AnswerMaxOutputTokens, TypeInteger, SubtypeGeneration},
		{ToolsEnabled, TypeStringArray, SubtypeTools},
	}

	catalog := Catalog()
	if len(catalog) != len(want) {
		t.Fatalf("в каталоге %d ключей, в docs/settings.md %d", len(catalog), len(want))
	}
	seen := make(map[string]bool, len(catalog))
	for i, key := range catalog {
		if key.Name() != want[i].key.Name() {
			t.Errorf("ключ %d: %q, ожидался %q", i, key.Name(), want[i].key.Name())
		}
		if key.ValueType() != want[i].valueType {
			t.Errorf("%s: тип %q, ожидался %q", key.Name(), key.ValueType(), want[i].valueType)
		}
		if key.Subtype() != want[i].subtype {
			t.Errorf("%s: подтип %q, ожидался %q", key.Name(), key.Subtype(), want[i].subtype)
		}
		if !key.Required() {
			t.Errorf("%s: ключ не обязателен, а молчаливых значений по умолчанию нет", key.Name())
		}
		if key.HasDefault() {
			t.Errorf("%s: у ключа есть значение по умолчанию, каталог их запрещает", key.Name())
		}
		if seen[key.Name()] {
			t.Errorf("ключ %s объявлен дважды", key.Name())
		}
		seen[key.Name()] = true
	}
}

// TestCatalogOrderIsFixed — диагностика обязана быть воспроизводимой.
func TestCatalogOrderIsFixed(t *testing.T) {
	first := Catalog()
	for range 5 {
		next := Catalog()
		for i := range first {
			if first[i] != next[i] {
				t.Fatalf("порядок каталога изменился на позиции %d", i)
			}
		}
	}
	// Срез каталога не должен влиять на последующие вызовы.
	first[0] = Key{name: "сломанный ключ"}
	if Catalog()[0].Name() == "сломанный ключ" {
		t.Fatal("Catalog() отдал общий срез: изменение затронуло бы следующий вызов")
	}
}

// TestKeySetsAreNestedAndComplete — наборы ключей по операциям (ТЗ 3.3, 3.5).
func TestKeySetsAreNestedAndComplete(t *testing.T) {
	if len(EmbeddingKeys) != 9 {
		t.Fatalf("EmbeddingKeys: %d ключей, ожидалось 9", len(EmbeddingKeys))
	}
	if len(SearchKeys) != len(EmbeddingKeys)+1 {
		t.Errorf("SearchKeys должен добавлять ровно search.top_k")
	}
	if len(AnswerKeys) != len(SearchKeys)+6 {
		t.Errorf("AnswerKeys должен добавлять ровно шесть ключей ответа")
	}
	if len(AllKeys) != len(AnswerKeys) {
		t.Errorf("AllKeys должен содержать весь каталог")
	}
	// Наборы — разные срезы: иначе append одного расширил бы другой.
	if &SearchKeys[0] == &AllKeys[0] {
		t.Error("наборы ключей делят один буфер")
	}
}

// ── Приоритет уровней ─────────────────────────────────────────────

// TestKBLevelOverridesTenantLevel — перекрытие уровней (ТЗ раздел 9).
func TestKBLevelOverridesTenantLevel(t *testing.T) {
	tenant := baselineKeys()
	kb := map[string]any{EmbeddingModel.Name(): "kb-model"}
	service, _ := newService(tenant, kb)

	cfg, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb")
	if err != nil {
		t.Fatalf("IndexingConfig: %v", err)
	}
	if cfg.EmbeddingModel != "kb-model" {
		t.Errorf("модель %q, ожидалось значение уровня KB", cfg.EmbeddingModel)
	}
	// Остальные ключи берутся с уровня tenant: перекрытие точечное.
	if cfg.ChunkMaxTokens != 600 {
		t.Errorf("chunk_max_tokens %d, ожидалось 600 с уровня tenant", cfg.ChunkMaxTokens)
	}
	if cfg.EmbeddingDimension != 384 {
		t.Errorf("размерность %d, ожидалось 384", cfg.EmbeddingDimension)
	}
	if !cfg.NormalizeVectors {
		t.Error("normalize должен остаться true с уровня tenant")
	}
}

// TestSourceOfReportsLevelWhereValueCameFrom — диагностика показывает уровень.
func TestSourceOfReportsLevelWhereValueCameFrom(t *testing.T) {
	service, _ := newService(baselineKeys(), map[string]any{
		ChunkingMaxTokens.Name(): int64(800),
	})
	values, err := service.Resolve(testCtx(t), nil, "tenant", "kb", SearchKeys)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := values.SourceOf(ChunkingMaxTokens); got != SourceKB {
		t.Errorf("chunking.max_tokens: источник %q, ожидался kb", got)
	}
	if got := values.SourceOf(SearchTopK); got != SourceTenant {
		t.Errorf("search.top_k: источник %q, ожидался tenant", got)
	}
}

// ── Обязательные ключи ─────────────────────────────────────────────

// TestResolveListsEveryMissingKey — CONFIGURATION_MISSING со всем списком.
func TestResolveListsEveryMissingKey(t *testing.T) {
	tenant := baselineKeys()
	delete(tenant, ChunkingMaxTokens.Name())
	delete(tenant, ChunkingOverlapTokens.Name())
	service, _ := newService(tenant, nil)

	_, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb")
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ожидалась AppError, получено %v", err)
	}
	if appErr.Code != domain.CodeConfigurationMissing {
		t.Fatalf("код %q, ожидался %q", appErr.Code, domain.CodeConfigurationMissing)
	}
	keys := missingKeys(t, appErr)
	for _, want := range []string{ChunkingMaxTokens.Name(), ChunkingOverlapTokens.Name()} {
		if !contains(keys, want) {
			t.Errorf("в details.keys нет %q: %v", want, keys)
		}
	}
	if len(keys) != 2 {
		t.Errorf("details.keys = %v, ожидались ровно два ключа", keys)
	}
	// Заданный ключ в список не попадает.
	if contains(keys, SearchTopK.Name()) {
		t.Errorf("заданный ключ попал в список недостающих: %v", keys)
	}
}

// TestResolveListsMissingKeysSortedAndComplete — список недостающих ключей
// отсортирован и покрывает весь затребованный набор.
//
// Один ключ за раз чинить нельзя: аналитику пришлось бы перезапрашивать
// диагностику после каждой правки (ТЗ раздел 9, details.keys).
func TestResolveListsMissingKeysSortedAndComplete(t *testing.T) {
	service, _ := newService(map[string]any{}, map[string]any{})

	_, err := service.Resolve(testCtx(t), nil, "tenant", "kb", AnswerKeys)
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ожидалась AppError, получено %v", err)
	}
	keys := missingKeys(t, appErr)
	if len(keys) != len(AnswerKeys) {
		t.Fatalf("details.keys: %d ключей, ожидалось %d", len(keys), len(AnswerKeys))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Fatalf("details.keys не отсортированы: %v", keys)
		}
	}
}

// TestResolveOnEmptyCatalogFails — после alembic upgrade head таблица пуста.
func TestResolveOnEmptyCatalogFails(t *testing.T) {
	service, repo := newService(map[string]any{}, map[string]any{})

	_, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb")
	if appErr, ok := domain.AsAppError(err); !ok || appErr.Code != domain.CodeConfigurationMissing {
		t.Fatalf("ожидался CONFIGURATION_MISSING, получено %v", err)
	}
	// Настройки никогда не создаются программно (ТЗ 2.2): единственное
	// обращение к хранилищу — чтение.
	if repo.calls != 1 {
		t.Errorf("обращений к репозиторию %d, ожидалось одно чтение", repo.calls)
	}
}

// TestResolvePropagatesRepositoryError — сбой чтения не превращается в 409.
func TestResolvePropagatesRepositoryError(t *testing.T) {
	service, repo := newService(nil, nil)
	repo.err = context.DeadlineExceeded

	if _, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb"); err == nil {
		t.Fatal("сбой репозитория проглочен")
	}
}

func TestResolveHonoursCancelledContext(t *testing.T) {
	service, _ := newService(baselineKeys(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := service.IndexingConfig(ctx, nil, "tenant", "kb"); err == nil {
		t.Fatal("отменённый контекст проигнорирован")
	}
}

// ── Проверка значений ──────────────────────────────────────────────

// TestResolveRejectsWrongTypeWithKeyAndValue — CONFIGURATION_INVALID.
//
// details обязаны называть ключ, ожидаемый тип и фактическое значение: без них
// аналитик видит только имя ключа и должен сам догадываться, что записано в
// settings.value (ТЗ раздел 9).
func TestResolveRejectsWrongTypeWithKeyAndValue(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), EmbeddingNormalize, "true"), nil)

	_, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb")
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeConfigurationInvalid {
		t.Fatalf("ожидался CONFIGURATION_INVALID, получено %v", err)
	}
	if appErr.Details["key"] != EmbeddingNormalize.Name() {
		t.Errorf("details.key = %v, ожидался %q", appErr.Details["key"], EmbeddingNormalize.Name())
	}
	if appErr.Details["expected_type"] != string(TypeBoolean) {
		t.Errorf("details.expected_type = %v, ожидался %q", appErr.Details["expected_type"], TypeBoolean)
	}
	if appErr.Details["value"] != "true" {
		t.Errorf("details.value = %v, ожидалось фактическое значение \"true\"", appErr.Details["value"])
	}
	if reason, _ := appErr.Details["reason"].(string); reason == "" {
		t.Error("details.reason пуст")
	}
}

// TestResolveRejectsFractionalInteger — 384.0 отличается от 384.
//
// Адаптер отдаёт из jsonb целые как int64, а дробные как float64: проверка
// «положительное целое» обязана различать их, иначе аналитик записал бы
// 384.5 и получил бы вектор неверной размерности.
func TestResolveRejectsFractionalInteger(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), EmbeddingDimension, 384.5), nil)

	_, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb")
	if appErr, ok := domain.AsAppError(err); !ok || appErr.Code != domain.CodeConfigurationInvalid {
		t.Fatalf("ожидался CONFIGURATION_INVALID, получено %v", err)
	}
}

// TestResolveRejectsNonPositiveInteger — ноль и отрицательное отвергаются.
func TestResolveRejectsNonPositiveInteger(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), ChunkingMaxTokens, int64(0)), nil)

	if _, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb"); err == nil {
		t.Fatal("chunking.max_tokens = 0 принят")
	}
}

// TestResolveRejectsOverlapNotLessThanMaxTokens — связка ключей.
func TestResolveRejectsOverlapNotLessThanMaxTokens(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), ChunkingOverlapTokens, int64(600)), nil)

	_, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb")
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeConfigurationInvalid {
		t.Fatalf("ожидался CONFIGURATION_INVALID, получено %v", err)
	}
	if appErr.Details["key"] != ChunkingOverlapTokens.Name() {
		t.Errorf("details.key = %v, ожидался %q", appErr.Details["key"], ChunkingOverlapTokens.Name())
	}
}

// TestResolveRejectsDistanceOtherThanCosine — в MVP только cosine.
func TestResolveRejectsDistanceOtherThanCosine(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), EmbeddingDistance, "dot"), nil)

	if _, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb"); err == nil {
		t.Fatal("search.embedding_distance = dot принят")
	}
}

// TestResolveRejectsTopKOutOfRange — search.top_k ограничен 1…50 (ТЗ 7.3).
func TestResolveRejectsTopKOutOfRange(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), SearchTopK, int64(500)), nil)

	_, err := service.ResolveRetrieval(testCtx(t), nil, "tenant", "kb")
	if err == nil {
		t.Fatal("search.top_k = 500 принят")
	}
}

// TestResolveRejectsEmptyString — пустая строка не является значением.
func TestResolveRejectsEmptyString(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), EmbeddingModel, "   "), nil)

	if _, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb"); err == nil {
		t.Fatal("пустая модель принята")
	}
}

// TestResolveRejectsUnknownTool — множество инструментов закрыто.
func TestResolveRejectsUnknownTool(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), ToolsEnabled, []any{"kb_search", "sql"}), nil)

	if _, err := service.ResolveGeneration(testCtx(t), nil, "tenant", "kb"); err == nil {
		t.Fatal("неизвестный инструмент принят")
	}
}

// TestResolveGenerationRequiresKBSearchTool — связка ключей до вызова модели.
func TestResolveGenerationRequiresKBSearchTool(t *testing.T) {
	service, _ := newService(withKey(baselineKeys(), ToolsEnabled, []any{}), nil)

	_, err := service.ResolveGeneration(testCtx(t), nil, "tenant", "kb")
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeConfigurationInvalid {
		t.Fatalf("ожидался CONFIGURATION_INVALID, получено %v", err)
	}
	if appErr.Details["key"] != ToolsEnabled.Name() {
		t.Errorf("details.key = %v, ожидался %q", appErr.Details["key"], ToolsEnabled.Name())
	}
}

// TestResolveGenerationRequiresAllAnswerKeys — набор AnswerKeys целиком.
func TestResolveGenerationRequiresAllAnswerKeys(t *testing.T) {
	tenant := baselineKeys()
	delete(tenant, AnswerModel.Name())
	service, _ := newService(tenant, nil)

	_, err := service.ResolveGeneration(testCtx(t), nil, "tenant", "kb")
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ожидалась AppError, получено %v", err)
	}
	if !contains(missingKeys(t, appErr), AnswerModel.Name()) {
		t.Errorf("answer.model не в details.keys: %v", appErr.Details)
	}
}

// TestResolveGenerationCarriesProviderAndBudget — ответ получает настройки
// модели и бюджет контекста целиком.
func TestResolveGenerationCarriesProviderAndBudget(t *testing.T) {
	service, _ := newService(baselineKeys(), nil)

	generation, err := service.ResolveGeneration(testCtx(t), nil, "tenant", "kb")
	if err != nil {
		t.Fatalf("ResolveGeneration: %v", err)
	}
	if generation.ProviderID != "local-deterministic" || generation.Model != "stub-1" {
		t.Errorf("провайдер/модель: %q / %q", generation.ProviderID, generation.Model)
	}
	if generation.MaxContextTokens != 6000 || generation.MaxOutputTokens != 1000 {
		t.Errorf("бюджет: %d / %d", generation.MaxContextTokens, generation.MaxOutputTokens)
	}
	if generation.IndexingConfig.EmbeddingDimension != 384 {
		t.Errorf("профиль эмбеддинга не приложен: %d", generation.IndexingConfig.EmbeddingDimension)
	}
}

// ── Диагностика ────────────────────────────────────────────────────

// TestDiagnosticsOnEmptyCatalogListsEverything — после alembic таблица пуста.
func TestDiagnosticsOnEmptyCatalogListsEverything(t *testing.T) {
	service, _ := newService(map[string]any{}, map[string]any{})

	diagnostics, err := service.Diagnostics(testCtx(t), nil, "tenant", "kb", "")
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if diagnostics.Valid {
		t.Error("пустые настройки не могут быть валидными")
	}
	if len(diagnostics.MissingKeys) != len(Catalog()) {
		t.Errorf("missing_keys: %d, ожидалось %d", len(diagnostics.MissingKeys), len(Catalog()))
	}
	if diagnostics.EmbeddingProfileCompatible {
		t.Error("без настроек профиль не может быть совместим")
	}
	if len(diagnostics.Sources) != len(Catalog()) {
		t.Fatalf("источников %d, ожидалось по одному на ключ", len(diagnostics.Sources))
	}
	for _, source := range diagnostics.Sources {
		if source.HasValue || source.Source != SourceUnset {
			t.Errorf("%s: источник %q, задан %v", source.Key.Name(), source.Source, source.HasValue)
		}
	}
}

// TestDiagnosticsReportsLevelsWithoutValues — в ответе нет значений.
//
// Диагностика идёт по адресу, доступному администратору, но показывать она
// обязана состояние, а не содержимое настроек: значение любого ключа может
// оказаться чувствительным в следующей версии каталога.
func TestDiagnosticsReportsLevelsWithoutValues(t *testing.T) {
	tenant := baselineKeys()
	kb := map[string]any{ChunkingMaxTokens.Name(): int64(800)}
	service, _ := newService(tenant, kb)

	diagnostics, err := service.Diagnostics(testCtx(t), nil, "tenant", "kb", "")
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if !diagnostics.Valid {
		t.Errorf("настройки заполнены, диагностика невалидна: %v / %v",
			diagnostics.MissingKeys, diagnostics.InvalidKeys)
	}
	if !diagnostics.EmbeddingProfileCompatible {
		t.Error("профиль базы ещё не зафиксирован, а диагностика считает его несовместимым")
	}
	sources := sourcesByKey(t, diagnostics)
	if got := sources[ChunkingMaxTokens.Name()]; got.Source != SourceKB || !got.HasValue {
		t.Errorf("chunking.max_tokens: %+v, ожидался kb", got)
	}
	if got := sources[ChunkingTokenizerID.Name()]; got.Source != SourceTenant || !got.HasValue {
		t.Errorf("chunking.tokenizer_id: %+v, ожидался tenant", got)
	}

	// Ни одно значение не должно просочиться в диагностику.
	rendered := renderDiagnostics(t, diagnostics)
	for _, secret := range []string{"local-hash", "whitespace-approx", "local-deterministic", "800"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("в диагностике найдено значение %q: %s", secret, rendered)
		}
	}
}

// TestDiagnosticsMarksInvalidKey — неверное значение видно, но не содержится.
//
// Проверяется СВЯЗКА ключей, а не одиночное значение: overlap_tokens=700 при
// max_tokens=600 по отдельности корректны — оба целые и неотрицательные.
// Нарушение видно только при совместной проверке, поэтому и диагностика
// обязана её выполнять: иначе такая конфигурация прошла бы и сломала бы
// нарезку на этапе индексации.
func TestDiagnosticsMarksInvalidKey(t *testing.T) {
	service, _ := newService(
		withKey(baselineKeys(), ChunkingOverlapTokens, int64(700)), nil)

	diagnostics, err := service.Diagnostics(testCtx(t), nil, "tenant", "kb", "")
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if diagnostics.Valid {
		t.Error("настройка с overlap >= max невалидна, диагностика это пропустила")
	}
	if !contains(diagnostics.InvalidKeys, ChunkingOverlapTokens.Name()) {
		t.Errorf("invalid_keys = %v, ожидался %q", diagnostics.InvalidKeys, ChunkingOverlapTokens.Name())
	}
}

// TestDiagnosticsDetectsProfileChange — ТЗ 1.2.
//
// Профиль фиксируется при первой активации; смена любого из шести полей
// требует переиндексации, и диагностика обязана сказать об этом ДО загрузки
// документов, а не после первой неудачной активации.
func TestDiagnosticsDetectsProfileChange(t *testing.T) {
	service, _ := newService(baselineKeys(), nil)
	stale := "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	diagnostics, err := service.Diagnostics(testCtx(t), nil, "tenant", "kb", stale)
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if diagnostics.EmbeddingProfileCompatible {
		t.Error("профиль базы не совпадает с настройками, диагностика это пропустила")
	}
	if diagnostics.Valid {
		t.Error("при несовместимом профиле настройки не могут считаться валидными")
	}
	if !contains(diagnostics.InvalidKeys, EmbeddingModel.Name()) {
		t.Errorf("invalid_keys = %v, ожидался %q", diagnostics.InvalidKeys, EmbeddingModel.Name())
	}
}

// TestDiagnosticsAcceptsMatchingProfile — профиль совпал с зафиксированным.
func TestDiagnosticsAcceptsMatchingProfile(t *testing.T) {
	service, _ := newService(baselineKeys(), nil)
	cfg, err := service.IndexingConfig(testCtx(t), nil, "tenant", "kb")
	if err != nil {
		t.Fatalf("IndexingConfig: %v", err)
	}

	diagnostics, err := service.Diagnostics(testCtx(t), nil, "tenant", "kb", cfg.ProfileID())
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if !diagnostics.Valid || !diagnostics.EmbeddingProfileCompatible {
		t.Errorf("совпадающий профиль должен давать валидную диагностику: %+v", diagnostics)
	}
}

// ── helpers ───────────────────────────────────────────────────────

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// missingKeys достаёт details.keys из CONFIGURATION_MISSING.
func missingKeys(t *testing.T, appErr *domain.AppError) []string {
	t.Helper()
	raw, ok := appErr.Details["keys"].([]string)
	if !ok {
		t.Fatalf("details.keys = %#v, ожидался список строк", appErr.Details["keys"])
	}
	return raw
}

func sourcesByKey(t *testing.T, diagnostics Diagnostics) map[string]DiagnosticSource {
	t.Helper()
	out := make(map[string]DiagnosticSource, len(diagnostics.Sources))
	for _, source := range diagnostics.Sources {
		out[source.Key.Name()] = source
	}
	return out
}

// renderDiagnostics кодирует диагностику так же, как это сделает HTTP-слой.
func renderDiagnostics(t *testing.T, diagnostics Diagnostics) string {
	t.Helper()
	encoded, err := json.Marshal(diagnostics)
	if err != nil {
		t.Fatalf("кодирование диагностики: %v", err)
	}
	return string(encoded)
}
