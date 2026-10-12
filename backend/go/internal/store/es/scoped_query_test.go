package elasticsearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/elastic/go-elasticsearch/v9"
	domain "github.com/kbsearch/kb/internal/envelope"
)

// Тесты этого файла не требуют кластера: они проверяют ТЕЛО запроса, которое
// уехало бы в Elasticsearch, и то, что адаптер вообще не выходит в сеть, когда
// прав нет. Подставной кластер ниже отвечает как живой, но запоминает тела.

func scopedBody(t *testing.T, body any) string {
	t.Helper()
	blob, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(blob)
}

// scopedFilter — клозы фильтра из тела запроса (query.bool.filter).
func scopedFilter(t *testing.T, body map[string]any) string {
	t.Helper()
	return scopedBody(t, scopedClauses(t, body))
}

func scopedClauses(t *testing.T, body map[string]any) []any {
	t.Helper()
	query, ok := body["query"].(map[string]any)
	if !ok {
		t.Fatalf("в теле нет query: %v", body)
	}
	boolean, ok := query["bool"].(map[string]any)
	if !ok {
		t.Fatalf("в теле нет query.bool: %v", query)
	}
	clauses, ok := boolean["filter"].([]any)
	if !ok {
		t.Fatalf("в теле нет query.bool.filter: %v", boolean)
	}
	return clauses
}

// scopedKNNFilter — клозы фильтра ВНУТРИ knn.filter (§5 шаг 2).
func scopedKNNFilter(t *testing.T, body map[string]any) string {
	t.Helper()
	query := body["query"].(map[string]any)
	boolean := query["bool"].(map[string]any)
	must, ok := boolean["must"].([]any)
	if !ok || len(must) == 0 {
		t.Fatalf("в теле нет query.bool.must: %v", boolean)
	}
	knn, ok := must[0].(map[string]any)["knn"].(map[string]any)
	if !ok {
		t.Fatalf("в must нет knn: %v", must[0])
	}
	inner, ok := knn["filter"].(map[string]any)
	if !ok {
		t.Fatalf("в knn нет filter — фильтр прав остался снаружи kNN: %v", knn)
	}
	boolean, ok = inner["bool"].(map[string]any)
	if !ok {
		t.Fatalf("knn.filter не bool: %v", inner)
	}
	clauses, ok := boolean["filter"].([]any)
	if !ok {
		t.Fatalf("в knn.filter нет клоз: %v", inner)
	}
	return scopedBody(t, clauses)
}

// TestФильтрПравВТелеКлючевогоЗапроса — права обязаны быть частью ТЕЛА запроса.
// Это и есть требование §2 (ошибка №2) и §6: post-filter недопустим, потому что
// запрещённый чанк успевает попасть в память, в контекст LLM и в логи.
func TestФильтрПравВТелеКлючевогоЗапроса(t *testing.T) {
	cfg := testConfig(dimension)
	body := scopedKeywordQuery("выгрузка данных", scopedScope(), cfg, 25)
	text := scopedBody(t, body)

	for _, want := range []string{
		`"tenant_id":"tenant-1"`,
		`"kb_id":"kb-1"`,
		`"embedding_profile_id":"` + cfg.ProfileID() + `"`,
		`"space_id":["space-hr","space-platform"]`,
		`"acl":["group:team-backend","role:admin","user:anna"]`,
		`"minimum_should_match":1`,
		`"analyzer":"russian"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в теле текстового запроса нет %s: %s", want, text)
		}
	}
	if body["size"] != 25 {
		t.Errorf("size = %v, ожидалось 25", body["size"])
	}
}

// TestФильтрПравВТелеВекторногоЗапроса — вторая ветвь §5 несёт тот же фильтр:
// иначе RRF сравнивал бы разные выборки, а ещё и разные права.
func TestФильтрПравВТелеВекторногоЗапроса(t *testing.T) {
	cfg := testConfig(dimension)
	body := scopedVectorQuery(seedVector(1), cfg, scopedScope(), 25)
	text := scopedBody(t, body)

	for _, want := range []string{
		`"tenant_id":"tenant-1"`,
		`"kb_id":"kb-1"`,
		`"space_id":["space-hr","space-platform"]`,
		`"acl":["group:team-backend","role:admin","user:anna"]`,
		`"minimum_should_match":1`,
		`"field":"embedding"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в теле векторного запроса нет %s: %s", want, text)
		}
	}
	if !strings.Contains(text, `"k":25`) {
		t.Errorf("k не вырос при limit=25: %s", text)
	}
}

