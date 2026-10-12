package arch

// Файл — общие фикстуры ворот фазы A из §9 «Критерии готовности MVP»:
// права, удаление, свежесть коннекторов, пересборка индекса из lake.
//
// ГЛАВНОЕ ПРО ЭТОТ ФАЙЛ. Здесь нет ни одной копии производственного правила.
// Обе стороны ворот вызываются настоящие:
//
//   - оракул прав — internal/store/pg.ACLResolver.ResolveScope (реальный SQL
//     цепочки AGDLP поверх подставного Queryer);
//   - фильтр индекса — internal/store/es.Index.KeywordSearchScoped, который
//     внутри зовёт aclAccessClause (§6).
//
// Оба вызова идут через публичные методы адаптеров, поэтому ворота нельзя
// «случайно» наполнить зеркалом правила: переименование поля прав или потеря
// minimum_should_match ломают тест, потому что тест читает ТЕЛО запроса,
// которое ушло в сеть.
//
// Подменено ровно три вещи, и каждая подменена осознанно:
//
//  1. Elasticsearch — записывающим httptest-сервером gateWire. Он отдаёт
//     правдоподобные ответы и запоминает тела запросов. Настоящий кластер
//     проверяется отдельно, и без него тот тест пропускается.
//  2. PostgreSQL — подставным ports.Queryer (gateQueryer). Он отвечает на два
//     запроса оракула прав из разложенного в памяти мира AGDLP и ОТКАЗЫВАЕТ на
//     любом другом SQL: изменение текста запроса ломает ворота громко, а не
//     молча превращает оракул в «никто ничего не видит».
//  3. Исполнение самой Elasticsearch-клозы — крошечным матчером gateESMatch.
//     Это ТЕСТОВАЯ ЗАГЛУШКА, а не эмулятор Elasticsearch; её границы честно
//     перечислены в комментарии над gateESMatch.
//
// Чего этот файл принципиально не делает: не подменяет оракул и не строит
// клозу руками. Оба этих действия сделали бы ворота бессмысленными.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v9"
	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
	esadapter "github.com/kbsearch/kb/internal/store/es"
	pgadapter "github.com/kbsearch/kb/internal/store/pg"
)

// gateKBID — база знаний, в которой живёт весь фикстурный корпус.
//
// Одна база знаний выбрана намеренно: матрица прав §6 — про права, а не про
// kb_id и embedding_profile_id. Эти два терма проверяются отдельно
// (TestФильтрПравНесётОбязательныеТермы), и смешивать их с правами в одной
// матрице значило бы сравнивать оракул прав с оракулом базы знаний.
const gateKBID = "kb-gate"

// gateDimension — размерность вектора для фикстурных чанков. Восемь, а не 384:
// при подставном кластере вектор не считается, а validateChunks проверяет
// только длину.
const gateDimension = 8

// gateConfig возвращает профиль индекса для ворот.
func gateConfig() domain.IndexingConfig {
	return domain.IndexingConfig{
		EmbeddingProviderID: "local",
		EmbeddingModel:      "intfloat/multilingual-e5-small",
		EmbeddingRevision:   "gates-1",
		EmbeddingDimension:  gateDimension,
		ChunkMaxTokens:      320,
		ChunkOverlapTokens:  64,
		TokenizerID:         "e5",
		NormalizeVectors:    true,
		Distance:            "cosine",
	}
}

// ── Подставной Elasticsearch ────────────────────────────────────────────────

// gateRequest — один ушедший запрос: метод, путь и тело.
type gateRequest struct {
	Method string
	Path   string
	Body   string
}

// gateWire — Elasticsearch, который записывает запросы и отвечает правдоподобно.
//
// Ответы здесь не содержат ни одного документа: выдачу ворота не читают из
// ответа, а вычисляют сами из тела УШЕДШЕГО запроса (см. gateFilterOf).
// Это важно для смысла проверки: post-filter прав — именно то, что §6
// запрещает, поэтому «проверять, применён ли фильтр к ответу» здесь нельзя.
type gateWire struct {
	server *httptest.Server
	mu     sync.Mutex
	reqs   []gateRequest
}

func newGateWire(t *testing.T) *gateWire {
	t.Helper()
	wire := &gateWire{}
	wire.server = httptest.NewServer(http.HandlerFunc(wire.handle))
	t.Cleanup(wire.server.Close)
	return wire
}

// handle отвечает на все вызовы адаптера.
//
// Индекс всегда «не существует» (HEAD → 404): EnsureIndex создаст его, и
// createIndex/refresh/bulk отработают по ветке создания. Так путь записи
// чанков в индекс проверяется целиком, включая сборку _source.
func (w *gateWire) handle(rw http.ResponseWriter, r *http.Request) {
	payload, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	w.reqs = append(w.reqs, gateRequest{Method: r.Method, Path: r.URL.Path, Body: string(payload)})
	w.mu.Unlock()

	// Заголовок обязателен: клиент go-elasticsearch отказывается работать без него.
	rw.Header().Set("X-Elastic-Product", "Elasticsearch")
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(gateStatus(r))
	_, _ = rw.Write([]byte(gateReply(r.URL.Path, payload)))
}

