package search_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/application/search"
	"github.com/kbsearch/backend-go/internal/application/settings"
	"github.com/kbsearch/backend-go/internal/domain"
)

// Тесты сценария идут на заглушках портов: правила порядка, доступа и потолков
// нужно проверять на всех сочетаниях, а не на тех, что случайно оказались в
// тестовой базе.

// ── Заглушки портов ─────────────────────────────────────────────────

// fakeIndex запоминает фильтры и запросы, которые получил сценарий.
// Ветви поиска идут параллельно, поэтому записи защищены мьютексом.
type fakeIndex struct {
	mu          sync.Mutex
	keyword     []domain.SearchCandidate
	vector      []domain.SearchCandidate
	keywordErr  error
	vectorErr   error
	keywordCall int
	vectorCall  int
	lastFilter  domain.SearchFilter
	lastLimit   int
	lastQuery   string
	lastVector  []float32
}

func (f *fakeIndex) EnsureIndex(context.Context, domain.IndexingConfig) (string, error) {
	return "", errors.New("не используется")
}
func (f *fakeIndex) UpsertChunks(context.Context, domain.IndexingConfig, []domain.Chunk) (int, error) {
	return 0, errors.New("не используется")
}
func (f *fakeIndex) DeleteVersion(context.Context, string, string, string) (int, error) {
	return 0, errors.New("не используется")
}
func (f *fakeIndex) DeleteDocument(context.Context, string, string) (int, error) {
	return 0, errors.New("не используется")
}
func (f *fakeIndex) Ping(context.Context) error { return nil }

func (f *fakeIndex) KeywordSearch(
	_ context.Context, text string, filter domain.SearchFilter, limit int,
) ([]domain.SearchCandidate, error) {
	f.mu.Lock()
	f.keywordCall++
	f.lastFilter = filter
	f.lastLimit = limit
	f.lastQuery = text
	f.mu.Unlock()
	return f.keyword, f.keywordErr
}

func (f *fakeIndex) VectorSearch(
	_ context.Context, vec []float32, _ domain.IndexingConfig, filter domain.SearchFilter, limit int,
) ([]domain.SearchCandidate, error) {
	f.mu.Lock()
	f.vectorCall++
	f.lastFilter = filter
	f.lastLimit = limit
	f.lastVector = vec
	f.mu.Unlock()
	return f.vector, f.vectorErr
}

// fakeEmbedder отдаёт вектор заданной размерности.
type fakeEmbedder struct {
	dimension int
	err       error
	calls     int
}

func (f *fakeEmbedder) Embed(
	_ context.Context, _ []string, _ domain.IndexingConfig,
) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return [][]float32{make([]float32, f.dimension)}, nil
}
func (f *fakeEmbedder) EmbedQuery(
	_ context.Context, _ string, _ domain.IndexingConfig,
) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return make([]float32, f.dimension), nil
}
func (f *fakeEmbedder) Dimensions() int            { return f.dimension }
func (f *fakeEmbedder) Ping(context.Context) error { return nil }
func (f *fakeEmbedder) Close() error               { return nil }

// fakeKBRows отдаёт базу знаний или её отсутствие. Остальные методы порта
// сценарию поиска не нужны и падают, если он к ним обратится.
type fakeKBRows struct {
	kb  domain.KB
	err error
}

func (f *fakeKBRows) Require(
	context.Context, ports.Queryer, domain.Principal, string,
) (domain.KB, error) {
	return f.kb, f.err
}

func (f *fakeKBRows) Create(
	context.Context, ports.Queryer, domain.Principal, domain.CreateKBInput,
) (domain.KB, error) {
	return domain.KB{}, errUnusedPort
}
func (f *fakeKBRows) List(
	context.Context, ports.Queryer, domain.Principal, int, string,
) ([]domain.KB, string, error) {
	return nil, "", errUnusedPort
}
func (f *fakeKBRows) SetACL(context.Context, ports.Queryer, string, domain.ACL) error {
	return errUnusedPort
}
func (f *fakeKBRows) SetDocumentACL(
	context.Context, ports.Queryer, string, *domain.ACL,
) error {
	return errUnusedPort
}
func (f *fakeKBRows) EmbeddingProfileID(context.Context, ports.Queryer, string) (string, error) {
	return "", errUnusedPort
}