// TestФильтрПравВнутриKNN: известная ловушка Elasticsearch — фильтр, стоящий
// только на верхнем уровне запроса, не сужает отбор кандидатов kNN. HNSW сначала
// находит k ближайших чанков корпуса, и только потом верхний фильтр выбросит
// чужие, поэтому пользователю приедет пусто там, где могло быть N результатов.
// §5 шаг 2 требует класть фильтр внутрь knn.filter — тест проверяет именно
// вложенность, а не просто наличие строки в теле.
func TestФильтрПравВнутриKNN(t *testing.T) {
	cfg := testConfig(dimension)
	scope := scopedScope()
	body := scopedVectorQuery(seedVector(1), cfg, scope, 25)

	inner := scopedKNNFilter(t, body)
	for _, want := range []string{
		`"tenant_id":"tenant-1"`,
		`"space_id":["space-hr","space-platform"]`,
		`"acl":["group:team-backend","role:admin","user:anna"]`,
		`"minimum_should_match":1`,
	} {
		if !strings.Contains(inner, want) {
			t.Errorf("в knn.filter нет %s: %s", want, inner)
		}
	}
	// knn.filter и верхний фильтр обязаны быть одной и той же клозой: иначе
	// кандидаты отбираются по одним правам, а отдаются по другим.
	if outer := scopedFilter(t, body); outer != inner {
		t.Errorf("фильтры разошлись:\n knn.filter: %s\n query.filter: %s", inner, outer)
	}
}

// TestОбеВетвиНесутОдинФильтр: §5 шаг 2 — «два запроса с одним и тем же
// фильтром». Сравниваются не строки целиком, а только части с правами.
func TestОбеВетвиНесутОдинФильтр(t *testing.T) {
	cfg := testConfig(dimension)
	scope := scopedScope()
	keyword := scopedFilter(t, scopedKeywordQuery("текст", scope, cfg, 10))
	vector := scopedKNNFilter(t, scopedVectorQuery(seedVector(1), cfg, scope, 10))
	if keyword != vector {
		t.Errorf("текстовая и векторная ветви фильтруют по-разному:\n %s\n %s", keyword, vector)
	}
}

// TestПустаяОбластьДаётЗакрытыйЗапрос — главная защита от тотальной утечки:
// запрос без прав обязан физически не совпасть ни с одним документом, а не
// «не содержать фильтра». match_none делает это структурно.
func TestПустаяОбластьДаётЗакрытыйЗапрос(t *testing.T) {
	cfg := testConfig(dimension)
	empty := domain.AccessScope{TenantID: "tenant-1", KBID: "kb-1"}

	keyword := scopedBody(t, scopedKeywordQuery("текст", empty, cfg, 10))
	vector := scopedBody(t, scopedVectorQuery(seedVector(1), cfg, empty, 10))
	for name, text := range map[string]string{"текстовый": keyword, "векторный": vector} {
		if !strings.Contains(text, `"match_none":{}`) {
			t.Errorf("%s запрос без прав не закрыт match_none: %s", name, text)
		}
		for _, forbidden := range []string{`"minimum_should_match"`, `"space_id"`, `"acl"`} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s запрос без прав упоминает %s: %s", name, forbidden, text)
			}
		}
	}
	// Закрыт должен быть и kNN: пустой knn.filter без match_none наполнил бы
	// выдачу соседями корпуса, которые верхний фильтр потом выбросил бы.
	if !strings.Contains(vector, `"match_none":{}`) {
		t.Errorf("knn.filter без прав не закрыт: %s", vector)
	}
}

