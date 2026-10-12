package search_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/docs/settings"
	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
	search "github.com/kbsearch/kb/internal/search/service"
)

// Тесты сценария идут на заглушках портов: правила порядка, доступа и потолков
// нужно проверять на всех сочетаниях, а не на тех, что случайно оказались в
// тестовой базе.

// ── Заглушки портов ─────────────────────────────────────────────────

// fakeIndex реализует ОБА порта индекса и помнит, какой из них позвали.
//
// Заглушка нужна именно в таком виде: сценарий §6 зовёт scoped-порт
// (KeywordSearchScoped / VectorSearchScoped), а legacy-порт с
// domain.SearchFilter.AllowedDocumentIDs позвать не должен вовсе. Поэтому вызовы
// считаются раздельно, а legacy-методы возвращают ошибку, а не пустую выдачу:
// вернули бы — и «успешный» поиск по нефильтрованному индексу выглядел бы
// рабочим.
//
// Ветви идут параллельно, поэтому записи защищены мьютексом.
type fakeIndex struct {
	mu          sync.Mutex
	keyword     []domain.SearchCandidate
	vector      []domain.SearchCandidate
	keywordErr  error
	vectorErr   error
	keywordCall int
	vectorCall  int
	legacyCalls int
	lastScope   domain.AccessScope
	lastConfig  domain.IndexingConfig
	lastLimit   int
	lastQuery   string
	lastVector  []float32
	// byScope отвечает по области доступа — им проверяется, что два разных
	// пользователя с разными правами получают разные выдачи.
	byScope func(domain.AccessScope) []domain.SearchCandidate
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

// branch отдаёт выдачу ветви: заданную по области, если она есть, иначе общую.
func (f *fakeIndex) branch(scope domain.AccessScope, fallback []domain.SearchCandidate) []domain.SearchCandidate {
	if f.byScope == nil {
		return fallback
	}
	return f.byScope(scope)
}

// KeywordSearchScoped — текстовая ветвь порта §6. Область приходит аргументом
// и уезжает внутрь запроса; список документов сценарий больше не строит.
func (f *fakeIndex) KeywordSearchScoped(
	_ context.Context, text string, scope domain.AccessScope, cfg domain.IndexingConfig, limit int,
) ([]domain.SearchCandidate, error) {
	f.mu.Lock()
	f.keywordCall++
	f.lastScope = scope
	f.lastConfig = cfg
	f.lastLimit = limit
	f.lastQuery = text
	f.mu.Unlock()
	if f.keywordErr != nil {
		return nil, f.keywordErr
	}
	return f.branch(scope, f.keyword), nil
}

// VectorSearchScoped — векторная ветвь порта §6. Порядок параметров здесь
// (cfg, scope), у текстовой — (scope, cfg); это подпись порта.
func (f *fakeIndex) VectorSearchScoped(
	_ context.Context, vec []float32, cfg domain.IndexingConfig, scope domain.AccessScope, limit int,
) ([]domain.SearchCandidate, error) {
	f.mu.Lock()
	f.vectorCall++
	f.lastScope = scope
	f.lastConfig = cfg
	f.lastLimit = limit
	f.lastVector = vec
	f.mu.Unlock()
	if f.vectorErr != nil {
		return nil, f.vectorErr
	}
	return f.branch(scope, f.vector), nil
}

// KeywordSearch — legacy-путь §6 запретил.
//
// Счётчик legacyCalls — страховка от тихого возврата: пока Service.index имеет
// тип ports.SecuredSearchIndex, эти методы недостижимы компилятором. Но стоит
// расширить поле обратно до ports.SearchIndex, заглушка начнёт отдавать
// нефильтрованную выдачу, и тест это заметит — иначе «успешный» поиск мимо
// прав выглядел бы рабочим.
func (f *fakeIndex) KeywordSearch(
	context.Context, string, domain.SearchFilter, int,
) ([]domain.SearchCandidate, error) {
	f.mu.Lock()
	f.legacyCalls++
	f.mu.Unlock()
	return nil, errUnusedPort
}

// VectorSearch — legacy-путь §6 запретил (см. KeywordSearch).
func (f *fakeIndex) VectorSearch(
	context.Context, []float32, domain.IndexingConfig, domain.SearchFilter, int,
) ([]domain.SearchCandidate, error) {
	f.mu.Lock()
	f.legacyCalls++
	f.mu.Unlock()
	return nil, errUnusedPort
}

// Оба порта индекса описаны одной заглушкой намеренно: настоящий адаптер
// реализует их оба, и заглушка обязана быть таким же честным двойником —
// иначе она проверяла бы не тот контракт, по которому работает прод.
var (
	_ ports.SearchIndex        = (*fakeIndex)(nil)
	_ ports.SecuredSearchIndex = (*fakeIndex)(nil)
)

// fakeACL — оракул прав §6 на заглушке.
//
// Ключ области — user_id: два пользователя с разными правами обязаны получить
// разные области, а не одну «на всех». Отсутствие пользователя в таблице прав —
// пустая область, то есть «не разрешено ничего» (§6: запретов нет, есть только
// разрешения). err задаёт сбой оракула: он обязан дойти до сценария отказом.
type fakeACL struct {
	scopes map[string]domain.AccessScope
	err    error
	calls  int
}

func (f *fakeACL) ResolveScope(
	_ context.Context, _ ports.Queryer, p domain.Principal, _ string,
) (domain.AccessScope, error) {
	f.calls++
	if f.err != nil {
		return domain.AccessScope{}, f.err
	}
	scope, ok := f.scopes[p.UserID]
	if !ok {
		return domain.AccessScope{TenantID: p.TenantID.String()}, nil
	}
	return scope, nil
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

// DocumentRights — оракул прав. Поиск права не разрешает: он спрашивает у
// оракула права СВОЕГО пользователя (ACLResolver), а права документа приходят в
// индекс на стадии индексации. Заглушка отвечает «наследование», и документ
// виден по своему контейнеру.
func (f *fakeDocs) DocumentRights(
	_ context.Context, _ ports.Queryer, _, _ string,
) (domain.DocumentRights, error) {
	return domain.DocumentRights{SpaceID: ports.InheritRights}, nil
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

// LoadVersion стадии convert: поиск и ответы версию не читают.
func (f *fakeDocs) LoadVersion(
	context.Context, ports.Queryer, ports.FileStorage, string, string,
) (domain.ConvertVersion, error) {
	return domain.ConvertVersion{}, errUnusedPort
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

// Документ и контейнеры §6 для тестов прав: hr-контейнер виден кадровику,
// платформенный — инженеру, и каждый документ читается только своим.
const (
	hrSpace            = "space-hr"
	platformSpace      = "space-platform"
	hrDocumentID       = "55555555-5555-4555-8555-555555555555"
	hrVersionID        = "66666666-6666-4666-8666-666666666666"
	hrChunkID          = "sha256:hr"
	platformDocumentID = "77777777-7777-4777-8777-777777777777"
	platformVersionID  = "88888888-8888-4888-8888-888888888888"
	platformChunkID    = "sha256:platform"

	// revokedDocumentID — документ, право на который индекс ещё отдаёт, а
	// Postgres уже не считает читаемым: проверка защиты в глубину.
	revokedDocumentID = "99999999-9999-4999-8999-999999999999"
	revokedVersionID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

// principalWithID — пользователь локального тенанта без групп: его область
// целиком задаёт оракул.
func principalWithID(userID string) domain.Principal {
	return domain.Principal{TenantID: domain.LocalTenantID, UserID: userID}
}

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

// newService собирает сценарий. acl обязателен: §6 запрещает собирать поиск без
// оракула прав, поэтому «сценарного» оракула по умолчанию тут нет — есть две
// явные области: localACL (пользователь с правами) и noRights (без прав).
func newService(
	t *testing.T, index *fakeIndex, docs *fakeDocs, acl *fakeACL,
	embedder *fakeEmbedder, kb domain.KB,
) (*search.Service, domain.IndexingConfig) {
	t.Helper()
	return search.New(
		fakeQueryer{},
		&fakeKBRows{kb: kb},
		docs,
		index,
		acl,
		embedder,
		settings.New(&fakeSettings{tenantLevel: baselineKeys()}),
	), testIndexingConfig(t)
}

// localScope — область стендового пользователя: контейнер платформы и личный
// терм «user:». Оба терма §6 перечисляет как содержащие документ.
func localScope() domain.AccessScope {
	return domain.AccessScope{
		TenantID:   domain.LocalTenantID.String(),
		KBID:       testKBID.String(),
		SpaceIDs:   []string{"space-platform"},
		Principals: []string{"user:" + domain.LocalUserID},
	}
}

// localACL — оракул, который стендовому пользователю выдаёт localScope.
func localACL() *fakeACL {
	return &fakeACL{scopes: map[string]domain.AccessScope{domain.LocalUserID: localScope()}}
}

// noRights — оракул, у которого для пользователя нет ни одного контейнера и ни
// одного принципала. Это не «ошибка прав», а честное «нечего не видно».
func noRights() *fakeACL { return &fakeACL{scopes: map[string]domain.AccessScope{}} }

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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, docs, localACL(), &fakeEmbedder{dimension: 384}, domain.KB{KBID: testKBID.String()})

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

// TestSearchPassesIsolationFilterToBothBranches: обе ветви получают одно и то же
// изоляционное ограничение — тенант, базу, профиль и права.
//
// Форма ограничения после §6 — область доступа (AccessScope) плюс профиль в
// IndexingConfig, а не список разрешённых document_id: список упирался в
// index.max_terms_count и требовал переиндексации при смене прав. Изоляционная
// часть проверки (тенант, база, профиль) осталась ровно той же.
func TestSearchPassesIsolationFilterToBothBranches(t *testing.T) {
	docs := &fakeDocs{active: []domain.ActiveVersion{
		{DocumentID: testDocumentID, ActiveVersionID: testVersionID},
		{DocumentID: "66666666-6666-4666-8666-666666666666", ActiveVersionID: "77777777-7777-4777-8777-777777777777"},
	}}
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
		vector:  []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
	}
	svc, cfg := newService(t, index, docs, localACL(), &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String(), EmbeddingProfileID: cfgProfile(t)})

	if _, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest()); err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}

	if index.keywordCall != 1 || index.vectorCall != 1 {
		t.Fatalf("вызовов ветвей: keyword %d, vector %d — ожидалось по одному", index.keywordCall, index.vectorCall)
	}
	want := localScope()
	if !sameScope(index.lastScope, want) {
		t.Errorf("область доступа в ветвях:\n%+v\nожидалась:\n%+v", index.lastScope, want)
	}
	if index.lastConfig.ProfileID() != cfg.ProfileID() {
		t.Errorf("профиль в ветвях %q, ожидался %q", index.lastConfig.ProfileID(), cfg.ProfileID())
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
	svc, _ := newService(t, index, &fakeDocs{}, noRights(), embedder, domain.KB{KBID: testKBID.String()})

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
		index, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Hits) != 1 || out.Hits[0].ChunkID != "sha256:aaa" {
		t.Fatalf("хиты %v, ожидался только sha256:aaa", chunkIDs(out.Hits))
	}
}