// fakeDocs отдаёт список активных версий и source_url версий.
type fakeDocs struct {
	active  []domain.ActiveVersion
	sources map[string]string
	err     error
	calls   int
}

func (f *fakeDocs) GetAllowedActiveVersions(
	context.Context, ports.Queryer, domain.Principal, string,
) ([]domain.ActiveVersion, error) {
	f.calls++
	return f.active, f.err
}

func (f *fakeDocs) VersionSources(
	_ context.Context, _ ports.Queryer, _ domain.Principal, _ string, _ map[string]string,
) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sources, nil
}

func (f *fakeDocs) LoadParsed(
	context.Context, ports.Queryer, ports.FileStorage, string, string,
) (domain.ParsedDocument, error) {
	return domain.ParsedDocument{}, errUnusedPort
}
func (f *fakeDocs) ActivateVersion(
	context.Context, ports.Queryer, domain.ActivateParams,
) (string, error) {
	return "", errUnusedPort
}
func (f *fakeDocs) IngestBusy(context.Context, ports.Queryer, string) (bool, error) {
	return false, errUnusedPort
}
func (f *fakeDocs) IsVersionActive(context.Context, ports.Queryer, string, string) (bool, error) {
	return false, errUnusedPort
}

// errUnusedPort — заглушка для методов порта, которые сценарий вызывать не
// должен: молчащий успех скрыл бы ошибку вызова.
var errUnusedPort = errors.New("метод порта не используется сценарием")

// fakeQueryer не используется сценарием: он нужен только чтобы удовлетворить
// сигнатуры репозиториев.
type fakeQueryer struct{}

func (fakeQueryer) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, errors.New("не используется")
}
func (fakeQueryer) QueryRow(context.Context, string, ...any) ports.Row {
	return nil
}
func (fakeQueryer) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, errors.New("не используется")
}

// fakeSettings отдаёт заранее заданные уровни настроек.
type fakeSettings struct {
	kbLevel     map[string]any
	tenantLevel map[string]any
}

func (f *fakeSettings) Levels(
	context.Context, ports.Queryer, string, string,
) (map[string]any, map[string]any, error) {
	return f.kbLevel, f.tenantLevel, nil
}

// ── Общее ───────────────────────────────────────────────────────────

var (
	testKBID       = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	testDocumentID = "33333333-3333-4333-8333-333333333333"
	testVersionID  = "44444444-4444-4444-8444-444444444444"
)

// baselineKeys повторяет значения локального стенда (docs/settings.md).
func baselineKeys() map[string]any {
	return map[string]any{
		settings.EmbeddingProviderID.Name():   "primary",
		settings.EmbeddingModel.Name():        "multilingual-e5-small",
		settings.EmbeddingRevision.Name():     "pilot-1",
		settings.EmbeddingDimension.Name():    int64(384),
		settings.EmbeddingNormalize.Name():    true,
		settings.EmbeddingDistance.Name():     "cosine",
		settings.ChunkingTokenizerID.Name():   "e5",
		settings.ChunkingMaxTokens.Name():     int64(600),
		settings.ChunkingOverlapTokens.Name(): int64(100),
		settings.SearchTopK.Name():            int64(10),
	}
}

// testIndexingConfig повторяет профиль из baselineKeys: он нужен заглушкам,
// которые сравнивают profile_id.
func testIndexingConfig(t *testing.T) domain.IndexingConfig {
	t.Helper()
	svc := settings.New(&fakeSettings{tenantLevel: baselineKeys()})
	cfg, err := svc.IndexingConfig(context.Background(), fakeQueryer{},
		domain.LocalTenantID.String(), testKBID.String())
	if err != nil {
		t.Fatalf("профиль из настроек не собирается: %v", err)
	}
	return cfg
}

func newService(
	t *testing.T, index *fakeIndex, docs *fakeDocs, embedder *fakeEmbedder, kb domain.KB,
) (*search.Service, domain.IndexingConfig) {
	t.Helper()
	return search.New(
		fakeQueryer{},
		&fakeKBRows{kb: kb},
		docs,
		index,
		embedder,
		settings.New(&fakeSettings{tenantLevel: baselineKeys()}),
	), testIndexingConfig(t)
}

func testRequest() search.Request {
	return search.Request{KBID: testKBID, Query: "как настроить выгрузку", TopK: 10}
}