// gateStatus — код ответа. Только HEAD (проверка существования индекса)
// отвечает 404: так EnsureIndex идёт по ветке создания.
func gateStatus(r *http.Request) int {
	if r.Method == http.MethodHead {
		return http.StatusNotFound
	}
	return http.StatusOK
}

// gateReply собирает ответ по назначению вызова.
func gateReply(path string, payload []byte) string {
	switch {
	case strings.HasSuffix(path, "_bulk"):
		return gateBulkReply(payload)
	case strings.HasSuffix(path, "_delete_by_query"):
		return `{"deleted":1,"failures":[]}`
	default:
		return `{"acknowledged":true,"hits":{"hits":[]}}`
	}
}

// gateBulkReply подтверждает запись ровно того числа элементов, которое было
// в теле: адаптер падает на несовпадении, и уменьшенный ответ не должен
// выглядеть как успех.
func gateBulkReply(payload []byte) string {
	var items []string
	for _, line := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
		if !strings.HasPrefix(line, `{"index"`) {
			continue
		}
		items = append(items, `{"index":{"status":201}}`)
	}
	return `{"took":1,"errors":false,"items":[` + strings.Join(items, ",") + `]}`
}

// requests отдаёт записанные запросы.
func (w *gateWire) requests() []gateRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]gateRequest, len(w.reqs))
	copy(out, w.reqs)
	return out
}

// lastBodyOf — тело последнего обращения к пути с данным суффиксом.
func (w *gateWire) lastBodyOf(t *testing.T, suffix string) string {
	t.Helper()
	reqs := w.requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if strings.HasSuffix(reqs[i].Path, suffix) {
			return reqs[i].Body
		}
	}
	t.Fatalf("к подставному кластеру не обращались по пути *%s", suffix)
	return ""
}

// searches — число поисковых запросов _search. Ноль означает, что адаптер
// ответил без сети: пустая область доступа не должна стоить обращения.
func (w *gateWire) searches() int {
	n := 0
	for _, req := range w.requests() {
		if strings.HasSuffix(req.Path, "_search") {
			n++
		}
	}
	return n
}

// deletes — тела всех _delete_by_query, попавших в кластер.
func (w *gateWire) deletes() []string {
	var out []string
	for _, req := range w.requests() {
		if strings.HasSuffix(req.Path, "_delete_by_query") {
			out = append(out, req.Body)
		}
	}
	return out
}

// gateAdapter собирает настоящий адаптер Elasticsearch поверх подставного
// кластера.
func gateAdapter(t *testing.T, wire *gateWire) *esadapter.Index {
	t.Helper()
	client, err := elasticsearch.New(elasticsearch.WithAddresses(wire.server.URL))
	if err != nil {
		t.Fatalf("клиент Elasticsearch: %v", err)
	}
	index, err := esadapter.NewWithClient(client)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	t.Cleanup(func() { _ = index.Close(context.Background()) })
	return index
}

// ── Корпус: документы, записанные настоящим UpsertChunks ────────────────────

// gateDoc — один документ ворот: метка для отчёта и права §6.
//
// Поле Source заполняется НЕ вручную: это тело _source, которое адаптер
// собрал сам и отправил в bulk. Права документа едут в индекс по одной строке
// кода (toDocSource), и если этот код разойдётся с конвертом, ворота увидят
// расхождение, а не повторят его у себя.
type gateDoc struct {
	ID       string
	TenantID string
	KBID     string
	Document string
	Version  string
	Rights   domain.DocumentRights
	Text     string

	// Source — реальный _source индекса (nil до gateIndexCorpus).
	Source map[string]any
}

// gateChunk собирает настоящий чанк для записи в индекс.
//
// Вектор ЕДИНИЧНЫЙ, а не нулевой: Elasticsearch с косинусной мерой отвергает
// вектор нулевой длины («does not support vectors with zero magnitude»), и живой
// прогон ворот удаления на этом споткнулся бы — то есть ворота падали бы по
// причине, не связанной с правами. Чанк на то и настоящий, что проходит
// validateChunks и принимается кластером.
//
// ChunkID берётся из настоящего domain.ChunkIDFor, чтобы идентификаторы в
// _source были настоящими.
func gateChunk(cfg domain.IndexingConfig, doc gateDoc) domain.Chunk {
	kb := doc.KBID
	if kb == "" {
		kb = gateKBID
	}
	version := doc.Version
	if version == "" {
		version = doc.Document + "-v1"
	}
	embedding := make([]float32, cfg.EmbeddingDimension)
	embedding[0] = 1
	return domain.Chunk{
		SchemaVersion: "1.0",
		ChunkID: domain.ChunkIDFor(domain.ChunkIDParams{
			VersionID:   version,
			ProfileID:   cfg.ProfileID(),
			TokenizerID: cfg.TokenizerID,
			MaxTokens:   cfg.ChunkMaxTokens,
			Overlap:     cfg.ChunkOverlapTokens,
			Number:      0,
		}),
		TenantID:           doc.TenantID,
		KBID:               kb,
		DocumentID:         doc.Document,
		VersionID:          version,
		Rights:             doc.Rights,
		ChunkNumber:        0,
		Title:              doc.ID,
		Text:               doc.Text,
		EmbeddingProfileID: cfg.ProfileID(),
		Embedding:          embedding,
	}
}

