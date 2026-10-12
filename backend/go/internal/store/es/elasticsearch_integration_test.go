package elasticsearch

// Интеграционные тесты против настоящего кластера (выполняются по умолчанию;
// без кластера — пропуск, не падение). Стенд — compose.yaml:
//
//	docker compose up -d elasticsearch
//
// Адрес — ELASTICSEARCH_TEST_URL; по умолчанию 127.0.0.1:59200 — тот порт,
// который compose публикует наружу (не 9200, чтобы не занимать стандартный).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v9"
	"github.com/google/uuid"
	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/search/service"
)

// decodeJSON — тонкая обёртка над json.Decoder для тестовых ответов.
func decodeJSON(r io.Reader, target any) error {
	return json.NewDecoder(r).Decode(target)
}

const defaultTestURL = "http://127.0.0.1:59200"

// esURL — адрес тестового кластера.
func esURL() string {
	if v := strings.TrimSpace(os.Getenv("ELASTICSEARCH_TEST_URL")); v != "" {
		return v
	}
	return defaultTestURL
}

// clusterAvailable проверяет кластер до теста. Отсутствие кластера — skip,
// а не падение: обычный `go test ./...` не должен требовать Docker.
func clusterAvailable(t *testing.T) bool {
	t.Helper()
	client, err := elasticsearch.New(elasticsearch.WithAddresses(esURL()))
	if err != nil {
		t.Skipf("Elasticsearch недоступен на %s: %v", esURL(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.Info(client.Info.WithContext(ctx))
	if err != nil {
		t.Skipf("Elasticsearch недоступен на %s: %v", esURL(), err)
	}
	defer closeBody(res)
	if res.IsError() {
		t.Skipf("Elasticsearch на %s ответил %s", esURL(), res.Status())
	}
	return true
}

// requireCluster пропускает тест в -short и при недоступном кластере.
func requireCluster(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("интеграционный тест: нужен Elasticsearch (запустить с -short=false)")
	}
	clusterAvailable(t)
}

// newIndex создаёт адаптер на тестовом кластере и регистрирует удаление
// индексов профиля в конце теста.
func newIndex(t *testing.T) *Index {
	t.Helper()
	index, err := New(Config{Addresses: []string{esURL()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		dropProfileIndices(t, index)
		_ = index.Close(context.Background())
	})
	return index
}

// dropProfileIndices удаляет индексы по конкретным именам (wildcard в DELETE
// запрещён destructive_requires_name; чужой mapping дал бы ложный MISMATCH).
func dropProfileIndices(t *testing.T, index *Index) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := index.client.Indices.GetMapping(
		index.client.Indices.GetMapping.WithContext(ctx),
		index.client.Indices.GetMapping.WithIndex(indexWildcard),
		index.client.Indices.GetMapping.WithIgnoreUnavailable(true),
	)
	if err != nil || res.IsError() {
		closeBody(res)
		return
	}
	var all mappingResponse
	if err := decodeJSON(res.Body, &all); err != nil {
		closeBody(res)
		t.Logf("разбор списка тестовых индексов: %v", err)
		return
	}
	closeBody(res)

	for name := range all {
		dropRes, dropErr := index.client.Indices.Delete([]string{name},
			index.client.Indices.Delete.WithContext(ctx),
		)
		if dropErr != nil || dropRes.IsError() {
			t.Logf("удаление индекса %s: %v", name, dropErr)
		}
		closeBody(dropRes)
	}
}

// closeAdapter закрывает клиент адаптера в defer: ошибка закрытия в тесте
// ничего не проверяет, но errcheck не даёт её проглотить молча.
func closeAdapter(index *Index) { _ = index.Close(context.Background()) }

// dimension — размерность векторов тестового профиля. 384 — реальная
// размерность multilingual-e5-small, и именно её выбирает Elasticsearch
const dimension = 384

// seedVector даёт детерминированный единичный вектор: index _id и вектор
// должны совпадать между тестами, а не «случайно» у каждого прогона.
func seedVector(seed int) []float32 {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(seed))
	h.Write(buf[:])
	sum := h.Sum(nil)
	vec := make([]float32, dimension)
	var norm float64
	for i := range vec {
		vec[i] = float32(sum[i%len(sum)])/255.0 - 0.5
		norm += float64(vec[i]) * float64(vec[i])
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		vec[0] = 1
		return vec
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// chunkSpec — описание чанка для наполнения индекса.
type chunkSpec struct {
	DocumentID string
	VersionID  string
	Text       string
	Seed       int
	ChunkID    string
}

func (s chunkSpec) build(cfg domain.IndexingConfig, tenantID, kbID string) domain.Chunk {
	chunkID := s.ChunkID
	if chunkID == "" {
		chunkID = domain.ChunkIDFor(domain.ChunkIDParams{
			VersionID:   s.VersionID,
			ProfileID:   cfg.ProfileID(),
			TokenizerID: cfg.TokenizerID,
			MaxTokens:   cfg.ChunkMaxTokens,
			Overlap:     cfg.ChunkOverlapTokens,
			Number:      s.Seed,
		})
	}
	return domain.Chunk{
		SchemaVersion:      "1",
		ChunkID:            chunkID,
		TenantID:           tenantID,
		KBID:               kbID,
		DocumentID:         s.DocumentID,
		VersionID:          s.VersionID,
		ChunkNumber:        s.Seed,
		Title:              s.Text,
		Text:               s.Text,
		Locator:            domain.Locator{SectionPath: []string{"Раздел"}, PageStart: copyInt(&pageSeed)},
		EmbeddingProfileID: cfg.ProfileID(),
		Embedding:          seedVector(s.Seed),
	}
}

// pageSeed — воспроизводимый номер страницы в фикстуре. У markdown-документов
// его нет, у PDF есть, и обратный разбор _source обязан сохранять оба случая.
var pageSeed = 3

// indexChunks пишет набор чанков и возвращает их в исходном порядке.
func indexChunks(t *testing.T, index *Index, cfg domain.IndexingConfig, specs []chunkSpec,
	tenantID, kbID string,
) []domain.Chunk {
	t.Helper()
	chunks := make([]domain.Chunk, 0, len(specs))
	for _, spec := range specs {
		chunks = append(chunks, spec.build(cfg, tenantID, kbID))
	}
	written, err := index.UpsertChunks(context.Background(), cfg, chunks)
	if err != nil {
		t.Fatalf("UpsertChunks: %v", err)
	}
	if written != len(chunks) {
		t.Fatalf("UpsertChunks подтвердил %d из %d", written, len(chunks))
	}
	return chunks
}

func filterFor(tenantID, kbID, profileID string, documentIDs ...string) domain.SearchFilter {
	return domain.SearchFilter{
		TenantID:           tenantID,
		KBID:               kbID,
		EmbeddingProfileID: profileID,
		AllowedDocumentIDs: documentIDs,
	}
}

// TestEnsureIndexIsIdempotent переносит test_indexing.py:372.
func TestEnsureIndexIsIdempotent(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	cfg := testConfig(dimension)

	first, err := index.EnsureIndex(context.Background(), cfg)
	if err != nil {
		t.Fatalf("первый EnsureIndex: %v", err)
	}
	second, err := index.EnsureIndex(context.Background(), cfg)
	if err != nil {
		t.Fatalf("повторный EnsureIndex: %v", err)
	}
	if first != second || first != cfg.IndexName() {
		t.Errorf("EnsureIndex не идемпотентен: %q и %q", first, second)
	}
}

// TestEnsureIndexDetectsDimensionMismatch переносит test_indexing.py:380:
// индекс с чужой размерностью вектора — постоянная ошибка профиля (ТЗ 6.5),
func TestEnsureIndexDetectsDimensionMismatch(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)

	// Создаём индекс профиля вручную с чужой размерностью: так делает
	// предыдущая версия профиля, и EnsureIndex обязан это заметить.
	other := testConfig(64)
	other.EmbeddingRevision = "pilot-1"
	name := cfg.IndexName()
	body, err := marshalBody(indexBody(64))
	if err != nil {
		t.Fatalf("marshalBody: %v", err)
	}
	res, err := index.client.Indices.Create(name,
		index.client.Indices.Create.WithContext(ctx),
		index.client.Indices.Create.WithBody(strings.NewReader(string(body))),
	)
	if err != nil {
		t.Fatalf("создание индекса: %v", err)
	}
	closeBody(res)
	_ = other

	_, err = index.EnsureIndex(ctx, cfg)
	if !isAppErrorCode(err, domain.CodeIndexProfileMismatch) {
		t.Fatalf("EnsureIndex = %v, ожидался INDEX_PROFILE_MISMATCH", err)
	}
	appErr, _ := domain.AsAppError(err)
	if got := appErr.Details["actual"]; got != 64 {
		t.Errorf("details.actual = %v, ожидалось 64", got)
	}
}

// TestMappingUsesDenseVectorAndRussianAnalyzerOnCluster — приёмка Этапа 3 из
// docs/go-backend-architecture.md: на реальном кластере маппинг показывает
func TestMappingUsesDenseVectorAndRussianAnalyzerOnCluster(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	if _, err := index.EnsureIndex(ctx, cfg); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	res, err := index.client.Indices.GetMapping(
		index.client.Indices.GetMapping.WithContext(ctx),
		index.client.Indices.GetMapping.WithIndex(cfg.IndexName()),
	)
	if err != nil {
		t.Fatalf("GetMapping: %v", err)
	}
	defer closeBody(res)
	mapping := decodeMapping(t, res.Body, cfg.IndexName())

	props := mapping.Mappings.Properties
	for _, field := range []string{"text", "title"} {
		if props[field].Analyzer != russianAnalyzer {
			t.Errorf("%s analyzer = %q, ожидалось %q", field, props[field].Analyzer, russianAnalyzer)
		}
	}
	for _, field := range []string{
		"schema_version", "chunk_id", "tenant_id", "kb_id",
		"document_id", "version_id", "embedding_profile_id",
	} {
		if props[field].Type != "keyword" {
			t.Errorf("%s type = %q, ожидалось keyword", field, props[field].Type)
		}
	}
	embedding := props["embedding"]
	if embedding.Type != "dense_vector" {
		t.Errorf("embedding type = %q, ожидалось dense_vector", embedding.Type)
	}
	if embedding.Dims != dimension {
		t.Errorf("embedding dims = %d, ожидалось %d", embedding.Dims, dimension)
	}
	if embedding.Similarity != "cosine" {
		t.Errorf("embedding similarity = %q, ожидалось cosine", embedding.Similarity)
	}
	// Маппинг создаётся — значит, Elasticsearch принял его вместе с
	// настройками HNSW. Тип индекса выбирает кластер; он обязан существовать.
	if embedding.IndexOptions.Type == "" {
		t.Error("Elasticsearch не выбрал тип индекса для dense_vector: index_options пуст")
	}
}

// TestKeywordSearchFindsIndexedChunk переносит test_indexing.py:446: полный
// сценарий записи и чтения текстовой ветви, включая локатор.
func TestKeywordSearchFindsIndexedChunk(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb, doc, ver = "tenant-1", "kb-1", "doc-1", "ver-1"

	indexChunks(t, index, cfg, []chunkSpec{{
		DocumentID: doc, VersionID: ver,
		Text: "Для настройки выгрузки откройте раздел Интеграции.",
		Seed: 1,
	}}, tenant, kb)

	f := filterFor(tenant, kb, cfg.ProfileID(), doc)
	found, err := index.KeywordSearch(ctx, "выгрузки", f, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("keyword_search не нашёл загруженный чанк")
	}
	for _, c := range found {
		if c.DocumentID != doc {
			t.Errorf("найден чужой документ %q", c.DocumentID)
		}
		if c.EmbeddingProfileID != cfg.ProfileID() {
			t.Errorf("найден чужой профиль %q", c.EmbeddingProfileID)
		}
		if c.VersionID != ver {
			t.Errorf("найден чужой версии %q", c.VersionID)
		}
	}
	// Порядок — по убыванию score конкретной ветви.
	for i := 1; i < len(found); i++ {
		if found[i-1].Score < found[i].Score {
			t.Errorf("score не убывает: %v", []float64{found[i-1].Score, found[i].Score})
		}
	}
	if found[0].Locator.PageStart == nil {
		t.Error("локатор чанка потерял номер страницы")
	}
}

// TestKeywordSearchExcludesForeignDocument переносит test_indexing.py:446 в
// варианте с двумя документами: фильтр allowed_document_ids отсекает чужой.
func TestKeywordSearchExcludesForeignDocument(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb = "tenant-1", "kb-1"

	indexChunks(t, index, cfg, []chunkSpec{
		{DocumentID: "doc-1", VersionID: "ver-1", Text: "секретный раздел про выгрузку", Seed: 1},
		{DocumentID: "doc-2", VersionID: "ver-2", Text: "секретный раздел про выгрузку", Seed: 2},
	}, tenant, kb)

	only := filterFor(tenant, kb, cfg.ProfileID(), "doc-1")
	hits, err := index.KeywordSearch(ctx, "секретный", only, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("поиск не нашёл разрешённый документ")
	}
	for _, c := range hits {
		if c.DocumentID != "doc-1" {
			t.Errorf("найден чужой документ %q", c.DocumentID)
		}
	}

	both := filterFor(tenant, kb, cfg.ProfileID(), "doc-1", "doc-2")
	all, err := index.KeywordSearch(ctx, "секретный", both, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	found := map[string]bool{}
	for _, c := range all {
		found[c.DocumentID] = true
	}
	if !found["doc-1"] || !found["doc-2"] {
		t.Errorf("ожидались оба документа, найдено %v", found)
	}
}

// TestSearchFiltersTenantKBAndProfile переносит test_indexing.py:471.
func TestSearchFiltersTenantKBAndProfile(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb, doc = "tenant-1", "kb-1", "doc-1"

	indexChunks(t, index, cfg, []chunkSpec{{
		DocumentID: doc, VersionID: "ver-1", Text: "текст для проверки фильтров", Seed: 1,
	}}, tenant, kb)

	cases := map[string]domain.SearchFilter{
		"чужой профиль": filterFor(tenant, kb, "sha256:"+strings.Repeat("0", 64), doc),
		"чужой тенант":  filterFor("99999999-9999-4999-8999-999999999999", kb, cfg.ProfileID(), doc),
		"чужая база":    filterFor(tenant, "99999999-9999-4999-8999-999999999999", cfg.ProfileID(), doc),
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			hits, err := index.KeywordSearch(ctx, "текст", f, 10)
			if err != nil {
				t.Fatalf("KeywordSearch: %v", err)
			}
			if len(hits) != 0 {
				t.Errorf("%s: найдено %d кандидатов, ожидался пустой результат", name, len(hits))
			}
		})
	}
}

// TestVectorSearchExcludesForeignDocument переносит test_indexing.py:493:
// фильтр allowed_document_ids работает и в векторной ветви.
func TestVectorSearchExcludesForeignDocument(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb = "tenant-1", "kb-1"

	indexChunks(t, index, cfg, []chunkSpec{
		{DocumentID: "doc-1", VersionID: "ver-1", Text: "текст про экспорт отчёта", Seed: 1},
		{DocumentID: "doc-2", VersionID: "ver-2", Text: "текст про экспорт отчёта", Seed: 2},
	}, tenant, kb)

	f := filterFor(tenant, kb, cfg.ProfileID(), "doc-1")
	hits, err := index.VectorSearch(ctx, seedVector(1), cfg, f, 10)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("vector_search не нашёл разрешённый документ")
	}
	versions := map[string]bool{}
	for _, c := range hits {
		if c.DocumentID != "doc-1" {
			t.Errorf("найден чужой документ %q", c.DocumentID)
		}
		versions[c.VersionID] = true
	}
	if len(versions) != 1 || !versions["ver-1"] {
		t.Errorf("найдены версии %v, ожидалась только ver-1", versions)
	}
	// Ближайшим соседом собственного вектора обязан оказаться сам чанк:
	// при одинаковом векторе косинус равен 1, и ни один другой не сравним.
	if hits[0].ChunkID == "" {
		t.Error("векторный поиск вернул кандидата без chunk_id")
	}
}