// ── §6: права внутри запроса (регрессия переезда) ──────────────────────────────

// TestSearchCallsScopedIndexAndNeverLegacyFilter — регрессионный тест переезда
// на §6: сценарий зовёт ports.SecuredSearchIndex и НИ РАЗУ не собирает
// domain.SearchFilter с AllowedDocumentIDs.
//
// Смысл проверки в одну строку: старый путь материализовал все доступные
// document_id на каждый запрос и увозил их в Elasticsearch, а §6 требует, чтобы
// права ехали в запрос термами (tenant_id + space_id + acl). Вернуть старый путь
// легко и незаметно — порты и адаптер для этого по-прежнему живы, — поэтому
// запрет закреплён тестом, а не комментарием.
func TestSearchCallsScopedIndexAndNeverLegacyFilter(t *testing.T) {
	index := &fakeIndex{
		keyword: []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
		vector:  []domain.SearchCandidate{hit("sha256:aaa", testDocumentID, testVersionID, 1)},
	}
	// Два документа, чтобы список «разрешённых» выглядел бы максимально
	// заметным, если бы сценарий его строил.
	docs := &fakeDocs{active: []domain.ActiveVersion{
		{DocumentID: testDocumentID, ActiveVersionID: testVersionID},
		{DocumentID: revokedDocumentID, ActiveVersionID: revokedVersionID},
	}}
	svc, cfg := newService(t, index, docs, localACL(), &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	if _, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest()); err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}

	if index.keywordCall != 1 || index.vectorCall != 1 {
		t.Fatalf("вызовов ветвей: keyword %d, vector %d — ожидалось по одному",
			index.keywordCall, index.vectorCall)
	}
	if index.legacyCalls != 0 {
		t.Fatalf("вызовов legacy-порта (SearchFilter.AllowedDocumentIDs): %d, ожидался 0 — §6 запретил этот путь",
			index.legacyCalls)
	}
	// Область приходит ровно та, которую выдал оракул прав: термы запроса, а не
	// идентификаторы документов.
	if !sameScope(index.lastScope, localScope()) {
		t.Errorf("область доступа в ветвях:\n%+v\nожидалась:\n%+v", index.lastScope, localScope())
	}
	if index.lastConfig.ProfileID() != cfg.ProfileID() {
		t.Errorf("профиль эмбеддинга в ветвях %q, ожидался %q",
			index.lastConfig.ProfileID(), cfg.ProfileID())
	}
	if index.lastQuery != testRequest().Query {
		t.Errorf("текст запроса в ветви %q, ожидался %q", index.lastQuery, testRequest().Query)
	}
}