// gateIndexCorpus записывает корпус настоящим UpsertChunks и возвращает
// документы с заполненным Source.
//
// По одному чанку на документ: так _source каждого документа приходит из
// своего ответа bulk, и смешать источники невозможно. Плоский корпус —
// сознательное упрощение: ворота проверяют права, а не многокусковую
// нарезку, и права у всех чанков документа по §6 одинаковы.
func gateIndexCorpus(t *testing.T, index *esadapter.Index, wire *gateWire,
	cfg domain.IndexingConfig, docs []gateDoc) []gateDoc {
	t.Helper()
	ctx := context.Background()
	out := make([]gateDoc, 0, len(docs))
	for _, doc := range docs {
		written, err := index.UpsertChunks(ctx, cfg, []domain.Chunk{gateChunk(cfg, doc)})
		if err != nil {
			t.Fatalf("UpsertChunks(%s): %v", doc.ID, err)
		}
		if written != 1 {
			t.Fatalf("UpsertChunks(%s) записал %d чанков, ожидался 1", doc.ID, written)
		}
		indexed := doc
		indexed.Source = gateLastSource(t, wire)
		out = append(out, indexed)
	}
	return out
}

// gateLastSource достаёт _source последнего записанного чанка из тела bulk.
//
// Тело bulk — NDJSON: нечётные строки — операции, чётные — документы. Здесь
// берётся последняя строка документа: UpsertChunks вызывается по одному
// чанку, и это ровно что он только что записал.
func gateLastSource(t *testing.T, wire *gateWire) map[string]any {
	t.Helper()
	var docs []string
	for _, line := range strings.Split(strings.TrimRight(wire.lastBodyOf(t, "_bulk"), "\n"), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, `{"index"`) {
			continue
		}
		docs = append(docs, line)
	}
	if len(docs) == 0 {
		t.Fatalf("в теле bulk нет ни одного _source: %s", wire.lastBodyOf(t, "_bulk"))
	}
	var source map[string]any
	if err := json.Unmarshal([]byte(docs[len(docs)-1]), &source); err != nil {
		t.Fatalf("разбор _source документа: %v", err)
	}
	return source
}

// ── Фильтр прав: тело настоящего запроса ────────────────────────────────────

// gateFilter — фильтр, который адаптер реально отправил в Elasticsearch.
type gateFilter struct {
	// Clauses — список из query.bool.filter: term по тенанту, по базе, по
	// профилю и клоза прав §6.
	Clauses []any
	// Requested — false, если адаптер ответил вообще без обращения к сети.
	// Так ведёт себя KeywordSearchScoped на пустой области доступа, и это
	// тоже fail-closed: пустой результат без сети.
	Requested bool
}

// gateFilterOf строит фильтр настоящим KeywordSearchScoped и достаёт его из
// тела ушедшего запроса.
//
// Это единственное место, где ворота получают клозу прав: она приходит из
// scopedKeywordQuery → scopedFilterClauses → aclAccessClause, то есть из
// production-кода. Ни одна строка фильтра здесь не написана руками.
func gateFilterOf(t *testing.T, index *esadapter.Index, wire *gateWire,
	scope domain.AccessScope, cfg domain.IndexingConfig) gateFilter {
	t.Helper()
	before := wire.searches()
	// Вопрос произвольный: тело запроса от его текста не зависит, а непустая
	// строка нужна, чтобы KeywordSearchScoped не срезался проверкой запроса.
	if _, err := index.KeywordSearchScoped(context.Background(), "учебная тревога", scope, cfg, 10); err != nil {
		t.Fatalf("KeywordSearchScoped: %v", err)
	}
	if wire.searches() == before {
		return gateFilter{Requested: false}
	}
	return gateFilter{Clauses: gateFilterClauses(t, wire.lastBodyOf(t, "_search")), Requested: true}
}

// gateFilterClauses вынимает query.bool.filter из тела запроса.
func gateFilterClauses(t *testing.T, body string) []any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("разбор тела поискового запроса: %v\n%s", err, body)
	}
	query, ok := parsed["query"].(map[string]any)
	if !ok {
		t.Fatalf("в теле запроса нет query: %s", body)
	}
	boolean, ok := query["bool"].(map[string]any)
	if !ok {
		t.Fatalf("в теле запроса нет query.bool: %s", body)
	}
	clauses, ok := boolean["filter"].([]any)
	if !ok || len(clauses) == 0 {
		t.Fatalf("в теле запроса нет query.bool.filter: %s", body)
	}
	return clauses
}