// allowedDocs — один доступный документ с активной версией.
func allowedDocs() []domain.ActiveVersion {
	return []domain.ActiveVersion{{
		DocumentID: testDocumentID, ActiveVersionID: testVersionID,
	}}
}

// hit строит кандидата с заданным идентификатором и номером документа.
func hit(chunkID, documentID, versionID string, number int) domain.SearchCandidate {
	return domain.SearchCandidate{
		ChunkID:     chunkID,
		TenantID:    domain.LocalTenantID.String(),
		KBID:        testKBID.String(),
		DocumentID:  documentID,
		VersionID:   versionID,
		ChunkNumber: number,
		Title:       "Инструкция по выгрузке",
		Text:        "Для настройки выгрузки откройте раздел Интеграции.",
		Locator:     domain.Locator{SectionPath: []string{"Настройка", "Выгрузка"}},
		Score:       7,
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// ── Слияние ветвей и порядок ────────────────────────────────────────

// TestSearchMergesBranchesByRRF: чанк из обеих ветвей идёт первым и несёт в
// выдаче RRF-score, а не score ветви.
func TestSearchMergesBranchesByRRF(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{
			hit("sha256:aaa", testDocumentID, testVersionID, 1),
			hit("sha256:bbb", testDocumentID, testVersionID, 2),
		},
		vector: []domain.SearchCandidate{
			hit("sha256:bbb", testDocumentID, testVersionID, 2),
			hit("sha256:ccc", testDocumentID, testVersionID, 3),
		},
	}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if out.Status != search.StatusOK {
		t.Fatalf("status = %q, ожидался %q", out.Status, search.StatusOK)
	}
	if len(out.Hits) != 3 {
		t.Fatalf("хитов %d, ожидалось 3", len(out.Hits))
	}
	if out.Hits[0].ChunkID != "sha256:bbb" {
		t.Errorf("первым идёт %s, ожидался sha256:bbb — чанк из обеих ветвей", out.Hits[0].ChunkID)
	}
	want := 1.0/62 + 1.0/61
	if diff := out.Hits[0].Score - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("score = %v, ожидался RRF %v", out.Hits[0].Score, want)
	}
	if out.Hits[0].Score == 7 {
		t.Error("в выдачу попал score ветви вместо RRF")
	}
}

// TestSearchIsReproducibleOnEqualScores: при равном RRF порядок задан
// chunk_id, а не порядком ветвей и не порядком обхода карты.
func TestSearchIsReproducibleOnEqualScores(t *testing.T) {
	// Взаимно перевёрнутые ветви дают обоим чанкам один и тот же RRF, и
	// единственный различающий их признак — chunk_id.
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{
			hit("sha256:ccc", testDocumentID, testVersionID, 1),
			hit("sha256:aaa", testDocumentID, testVersionID, 2),
		},
		vector: []domain.SearchCandidate{
			hit("sha256:aaa", testDocumentID, testVersionID, 1),
			hit("sha256:ccc", testDocumentID, testVersionID, 2),
		},
	}
	docs := &fakeDocs{active: allowedDocs()}
	svc, _ := newService(t, index, docs, &fakeEmbedder{dimension: 384}, domain.KB{KBID: testKBID.String()})

	first, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if first.Hits[0].ChunkID != "sha256:aaa" {
		t.Errorf("первым идёт %s, ожидался sha256:aaa при равном score", first.Hits[0].ChunkID)
	}
	for i := 0; i < 5; i++ {
		again, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
		if err != nil {
			t.Fatalf("поиск вернул ошибку на повторе %d: %v", i, err)
		}
		if !sameOrder(first.Hits, again.Hits) {
			t.Fatalf("порядок изменился на повторе %d:\n%v\n%v", i, chunkIDs(first.Hits), chunkIDs(again.Hits))
		}
	}
}

// ── Доступ и изоляция ───────────────────────────────────────────────