// TestОбластьБезТенантаНеОткрываетДоступ: права есть, tenant_id потерян. Такой
// запрос обязан совпасть с нулём документов — term по пустому тенанту не
// матчит ничего, и подставлять term нельзя «по умолчанию» (например, именем
// локального тенанта из другой сборки).
func TestОбластьБезТенантаНеОткрываетДоступ(t *testing.T) {
	cfg := testConfig(dimension)
	orphan := domain.AccessScope{SpaceIDs: []string{"space-hr"}, Principals: []string{"user:anna"}}
	text := scopedFilter(t, scopedKeywordQuery("текст", orphan, cfg, 10))
	if !strings.Contains(text, `"tenant_id":""`) {
		t.Errorf("потерянный tenant_id не попал в term (значит, он подставлен): %s", text)
	}
}

// TestТенантОстаётсяВФильтреВсегда — регрессия на «фильтр по правам без
// tenant_id». Область тенанта A обязана остаться внутри своего tenant_id, даже
// если права у неё полные.
func TestТенантОстаётсяВФильтреВсегда(t *testing.T) {
	cfg := testConfig(dimension)
	tenantA := domain.AccessScope{TenantID: "tenant-a", KBID: "kb-1", Principals: []string{"user:anna"}}
	tenantB := domain.AccessScope{TenantID: "tenant-b", KBID: "kb-1", Principals: []string{"user:anna"}}

	a := scopedFilter(t, scopedKeywordQuery("текст", tenantA, cfg, 10))
	b := scopedFilter(t, scopedKeywordQuery("текст", tenantB, cfg, 10))

	if !strings.Contains(a, `"tenant_id":"tenant-a"`) || strings.Contains(a, `"tenant_id":"tenant-b"`) {
		t.Errorf("фильтр тенанта A искажён: %s", a)
	}
	if !strings.Contains(b, `"tenant_id":"tenant-b"`) || strings.Contains(b, `"tenant_id":"tenant-a"`) {
		t.Errorf("фильтр тенанта B искажён: %s", b)
	}
	if clauses := scopedClauses(t, scopedKeywordQuery("текст", tenantA, cfg, 10)); len(clauses) != 4 {
		t.Errorf("в фильтре %d клоз, ожидалось 4 (тенант, база, профиль, права)", len(clauses))
	}
}

// TestВТелеНетСпискаДокументов — запрет материализации прав (§6): id разрешённых
// документов в теле запроса означает, что сервер заранее выгрузил весь активный
// корпус в память.
func TestВТелеНетСпискаДокументов(t *testing.T) {
	cfg := testConfig(dimension)
	scope := scopedScope()
	for name, body := range map[string]map[string]any{
		"текстовый": scopedKeywordQuery("текст", scope, cfg, 10),
		"векторный": scopedVectorQuery(seedVector(1), cfg, scope, 10),
		"закрытый":  scopedKeywordQuery("текст", domain.AccessScope{TenantID: "t"}, cfg, 10),
	} {
		if text := scopedBody(t, body); strings.Contains(text, "document_id") {
			t.Errorf("%s запрос фильтрует по document_id: %s", name, text)
		}
	}
}

// TestОбластьДоступаНеМожетНестиСписокДокументов — структурная защита от
// возврата к старой форме: если в AccessScope появится поле со списком
// документов, отказоустойчивость вернётся к нулю, а тест об этом скажет.
func TestОбластьДоступаНеМожетНестиСписокДокументов(t *testing.T) {
	scopeType := reflect.TypeOf(domain.AccessScope{})
	for i := range scopeType.NumField() {
		name := scopeType.Field(i).Name
		if strings.Contains(strings.ToLower(name), "document") ||
			strings.Contains(strings.ToLower(name), "chunk") {
			t.Errorf("AccessScope снова содержит поле %s — это путь к post-filter прав", name)
		}
	}
}