// ── Крошечный исполнитель Elasticsearch-клозы ────────────────────────────────

// gateESMatch — ТЕСТОВАЯ ЗАГЛУШКА, а НЕ ЭМУЛЯТОР Elasticsearch.
//
// Она отвечает на один вопрос: «матчит ли вот этот _source вот эту клозу?».
// Поддерживаются ровно те формы, которые строит internal/store/es:
//
//	{"match_none": {}}
//	{"term":   {"поле": значение}}
//	{"terms":  {"поле": [значения]}}
//	{"bool":   {"must": [...], "filter": [...], "should": [...], "minimum_should_match": 0|1}}
//	[клоза, ...]                      — контекст filter: конъюнкция
//
// ЧЕГО ОНА НЕ ДЕЛАЕТ и чем расходится с настоящим Elasticsearch:
//
//  1. Анализ текста. Настоящий Elasticsearch анализирует только поля типа
//     text; поля прав объявлены keyword (index_body.go), и для keyword
//     сравнение точное — совпадает с эмуляцией. Если кто-то объявит space_id
//     или acl как text, настоящий кластер начнёт разбирать «user:anna» на
//     части, а эта заглушка — нет. Поэтому отдельный тест
//     (TestПоляПравВМаппингеИндексаОбъявленыKeyword) проверяет mapping, и
//     заглушка вместе с ним образует замкнутую пару: сломать её можно, но
//     не молча.
//  2. minimum_should_match кроме 0 и 1. Настоящий Elasticsearch понимает
//     проценты и условия («2<75%»). Такое значение считается аварийной
//     ситуацией и роняет тест, а не угадывается.
//  3. Неизвестная форма клозы. Роняет тест, а не трактуется как «не совпало».
//  4. null внутри terms. Настоящий Elasticsearch матчит по нему документы без
//     поля; здесь это аварийная ситуация (production такого не пишет).
//  5. Никаких оценок, kNN, пагинации и refresh — вне ворот прав они не
//     проверяются.
//
// Итог: расхождение заглушки с настоящим кластером возможно только там, где
// ворота прав ничего не проверяют.
func gateESMatch(t *testing.T, clause any, source map[string]any) bool {
	t.Helper()
	switch typed := clause.(type) {
	case []any:
		// Контекст filter: все клозы обязаны совпасть.
		for _, inner := range typed {
			if !gateESMatch(t, inner, source) {
				return false
			}
		}
		return true
	case map[string]any:
		if len(typed) != 1 {
			t.Fatalf("клоза с несколькими ключами не разбирается: %v", typed)
		}
		for name, body := range typed {
			switch name {
			case "match_none":
				return false
			case "term":
				return gateTerm(t, body, source)
			case "terms":
				return gateTerms(t, body, source)
			case "bool":
				return gateBool(t, body, source)
			default:
				t.Fatalf("неизвестная клоза %q — заглушка не эмулирует её молча", name)
			}
		}
	default:
		t.Fatalf("неизвестная форма клозы %T: %v", clause, clause)
	}
	return false
}

// gateTerm — точное совпадение одного значения.
func gateTerm(t *testing.T, body any, source map[string]any) bool {
	t.Helper()
	field, want, ok := gateSingleField(t, "term", body)
	if !ok {
		return false
	}
	values := gateValues(t, source, field)
	return len(values) == 1 && values[0] == gateScalar(t, want)
}

// gateTerms — вхождение хотя бы одного значения. Отсутствующее поле не
// матчит ничего: ровно то же, что делает настоящий Elasticsearch.
func gateTerms(t *testing.T, body any, source map[string]any) bool {
	t.Helper()
	field, want, ok := gateSingleField(t, "terms", body)
	if !ok {
		return false
	}
	list, isList := want.([]any)
	if !isList {
		t.Fatalf("terms по полю %q ожидает массив, получено %T", field, want)
	}
	allowed := make([]string, 0, len(list))
	for _, item := range list {
		if item == nil {
			t.Fatalf("null в terms по полю %q: production такого не пишет", field)
		}
		allowed = append(allowed, gateScalar(t, item))
	}
	for _, candidate := range gateValues(t, source, field) {
		if slices.Contains(allowed, candidate) {
			return true
		}
	}
	return false
}

// gateSingleField достаёт «поле → значение» из клозы term/terms.
func gateSingleField(t *testing.T, kind string, body any) (string, any, bool) {
	t.Helper()
	fields, isMap := body.(map[string]any)
	if !isMap || len(fields) != 1 {
		t.Fatalf("%s ожидает ровно одно поле, получено %v", kind, body)
	}
	for field, value := range fields {
		return field, value, true
	}
	return "", nil, false
}