// TestSearchPassesIsolationFilterToBothBranches: обе ветви получают один и тот
// же фильтр с tenant, базой, профилем и списком разрешённых документов.
func TestSearchPassesIsolationFilterToBothBranches(t *testing.T) {
	docs := &fakeDocs{active: []domain.ActiveVersion{
		{DocumentID: testDocumentID, ActiveVersionID: testVersionID},
		{DocumentID: "66666666-6666-4666-8666-666666666666", ActiveVersionID: "77777777-7777-4777-8777-777777777777"},
	}}
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
		vector:  []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
	}
	svc, cfg := newService(t, index, docs, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String(), EmbeddingProfileID: cfgProfile(t)})

	if _, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest()); err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}

	if index.keywordCall != 1 || index.vectorCall != 1 {
		t.Fatalf("вызовов ветвей: keyword %d, vector %d — ожидалось по одному", index.keywordCall, index.vectorCall)
	}
	want := domain.SearchFilter{
		TenantID:           domain.LocalTenantID.String(),
		KBID:               testKBID.String(),
		EmbeddingProfileID: cfg.ProfileID(),
		AllowedDocumentIDs: []string{
			testDocumentID,
			"66666666-6666-4666-8666-666666666666",
		},
	}
	if !sameFilter(index.lastFilter, want) {
		t.Errorf("фильтр индекса:\n%+v\nожидался:\n%+v", index.lastFilter, want)
	}
	if len(index.lastVector) != cfg.EmbeddingDimension {
		t.Errorf("размерность вектора вопроса %d, ожидалось %d", len(index.lastVector), cfg.EmbeddingDimension)
	}
}

// TestSearchSkipsIndexWhenNoDocumentsAllowed: у principal без доступа к
// документам выдача пуста, и индекс не трогается (ТЗ 2.1, 6.4).
func TestSearchSkipsIndexWhenNoDocumentsAllowed(t *testing.T) {
	index := &fakeIndex{keyword: []domain.SearchCandidate{
		hit("sha256:aaa", testDocumentID, testVersionID, 1),
	}}
	embedder := &fakeEmbedder{dimension: 384}
	svc, _ := newService(t, index, &fakeDocs{}, embedder, domain.KB{KBID: testKBID.String()})

	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if out.Status != search.StatusNoResults {
		t.Errorf("status = %q, ожидался %q", out.Status, search.StatusNoResults)
	}
	if out.Hits == nil {
		t.Error("hits = nil: в JSON поле должно стать [], а не null")
	}
	if index.keywordCall != 0 || index.vectorCall != 0 {
		t.Errorf("индекс запрошен без доступных документов: keyword %d, vector %d",
			index.keywordCall, index.vectorCall)
	}
	if embedder.calls != 0 {
		t.Errorf("эмбеддер вызван без доступных документов: %d раз", embedder.calls)
	}
}

// TestSearchRejectsForeignKB: отказ Require на чужую базу останавливает поиск
// до чтения настроек и до обращения к индексу.
func TestSearchRejectsForeignKB(t *testing.T) {
	index := &fakeIndex{keyword: []domain.SearchCandidate{
		hit("sha256:aaa", testDocumentID, testVersionID, 1),
	}}
	kbErr := domain.NotFound("База знаний не найдена")
	denied := search.New(fakeQueryer{}, &fakeKBRows{err: kbErr}, &fakeDocs{active: allowedDocs()},
		index, &fakeEmbedder{dimension: 384},
		settings.New(&fakeSettings{tenantLevel: baselineKeys()}))

	if _, err := denied.Search(testCtx(t), domain.LocalPrincipal(), testRequest()); !errors.Is(err, kbErr) {
		t.Fatalf("ошибка доступа = %v, ожидалась %v", err, kbErr)
	}
	if index.keywordCall != 0 || index.vectorCall != 0 {
		t.Errorf("индекс запрошен для чужой базы: keyword %d, vector %d",
			index.keywordCall, index.vectorCall)
	}
}

// TestSearchDropsInactiveVersions: чанк версии, которая больше не активна, в
// выдачу не попадает (ТЗ 7.4 п.7).
func TestSearchDropsInactiveVersions(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{
			hit("sha256:aaa", testDocumentID, testVersionID, 1),
			hit("sha256:bbb", testDocumentID, "99999999-9999-4999-8999-999999999999", 2),
		},
	}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Hits) != 1 || out.Hits[0].ChunkID != "sha256:aaa" {
		t.Fatalf("хиты %v, ожидался только sha256:aaa", chunkIDs(out.Hits))
	}
}

// ── Ссылки на источник ──────────────────────────────────────────────

