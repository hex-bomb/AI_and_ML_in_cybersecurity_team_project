package elasticsearch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	domain "github.com/kbsearch/kb/internal/envelope"
)

// Тесты этого файла не требуют кластера: проверяют форму запросов, которая
// уехала бы в сеть, и разбор ответов. Интеграция — в elasticsearch_integration_test.go.

func testConfig(dimension int) domain.IndexingConfig {
	return domain.IndexingConfig{
		EmbeddingProviderID: "local",
		EmbeddingModel:      "intfloat/multilingual-e5-small",
		EmbeddingRevision:   "test-1",
		EmbeddingDimension:  dimension,
		ChunkMaxTokens:      320,
		ChunkOverlapTokens:  64,
		TokenizerID:         "e5",
		NormalizeVectors:    true,
		Distance:            "cosine",
	}
}

func testChunk(profileID string, dimension int, text string) domain.Chunk {
	vector := make([]float32, dimension)
	for i := range vector {
		vector[i] = float32(i%7) / float32(dimension)
	}
	blockID := uuid.New()
	page := 3
	return domain.Chunk{
		SchemaVersion: "1",
		ChunkID:       "sha256:" + strings.Repeat("a", 64),
		TenantID:      "tenant-1",
		KBID:          "kb-1",
		DocumentID:    "doc-1",
		VersionID:     "ver-1",
		ChunkNumber:   2,
		Title:         "Заголовок",
		Text:          text,
		Locator: domain.Locator{
			BlockIDs:    []uuid.UUID{blockID},
			SectionPath: []string{"Раздел"},
			PageStart:   &page,
			PageEnd:     &page,
		},
		EmbeddingProfileID: profileID,
		Embedding:          vector,
	}
}

// TestIndexNameMatchesProfileHex переносит test_indexing.py:364. Форма имени
// индекса перенесена из Python без изменений: по старому имени находится уже
func TestIndexNameMatchesProfileHex(t *testing.T) {
	cfg := testConfig(384)
	name := cfg.IndexName()
	hex := strings.TrimPrefix(cfg.ProfileID(), "sha256:")
	if name != "kb-chunks-v1-"+hex {
		t.Errorf("IndexName = %q, ожидалось kb-chunks-v1-%s", name, hex)
	}
	if strings.Contains(name, "sha256:") {
		t.Errorf("имя индекса содержит префикс sha256: %q", name)
	}
}

// TestIndexBodyUsesDenseVectorAndRussianAnalyzer — аналог
// test_indexing.py:395, переписанный под Elasticsearch: вместо
func TestIndexBodyUsesDenseVectorAndRussianAnalyzer(t *testing.T) {
	body := indexBody(384)
	props := body["mappings"].(map[string]any)["properties"].(map[string]any)

	for _, field := range []string{"text", "title"} {
		got := props[field].(map[string]any)
		if got["type"] != "text" {
			t.Errorf("%s type = %v, ожидалось text", field, got["type"])
		}
		if got["analyzer"] != "russian" {
			t.Errorf("%s analyzer = %v, ожидалось russian", field, got["analyzer"])
		}
	}
	for _, field := range []string{
		"schema_version", "chunk_id", "tenant_id", "kb_id",
		"document_id", "version_id", "embedding_profile_id",
	} {
		got := props[field].(map[string]any)
		if got["type"] != "keyword" {
			t.Errorf("%s type = %v, ожидалось keyword", field, got["type"])
		}
	}

	embedding := props["embedding"].(map[string]any)
	if embedding["type"] != "dense_vector" {
		t.Errorf("embedding type = %v, ожидалось dense_vector", embedding["type"])
	}
	if embedding["dims"] != 384 {
		t.Errorf("embedding dims = %v, ожидалось 384", embedding["dims"])
	}
	if embedding["similarity"] != "cosine" {
		t.Errorf("embedding similarity = %v, ожидалось cosine", embedding["similarity"])
	}
	if embedding["index"] != true {
		t.Errorf("embedding index = %v, ожидалось true (HNSW)", embedding["index"])
	}
	// index_options намеренно не задаётся: тип HNSW выбирает кластер по числу
	// измерений, и жёстко заданный тип стал бы второй причиной расхождения
	if _, ok := embedding["index_options"]; ok {
		t.Errorf("index_options задан в коде: %v", embedding["index_options"])
	}
}