// gateBool — конъюнкция must/filter плюс порог should.
//
// Порог should по умолчанию 1, если в bool нет ни must, ни filter, и 0 иначе —
// ровно то правило, которое документирует Elasticsearch.
func gateBool(t *testing.T, body any, source map[string]any) bool {
	t.Helper()
	inner, isMap := body.(map[string]any)
	if !isMap {
		t.Fatalf("bool ожидает объект, получено %T", body)
	}
	musts := gateClauseList(inner, "must")
	filters := gateClauseList(inner, "filter")
	shadows := gateClauseList(inner, "should")

	for _, clause := range slices.Concat(musts, filters) {
		if !gateESMatch(t, clause, source) {
			return false
		}
	}

	if _, hasShould := inner["should"]; !hasShould {
		return true
	}
	threshold := 1
	if len(musts) > 0 || len(filters) > 0 {
		threshold = 0
	}
	if explicit, hasExplicit := inner["minimum_should_match"]; hasExplicit {
		number, isNumber := explicit.(float64)
		if !isNumber || (int(number) != 0 && int(number) != 1) {
			t.Fatalf("minimum_should_match=%v заглушка не умеет — порог изменился, "+
				"перепишите её явно", explicit)
		}
		threshold = int(number)
	}

	matched := 0
	for _, clause := range shadows {
		if gateESMatch(t, clause, source) {
			matched++
		}
	}
	return matched >= threshold
}

// gateClauseList достаёт список клоз из bool. Отсутствующее или пустое поле
// даёт пустой список — в bool без should это единственно разумное чтение.
func gateClauseList(inner map[string]any, key string) []any {
	raw, ok := inner[key]
	if !ok || raw == nil {
		return nil
	}
	list, isList := raw.([]any)
	if !isList {
		return []any{raw}
	}
	return list
}

// gateValues — строковые значения поля _source. Отсутствующее поле даёт пустой
// список, и это fail-closed: terms по нему не совпадёт ни с чем.
func gateValues(t *testing.T, source map[string]any, field string) []string {
	t.Helper()
	raw, ok := source[field]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case string:
		return []string{typed}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if item != nil {
				out = append(out, gateScalar(t, item))
			}
		}
		return out
	default:
		t.Fatalf("поле %q в _source имеет неожиданный тип %T", field, raw)
		return nil
	}
}

// gateScalar приводит значение к строке. Числа приходят float64 после
// json.Unmarshal, а в _source поля прав записаны как строки.
func gateScalar(t *testing.T, value any) string {
	t.Helper()
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%v", typed)
	default:
		t.Fatalf("значение %v (%T) не является скаляром", value, value)
		return ""
	}
}

// ── Видимость: соединение оракула и фильтра ──────────────────────────────────

// gateVisible — документы корпуса, которые пользователь реально увидит.
//
// Считается по настоящему фильтру, ушедшему в Elasticsearch, и по настоящему
// _source, записанному в индекс. Ни одна сторона не подменена.
func gateVisible(t *testing.T, filter gateFilter, corpus []gateDoc) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if !filter.Requested {
		// Адаптер ответил без сети — пустая область доступа. Видимого нет.
		return out
	}
	for _, doc := range corpus {
		if doc.Source == nil {
			t.Fatalf("документ %s не записан в индекс: корпус собран неверно", doc.ID)
		}
		if gateESMatch(t, filter.Clauses, doc.Source) {
			out[doc.ID] = true
		}
	}
	return out
}

// ── Мир прав AGDLP для оракула ──────────────────────────────────────────────

// gateWorld — содержимое таблиц §6 в памяти: пользователи, группы,
// контейнеры, membership и права.
//
// Карты устроены как «tenant → ключ → id», потому что уникальность во всех
// таблицах §6 именно такая: (tenant_id, user_key), (tenant_id, group_key),
// (tenant_id, container_key). Два тенанта вправе держать контейнеры с ОДНИМ И
// ТЕМ ЖЕ container_key — и ворота это проверяют.
type gateWorld struct {
	users          map[uuid.UUID]map[string]uuid.UUID
	globalGroups   map[uuid.UUID]map[string]uuid.UUID
	containers     map[uuid.UUID]map[string]uuid.UUID
	resourceGroups map[uuid.UUID]map[string]uuid.UUID
	globalMembers  map[uuid.UUID][]gateMembership
	resourceMember map[uuid.UUID][]gateResourceMembership
	permissions    map[uuid.UUID][]gatePermission
}

// gateMembership — пользователь состоит в глобальной группе.
type gateMembership struct {
	Group uuid.UUID
	User  uuid.UUID
}

// gateResourceMembership — глобальная группа входит в ресурсную.
type gateResourceMembership struct {
	Resource uuid.UUID
	Global   uuid.UUID
}

// gatePermission — право ресурсной группы на контейнер.
type gatePermission struct {
	Resource  uuid.UUID
	Container uuid.UUID
	Action    string
}