// scopedWire — подставной Elasticsearch, который ЗАПОМИНАЕТ тела запросов.
// Проверять «фильтр применён к ответу» нельзя: это и есть post-filter. Поэтому
// тест смотрит на то, что ушло в сеть.
type scopedWire struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
	paths  []string
	calls  atomic.Int64
	// reply — тело ответа на _search.
	reply string
}

func newScopedWire(t *testing.T) *scopedWire {
	t.Helper()
	wire := &scopedWire{reply: `{"hits":{"hits":[]}}`}
	wire.server = httptest.NewServer(http.HandlerFunc(wire.handle))
	t.Cleanup(wire.server.Close)
	return wire
}

func (w *scopedWire) handle(rw http.ResponseWriter, r *http.Request) {
	w.calls.Add(1)
	payload, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	w.bodies = append(w.bodies, string(payload))
	w.paths = append(w.paths, r.URL.Path)
	w.mu.Unlock()
	// Заголовок обязателен: клиент go-elasticsearch отказывается работать без него.
	rw.Header().Set("X-Elastic-Product", "Elasticsearch")
	rw.Header().Set("Content-Type", "application/json")
	_, _ = rw.Write([]byte(w.reply))
}

// Calls возвращает число обращений к подставному кластеру.
func (w *scopedWire) Calls() int64 { return w.calls.Load() }

// Body — тело последнего обращения.
func (w *scopedWire) Body(t *testing.T) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.bodies) == 0 {
		t.Fatal("к подставному кластеру не обращались")
	}
	return w.bodies[len(w.bodies)-1]
}

