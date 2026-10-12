package ports

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
)

// Подделки внизу существуют ради одной проверки формы: пока интерфейс жив, он
// собирается ими, и любая правка сигнатуры роняет тест, а не сценарий в
// проде. Поведения у них ровно столько, сколько нужно тестам ниже.

type fakeConnector struct {
	// pages отдаются по порядку, курсор — номер страницы.
	pages      [][]domain.RawItem
	principals []domain.Principal
	pulls      []string
}

func (f *fakeConnector) Name() string { return "fake" }

// Pull отдаёт страницу pages[cursor]; пустой курсор означает начало полного
// обхода — это и закрепляет §4.
func (f *fakeConnector) Pull(_ context.Context, cursor string) ([]domain.RawItem, string, error) {
	f.pulls = append(f.pulls, cursor)
	from := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, "", domain.InvalidMetadata("курсор не число: " + cursor)
		}
		from = parsed
	}
	if from >= len(f.pages) {
		return nil, "", nil
	}
	return f.pages[from], strconv.Itoa(from + 1), nil
}

func (f *fakeConnector) Permissions(_ context.Context, _ domain.RawItem) ([]domain.Principal, error) {
	return slices.Clone(f.principals), nil
}

type fakeBus struct {
	published []domain.Event
	closed    bool
}

func (f *fakeBus) Publish(_ context.Context, event domain.Event) error {
	f.published = append(f.published, event)
	return nil
}