// TestSearchScopesDifferentlyPerPrincipal: два пользователя с разными правами
// получают РАЗНЫЕ области и разные выдачи (таблично).
//
// Если бы права не попадали в запрос, оба видели бы одно и то же — и разница
// между «свой контейнер» и «чужой контейнер» исчезла бы ровно там, где она и
// нужна.
func TestSearchScopesDifferentlyPerPrincipal(t *testing.T) {
	cases := []struct {
		name      string
		principal domain.Principal
		wantSpace string
		wantChunk string
	}{
		{
			name:      "кадровый сотрудник видит свой контейнер",
			principal: principalWithID("anna"),
			wantSpace: hrSpace,
			wantChunk: hrChunkID,
		},
		{
			name:      "инженер видит свой контейнер",
			principal: principalWithID("petr"),
			wantSpace: platformSpace,
			wantChunk: platformChunkID,
		},
	}
	seen := map[string]domain.AccessScope{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Индекс отвечает по области: контейнер определяет, что вернётся.
			// Это и есть проверка «права едут в запрос», а не проверка ответа.
			index := &fakeIndex{byScope: func(scope domain.AccessScope) []domain.SearchCandidate {
				if slices.Contains(scope.SpaceIDs, hrSpace) {
					return []domain.SearchCandidate{
						hit(hrChunkID, hrDocumentID, hrVersionID, 1)}
				}
				return []domain.SearchCandidate{
					hit(platformChunkID, platformDocumentID, platformVersionID, 1)}
			}}
			docs := &fakeDocs{active: []domain.ActiveVersion{
				{DocumentID: hrDocumentID, ActiveVersionID: hrVersionID},
				{DocumentID: platformDocumentID, ActiveVersionID: platformVersionID},
			}}
			acl := &fakeACL{scopes: map[string]domain.AccessScope{
				"anna": {SpaceIDs: []string{hrSpace}, Principals: []string{"user:anna"}},
				"petr": {SpaceIDs: []string{platformSpace}, Principals: []string{"user:petr"}},
			}}
			svc, _ := newService(t, index, docs, acl, &fakeEmbedder{dimension: 384},
				domain.KB{KBID: testKBID.String()})

			out, err := svc.Search(testCtx(t), tc.principal, testRequest())
			if err != nil {
				t.Fatalf("поиск вернул ошибку: %v", err)
			}
			if out.Status != search.StatusOK {
				t.Fatalf("status = %q, ожидался %q", out.Status, search.StatusOK)
			}
			if len(out.Hits) != 1 || out.Hits[0].ChunkID != tc.wantChunk {
				t.Fatalf("хиты %v, ожидался только %s", chunkIDs(out.Hits), tc.wantChunk)
			}
			if !slices.Contains(index.lastScope.SpaceIDs, tc.wantSpace) {
				t.Errorf("в область ушёл контейнер %v, ожидался %s",
					index.lastScope.SpaceIDs, tc.wantSpace)
			}
			seen[tc.wantSpace] = index.lastScope
		})
	}
	if sameScope(seen[hrSpace], seen[platformSpace]) {
		t.Error("у пользователей с разными правами область доступа совпала")
	}
}