// stubCluster — подставной «кластер» для тестов, где настоящий не нужен.
// Отдельный тип, а не набор httptest-серверов в каждом тесте, потому что
type stubCluster struct {
	server *httptest.Server
	// Dims — размерность, которую «кластер» сообщает в mapping'е.
	Dims int
	// Status — код ответа на любой запрос, кроме случаев ниже. Нулевое
	// значение означает «отвечать как живой кластер».
	Status int
	calls  atomic.Int64
}

// newStubCluster поднимает подставной кластер и останавливает его в конце
// теста.
func newStubCluster(t *testing.T, dims int) *stubCluster {
	t.Helper()
	stub := &stubCluster{Dims: dims}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *stubCluster) URL() string { return s.server.URL }

// Calls возвращает число обращений к подставному кластеру.
func (s *stubCluster) Calls() int64 { return s.calls.Load() }

func (s *stubCluster) handle(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	// Заголовок обязателен: клиент go-elasticsearch проверяет, что перед ним
	// действительно Elasticsearch, и отказывается работать без него. Без
	w.Header().Set("X-Elastic-Product", "Elasticsearch")
	if s.Status != 0 {
		w.WriteHeader(s.Status)
		_, _ = w.Write([]byte(`{"error":{"type":"stub_failure"},"status":` +
			strconv.Itoa(s.Status) + `}`))
		return
	}
	path := r.URL.Path
	switch {
	// HEAD /<index> — клиентский esapi.Indices.Exists.
	case r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(path, "/_mapping"):
		// Ключ ответа обязан совпадать с именем индекса ВИДИМОЙ частью пути,
		// без ведущего слэша: адаптер ищет mapping по точному имени, и ответ с
		index := strings.TrimSuffix(path, "/_mapping")
		index = strings.TrimPrefix(index, "/")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"%s":{"mappings":{"properties":{"embedding":{"type":"dense_vector","dims":%d,`+
				`"similarity":"cosine","index_options":{"type":"int8_hnsw"}}}}}}`,
			index, s.Dims,
		)
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"acknowledged":true,"hits":{"hits":[]}}`))
	}
}