func (f *fakeBus) PublishBatch(ctx context.Context, events []domain.Event) error {
	for _, event := range events {
		if err := f.Publish(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeBus) Close() error {
	f.closed = true
	return nil
}

type fakeStore struct {
	events  []domain.Event
	queries []domain.EventQuery
	closed  bool
}

// Read отдаёт lake по порядку добавления и фильтрует по запросу: порядок
// чтения часть контракта, от него зависит пересборка индекса.
func (f *fakeStore) Read(_ context.Context, q domain.EventQuery) ([]domain.Event, error) {
	f.queries = append(f.queries, q)
	var out []domain.Event
	for _, event := range f.events {
		if event.TenantID != q.TenantID {
			continue
		}
		if q.Stage != "" && event.Stage != q.Stage {
			continue
		}
		if q.DocID != "" && event.DocID != q.DocID {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

func (f *fakeStore) Ping(context.Context) error { return nil }

func (f *fakeStore) Close() error {
	f.closed = true
	return nil
}

type fakeRanker struct {
	queries []string
}

func (f *fakeRanker) Name() string { return "fake-ranker" }

func (f *fakeRanker) Rank(
	_ context.Context, query string, candidates []domain.SearchCandidate, limit int,
) ([]domain.SearchCandidate, error) {
	f.queries = append(f.queries, query)
	if limit > 0 && len(candidates) > limit {
		return candidates[:limit], nil
	}
	return candidates, nil
}

// fakeExpander — пустая реализация §5/§8: возвращает исходный запрос.
type fakeExpander struct{}

func (fakeExpander) Name() string { return "noop" }

func (fakeExpander) Expand(_ context.Context, query string) ([]string, error) {
	return []string{query}, nil
}

type fakeProfiledEmbedder struct {
	profile domain.EmbeddingProfile
}

func (f *fakeProfiledEmbedder) Profile() domain.EmbeddingProfile { return f.profile }

type fakeSecuredIndex struct {
	scopes []domain.AccessScope
}

func (f *fakeSecuredIndex) KeywordSearchScoped(
	_ context.Context, _ string, scope domain.AccessScope, _ domain.IndexingConfig, _ int,
) ([]domain.SearchCandidate, error) {
	f.scopes = append(f.scopes, scope)
	if scope.AllowsNothing() {
		return nil, nil
	}
	return []domain.SearchCandidate{{ChunkID: "chunk-1", TenantID: scope.TenantID}}, nil
}

func (f *fakeSecuredIndex) VectorSearchScoped(
	_ context.Context, _ []float32, _ domain.IndexingConfig, scope domain.AccessScope, _ int,
) ([]domain.SearchCandidate, error) {
	f.scopes = append(f.scopes, scope)
	if scope.AllowsNothing() {
		return nil, nil
	}
	return []domain.SearchCandidate{{ChunkID: "chunk-2", TenantID: scope.TenantID}}, nil
}

// Форма швов зафиксирована на этапе компиляции: пока подделка удовлетворяет
// порту, смена сигнатуры видна сразу, а не в момент, когда кто-то позовёт
// адаптер.
var (
	_ Connector          = (*fakeConnector)(nil)
	_ EventBus           = (*fakeBus)(nil)
	_ EventStore         = (*fakeStore)(nil)
	_ Ranker             = (*fakeRanker)(nil)
	_ QueryExpander      = fakeExpander{}
	_ ProfiledEmbedder   = (*fakeProfiledEmbedder)(nil)
	_ SecuredSearchIndex = (*fakeSecuredIndex)(nil)
)

// transportVendors — словарь имён, которых в подписях швов быть не должно.
var transportVendors = []string{
	"kafka", "clickhouse", "segmentio", "ch-go", "chclient", "sarama", "pgx", "elasticsearch", "hugot",
}

// vendorViolations returns the vendor names found in the signatures of iface.
func vendorViolations(iface any) []string {
	ptr := reflect.TypeOf(iface)
	if ptr == nil || ptr.Kind() != reflect.Pointer || ptr.Elem().Kind() != reflect.Interface {
		return []string{"не указатель на интерфейс"}
	}
	var out []string
	it := ptr.Elem()
	for i := range it.NumMethod() {
		m := it.Method(i)
		types := []reflect.Type{m.Type}
		for arg := range m.Type.NumIn() {
			types = append(types, m.Type.In(arg))
		}
		for res := range m.Type.NumOut() {
			types = append(types, m.Type.Out(res))
		}
		for _, typ := range types {
			signature := strings.ToLower(typ.String())
			for _, vendor := range transportVendors {
				if strings.Contains(signature, vendor) {
					out = append(out, ptr.Elem().Name()+"."+m.Name+" → "+vendor)
				}
			}
		}
	}
	return out
}

// TestEventSeamsDoNotNameVendors — правило §8: Kafka и ClickHouse можно
// заменить, не трогая остальной код. Названный в подписи вендор превращает
// такую замену в правку всех сценариев разом.
func TestEventSeamsDoNotNameVendors(t *testing.T) {
	seams := map[string]any{
		"Connector":          (*Connector)(nil),
		"EventBus":           (*EventBus)(nil),
		"EventStore":         (*EventStore)(nil),
		"Ranker":             (*Ranker)(nil),
		"QueryExpander":      (*QueryExpander)(nil),
		"ProfiledEmbedder":   (*ProfiledEmbedder)(nil),
		"SecuredSearchIndex": (*SecuredSearchIndex)(nil),
	}
	for name, seam := range seams {
		t.Run(name, func(t *testing.T) {
			if got := vendorViolations(seam); len(got) != 0 {
				t.Errorf("подпись шва называет транспорт: %v", got)
			}
		})
	}
}

// kafkaMessage и badBus существуют только для проверки самого запрета: пока
// нарушение ловится, тест выше что-то охраняет; если перестанет — оба теста
// станут зелёными на любом интерфейсе.
type kafkaMessage struct{ topic string }

type badBus interface {
	Publish(ctx context.Context, message kafkaMessage) error
}

func TestVendorGuardCatchesAViolation(t *testing.T) {
	if got := vendorViolations((*badBus)(nil)); len(got) == 0 {
		t.Error("проверка запрета не заметила kafkaMessage в подписи")
	}
}

// TestEventBusAndEventStoreAreSeparateSeams: §8 держит их разными швами, и
// фаза 2 меняет Kafka и ClickHouse за ними по отдельности. Слитый интерфейс
// означал бы, что читатель lake зависит от писателя.
func TestEventBusAndEventStoreAreSeparateSeams(t *testing.T) {
	// Close есть у обоих и ничего не говорит о разделении: это жизненный цикл
	// соединения, а не сторона записи или чтения.
	for _, busMethod := range []string{"Publish", "PublishBatch"} {
		if _, ok := reflect.TypeOf((*EventStore)(nil)).Elem().MethodByName(busMethod); ok {
			t.Errorf("EventStore не должен иметь метод писателя %s", busMethod)
		}
	}
	for _, storeMethod := range []string{"Read", "Ping"} {
		if _, ok := reflect.TypeOf((*EventBus)(nil)).Elem().MethodByName(storeMethod); ok {
			t.Errorf("EventBus не должен иметь метод читателя %s", storeMethod)
		}
	}
	if _, ok := reflect.TypeOf((*EventStore)(nil)).Elem().MethodByName("Read"); !ok {
		t.Error("EventStore без Read: пересборка из lake не сможет прочитать события")
	}
}

// TestConnectorWalksFromEmptyCursor: пустой курсор означает полный обход,
// поэтому конвейер начинает без курсора и идёт, пока страницы не кончатся.
func TestConnectorWalksFromEmptyCursor(t *testing.T) {
	connector := &fakeConnector{pages: [][]domain.RawItem{
		{{ExternalID: "1", Source: "note"}},
		{{ExternalID: "2", Source: "note"}},
	}}

	cursor := ""
	var seen []string
	for range 4 { // страницы две; лишние итерации не должны ничего ломать
		items, next, err := connector.Pull(context.Background(), cursor)
		if err != nil {
			t.Fatalf("Pull() вернул ошибку: %v", err)
		}
		for _, item := range items {
			seen = append(seen, item.ExternalID)
		}
		if len(items) == 0 {
			break
		}
		cursor = next
	}

	if len(seen) != 2 || seen[0] != "1" || seen[1] != "2" {
		t.Errorf("обход вернул %v, ожидались [1 2]", seen)
	}
	if connector.pulls[0] != "" {
		t.Errorf("первый Pull пришёл с курсором %q, а полный обход начинается с пустого", connector.pulls[0])
	}
}

func TestConnectorPermissions(t *testing.T) {
	want := []domain.Principal{{UserID: "anna"}, {UserID: "bob"}}
	connector := &fakeConnector{principals: want}

	got, err := connector.Permissions(context.Background(), domain.RawItem{ExternalID: "1"})
	if err != nil {
		t.Fatalf("Permissions() вернул ошибку: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Permissions() вернул %d принципалов, ожидалось 2", len(got))
	}
	got[0].UserID = "мусор"
	if connector.principals[0].UserID != "anna" {
		t.Error("Permissions() отдал общий срез: правка результата меняет состояние коннектора")
	}
}

// TestEventStoreReadsLakeInOrder: порядок чтения lake определяет порядок
// пересборки индекса, поэтому хранилище отдаёт добавленное как есть.
func TestEventStoreReadsLakeInOrder(t *testing.T) {
	tenantID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	collect := mustLakeEvent(t, tenantID, domain.CollectStage, "doc-1", "hash-1")
	enrich := mustLakeEvent(t, tenantID, domain.EnrichStage, "doc-1", "hash-1")
	store := &fakeStore{events: []domain.Event{collect, enrich}}

	all, err := store.Read(context.Background(), domain.EventQuery{TenantID: tenantID})
	if err != nil {
		t.Fatalf("Read() вернул ошибку: %v", err)
	}
	if len(all) != 2 || all[0].EventID != collect.EventID || all[1].EventID != enrich.EventID {
		t.Fatalf("Read() вернул %v, ожидались события в порядке добавления", all)
	}

	onlyEnrich, err := store.Read(context.Background(), domain.EventQuery{
		TenantID: tenantID,
		Stage:    domain.EnrichStage,
	})
	if err != nil {
		t.Fatalf("Read() с фильтром вернул ошибку: %v", err)
	}
	if len(onlyEnrich) != 1 || onlyEnrich[0].Stage != domain.EnrichStage {
		t.Errorf("фильтр по стадии вернул %v, ожидалось только событие enrich", onlyEnrich)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() вернул ошибку: %v", err)
	}
	if !store.closed {
		t.Error("Close() не закрыл lake")
	}
}

func TestEventBusPublishesEveryEvent(t *testing.T) {
	tenantID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	bus := &fakeBus{}
	ctx := context.Background()

	if err := bus.Publish(ctx, mustLakeEvent(t, tenantID, domain.CollectStage, "doc-1", "hash-1")); err != nil {
		t.Fatalf("Publish() вернул ошибку: %v", err)
	}
	if err := bus.PublishBatch(ctx, []domain.Event{
		mustLakeEvent(t, tenantID, domain.NormalizeStage, "doc-1", "hash-1"),
		mustLakeEvent(t, tenantID, domain.IndexStage, "doc-1", "hash-1"),
	}); err != nil {
		t.Fatalf("PublishBatch() вернул ошибку: %v", err)
	}
	if len(bus.published) != 3 {
		t.Fatalf("в шину ушло %d событий, ожидалось 3", len(bus.published))
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("Close() вернул ошибку: %v", err)
	}
	if !bus.closed {
		t.Error("Close() не закрыл шину: сценарий ждёт, что адаптер отпустит соединение")
	}
}

func TestRankerLimitsToTopN(t *testing.T) {
	ranker := &fakeRanker{}
	candidates := []domain.SearchCandidate{{ChunkID: "a"}, {ChunkID: "b"}, {ChunkID: "c"}}

	got, err := ranker.Rank(context.Background(), "вопрос", candidates, 2)
	if err != nil {
		t.Fatalf("Rank() вернул ошибку: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Rank() вернул %d кандидатов, ожидалось 2", len(got))
	}
	if len(ranker.queries) != 1 || ranker.queries[0] != "вопрос" {
		t.Errorf("ранкер не получил исходный вопрос: %v", ranker.queries)
	}
}

func mustLakeEvent(t *testing.T, tenantID uuid.UUID, stage domain.Stage, docID, hash string) domain.Event {
	t.Helper()
	event, err := domain.NewEvent(domain.NewEventParams{
		TenantID:    tenantID,
		Stage:       stage,
		DocID:       docID,
		ContentHash: hash,
	})
	if err != nil {
		t.Fatalf("NewEvent() вернул ошибку: %v", err)
	}
	return event
}

// TestAccessScopeForExpandsPrincipal: §5 требует развернуть пользователя в
// список принципалов, §6 — хранить их как user:/role:/group:.
func TestAccessScopeForExpandsPrincipal(t *testing.T) {
	principal := domain.Principal{
		TenantID: uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		UserID:   "anna",
		Roles:    []string{"reader", "admin"},
		Groups:   []string{"team-a", "team-a"},
	}
	spaces := []string{"space-platform", "space-hr"}

	scope := domain.AccessScopeFor(principal, "kb-1", spaces)

	want := []string{"group:team-a", "role:admin", "role:reader", "user:anna"}
	if !slices.Equal(scope.Principals, want) {
		t.Errorf("Principals = %v, ожидались %v", scope.Principals, want)
	}
	if scope.TenantID != principal.TenantID.String() {
		t.Errorf("TenantID = %q, ожидался тенант принципала", scope.TenantID)
	}
	if scope.KBID != "kb-1" {
		t.Errorf("KBID = %q, ожидался kb-1", scope.KBID)
	}
	if !slices.Equal(scope.SpaceIDs, spaces) {
		t.Errorf("SpaceIDs = %v, ожидались %v", scope.SpaceIDs, spaces)
	}

	// Список space копируется: сценарий, меняющий доступные контейнеры,
	// не должен переписывать то, что выдал источник прав.
	scope.SpaceIDs[0] = "мусор"
	if spaces[0] != "space-platform" {
		t.Error("AccessScopeFor отдал общий срез space")
	}
}

// TestAccessScopeAllowsNothing — правило безопасности §6: пустой список прав
// означает «не показываем ничего», а не «фильтр не задан». Второе прочтение
// отдало бы тенанту весь корпус.
func TestAccessScopeAllowsNothing(t *testing.T) {
	cases := []struct {
		name string
		in   domain.AccessScope
		want bool
	}{
		{"пустой scope", domain.AccessScope{TenantID: "t"}, true},
		{"есть space", domain.AccessScope{SpaceIDs: []string{"space-a"}}, false},
		{"есть принципал", domain.AccessScope{Principals: []string{"user:anna"}}, false},
		{"space и принципал", domain.AccessScope{SpaceIDs: []string{"s"}, Principals: []string{"u"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.AllowsNothing(); got != tc.want {
				t.Errorf("AllowsNothing() = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

func TestSecuredSearchIndexCarriesScopeIntoTheQuery(t *testing.T) {
	index := &fakeSecuredIndex{}
	scope := domain.AccessScope{TenantID: "t1", SpaceIDs: []string{"space-a"}}
	cfg := domain.IndexingConfig{}

	keyword, err := index.KeywordSearchScoped(context.Background(), "права", scope, cfg, 10)
	if err != nil {
		t.Fatalf("KeywordSearchScoped() вернул ошибку: %v", err)
	}
	vector, err := index.VectorSearchScoped(context.Background(), []float32{1, 2}, cfg, scope, 10)
	if err != nil {
		t.Fatalf("VectorSearchScoped() вернул ошибку: %v", err)
	}
	if len(keyword) != 1 || len(vector) != 1 {
		t.Fatalf("ветки вернули %d и %d кандидатов, ожидался один каждый", len(keyword), len(vector))
	}
	if len(index.scopes) != 2 {
		t.Errorf("в индекс ушло %d фильтров, ожидалось 2 — права терялись между ветками", len(index.scopes))
	}

	empty, err := index.KeywordSearchScoped(context.Background(), "права", domain.AccessScope{TenantID: "t1"}, cfg, 10)
	if err != nil {
		t.Fatalf("пустой scope вернул ошибку: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("пустой scope вернул %d кандидатов, ожидалось 0", len(empty))
	}
}

// TestEmbeddingProfileMatchesIndexingProfile: §4 требует хранить в документе
// версию модели, а документ хранит profile_id. Значит profile_id профиля и
// profile_id индекса обязаны совпадать, иначе поиск пойдёт по чужому индексу.
func TestEmbeddingProfileMatchesIndexingProfile(t *testing.T) {
	cfg := domain.IndexingConfig{
		EmbeddingProviderID: "local-hugot",
		EmbeddingModel:      "intfloat/multilingual-e5-small",
		EmbeddingRevision:   "pilot-1",
		EmbeddingDimension:  384,
		ChunkMaxTokens:      600,
		ChunkOverlapTokens:  100,
		TokenizerID:         "hugot/multilingual-e5-small",
		NormalizeVectors:    true,
		Distance:            "cosine",
	}

	profile := domain.NewEmbeddingProfileFrom(cfg)

	if profile.ProfileID() != cfg.ProfileID() {
		t.Errorf("profile_id профиля %s != профиля индекса %s", profile.ProfileID(), cfg.ProfileID())
	}
	if !profile.Valid() {
		t.Errorf("профиль из корректной конфигурации невалиден: %+v", profile)
	}
	if profile.Dimension != 384 || profile.Revision != "pilot-1" {
		t.Errorf("профиль потерял модель: %+v", profile)
	}

	broken := profile
	broken.Dimension = 0
	if broken.Valid() {
		t.Error("профиль без размерности прошёл проверку")
	}
}

// TestProfiledEmbedderIsOptionalForExistingEmbedders: узкий порт не ломает
// Embedder — реализация, у которой нет Profile(), продолжает быть Embedder.
func TestProfiledEmbedderIsOptionalForExistingEmbedders(t *testing.T) {
	plain := &fakeLegacyEmbedder{}
	var embedder Embedder = plain
	if embedder.Dimensions() != 384 {
		t.Errorf("Dimensions() = %d, ожидалось 384", embedder.Dimensions())
	}
	if _, ok := any(plain).(ProfiledEmbedder); ok {
		t.Error("подделка без Profile() не должна удовлетворять ProfiledEmbedder")
	}

	profiled := &fakeProfiledEmbedder{profile: domain.EmbeddingProfile{
		ProviderID: "local-hugot",
		Model:      "intfloat/multilingual-e5-small",
		Revision:   "pilot-1",
		Dimension:  384,
		Distance:   "cosine",
	}}
	if got := profiled.Profile().Model; got != "intfloat/multilingual-e5-small" {
		t.Errorf("Profile().Model = %q", got)
	}
}

// fakeLegacyEmbedder — реализация старого Embedder без Profile(): доказать,
// что новый узкий порт ничего не потребовал от существующих.
type fakeLegacyEmbedder struct{}

func (f *fakeLegacyEmbedder) Embed(
	context.Context, []string, domain.IndexingConfig,
) ([][]float32, error) {
	return nil, nil
}

func (f *fakeLegacyEmbedder) EmbedQuery(
	context.Context, string, domain.IndexingConfig,
) ([]float32, error) {
	return nil, nil
}

func (f *fakeLegacyEmbedder) Dimensions() int { return 384 }

func (f *fakeLegacyEmbedder) Ping(context.Context) error { return nil }

func (f *fakeLegacyEmbedder) Close() error { return nil }

// TestNoOpExpanderKeepsQuery: §5 и §8 — в MVP QueryExpander ничего не
// добавляет, и пустая реализация обязана вернуть исходный запрос, а не
// пустой список (иначе поиск потеряет вопрос).
func TestNoOpExpanderKeepsQuery(t *testing.T) {
	expander := fakeExpander{}
	got, err := expander.Expand(context.Background(), "как устроен lake")
	if err != nil {
		t.Fatalf("Expand() вернул ошибку: %v", err)
	}
	if len(got) != 1 || got[0] != "как устроен lake" {
		t.Errorf("Expand() = %v, ожидался исходный запрос без изменений", got)
	}
	if expander.Name() != "noop" {
		t.Errorf("Name() = %q, ожидалось noop", expander.Name())
	}
}