// TestSearchWithoutRightsReturnsNoResultsWithoutNetworkCall: у пользователя без
// прав выдача пуста, и не происходит НИ ОДНОГО сетевого вызова (таблично).
//
// «Нет прав» и «фильтр не задан» — разные вещи: второе в Elasticsearch означает
// «не ограничивать», то есть выдать все документы тенанта. Поэтому область без
// прав обязана закрывать запрос, а не оставлять его пустым.
func TestSearchWithoutRightsReturnsNoResultsWithoutNetworkCall(t *testing.T) {
	cases := []struct {
		name string
		acl  *fakeACL
	}{
		{
			name: "оракул не выдал ни контейнеров, ни принципалов",
			acl:  &fakeACL{scopes: map[string]domain.AccessScope{domain.LocalUserID: {}}},
		},
		{
			name: "у пользователя нет записей о правах",
			acl:  &fakeACL{scopes: map[string]domain.AccessScope{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := &fakeIndex{keyword: []domain.SearchCandidate{
				hit("sha256:aaa", testDocumentID, testVersionID, 1)}}
			embedder := &fakeEmbedder{dimension: 384}
			docs := &fakeDocs{active: allowedDocs()}
			svc, _ := newService(t, index, docs, tc.acl, embedder, domain.KB{KBID: testKBID.String()})

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
				t.Errorf("индекс запрошен без прав: keyword %d, vector %d",
					index.keywordCall, index.vectorCall)
			}
			if index.legacyCalls != 0 {
				t.Errorf("legacy-порт запрошен без прав: %d", index.legacyCalls)
			}
			if embedder.calls != 0 {
				t.Errorf("эмбеддер вызван без прав: %d раз", embedder.calls)
			}
			if tc.acl.calls != 1 {
				t.Errorf("оракул прав спрошен %d раз, ожидался 1 — права разрешаются один раз на запрос",
					tc.acl.calls)
			}
		})
	}
}