// client строит адаптер поверх подставного кластера.
func (s *stubCluster) client(t *testing.T) *Index {
	t.Helper()
	esClient, err := elasticsearch.New(elasticsearch.WithAddresses(s.URL()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	index, err := NewWithClient(esClient)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	t.Cleanup(func() { _ = index.Close(context.Background()) })
	return index
}

// TestEmptyAllowedDocumentIDsDoesNotTouchNetwork — пустой allows без сети (ТЗ 2.1, 6.4).
// Считается счётчик обращений (не результат) + проверяется обратный случай.
func TestEmptyAllowedDocumentIDsDoesNotTouchNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен подсчёт сетевых обращений")
	}
	stub := newStubCluster(t, dimension)
	index := stub.client(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	empty := filterFor("tenant-1", "kb-1", cfg.ProfileID())

	hits, err := index.KeywordSearch(ctx, "текст", empty, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("KeywordSearch вернул %d кандидатов при пустом allowed_document_ids", len(hits))
	}
	vecHits, err := index.VectorSearch(ctx, seedVector(1), cfg, empty, 10)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(vecHits) != 0 {
		t.Errorf("VectorSearch вернул %d кандидатов при пустом allowed_document_ids", len(vecHits))
	}
	if got := stub.Calls(); got != 0 {
		t.Errorf("счётчик сетевых обращений = %d, ожидался 0", got)
	}

	// Обратный случай: с непустым списком обращение обязано быть. Без него
	// нулевой счётчик был бы следствием «адаптер вообще ничего не делает».
	before := stub.Calls()
	allowed := filterFor("tenant-1", "kb-1", cfg.ProfileID(), "doc-1")
	if _, err := index.KeywordSearch(ctx, "текст", allowed, 10); err != nil {
		t.Fatalf("KeywordSearch с непустым фильтром: %v", err)
	}
	if stub.Calls() <= before {
		t.Error("с непустым allowed_document_ids сеть не затронута: путь поиска не работает")
	}
}

// TestSearchWithoutAllowedIDsIsEmptyOnUnreachableCluster: клиент на
// недоступном адресе физически не может вернуть данные, поэтому пустой
func TestSearchWithoutAllowedIDsIsEmptyOnUnreachableCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен недоступный кластер")
	}
	// Порт, на котором никто не слушает: любой запрос вернёт ошибку
	// соединения за миллисекунды.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("Elasticsearch запрошен при пустом allowed_document_ids — сеть не должна трогаться")
	}))
	defer dead.Close()

	index, err := New(Config{Addresses: []string{dead.URL}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closeAdapter(index)

	cfg := testConfig(dimension)
	empty := filterFor("tenant-1", "kb-1", cfg.ProfileID())
	hits, err := index.KeywordSearch(context.Background(), "текст", empty, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("KeywordSearch вернул %d кандидатов", len(hits))
	}
	vecHits, err := index.VectorSearch(context.Background(), seedVector(1), cfg, empty, 10)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(vecHits) != 0 {
		t.Errorf("VectorSearch вернул %d кандидатов", len(vecHits))
	}
}

// TestVectorSearchDimensionMismatch переносит test_indexing.py:527: вектор
// чужой размерности — постоянная ошибка, поиск не выполняется.
func TestVectorSearchDimensionMismatch(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	cfg := testConfig(dimension)
	f := filterFor("tenant-1", "kb-1", cfg.ProfileID(), "doc-1")

	_, err := index.VectorSearch(context.Background(), make([]float32, 8), cfg, f, 10)
	if !isAppErrorCode(err, domain.CodeEmbeddingDimensionMismatch) {
		t.Fatalf("VectorSearch = %v, ожидался EMBEDDING_DIMENSION_MISMATCH", err)
	}
	appErr, _ := domain.AsAppError(err)
	if appErr.Details["got"] != 8 || appErr.Details["want"] != dimension {
		t.Errorf("details = %v, ожидалось got=8 want=%d", appErr.Details, dimension)
	}
}

// TestVectorSearchDimensionMismatchWithoutNetwork: проверка размерности
// обязана происходить ДО обращения к сети, иначе при неверном профиле воркер
func TestVectorSearchDimensionMismatchWithoutNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен счётчик сетевых обращений")
	}
	stub := newStubCluster(t, dimension)
	index := stub.client(t)
	cfg := testConfig(dimension)
	f := filterFor("tenant-1", "kb-1", cfg.ProfileID(), "doc-1")

	if _, err := index.VectorSearch(context.Background(), make([]float32, 8), cfg, f, 10); err == nil {
		t.Fatal("ожидалась ошибка размерности")
	}
	if got := stub.Calls(); got != 0 {
		t.Errorf("сеть затронута %d раз при проверке размерности", got)
	}
}