// TestSearchFillsSourceAndDownloadURL: source_url приходит из метаданных
// PostgreSQL, download_url собирается сервером (ТЗ 7.4 п.8).
func TestSearchFillsSourceAndDownloadURL(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
	}
	docs := &fakeDocs{
		active:  allowedDocs(),
		sources: map[string]string{testVersionID: "https://portal.example/guide/export"},
	}
	svc, _ := newService(t, index, docs, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Hits) != 1 {
		t.Fatalf("хитов %d, ожидался 1", len(out.Hits))
	}
	if out.Hits[0].SourceURL == nil {
		t.Fatal("SourceURL = nil, ожидался source_url из метаданных")
	}
	if *out.Hits[0].SourceURL != "https://portal.example/guide/export" {
		t.Errorf("SourceURL = %q", *out.Hits[0].SourceURL)
	}
	wantDownload := "/api/v1/documents/" + testDocumentID + "/source?version_id=" + testVersionID
	if out.Hits[0].DownloadURL != wantDownload {
		t.Errorf("DownloadURL = %q, ожидалось %q", out.Hits[0].DownloadURL, wantDownload)
	}
}

// TestSearchUsesInjectedAPIPrefix: префикс ссылок задаётся полем, пустое
// поле сохраняет контракт /api/v1.
func TestSearchUsesInjectedAPIPrefix(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
	}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})
	svc.APIPrefix = "/custom"

	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	want := "/custom/documents/" + testDocumentID + "/source?version_id=" + testVersionID
	if out.Hits[0].DownloadURL != want {
		t.Errorf("DownloadURL = %q, ожидалось %q", out.Hits[0].DownloadURL, want)
	}
}

// TestSearchWithoutSourceURLLeavesLinkEmpty: у версии без source_url ссылка
// на источник не выдумывается, download_url остаётся.
func TestSearchWithoutSourceURLLeavesLinkEmpty(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
	}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if out.Hits[0].SourceURL != nil {
		t.Errorf("SourceURL = %v, ожидался nil", *out.Hits[0].SourceURL)
	}
	if out.Hits[0].DownloadURL == "" {
		t.Error("DownloadURL пуст")
	}
}

// ── Размер выдачи ───────────────────────────────────────────────────

// TestSearchClampsTopK: top_k выше потолка обрезается, и ветви получают
// limit = min(100, 5 * top_k).
func TestSearchClampsTopK(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
	}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	in := testRequest()
	in.TopK = 5000
	if _, err := svc.Search(testCtx(t), domain.LocalPrincipal(), in); err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if index.lastLimit != search.CandidateLimit(settings.TopKMax) {
		t.Errorf("limit ветви %d, ожидалось %d", index.lastLimit, search.CandidateLimit(settings.TopKMax))
	}
}

// TestSearchTakesTopKFromSettings: top_k = 0 означает «взять search.top_k».
func TestSearchTakesTopKFromSettings(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{
			hit("sha256:aaa", testDocumentID, testVersionID, 1),
			hit("sha256:bbb", testDocumentID, testVersionID, 2),
			hit("sha256:ccc", testDocumentID, testVersionID, 3),
		},
	}
	tenant := baselineKeys()
	tenant[settings.SearchTopK.Name()] = int64(2)
	svc := search.New(fakeQueryer{}, &fakeKBRows{kb: domain.KB{KBID: testKBID.String()}},
		&fakeDocs{active: allowedDocs()}, index, &fakeEmbedder{dimension: 384},
		settings.New(&fakeSettings{tenantLevel: tenant}))

	in := testRequest()
	in.TopK = 0
	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), in)
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Hits) != 2 {
		t.Errorf("хитов %d, ожидалось 2 по search.top_k из настроек", len(out.Hits))
	}
}

// TestSearchKeepsOnlyTopKHits: выдача обрезается по top_k даже при большем
// числе найденных чанков.
func TestSearchKeepsOnlyTopKHits(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{
			hit("sha256:a01", testDocumentID, testVersionID, 1),
			hit("sha256:a02", testDocumentID, testVersionID, 2),
			hit("sha256:a03", testDocumentID, testVersionID, 3),
		},
	}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	in := testRequest()
	in.TopK = 2
	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), in)
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Hits) != 2 {
		t.Errorf("хитов %d, ожидалось 2", len(out.Hits))
	}
}