// TestSearchKeepsPersonalNotesTermForOwner: область, где есть только личный
// терм «user:<id>», — это права, а не пустота.
//
// §6: «Права личных заметок: acl = [user:<id>] и больше никто». Значит
// AllowsNothing() здесь не должен срабатывать: иначе владелец не нашёл бы
// собственную заметку, и «нет прав» означало бы «нет вообще ничего».
func TestSearchKeepsPersonalNotesTermForOwner(t *testing.T) {
	index := &fakeIndex{keyword: []domain.SearchCandidate{
		hit("sha256:aaa", testDocumentID, testVersionID, 1)}}
	acl := &fakeACL{scopes: map[string]domain.AccessScope{
		domain.LocalUserID: {Principals: []string{"user:" + domain.LocalUserID}},
	}}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, acl,
		&fakeEmbedder{dimension: 384}, domain.KB{KBID: testKBID.String()})

	if _, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest()); err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if index.keywordCall != 1 || index.vectorCall != 1 {
		t.Errorf("личная заметка не ищется: keyword %d, vector %d — вызовов",
			index.keywordCall, index.vectorCall)
	}
	want := []string{"user:" + domain.LocalUserID}
	if !sameTerms(index.lastScope.Principals, want) {
		t.Errorf("принципалы в области %v, ожидались %v", index.lastScope.Principals, want)
	}
}