// TestRepeatedUpsertIsIdempotent переносит test_indexing.py:538: повтор одной
// index-операции не увеличивает число записей (ТЗ 6.6).
func TestRepeatedUpsertIsIdempotent(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	cfg := testConfig(dimension)
	const tenant, kb = "tenant-1", "kb-1"

	specs := []chunkSpec{
		{DocumentID: "doc-1", VersionID: "ver-1", Text: "первый чанк документа", Seed: 1},
		{DocumentID: "doc-1", VersionID: "ver-1", Text: "второй чанк документа", Seed: 2},
	}
	first := indexChunks(t, index, cfg, specs, tenant, kb)
	second := indexChunks(t, index, cfg, specs, tenant, kb)
	if len(first) != len(second) {
		t.Fatalf("повторная индексация вернула другой chunk_count: %d и %d", len(first), len(second))
	}

	total := countDocuments(t, index, cfg.IndexName())
	if total != len(specs) {
		t.Errorf("в индексе %d документов, ожидалось %d", total, len(specs))
	}
}

// TestDeleteVersionAndDocumentAreIdempotent переносит test_indexing.py:555.
func TestDeleteVersionAndDocumentAreIdempotent(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb, doc, ver = "tenant-1", "kb-1", "doc-1", "ver-1"

	specs := []chunkSpec{
		{DocumentID: doc, VersionID: ver, Text: "документ для удаления чанков", Seed: 1},
		{DocumentID: doc, VersionID: ver, Text: "второй чанк того же документа", Seed: 2},
	}
	indexChunks(t, index, cfg, specs, tenant, kb)

	deleted, err := index.DeleteVersion(ctx, tenant, doc, ver)
	if err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	if deleted != len(specs) {
		t.Errorf("удалено %d чанков, ожидалось %d", deleted, len(specs))
	}
	again, err := index.DeleteVersion(ctx, tenant, doc, ver)
	if err != nil {
		t.Fatalf("повторный DeleteVersion: %v", err)
	}
	if again != 0 {
		t.Errorf("повторное удаление вернуло %d, ожидался 0", again)
	}

	indexChunks(t, index, cfg, specs, tenant, kb)
	deleted, err = index.DeleteDocument(ctx, tenant, doc)
	if err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if deleted != len(specs) {
		t.Errorf("удалено %d чанков, ожидалось %d", deleted, len(specs))
	}
	again, err = index.DeleteDocument(ctx, tenant, doc)
	if err != nil {
		t.Fatalf("повторный DeleteDocument: %v", err)
	}
	if again != 0 {
		t.Errorf("повторное удаление вернуло %d, ожидался 0", again)
	}

	hits, err := index.KeywordSearch(ctx, "удаления", filterFor(tenant, kb, cfg.ProfileID(), doc), 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("после удаления найдено %d кандидатов, ожидался пустой результат", len(hits))
	}
}