func newGateWorld() *gateWorld {
	return &gateWorld{
		users:          map[uuid.UUID]map[string]uuid.UUID{},
		globalGroups:   map[uuid.UUID]map[string]uuid.UUID{},
		containers:     map[uuid.UUID]map[string]uuid.UUID{},
		resourceGroups: map[uuid.UUID]map[string]uuid.UUID{},
		globalMembers:  map[uuid.UUID][]gateMembership{},
		resourceMember: map[uuid.UUID][]gateResourceMembership{},
		permissions:    map[uuid.UUID][]gatePermission{},
	}
}

func (w *gateWorld) addUser(tenant uuid.UUID, key string, id uuid.UUID) {
	w.ensure(w.users, tenant)[key] = id
}

func (w *gateWorld) addGlobalGroup(tenant uuid.UUID, key string, id uuid.UUID) {
	w.ensure(w.globalGroups, tenant)[key] = id
}

func (w *gateWorld) addContainer(tenant uuid.UUID, key string, id uuid.UUID) {
	w.ensure(w.containers, tenant)[key] = id
}

func (w *gateWorld) addResourceGroup(tenant uuid.UUID, key string, id uuid.UUID) {
	w.ensure(w.resourceGroups, tenant)[key] = id
}

func (w *gateWorld) addGlobalMember(tenant, group, user uuid.UUID) {
	w.globalMembers[tenant] = append(w.globalMembers[tenant], gateMembership{Group: group, User: user})
}

func (w *gateWorld) addResourceMember(tenant, resource, global uuid.UUID) {
	w.resourceMember[tenant] = append(w.resourceMember[tenant],
		gateResourceMembership{Resource: resource, Global: global})
}

func (w *gateWorld) addPermission(tenant, resource, container uuid.UUID, action string) {
	w.permissions[tenant] = append(w.permissions[tenant], gatePermission{
		Resource: resource, Container: container, Action: action,
	})
}

func (w *gateWorld) ensure(m map[uuid.UUID]map[string]uuid.UUID, tenant uuid.UUID) map[string]uuid.UUID {
	if m[tenant] == nil {
		m[tenant] = map[string]uuid.UUID{}
	}
	return m[tenant]
}

// readableByUser повторяет membershipByUserSQL: пользователь по user_key →
// глобальные группы (membership-таблица) → ресурсные → права action на
// контейнеры. Возвращает пары (container_key, resource_key), как их отдаёт
// SELECT DISTINCT.
func (w *gateWorld) readableByUser(tenant uuid.UUID, userKey, action string) [][2]string {
	user, found := w.users[tenant][userKey]
	if !found {
		// JIT: пользователя может не быть в acl_users вовсе, и тогда у него
		// нет ни одной глобальной группы.
		return nil
	}
	globals := map[uuid.UUID]bool{}
	for _, member := range w.globalMembers[tenant] {
		if member.User == user {
			globals[member.Group] = true
		}
	}
	return w.permissionsReachable(tenant, globals, action)
}

// readableByGroups повторяет membershipByGroupsSQL: группы из claim →
// глобальные группы по group_key → ресурсные → права action на контейнеры.
func (w *gateWorld) readableByGroups(tenant uuid.UUID, groups []string, action string) [][2]string {
	globals := map[uuid.UUID]bool{}
	for _, key := range groups {
		if id, found := w.globalGroups[tenant][key]; found {
			globals[id] = true
		}
	}
	return w.permissionsReachable(tenant, globals, action)
}