// TestSearchFailsClosedWhenRightsCannotBeResolved: неизвестные права — отказ, а
// не поиск без прав (таблично).
//
// Закрытая точка здесь одна и она смертельная: «не знаю прав» превратилось бы в
// пустую область, пустая область — в «ничего не найдено», а пользователь увидел
// бы тишину вместо отказа. Ещё хуже обратный вариант — продолжить поиск без
// фильтра, то есть отдать все документы тенанта.
func TestSearchFailsClosedWhenRightsCannotBeResolved(t *testing.T) {
	cases := []struct {
		name     string
		acl      *fakeACL
		wantErr  error
		wantCode string
	}{
		{
			name:     "ошибка оракула прав",
			acl:      &fakeACL{err: errors.New("postgres недоступен")},
			wantErr:  errUnusedPort, // заменяется ниже на реальную ошибку оракула
			wantCode: domain.CodeServiceUnavailable,
		},
		{
			name:     "оракул не подключён",
			acl:      nil,
			wantCode: domain.CodeInternalError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := &fakeIndex{keyword: []domain.SearchCandidate{
				hit("sha256:aaa", testDocumentID, testVersionID, 1)}}
			embedder := &fakeEmbedder{dimension: 384}
			docs := &fakeDocs{active: allowedDocs()}
			var (
				svc       *search.Service
				aclErr    = errors.New("postgres недоступен")
				aclBroken *fakeACL
			)
			if tc.acl == nil {
				// Настоящий nil: сборка без оракула прав. Типизированный nil
				// здесь не годится — он даст непустой интерфейс и упадёт позже,
				// уже внутри метода.
				svc = search.New(fakeQueryer{}, &fakeKBRows{kb: domain.KB{KBID: testKBID.String()}},
					docs, index, nil, embedder,
					settings.New(&fakeSettings{tenantLevel: baselineKeys()}))
			} else {
				aclBroken = tc.acl
				if tc.wantErr != nil {
					aclBroken.err = aclErr
					tc.wantErr = aclErr
				}
				svc, _ = newService(t, index, docs, aclBroken, embedder,
					domain.KB{KBID: testKBID.String()})
			}

			_, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
			if err == nil {
				t.Fatal("поиск без прав завершился успехом, ожидался отказ")
			}
			appErr, ok := domain.AsAppError(err)
			if !ok || appErr.Code != tc.wantCode {
				t.Fatalf("ошибка = %v, ожидался код %s", err, tc.wantCode)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("ошибка = %v, ожидалась причина %v — отказ обязан сохранять её",
					err, tc.wantErr)
			}
			if index.keywordCall != 0 || index.vectorCall != 0 {
				t.Errorf("индекс запрошен при неизвестных правах: keyword %d, vector %d",
					index.keywordCall, index.vectorCall)
			}
			if index.legacyCalls != 0 {
				t.Errorf("legacy-порт запрошен при неизвестных правах: %d", index.legacyCalls)
			}
			if docs.calls != 0 {
				t.Errorf("документы прочитаны при неизвестных правах: %d — права обязаны пасть "+
					"до любого чтения, иначе отказ приходит слишком поздно", docs.calls)
			}
		})
	}
}