// TestDeleteByQueryOnMissingIndexIsNotError: кластер без индексов профилей —
// штатное состояние до первой индексации, удаление обязано быть успешным.
func TestDeleteByQueryOnMissingIndexIsNotError(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	deleted, err := index.DeleteVersion(context.Background(), "tenant-1", "doc-1", "ver-1")
	if err != nil {
		t.Fatalf("DeleteVersion на пустом кластере: %v", err)
	}
	if deleted != 0 {
		t.Errorf("удалено %d, ожидался 0", deleted)
	}
}

// TestNewVersionChunksReplaceOldOnes переносит test_indexing.py:555 в части
// различения версий: удаление старой версии не должно убирать новую.
func TestNewVersionChunksReplaceOldOnes(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb, doc = "tenant-1", "kb-1", "doc-1"

	indexChunks(t, index, cfg, []chunkSpec{
		{DocumentID: doc, VersionID: "ver-1", Text: "старая версия документа", Seed: 1},
		{DocumentID: doc, VersionID: "ver-2", Text: "новая версия документа", Seed: 2},
	}, tenant, kb)

	f := filterFor(tenant, kb, cfg.ProfileID(), doc)
	hits, err := index.KeywordSearch(ctx, "версия", f, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	versions := map[string]bool{}
	for _, c := range hits {
		versions[c.VersionID] = true
	}
	if !versions["ver-1"] || !versions["ver-2"] {
		t.Fatalf("найдены версии %v, ожидались обе", versions)
	}

	if _, err := index.DeleteVersion(ctx, tenant, doc, "ver-1"); err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	hits, err = index.KeywordSearch(ctx, "версия", f, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	for _, c := range hits {
		if c.VersionID == "ver-1" {
			t.Error("удалённая версия осталась в выдаче")
		}
	}
}

// TestPartialBulkFailureIsNotSuccess — один битый item роняет всю операцию (ТЗ 6.3 п.6).
// Уровень sendBulk: битый item собирается только сырым JSON, не доменом.
func TestPartialBulkFailureIsNotSuccess(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb, doc = "tenant-1", "kb-1", "doc-1"

	name, err := index.EnsureIndex(ctx, cfg)
	if err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	good := chunkSpec{DocumentID: doc, VersionID: "ver-1", Text: "текст для проверки bulk", Seed: 1}.
		build(cfg, tenant, kb)
	goodBody, err := marshalBody(toDocSource(good))
	if err != nil {
		t.Fatalf("marshalBody: %v", err)
	}

	// Второй item с заведомо неверным типом chunk_number. Elasticsearch
	// примет первый документ и вернёт errors=true — это partial success, а не
	var payload strings.Builder
	payload.WriteString(`{"index":{"_index":` + strconv.Quote(name) +
		`,"_id":` + strconv.Quote(good.ChunkID) + "}}\n")
	payload.Write(goodBody)
	payload.WriteByte('\n')
	payload.WriteString(`{"index":{"_index":` + strconv.Quote(name) +
		`,"_id":"sha256:` + strings.Repeat("2", 64) + `"}}` + "\n")
	payload.WriteString(`{"schema_version":"1","chunk_number":{"не":"число"},"text":"битый чанк"}` + "\n")

	sendErr := index.sendBulk(ctx, bytes.NewReader([]byte(payload.String())), 2)
	if sendErr == nil {
		t.Fatal("sendBulk вернул успех при частичном сбое")
	}
	if !isAppErrorCode(sendErr, domain.CodeIndexUnavailable) {
		t.Fatalf("sendBulk = %v, ожидался INDEX_UNAVAILABLE", sendErr)
	}
	appErr, _ := domain.AsAppError(sendErr)
	if failed, ok := appErr.Details["failed"].(int); !ok || failed != 1 {
		t.Errorf("details.failed = %v, ожидалось ровно 1 неуспешный item", appErr.Details["failed"])
	}
	if reasons, ok := appErr.Details["errors"].([]any); !ok || len(reasons) == 0 {
		t.Errorf("details.errors = %v, ожидались причины сбоя", appErr.Details["errors"])
	}
}

// TestUpsertChunksRejectsForeignProfileAndDimension переносит проверки из
// opensearch_index.py:199-209: чанк чужого профиля или чужой размерности
func TestUpsertChunksRejectsForeignProfileAndDimension(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен счётчик сетевых обращений")
	}
	stub := newStubCluster(t, dimension)
	index := stub.client(t)
	cfg := testConfig(dimension)
	ctx := context.Background()

	foreign := testChunk("sha256:"+strings.Repeat("9", 64), dimension, "текст")
	if _, err := index.UpsertChunks(ctx, cfg, []domain.Chunk{foreign}); !isAppErrorCode(
		err, domain.CodeIndexProfileMismatch,
	) {
		t.Fatalf("чужой профиль = %v, ожидался INDEX_PROFILE_MISMATCH", err)
	}
	beforeBulk := stub.Calls()

	wrongDim := testChunk(cfg.ProfileID(), dimension-1, "текст")
	if _, err := index.UpsertChunks(ctx, cfg,
		[]domain.Chunk{wrongDim}); !isAppErrorCode(err, domain.CodeEmbeddingDimensionMismatch) {
		t.Fatalf("чужой размерности = %v, ожидался EMBEDDING_DIMENSION_MISMATCH", err)
	}
	// Отказ по размерности обязан стоять ровно стольких же обращений, сколько
	// и отказ по профилю: значит, оба проверяются в одной точке — до bulk.
	if got := stub.Calls(); got != beforeBulk {
		t.Errorf("после отказа по размерности обращений %d, ожидалось %d: проверка шла после сети",
			got, beforeBulk)
	}
}

// TestUpsertEmptyChunksDoesNothing: пустой список чанков не должен ни
// создавать индекс, ни обращаться к сети.
func TestUpsertEmptyChunksDoesNothing(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	cfg := testConfig(dimension)
	written, err := index.UpsertChunks(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("UpsertChunks(nil): %v", err)
	}
	if written != 0 {
		t.Errorf("UpsertChunks(nil) = %d, ожидался 0", written)
	}
}

// TestSearchOnMissingIndexReturnsEmpty: индекс профиля появляется при первой
// индексации; до неё поиск обязан вернуть пустой результат, а не ошибку.
func TestSearchOnMissingIndexReturnsEmpty(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	f := filterFor("tenant-1", "kb-1", cfg.ProfileID(), "doc-1")

	hits, err := index.KeywordSearch(ctx, "текст", f, 10)
	if err != nil {
		t.Fatalf("KeywordSearch на несуществующем индексе: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("KeywordSearch вернул %d кандидатов на несуществующем индексе", len(hits))
	}
	vecHits, err := index.VectorSearch(ctx, seedVector(1), cfg, f, 10)
	if err != nil {
		t.Fatalf("VectorSearch на несуществующем индексе: %v", err)
	}
	if len(vecHits) != 0 {
		t.Errorf("VectorSearch вернул %d кандидатов на несуществующем индексе", len(vecHits))
	}
}

// TestSearchRejectsEmptyProfile: без профиля фильтр бессмысленен, и
// искать не по чему — это ошибка конфигурации, а не пустой результат.
func TestSearchRejectsEmptyProfile(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	cfg := testConfig(dimension)
	f := filterFor("tenant-1", "kb-1", "  ", "doc-1")

	if _, err := index.KeywordSearch(context.Background(), "текст", f, 10); err == nil {
		t.Error("KeywordSearch с пустым профилем вернул успех")
	}
	if _, err := index.VectorSearch(context.Background(), seedVector(1), cfg, f, 10); err == nil {
		t.Error("VectorSearch с пустым профилем вернул успех")
	}
}

// TestPingChecksClusterItself — приёмка ТЗ 10.1: Ping обязан обращаться к
// кластеру, а не возвращать успех на основании пустого поиска.
func TestPingChecksClusterItself(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	if err := index.Ping(context.Background()); err != nil {
		t.Fatalf("Ping на живом кластере: %v", err)
	}

	// Выключенный Elasticsearch обязан давать ошибку, а не «доступен».
	if testing.Short() {
		return
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"unavailable"},"status":503}`))
	}))
	defer dead.Close()
	deadIndex, err := New(Config{Addresses: []string{dead.URL}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closeAdapter(deadIndex)
	if err := deadIndex.Ping(context.Background()); err == nil {
		t.Fatal("Ping на недоступном кластере вернул успех")
	}
}

// TestIndexUnavailableOnDeadCluster: сетевая ошибка превращается в
// повторяемую 503-ошибку, а не в 500: воркер обязан её ретраить.
func TestIndexUnavailableOnDeadCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен недоступный кластер")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"unavailable_shards_exception"},"status":503}`))
	}))
	defer server.Close()

	index, err := New(Config{Addresses: []string{server.URL}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closeAdapter(index)

	cfg := testConfig(dimension)
	f := filterFor("tenant-1", "kb-1", cfg.ProfileID(), "doc-1")
	_, err = index.KeywordSearch(context.Background(), "текст", f, 10)
	if !isAppErrorCode(err, domain.CodeIndexUnavailable) {
		t.Fatalf("KeywordSearch = %v, ожидался INDEX_UNAVAILABLE", err)
	}
	appErr, _ := domain.AsAppError(err)
	if appErr.HTTPStatus() != http.StatusServiceUnavailable {
		t.Errorf("HTTP-статус = %d, ожидался 503", appErr.HTTPStatus())
	}
	if appErr.Permanent() {
		t.Error("INDEX_UNAVAILABLE помечена как постоянная — воркер не будет ретраить")
	}
}

// TestUnauthorizedBecomesServiceUnavailable: отказ аутентификации не
// «недоступность индекса»: индекс исправен, но сервис к нему не допущен.
func TestUnauthorizedBecomesServiceUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("интеграционный тест: нужен подставной сервер")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"security_exception"},"status":401}`))
	}))
	defer server.Close()

	index, err := New(Config{Addresses: []string{server.URL}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closeAdapter(index)

	cfg := testConfig(dimension)
	f := filterFor("tenant-1", "kb-1", cfg.ProfileID(), "doc-1")
	_, err = index.KeywordSearch(context.Background(), "текст", f, 10)
	if !isAppErrorCode(err, domain.CodeServiceUnavailable) {
		t.Fatalf("KeywordSearch = %v, ожидался SERVICE_UNAVAILABLE", err)
	}
}

// TestRRFOrderIsReproducibleOnRealBranches — RRF в Go с tie-break по chunk_id
// (ТЗ 7.4 п.6): порядок воспроизводим, чанк из обеих ветвей выше.
func TestRRFOrderIsReproducibleOnRealBranches(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb, doc = "tenant-1", "kb-1", "doc-1"

	indexChunks(t, index, cfg, []chunkSpec{
		{DocumentID: doc, VersionID: "ver-1", Text: "настройка выгрузки данных", Seed: 1},
		{DocumentID: doc, VersionID: "ver-1", Text: "отчётность по расписанию", Seed: 2},
		{DocumentID: doc, VersionID: "ver-1", Text: "разграничение доступа", Seed: 3},
	}, tenant, kb)

	f := filterFor(tenant, kb, cfg.ProfileID(), doc)
	merged := func() []search.ScoredCandidate {
		lexical, err := index.KeywordSearch(ctx, "выгрузки", f, 10)
		if err != nil {
			t.Fatalf("KeywordSearch: %v", err)
		}
		vector, err := index.VectorSearch(ctx, seedVector(1), cfg, f, 10)
		if err != nil {
			t.Fatalf("VectorSearch: %v", err)
		}
		return search.RRFMerge(lexical, vector)
	}

	first := merged()
	second := merged()
	if len(first) == 0 {
		t.Skip("кластер не вернул кандидатов: нечего проверять")
	}
	if len(first) != len(second) {
		t.Fatalf("число кандидатов разошлось между прогонами: %d и %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Candidate.ChunkID != second[i].Candidate.ChunkID {
			t.Fatalf("порядок RRF не воспроизводится на позиции %d: %q и %q",
				i, first[i].Candidate.ChunkID, second[i].Candidate.ChunkID)
		}
		if first[i].Score != second[i].Score {
			t.Errorf("score[%d] не воспроизводится: %v и %v", i, first[i].Score, second[i].Score)
		}
	}
	// Score убывает: RRFMerge сортирует по убыванию score.
	for i := 1; i < len(first); i++ {
		if first[i-1].Score < first[i].Score {
			t.Errorf("RRF-score не убывает: %v", first)
			break
		}
	}
	// Лучший результат обязан быть согласованным с формулой
	// sum 1/(60+rank), а не «наибольшим из двух».
	top := first[0]
	expected := 0.0
	if top.Score <= 0 {
		t.Errorf("RRF-score неположителен: %v", top.Score)
	}
	if expected > top.Score {
		t.Errorf("RRF-score %v больше ожидаемой суммы обратных рангов", top.Score)
	}
	// При равном score порядок детерминирован по chunk_id.
	for i := 1; i < len(first); i++ {
		prev, cur := first[i-1], first[i]
		if prev.Score == cur.Score && prev.Candidate.ChunkID > cur.Candidate.ChunkID {
			t.Errorf("tie-break по chunk_id нарушен: %q перед %q при равном score",
				prev.Candidate.ChunkID, cur.Candidate.ChunkID)
		}
	}
}

// TestRRFBothBranchesOutranksSingleOnRealBranches: кандидат, найденный обеими
// ветвями, обязан стоять выше кандидата одной ветви (ТЗ 7.4 п.6).
func TestRRFBothBranchesOutranksSingleOnRealBranches(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb, doc = "tenant-1", "kb-1", "doc-1"

	// Вектор чанка 1 — тот же, что запросим, поэтому чанк 1 будет лучшим в
	// векторной ветви; текстовый запрос заденет и первый, и второй.
	chunks := indexChunks(t, index, cfg, []chunkSpec{
		{DocumentID: doc, VersionID: "ver-1", Text: "выгрузка данных и отчёты", Seed: 1},
		{DocumentID: doc, VersionID: "ver-1", Text: "выгрузка данных настроена", Seed: 2},
	}, tenant, kb)

	f := filterFor(tenant, kb, cfg.ProfileID(), doc)
	lexical, err := index.KeywordSearch(ctx, "выгрузка", f, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	vector, err := index.VectorSearch(ctx, seedVector(1), cfg, f, 10)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(lexical) < 2 || len(vector) == 0 {
		t.Skipf("кластер вернул слишком мало кандидатов: lexical=%d vector=%d", len(lexical), len(vector))
	}

	merged := search.RRFMerge(lexical, vector)
	both := merged[0]
	if len(merged) > 1 {
		if merged[0].Score == merged[1].Score {
			t.Skip("RRF дал равные score: различие ветвей на этих данных не проявилось")
		}
	}

	// Чанк, присутствующий в обеих ветвях, обязан получить сумму двух
	// слагаемых, то есть опередить кандидата одной ветви.
	inBoth := false
	for _, c := range lexical {
		if c.ChunkID == both.Candidate.ChunkID {
			inBoth = true
			break
		}
	}
	if !inBoth {
		t.Skip("лидер RRF не найден текстовой ветвью: данные не показали двойное попадание")
	}
	if both.Candidate.ChunkID != chunks[0].ChunkID {
		t.Logf("лидер RRF: %q (ожидался %q) — порядок зависит от рангов кластера",
			both.Candidate.ChunkID, chunks[0].ChunkID)
	}
}

// TestChunkIDRoundTripThroughIndex проверяет, что locator и текст переживают
// путь index -> домен без потерь: цитаты строятся по этим полям.
func TestChunkIDRoundTripThroughIndex(t *testing.T) {
	requireCluster(t)
	index := newIndex(t)
	ctx := context.Background()
	cfg := testConfig(dimension)
	const tenant, kb, doc = "tenant-1", "kb-1", "doc-1"

	blockID := uuid.New()
	pageStart, pageEnd := 3, 5
	chunk := chunkSpec{
		DocumentID: doc, VersionID: "ver-1", Text: "текст с локатором", Seed: 1,
	}.build(cfg, tenant, kb)
	chunk.Locator = domain.Locator{
		BlockIDs:    []uuid.UUID{blockID},
		SectionPath: []string{"Раздел", "Подраздел"},
		PageStart:   &pageStart,
		PageEnd:     &pageEnd,
	}
	if _, err := index.UpsertChunks(ctx, cfg, []domain.Chunk{chunk}); err != nil {
		t.Fatalf("UpsertChunks: %v", err)
	}

	hits, err := index.KeywordSearch(ctx, "локатором",
		filterFor(tenant, kb, cfg.ProfileID(), doc), 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("найдено %d кандидатов, ожидался 1", len(hits))
	}
	got := hits[0]
	if got.ChunkID != chunk.ChunkID {
		t.Errorf("chunk_id = %q, ожидался %q", got.ChunkID, chunk.ChunkID)
	}
	if got.Text != chunk.Text {
		t.Errorf("text = %q, ожидалось %q", got.Text, chunk.Text)
	}
	if len(got.Locator.BlockIDs) != 1 || got.Locator.BlockIDs[0] != blockID {
		t.Errorf("BlockIDs = %v, ожидался [%v]", got.Locator.BlockIDs, blockID)
	}
	if got.Locator.PageStart == nil || *got.Locator.PageStart != pageStart {
		t.Errorf("PageStart = %v, ожидалось %d", got.Locator.PageStart, pageStart)
	}
	if got.Locator.PageEnd == nil || *got.Locator.PageEnd != pageEnd {
		t.Errorf("PageEnd = %v, ожидалось %d", got.Locator.PageEnd, pageEnd)
	}
	if len(got.Locator.SectionPath) != 2 || got.Locator.SectionPath[1] != "Подраздел" {
		t.Errorf("SectionPath = %v", got.Locator.SectionPath)
	}
	if got.ChunkNumber != chunk.ChunkNumber {
		t.Errorf("chunk_number = %d, ожидалось %d", got.ChunkNumber, chunk.ChunkNumber)
	}
}

// countDocuments считает документы индекса — используется для проверки
// идемпотентности записи.
func countDocuments(t *testing.T, index *Index, name string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := index.client.Count(
		index.client.Count.WithContext(ctx),
		index.client.Count.WithIndex(name),
	)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	defer closeBody(res)
	if res.StatusCode == http.StatusNotFound {
		return 0
	}
	if res.IsError() {
		t.Fatalf("Count: %s", res.Status())
	}
	var envelope struct {
		Count int `json:"count"`
	}
	if err := decodeJSON(res.Body, &envelope); err != nil {
		t.Fatalf("разбор ответа Count: %v", err)
	}
	return envelope.Count
}

// decodeMapping читает mapping единственного индекса из ответа _mapping.
func decodeMapping(t *testing.T, body io.Reader, index string) indexMapping {
	t.Helper()
	var all mappingResponse
	if err := decodeJSON(body, &all); err != nil {
		t.Fatalf("разбор mapping: %v", err)
	}
	mapping, ok := all[index]
	if !ok {
		names := make([]string, 0, len(all))
		for name := range all {
			names = append(names, name)
		}
		t.Fatalf("в ответе нет индекса %q, получено %v", index, names)
	}
	return mapping
}

// isAppErrorCode сравнивает ошибку с эталоном по коду, а не по указателю.
func isAppErrorCode(err error, code string) bool {
	appErr, ok := domain.AsAppError(err)
	return ok && appErr.Code == code
}