// ── Отказы ──────────────────────────────────────────────────────────

// TestSearchFailsWhenEmbeddingUnavailable: сбой эмбеддера не превращается в
// молчаливый поиск только по BM25 (ТЗ 7.4).
func TestSearchFailsWhenEmbeddingUnavailable(t *testing.T) {
	embedErr := domain.ServiceUnavailable("Модель эмбеддинга недоступна")
	index := &fakeIndex{keyword: []domain.SearchCandidate{
		hit("sha256:aaa", testDocumentID, testVersionID, 1),
	}}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()},
		&fakeEmbedder{err: embedErr}, domain.KB{KBID: testKBID.String()})

	if _, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest()); !errors.Is(err, embedErr) {
		t.Fatalf("ошибка = %v, ожидалась %v", err, embedErr)
	}
	if index.keywordCall != 0 {
		t.Errorf("ветвь BM25 выполнена при отказе эмбеддера: %d", index.keywordCall)
	}
}

// TestSearchFailsWhenBranchFails: отказ любой ветви — ошибка, а не половина
// выдачи.
func TestSearchFailsWhenBranchFails(t *testing.T) {
	indexErr := domain.IndexUnavailable("Поисковый запрос не выполнен")
	index := &fakeIndex{
		keyword:   []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
		vectorErr: indexErr,
	}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	if _, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest()); !errors.Is(err, indexErr) {
		t.Fatalf("ошибка = %v, ожидалась %v", err, indexErr)
	}
}

// TestSearchStopsOnCancelledContext: отменённый запрос не доходит до индекса.
func TestSearchStopsOnCancelledContext(t *testing.T) {
	index := &fakeIndex{}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.Search(ctx, domain.LocalPrincipal(), testRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ошибка = %v, ожидался context.Canceled", err)
	}
	if index.keywordCall != 0 || index.vectorCall != 0 {
		t.Error("индекс запрошен после отмены контекста")
	}
}

// TestSearchRejectsEmptyQuery: пустой вопрос — ошибка ввода, а не пустая выдача.
func TestSearchRejectsEmptyQuery(t *testing.T) {
	index := &fakeIndex{}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	in := testRequest()
	in.Query = "   "
	_, err := svc.Search(testCtx(t), domain.LocalPrincipal(), in)
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeInvalidMetadata {
		t.Fatalf("ошибка = %v, ожидался код %s", err, domain.CodeInvalidMetadata)
	}
}

// TestSearchRequiresRebuildWhenProfileChanged: настройки эмбеддинга изменились
// после индексации — 409 и требование переиндексации, а не поиск по чужому
// профилю.
func TestSearchRequiresRebuildWhenProfileChanged(t *testing.T) {
	index := &fakeIndex{}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String(), EmbeddingProfileID: "sha256:" + repeat("f", 64)})

	_, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if !errors.Is(err, domain.ErrEmbeddingProfileNeedsRebuild) {
		t.Fatalf("ошибка = %v, ожидался EMBEDDING_PROFILE_CHANGE_REQUIRES_REBUILD", err)
	}
	if index.keywordCall != 0 {
		t.Error("индекс запрошен при расхождении профиля")
	}
}

// ── Помощники ───────────────────────────────────────────────────────

// cfgProfile отдаёт профиль из настроек стенда.
func cfgProfile(t *testing.T) string {
	t.Helper()
	return testIndexingConfig(t).ProfileID()
}

func sameFilter(a, b domain.SearchFilter) bool {
	if a.TenantID != b.TenantID || a.KBID != b.KBID || a.EmbeddingProfileID != b.EmbeddingProfileID {
		return false
	}
	if len(a.AllowedDocumentIDs) != len(b.AllowedDocumentIDs) {
		return false
	}
	for i := range a.AllowedDocumentIDs {
		if a.AllowedDocumentIDs[i] != b.AllowedDocumentIDs[i] {
			return false
		}
	}
	return true
}

func chunkIDs(hits []domain.SearchCandidate) []string {
	out := make([]string, 0, len(hits))
	for _, item := range hits {
		out = append(out, item.ChunkID)
	}
	return out
}

func sameOrder(a, b []domain.SearchCandidate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ChunkID != b[i].ChunkID {
			return false
		}
	}
	return true
}