// permissionsReachable — общая половина обоих обходов: от достижимых
// глобальных групп до прав на контейнеры.
func (w *gateWorld) permissionsReachable(tenant uuid.UUID, globals map[uuid.UUID]bool, action string) [][2]string {
	resources := map[uuid.UUID]bool{}
	for _, member := range w.resourceMember[tenant] {
		if globals[member.Global] {
			resources[member.Resource] = true
		}
	}
	seen := map[[2]string]bool{}
	var out [][2]string
	for _, perm := range w.permissions[tenant] {
		if perm.Action != action || !resources[perm.Resource] {
			continue
		}
		pair := [2]string{w.containerKey(tenant, perm.Container), w.resourceKey(tenant, perm.Resource)}
		if seen[pair] {
			continue
		}
		seen[pair] = true
		out = append(out, pair)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

func (w *gateWorld) containerKey(tenant, id uuid.UUID) string {
	for key, value := range w.containers[tenant] {
		if value == id {
			return key
		}
	}
	return ""
}

func (w *gateWorld) resourceKey(tenant, id uuid.UUID) string {
	for key, value := range w.resourceGroups[tenant] {
		if value == id {
			return key
		}
	}
	return ""
}

// ── Подставной Queryer оракула ──────────────────────────────────────────────

// gateQueryer отвечает на два запроса оракула прав §6.
//
// ГЛАВНОЕ ПРО ПОСТАНОВКУ. Это ПОДСТАНОВКА БАЗЫ, а не подмена правила:
//
//   - текст запроса разбирается по узнаваемым кускам («FROM acl_users u» и
//     «FROM acl_global_groups gg»). Любой другой SQL — ошибка, а не пустой
//     результат: иначе оракул молча превратился бы в «никто ничего не видит»
//     и ворота прошли бы при любой утечке;
//   - число аргументов и действие 'read' тоже проверяются;
//   - строки собираются из того же gateWorld, по которому построены права
//     корпуса, — оракул и индекс читают один источник правды.
//
// Чего подстановка НЕ проверяет: правильность JOIN-ов в тексте SQL и
// ограничения CHECK в схеме. Оба закрываются прогоном матрицы против
// настоящего PostgreSQL, который без базы пропускается.
type gateQueryer struct {
	world *gateWorld
	// byUser и byGroup — счётчики вызовов. Тест обязан убедиться, что оракул
	// реально спрашивал базу, иначе матрица ничего не проверяла.
	byUser  int
	byGroup int
}

func (q *gateQueryer) Query(_ context.Context, sql string, args ...any) (ports.Rows, error) {
	normalized := strings.Join(strings.Fields(strings.ToLower(sql)), " ")
	switch {
	case strings.Contains(normalized, "from acl_users u"):
		q.byUser++
		tenant, key, action, err := gateUserArgs(args)
		if err != nil {
			return nil, err
		}
		return gateRowsOf(q.world.readableByUser(tenant, key, action)), nil
	case strings.Contains(normalized, "from acl_global_groups gg"):
		q.byGroup++
		tenant, groups, action, err := gateGroupArgs(args)
		if err != nil {
			return nil, err
		}
		return gateRowsOf(q.world.readableByGroups(tenant, groups, action)), nil
	default:
		return nil, fmt.Errorf(
			"подставной Queryer не знает этого SQL: %q — оракул прав изменился, "+
				"подстановку надо переписать, иначе ворота проверяют пустоту", sql)
	}
}

// QueryRow и Exec оракулу прав не нужны. Ошибка вместо пустого результата:
// молчаливая заглушка на неизвестном вызове — это ровно тот класс дефекта,
// который ворота прав обязаны ловить.
func (q *gateQueryer) QueryRow(context.Context, string, ...any) ports.Row {
	return gateErrorRow{err: errors.New("подставной Queryer оракула прав не обслуживает QueryRow")}
}

func (q *gateQueryer) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, errors.New("подставной Queryer оракула прав не обслуживает Exec")
}

// Queries отдаёт счётчики: сколько раз оракул пошёл по каждой из двух ветвей.
func (q *gateQueryer) Queries() (byUser, byGroup int) { return q.byUser, q.byGroup }

// gateUserArgs разбирает (tenant, user_key, action).
func gateUserArgs(args []any) (uuid.UUID, string, string, error) {
	if len(args) != 3 {
		return uuid.Nil, "", "", fmt.Errorf("запрос по пользователю ждёт 3 аргумента, получено %d", len(args))
	}
	tenant, isUUID := args[0].(uuid.UUID)
	if !isUUID {
		return uuid.Nil, "", "", fmt.Errorf("tenant_id ожидался uuid.UUID, получено %T", args[0])
	}
	key, isString := args[1].(string)
	if !isString {
		return uuid.Nil, "", "", fmt.Errorf("user_key ожидался string, получено %T", args[1])
	}
	action, isString := args[2].(string)
	if !isString {
		return uuid.Nil, "", "", fmt.Errorf("action ожидался string, получено %T", args[2])
	}
	if action != gateReadAction {
		return uuid.Nil, "", "", fmt.Errorf("оракул прав спрашивает действие %q, подстановка знает только %q",
			action, gateReadAction)
	}
	return tenant, key, action, nil
}

// gateGroupArgs разбирает (tenant, groups, action).
func gateGroupArgs(args []any) (uuid.UUID, []string, string, error) {
	if len(args) != 3 {
		return uuid.Nil, nil, "", fmt.Errorf("запрос по группам ждёт 3 аргумента, получено %d", len(args))
	}
	tenant, isUUID := args[0].(uuid.UUID)
	if !isUUID {
		return uuid.Nil, nil, "", fmt.Errorf("tenant_id ожидался uuid.UUID, получено %T", args[0])
	}
	groups, err := gateStringList(args[1])
	if err != nil {
		return uuid.Nil, nil, "", err
	}
	action, isString := args[2].(string)
	if !isString {
		return uuid.Nil, nil, "", fmt.Errorf("action ожидался string, получено %T", args[2])
	}
	if action != gateReadAction {
		return uuid.Nil, nil, "", fmt.Errorf("оракул прав спрашивает действие %q, подстановка знает только %q",
			action, gateReadAction)
	}
	return tenant, groups, action, nil
}

// gateStringList принимает []string и []any: драйвер может прислать любой.
func gateStringList(value any) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return []string{}, nil
	case []string:
		return typed, nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, isString := item.(string)
			if !isString {
				return nil, fmt.Errorf("в списке групп ожидались строки, получено %T", item)
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("список групп ожидался срезом строк, получено %T", value)
	}
}

// gateReadAction — единственное действие, которое спрашивает поиск (см.
// permissionRead в internal/store/pg/acl_resolver.go).
const gateReadAction = "read"