// TestSearchDropsCandidatesPostgresNoLongerAllows: защита в глубину §6 —
// «перед тем как отдать top-N в UI или в контекст LLM, повторно проверяем
// can_read(user, doc_ids) по Postgres».
//
// Индекс отстаёт по правам: пересчёт области занимает минуты, а отзыв доступа
// действует сразу. Поэтому документ, который индекс ещё отдаёт, но который
// Postgres уже не читает, обязан исчезнуть из выдачи ДО того, как её увидит
// пользователь или модель (таблично).
func TestSearchDropsCandidatesPostgresNoLongerAllows(t *testing.T) {
	cases := []struct {
		name      string
		active    []domain.ActiveVersion
		wantChunk []string
	}{
		{
			name: "право на документ отозвали после пересчёта области",
			active: []domain.ActiveVersion{
				{DocumentID: testDocumentID, ActiveVersionID: testVersionID},
			},
			wantChunk: []string{"sha256:aaa"},
		},
		{
			name:      "право отозвали на все найденные документы",
			active:    []domain.ActiveVersion{},
			wantChunk: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := &fakeIndex{
				keyword: []domain.SearchCandidate{
					hit("sha256:aaa", testDocumentID, testVersionID, 1),
					hit("sha256:bbb", revokedDocumentID, revokedVersionID, 1),
				},
				vector: []domain.SearchCandidate{
					hit("sha256:aaa", testDocumentID, testVersionID, 1),
					hit("sha256:bbb", revokedDocumentID, revokedVersionID, 1),
				},
			}
			svc, _ := newService(t, index, &fakeDocs{active: tc.active}, localACL(),
				&fakeEmbedder{dimension: 384}, domain.KB{KBID: testKBID.String()})

			out, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest())
			if err != nil {
				t.Fatalf("поиск вернул ошибку: %v", err)
			}
			if got := chunkIDs(out.Hits); !sameTerms(got, tc.wantChunk) {
				t.Fatalf("хиты %v, ожидались %v — нечитаемое по Postgres не должно "+
					"доходить до вызывающего", got, tc.wantChunk)
			}
			for _, item := range out.Hits {
				if item.DocumentID == revokedDocumentID {
					t.Fatalf("документ с отозванным правом %s дошёл до вызывающего", revokedDocumentID)
				}
			}
			wantStatus := search.StatusOK
			if len(tc.wantChunk) == 0 {
				wantStatus = search.StatusNoResults
			}
			if out.Status != wantStatus {
				t.Errorf("status = %q, ожидался %q", out.Status, wantStatus)
			}
		})
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
	svc, _ := newService(t, index, docs, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
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
		&fakeDocs{active: allowedDocs()}, index, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(),
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
		domain.KB{KBID: testKBID.String()})

	if _, err := svc.Search(testCtx(t), domain.LocalPrincipal(), testRequest()); !errors.Is(err, indexErr) {
		t.Fatalf("ошибка = %v, ожидалась %v", err, indexErr)
	}
}

// TestSearchStopsOnCancelledContext: отменённый запрос не доходит до индекса.
func TestSearchStopsOnCancelledContext(t *testing.T) {
	index := &fakeIndex{}
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
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
	svc, _ := newService(t, index, &fakeDocs{active: allowedDocs()}, localACL(), &fakeEmbedder{dimension: 384},
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

// sameScope сравнивает области доступа — это то, чем ветви поиска ограничены
// после §6. Порядок термов в области не важен для запроса, но сравнивается
// строго: оракул и AccessScope обязаны отдавать отсортированный список, иначе
// «фильтр не изменился» в тестах начнёт мигать без причины.
func sameScope(a, b domain.AccessScope) bool {
	if a.TenantID != b.TenantID || a.KBID != b.KBID {
		return false
	}
	return sameTerms(a.SpaceIDs, b.SpaceIDs) && sameTerms(a.Principals, b.Principals)
}

func sameTerms(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
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