func (w *scopedWire) adapter(t *testing.T) *Index {
	t.Helper()
	client, err := elasticsearch.New(elasticsearch.WithAddresses(w.server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	index, err := NewWithClient(client)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	t.Cleanup(func() { closeAdapter(index) })
	return index
}

// TestПраваУходятВКластерТеломЗапроса — сквозная проверка адаптера: и текстовая,
// и векторная ветвь обязаны отдать Elasticsearch тело с фильтром прав, а именем
// индекса — индекс профиля (§5: вектора разных моделей в разных индексах).
func TestПраваУходятВКластерТеломЗапроса(t *testing.T) {
	cfg := testConfig(dimension)
	wire := newScopedWire(t)
	index := wire.adapter(t)
	ctx := context.Background()

	if _, err := index.KeywordSearchScoped(ctx, "текст", scopedScope(), cfg, 10); err != nil {
		t.Fatalf("KeywordSearchScoped: %v", err)
	}
	keyword := wire.Body(t)
	if !strings.Contains(keyword, `"acl":["group:team-backend","role:admin","user:anna"]`) {
		t.Errorf("в ушедшем текстовом запросе нет прав: %s", keyword)
	}
	if path := wire.paths[len(wire.paths)-1]; !strings.Contains(path, cfg.IndexName()) {
		t.Errorf("запрошен индекс %q, ожидался %q", path, cfg.IndexName())
	}

	if _, err := index.VectorSearchScoped(ctx, seedVector(1), cfg, scopedScope(), 10); err != nil {
		t.Fatalf("VectorSearchScoped: %v", err)
	}
	vector := wire.Body(t)
	if !strings.Contains(vector, `"space_id":["space-hr","space-platform"]`) {
		t.Errorf("в ушедшем векторном запросе нет прав: %s", vector)
	}
	if !strings.Contains(vector, `"knn":{"field":"embedding"`) {
		t.Errorf("в ушедшем векторном запросе нет knn: %s", vector)
	}
}

// TestПустаяОбластьНеХодитВСеть: пользователь без прав — не ошибка, а пустая
// выдача, и она не должна стоить обращения к кластеру. Обратный случай (с
// правами сеть затрагивается) обязателен: иначе нулевой счётчик был бы следствием
// «адаптер вообще ничего не делает».
func TestПустаяОбластьНеХодитВСеть(t *testing.T) {
	cfg := testConfig(dimension)
	wire := newScopedWire(t)
	index := wire.adapter(t)
	ctx := context.Background()
	empty := domain.AccessScope{TenantID: "tenant-1", KBID: "kb-1"}

	hits, err := index.KeywordSearchScoped(ctx, "текст", empty, cfg, 10)
	if err != nil {
		t.Fatalf("KeywordSearchScoped: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("KeywordSearchScoped вернул %d кандидатов без прав", len(hits))
	}
	vecHits, err := index.VectorSearchScoped(ctx, seedVector(1), cfg, empty, 10)
	if err != nil {
		t.Fatalf("VectorSearchScoped: %v", err)
	}
	if len(vecHits) != 0 {
		t.Errorf("VectorSearchScoped вернул %d кандидатов без прав", len(vecHits))
	}
	if got := wire.Calls(); got != 0 {
		t.Errorf("обращений к кластеру = %d, ожидался 0: пустые права не требуют сети", got)
	}

	if _, err := index.KeywordSearchScoped(ctx, "текст", scopedScope(), cfg, 10); err != nil {
		t.Fatalf("KeywordSearchScoped с правами: %v", err)
	}
	if wire.Calls() == 0 {
		t.Error("с непустой областью сеть не затронута: путь поиска не работает")
	}
}

// TestПустойЗапросНеХодитВСеть — как у KeywordSearch: пустая строка не должна
// разгонять кластер (параллельно поведение старого пути).
func TestПустойЗапросНеХодитВСеть(t *testing.T) {
	cfg := testConfig(dimension)
	wire := newScopedWire(t)
	index := wire.adapter(t)

	if _, err := index.KeywordSearchScoped(context.Background(), "   ", scopedScope(), cfg, 10); err != nil {
		t.Fatalf("KeywordSearchScoped: %v", err)
	}
	if got := wire.Calls(); got != 0 {
		t.Errorf("обращений к кластеру = %d, ожидался 0: пустой запрос не ищет", got)
	}
}

// TestВекторЧужойРазмерностиОтвергаетсяДоСети: постоянная ошибка профиля не
// должна выглядеть как недоступность индекса и уходить в ретраи.
func TestВекторЧужойРазмерностиОтвергаетсяДоСети(t *testing.T) {
	cfg := testConfig(dimension)
	wire := newScopedWire(t)
	index := wire.adapter(t)

	_, err := index.VectorSearchScoped(context.Background(), make([]float32, dimension-1), cfg,
		scopedScope(), 10)
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeEmbeddingDimensionMismatch {
		t.Fatalf("VectorSearchScoped вернул %v, ожидался EMBEDDING_DIMENSION_MISMATCH", err)
	}
	if got := wire.Calls(); got != 0 {
		t.Errorf("обращений к кластеру = %d, ожидался 0", got)
	}
}

// TestОтветКластераРазбираетсяКакОбычно — адаптер не «придумывает» права: ответ
// с hit'ом возвращается как есть. Проверка прав после ответа — отдельный слой
// (повторная проверка по Postgres, §6 «защита в глубину»), и дублировать её
// здесь означало бы вернуться к post-filter.
func TestОтветКластераРазбираетсяКакОбычно(t *testing.T) {
	cfg := testConfig(dimension)
	wire := newScopedWire(t)
	wire.reply = `{"hits":{"hits":[{"_score":1.5,"_source":{"chunk_id":"sha256:a","tenant_id":` +
		`"tenant-1","kb_id":"kb-1","document_id":"doc-1","version_id":"ver-1","chunk_number":0,` +
		`"title":"Заголовок","text":"текст","embedding_profile_id":"` + cfg.ProfileID() +
		`","locator":{"block_ids":[],"section_path":[]}}}]}}`
	index := wire.adapter(t)

	hits, err := index.KeywordSearchScoped(context.Background(), "текст", scopedScope(), cfg, 10)
	if err != nil {
		t.Fatalf("KeywordSearchScoped: %v", err)
	}
	if len(hits) != 1 || hits[0].DocumentID != "doc-1" || hits[0].Score != 1.5 {
		t.Fatalf("hit не разобран: %+v", hits)
	}
}