// TestFilterClausesCoverMandatoryDimensions переносит смысл
// test_indexing.py:471: tenant, kb, профиль и список документов обязаны быть в
func TestFilterClausesCoverMandatoryDimensions(t *testing.T) {
	f := domain.SearchFilter{
		TenantID:           "tenant-1",
		KBID:               "kb-1",
		EmbeddingProfileID: "sha256:abc",
		AllowedDocumentIDs: []string{"doc-1", "doc-2"},
	}
	clauses := filterClauses(f)
	blob, err := json.Marshal(clauses)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(blob)
	for _, want := range []string{
		`"tenant_id":"tenant-1"`,
		`"kb_id":"kb-1"`,
		`"embedding_profile_id":"sha256:abc"`,
		`"document_id":["doc-1","doc-2"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в фильтре нет %s: %s", want, text)
		}
	}
}

// TestFilterClausesSerializeEmptyAllowedIDsAsArray: nil обязан сериализоваться
// в [], а не в null. null в terms означает «не ограничивать», и пустой
func TestFilterClausesSerializeEmptyAllowedIDsAsArray(t *testing.T) {
	clauses := filterClauses(domain.SearchFilter{TenantID: "t"})
	blob, err := json.Marshal(clauses)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), `"document_id":null`) {
		t.Errorf("пустой список сериализован как null: %s", blob)
	}
}

// TestKeywordQueryCarriesFiltersAndRussianAnalyzer проверяет форму
// текстовой ветви: те же фильтры и явный русский анализатор.
func TestKeywordQueryCarriesFiltersAndRussianAnalyzer(t *testing.T) {
	f := domain.SearchFilter{
		TenantID:           "tenant-1",
		KBID:               "kb-1",
		EmbeddingProfileID: "sha256:abc",
		AllowedDocumentIDs: []string{"doc-1"},
	}
	body := keywordQuery("выгрузка данных", f, 25)
	if body["size"] != 25 {
		t.Errorf("size = %v, ожидалось 25", body["size"])
	}
	blob, _ := json.Marshal(body)
	text := string(blob)
	if !strings.Contains(text, `"analyzer":"russian"`) {
		t.Errorf("в текстовом запросе нет русского анализатора: %s", text)
	}
	for _, field := range []string{"text", "title"} {
		if !strings.Contains(text, `"`+field+`"`) {
			t.Errorf("текстовый запрос не ищет по полю %s: %s", field, text)
		}
	}
	// Тот же набор фильтров, что у векторной ветви.
	for _, want := range []string{`"tenant_id":"tenant-1"`, `"document_id":["doc-1"]`} {
		if !strings.Contains(text, want) {
			t.Errorf("в текстовом запросе нет фильтра %s: %s", want, text)
		}
	}
}

// TestVectorQueryUsesSameFiltersAndKnnSyntax: векторная ветвь обязана нести
// ТЕ ЖЕ фильтры, что и текстовая, иначе RRF сравнивал бы разные выборки.
func TestVectorQueryUsesSameFiltersAndKnnSyntax(t *testing.T) {
	f := domain.SearchFilter{
		TenantID:           "tenant-1",
		KBID:               "kb-1",
		EmbeddingProfileID: "sha256:abc",
		AllowedDocumentIDs: []string{"doc-1"},
	}
	body := vectorQuery(make([]float32, 384), f, 5)
	blob, _ := json.Marshal(body)
	text := string(blob)
	if !strings.Contains(text, `"field":"embedding"`) {
		t.Errorf("в векторном запросе нет field: %s", text)
	}
	if !strings.Contains(text, `"query_vector"`) {
		t.Errorf("в векторном запросе нет query_vector: %s", text)
	}
	for _, want := range []string{`"tenant_id":"tenant-1"`, `"kb_id":"kb-1"`,
		`"embedding_profile_id":"sha256:abc"`, `"document_id":["doc-1"]`} {
		if !strings.Contains(text, want) {
			t.Errorf("в векторном запросе нет фильтра %s: %s", want, text)
		}
	}
	// k = max(limit, 10): при k == limit ветвь не набрала бы limit кандидатов
	// после фильтрации terms по document_id.
	if !strings.Contains(text, `"k":10`) {
		t.Errorf("k не поднят до 10 при limit=5: %s", text)
	}
}

// TestNumCandidatesGrowsWithK фиксирует нижнюю границу кандидатов HNSW: без
// неё приблизительный поиск теряет точность на границах запроса.
func TestNumCandidatesGrowsWithK(t *testing.T) {
	if got := numCandidates(1); got != minNumCandidates {
		t.Errorf("numCandidates(1) = %d, ожидалось %d", got, minNumCandidates)
	}
	if got := numCandidates(50); got != 500 {
		t.Errorf("numCandidates(50) = %d, ожидалось 500", got)
	}
}

// TestDeleteQueryAddsVersionOnlyWhenGiven: удаление версии фильтрует по
// version_id, удаление документа — по всем версиям.
func TestDeleteQueryAddsVersionOnlyWhenGiven(t *testing.T) {
	withVersion, _ := json.Marshal(deleteQuery("t", "d", "v"))
	if !strings.Contains(string(withVersion), `"version_id":"v"`) {
		t.Errorf("удаление версии без фильтра по version_id: %s", withVersion)
	}
	allVersions, _ := json.Marshal(deleteQuery("t", "d", ""))
	if strings.Contains(string(allVersions), "version_id") {
		t.Errorf("удаление документа фильтрует по версии: %s", allVersions)
	}
}

// TestToDocSourceCopiesSlices проверяет иммутабельность: чанк принадлежит
// сценарию индексации и может быть переиспользован, а общий срез в _source
func TestToDocSourceCopiesSlices(t *testing.T) {
	cfg := testConfig(384)
	chunk := testChunk(cfg.ProfileID(), 384, "текст чанка")
	source := toDocSource(chunk)

	// Мутируем оригинал после построения документа.
	chunk.Text = "изменённый текст"
	chunk.Locator.SectionPath[0] = "другой раздел"
	chunk.Embedding[0] = 42

	if source.Text != "текст чанка" {
		t.Errorf("Text в документе изменился вслед за чанком: %q", source.Text)
	}
	if source.Locator.SectionPath[0] != "Раздел" {
		t.Errorf("SectionPath в документе изменился вслед за чанком: %v",
			source.Locator.SectionPath)
	}
	if source.Embedding[0] == 42 {
		t.Error("вектор в документе изменился вслед за чанком")
	}
	if got := source.Locator.BlockIDs[0]; got != chunk.Locator.BlockIDs[0].String() {
		t.Errorf("block_id = %q, ожидалось %q", got, chunk.Locator.BlockIDs[0])
	}
}

// TestToCandidateParsesLocator переносит test_indexing.py:446: локатор
// обязан пережить путь индекс -> домен, включая номера страниц.
func TestToCandidateParsesLocator(t *testing.T) {
	cfg := testConfig(384)
	blockID := uuid.New()
	page := 7
	src := toDocSource(testChunk(cfg.ProfileID(), 384, "текст"))
	src.Locator = docLocator{
		BlockIDs:    []string{blockID.String()},
		SectionPath: []string{"Раздел", "Подраздел"},
		PageStart:   &page,
		PageEnd:     nil,
	}
	candidate := toCandidate(src, 1.5)
	if candidate.Score != 1.5 {
		t.Errorf("Score = %v, ожидалось 1.5", candidate.Score)
	}
	if len(candidate.Locator.BlockIDs) != 1 || candidate.Locator.BlockIDs[0] != blockID {
		t.Errorf("BlockIDs = %v, ожидался [%v]", candidate.Locator.BlockIDs, blockID)
	}
	if candidate.Locator.PageStart == nil || *candidate.Locator.PageStart != 7 {
		t.Errorf("PageStart = %v, ожидалось 7", candidate.Locator.PageStart)
	}
	if candidate.Locator.PageEnd != nil {
		t.Errorf("PageEnd = %v, ожидался nil", candidate.Locator.PageEnd)
	}
}

// TestToCandidateSkipsBrokenBlockID: один неразбираемый block_id не должен
// отнимать у пользователя весь результат — ссылка на блок пропадёт, кандидат
func TestToCandidateSkipsBrokenBlockID(t *testing.T) {
	src := toDocSource(testChunk(testConfig(384).ProfileID(), 384, "текст"))
	src.Locator.BlockIDs = []string{"не-uuid"}
	candidate := toCandidate(src, 1)
	if len(candidate.Locator.BlockIDs) != 0 {
		t.Errorf("BlockIDs = %v, ожидалось пусто", candidate.Locator.BlockIDs)
	}
	if candidate.ChunkID == "" {
		t.Error("кандидат потерял chunk_id из-за плохого block_id")
	}
}

// TestCollectBulkFailuresCountsEveryItem: частичный успех bulk — ошибка
// (ТЗ 6.3 п.6), поэтому считать надо ВСЕ неуспешные item'ы, а не первые.
func TestCollectBulkFailuresCountsEveryItem(t *testing.T) {
	failure := func(reason string) *struct {
		Reason string `json:"reason"`
		Type   string `json:"type"`
	} {
		return &struct {
			Reason string `json:"reason"`
			Type   string `json:"type"`
		}{Reason: reason, Type: "mapper_parsing_exception"}
	}
	items := []map[string]bulkItem{
		{"index": {Status: 201}},
		{"index": {Status: 400, Error: failure("плохой текст")}},
		{"index": {Status: 400, Error: failure("плохой вектор")}},
		{"index": {Status: 400, Error: failure("плохой локатор")}},
		{"index": {Status: 400, Error: failure("плохой title")}},
	}
	failed, sample := collectBulkFailures(items)
	if failed != 4 {
		t.Errorf("failed = %d, ожидалось 4", failed)
	}
	if len(sample) != 3 {
		t.Errorf("len(sample) = %d, ожидалось 3", len(sample))
	}
}

// TestIndexNameForRejectsEmptyProfile: без профиля имя индекса построить
// нельзя, и искать не по чему — это ошибка конфигурации, а не пустой результат.
func TestIndexNameForRejectsEmptyProfile(t *testing.T) {
	if _, err := indexNameFor("  "); err == nil {
		t.Fatal("пустой профиль принят")
	}
	name, err := indexNameFor("sha256:abc")
	if err != nil {
		t.Fatalf("indexNameFor: %v", err)
	}
	if name != "kb-chunks-v1-abc" {
		t.Errorf("name = %q, ожидалось kb-chunks-v1-abc", name)
	}
}

// TestValidateChunksKeepsMismatchDimensions — регрессия: вызов WithDetails
// ПОСЛЕ EmbeddingDimensionMismatch затирал записанные конструктором got/want,
func TestValidateChunksKeepsMismatchDimensions(t *testing.T) {
	cfg := testConfig(dimension)
	chunk := testChunk(cfg.ProfileID(), dimension-1, "текст")

	err := validateChunks(cfg, cfg.ProfileID(), []domain.Chunk{chunk})
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("validateChunks вернул %v, ожидалась AppError", err)
	}
	if appErr.Code != domain.CodeEmbeddingDimensionMismatch {
		t.Errorf("код = %q, ожидался %q", appErr.Code, domain.CodeEmbeddingDimensionMismatch)
	}
	for name, want := range map[string]int{
		"chunk_id": 0, // проверяется отдельно, ключ обязан присутствовать
		"got":      dimension - 1,
		"want":     dimension,
	} {
		if _, ok := appErr.Details[name]; !ok {
			t.Errorf("details[%q] отсутствует: %v", name, appErr.Details)
		}
		if name != "chunk_id" && appErr.Details[name] != want {
			t.Errorf("details[%q] = %v, ожидалось %d", name, appErr.Details[name], want)
		}
	}
	if appErr.Details["chunk_id"] != chunk.ChunkID {
		t.Errorf("details[chunk_id] = %v, ожидался %q", appErr.Details["chunk_id"], chunk.ChunkID)
	}
}

// TestValidateChunksRejectsForeignProfile: чужой профиль обязан отвергаться
// до сети, иначе векторы разных моделей смешались бы в одном индексе.
func TestValidateChunksRejectsForeignProfile(t *testing.T) {
	cfg := testConfig(dimension)
	chunk := testChunk("sha256:other", dimension, "текст")

	err := validateChunks(cfg, cfg.ProfileID(), []domain.Chunk{chunk})
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeIndexProfileMismatch {
		t.Fatalf("validateChunks вернул %v, ожидался INDEX_PROFILE_MISMATCH", err)
	}
}

// TestClampLimitKeepsSizeInContractRange: size вне 1..100 ушёл бы в кластер
// как 400, который вернулся бы повторяемым 503 и ушёл в три ретрая.
func TestClampLimitKeepsSizeInContractRange(t *testing.T) {
	cases := map[int]int{-5: 1, 0: 1, 1: 1, 20: 20, 100: 100, 5000: maxSearchLimit}
	for in, want := range cases {
		if got := clampLimit(in); got != want {
			t.Errorf("clampLimit(%d) = %d, ожидалось %d", in, got, want)
		}
	}
}