// gateRows — строки результата подставного Queryer.
type gateRows struct {
	data [][2]string
	pos  int
}

func gateRowsOf(pairs [][2]string) *gateRows {
	return &gateRows{data: pairs}
}

func (r *gateRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *gateRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return fmt.Errorf("оракул прав сканирует 2 колонки, получено %d", len(dest))
	}
	for i, target := range dest {
		text, isString := target.(*string)
		if !isString {
			return fmt.Errorf("колонка %d ожидалась *string, получено %T", i, target)
		}
		*text = r.data[r.pos-1][i]
	}
	return nil
}

func (r *gateRows) Err() error { return nil }

func (r *gateRows) Close() {}

// gateErrorRow — Row, который всегда возвращает ошибку.
type gateErrorRow struct{ err error }

func (r gateErrorRow) Scan(...any) error { return r.err }

// ── Оракул прав поверх подстановки ──────────────────────────────────────────

// gateResolveScope зовёт НАСТОЯЩИЙ оракул §6 над подставной базой.
func gateResolveScope(t *testing.T, world *gateWorld, principal domain.Principal) (domain.AccessScope, *gateQueryer) {
	t.Helper()
	q := &gateQueryer{world: world}
	scope, err := pgadapter.NewACLResolver().ResolveScope(context.Background(), q,
		pgadapter.ScopeParams{Principal: principal, KBID: gateKBID})
	if err != nil {
		t.Fatalf("ResolveScope(%s): %v", principal.UserID, err)
	}
	return scope, q
}

// gateSortedKeys — ключи отсортированные: отчёт об утечке должен быть
// воспроизводимым между прогонами.
func gateSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// gateErrText печатает ошибку вместе с её Details.
//
// domain.AppError.Error() отдаёт только код и сообщение, а самые полезные
// подробности (причина от Elasticsearch, ожидание против факта) лежат в
// Details. Без них падение ворот читается как «INDEX_UNAVAILABLE» и требует
// отдельного захода в отладчик.
func gateErrText(err error) string {
	var appErr *domain.AppError
	if !errors.As(err, &appErr) || len(appErr.Details) == 0 {
		return err.Error()
	}
	keys := make([]string, 0, len(appErr.Details))
	for key := range appErr.Details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, appErr.Details[key]))
	}
	return err.Error() + " [" + strings.Join(parts, " ") + "]"
}

// gateUserFixture — пользователь ворот: субъект запроса и ожидаемая видимость.
//
// Ожидания записаны явно и читаются независимо от реализации: матрица
// обязана совпасть и с оракулом, и с фильтром, и с этим списком. Три
// независимых источника правды — иначе тест подтверждал бы сам себя.
type gateUserFixture struct {
	Name      string
	Principal domain.Principal
	// Expect — метки документов, которые пользователь ОБЯЗАН видеть.
	Expect []string
	// Never — метки документов, которые он обязан НЕ видеть. Это подмножество
	// «не Expect»: полный запрет на утечку проверяется по всему корпусу, а
	// Never делает намерение видимым в коде теста.
	Never []string
}

// ── Живой PostgreSQL для ворот, которым нужна настоящая схема ───────────────

// gateOpenLiveSchema поднимает миграции в ОТДЕЛЬНОЙ схеме и отдаёт пул на неё.
//
// Своя схема обязательна, а не удобство: тесты internal/store/pg чистят свои
// таблицы TRUNCATE, и общая база сделала бы два пакета зависящими от порядка
// запуска. Изоляция по схеме исключает пересечение целиком.
//
// Миграции читаются с диска, а не копируются в тест: lake — источник правды,
// и проверка второй копии схемы хуже отсутствия проверки.
func gateOpenLiveSchema(t *testing.T, ctx context.Context, schema string, files ...string) *pgadapter.DB {
	t.Helper()
	if strings.Contains(schema, ";") || schema == "" {
		t.Fatalf("недопустимое имя схемы %q", schema)
	}
	dsn := gateLiveDSN()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := pgadapter.New(ctx, pgadapter.Config{
		URL:            dsn + sep + "search_path=" + schema,
		MaxConns:       4,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Skipf("PostgreSQL для ворот недоступен на %s: %v\n"+
			"Без него этот прогон ПРОПУЩЕН — см. комментарий над тестом, "+
			"что именно остаётся непроверенным", gateLiveDSN(), err)
	}
	t.Cleanup(db.Close)

	drop := func() {
		if _, err := db.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Errorf("сброс схемы %s: %v", schema, err)
		}
	}
	if _, err := db.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Fatalf("сброс схемы %s: %v", schema, err)
	}
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("создание схемы %s: %v", schema, err)
	}
	t.Cleanup(drop)

	for _, file := range files {
		path := filepath.Join("..", "..", "migrations", "sql", file)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("чтение миграции %s: %v", file, err)
		}
		if _, err := db.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("применение миграции %s в схеме %s: %v", file, schema, err)
		}
	}
	return db
}
