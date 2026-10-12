// Command kbeval прогоняет размеченный набор по поиску и выносит вердикт
// ворот §9: «nDCG@10 гибрида не ниже BM25-базы; бусты включаются только
// после этого замера».
//
// Команда существует ради CI: ненулевой код выхода означает закрытые
// ворота, а не «не смогли посчитать» и не «сет не нашёлся». Именно этим
// отличается проверка от догадки.
//
// Пакет eval ничего не знает про Elasticsearch и Postgres: выдачу в
// SearchFunc подставляет тот, кто запускает команду. Здесь это два
// бэкенда:
//
//	-api  боевой стенд: POST /api/v1/search по contracts/openapi.json
//	      (нужен запущенный API с наполненным индексом);
//	-backend=fixture  прогон без сети по JSON-файлу выдач, снятому
//	      ранее тем же -api. Нужен ровно для CI: гейт проверяется на
//	      записанных выдачах и не требует кластера.
//
// Флагов ровно столько, сколько нужно для решения «можно ли катить».
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	evaluation "github.com/kbsearch/kb/eval"
)

const (
	exitOK = 0
	// exitGate — ворота §9 закрыты: ранжирование хуже BM25-базы.
	exitGate = 1
	// exitError — прогон не состоялся: нет набора, нет бэкенда, ретривер
	// упал. Отдельно от провала качества, чтобы «индекс недоступен» не
	// выглядело как «качество упало» — и наоборот.
	exitError = 2
)

// defaultGolden — набор лежит рядом с пакетом eval; относительный путь
// рассчитан от каталога модуля (backend/go), как его запускает Makefile.
const defaultGolden = "eval/golden/golden.jsonl"

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("kbeval", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "kbeval — гейт §9: гибрид не хуже BM25-базы.\n\n")
		fmt.Fprintf(fs.Output(), "Использование:\n  kbeval [флаги]\n\n")
		fmt.Fprintf(fs.Output(),
			"Прогон без сети: kbeval -backend=fixture -api <файл выдач.json> -mode=compare\n"+
				"Прогон на стенде:  kbeval -api http://localhost:8080 -kb-id <uuid> -mode=compare\n\n")
		fs.PrintDefaults()
	}
	var (
		goldenPath = fs.String("golden", defaultGolden, "путь к набору вопросов (JSONL)")
		runMode    = fs.String("mode", "compare", "hybrid | bm25 | compare")
		k          = fs.Int("k", evaluation.DefaultK, "срез для nDCG (§9 требует 10)")
		outPath    = fs.String("out", "", "файл для машинного JSON-отчёта (пусто — только сводка в stdout)")
		apiURL     = fs.String("api", "", "база API со /api/v1/search")
		kbID       = fs.String("kb-id", "", "идентификатор базы знаний для -api")
		token      = fs.String("token", os.Getenv("KB_EVAL_TOKEN"), "токен доступа (или KB_EVAL_TOKEN)")
		backend    = fs.String("backend", "api", "api | fixture — где брать выдачу")
		topK       = fs.Int("top-k", 0, "сколько документов просить у выдачи (0 — не ограничивать сверху)")
		timeout    = fs.Duration("timeout", 10*time.Minute, "предел времени на весь прогон")
	)
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "kbeval: лишние аргументы %v\n", fs.Args())
		return exitError
	}
	if *k <= 0 {
		fmt.Fprintf(os.Stderr, "kbeval: -k должен быть положительным, задано %d\n", *k)
		return exitError
	}
	if *topK < 0 {
		fmt.Fprintf(os.Stderr, "kbeval: -top-k не может быть отрицательным, задано %d\n", *topK)
		return exitError
	}
	if err := validateBackend(*backend); err != nil {
		fmt.Fprintf(os.Stderr, "kbeval: %v\n", err)
		return exitError
	}

	cases, err := evaluation.LoadGoldenFile(*goldenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kbeval: %v\n", err)
		return exitError
	}

	modes, err := modesFor(*runMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kbeval: %v\n", err)
		return exitError
	}
	cfg := evaluation.RunnerConfig{K: *k, Modes: modes, RecallK: *topK}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	search, err := newSearch(ctx, searchOptions{
		backend: *backend,
		apiURL:  *apiURL,
		kbID:    *kbID,
		token:   *token,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "kbeval: %v\n", err)
		return exitError
	}

	result, runErr := evaluation.Run(ctx, cases, search, cfg)
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "kbeval: %v\n", runErr)
		return exitError
	}

	fmt.Fprint(os.Stdout, humanSummary(result))
	if *outPath != "" {
		if err := writeReport(*outPath, result); err != nil {
			fmt.Fprintf(os.Stderr, "kbeval: %v\n", err)
			return exitError
		}
		fmt.Fprintf(os.Stdout, "отчёт: %s\n", *outPath)
	}
	// Err() здесь единственный источник истины: он же формирует текст,
	// который человек читает в логе CI.
	if err := result.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "kbeval: %v\n", err)
		return exitGate
	}
	return exitOK
}

// modesFor переводит флаг -mode в список прогоняемых режимов.
func modesFor(raw string) ([]evaluation.Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "compare":
		// Оба режима: без базы сравнивать не с чем, без кандидата нечего
		// выкатывать.
		return []evaluation.Mode{evaluation.ModeBM25Only, evaluation.ModeHybrid}, nil
	case "hybrid", "bm25", "bm25_only", "lexical":
		mode, err := evaluation.ParseMode(raw)
		if err != nil {
			return nil, err
		}
		return []evaluation.Mode{mode}, nil
	default:
		return nil, fmt.Errorf("неизвестный -mode %q: ожидались hybrid, bm25 или compare", raw)
	}
}

func validateBackend(name string) error {
	switch name {
	case "api", "fixture":
		return nil
	default:
		return fmt.Errorf("неизвестный -backend %q: ожидались api или fixture", name)
	}
}

// Бэкенды выдачи.
//
// Ключевая мысль: пакет eval не знает, откуда взялись документы, и это
// позволяет проверять гейт в CI без Elasticsearch. Здесь описаны два
// источника, и оба подставляются в одну и ту же evaluation.SearchFunc.

// searchOptions — параметры выбора источника выдачи.
type searchOptions struct {
	// backend: "api" или "fixture".
	backend string
	// apiURL — база API. Для fixture это путь к JSON-файлу с выдачами.
	apiURL string
	kbID   string
	token  string
}

// newSearch строит SearchFunc для выбранного бэкенда.
func newSearch(ctx context.Context, opts searchOptions) (evaluation.SearchFunc, error) {
	if opts.apiURL == "" {
		return nil, fmt.Errorf("не указан -api: для бэкенда %q это %s", opts.backend,
			map[bool]string{
				true:  "файл со снятыми выдачами (JSON, см. -help)",
				false: "база API со /api/v1/search, например http://localhost:8080",
			}[opts.backend == "fixture"])
	}
	if opts.backend == "fixture" {
		return newFixtureSearch(opts.apiURL)
	}
	return newAPISearch(ctx, opts)
}

// --- бэкенд api -------------------------------------------------------------

type searchOptionsClient struct {
	baseURL string
	kbID    string
	token   string
	client  *http.Client
}

// searchBody — тело POST /api/v1/search по contracts/openapi.json.
// Структура повторяет контракт, а не импортирует его: пакет eval обязан
// оставаться независимым от контрактов, а лишняя копия схемы дешевле
// расхождения при изменении API, потому что расхождение проявится
// немедленно — запрос вернёт 422.
type searchBody struct {
	KBID  string `json:"kb_id"`
	Query string `json:"query"`
	TopK  int    `json:"top_k,omitempty"`
}

type searchResponse struct {
	Status string `json:"status"`
	Hits   []struct {
		ChunkID    string  `json:"chunk_id"`
		DocumentID string  `json:"document_id"`
		VersionID  string  `json:"version_id"`
		Score      float64 `json:"score"`
	} `json:"hits"`
	RequestID string `json:"request_id"`
}

func newAPISearch(ctx context.Context, opts searchOptions) (evaluation.SearchFunc, error) {
	if opts.kbID == "" {
		return nil, errors.New("не указан -kb-id: без него поиск идёт по всем базам, " +
			"и замер становится случайным")
	}
	if _, err := url.Parse(opts.apiURL); err != nil {
		return nil, fmt.Errorf("не разобрался -api %q: %w", opts.apiURL, err)
	}
	// Таймаут на запрос: зависший HTTP-клиент без срока удерживает CI
	// до общего -timeout, и прогон выглядит зависшим, а не сломанным.
	client := &http.Client{Timeout: 30 * time.Second}
	c := &searchOptionsClient{
		baseURL: strings.TrimRight(opts.apiURL, "/"),
		kbID:    opts.kbID,
		token:   opts.token,
		client:  client,
	}
	return c.search, nil
}

// search делает один запрос к /api/v1/search.
//
// Режим сюда не передаётся намеренно: публичный контракт поиска не имеет
// переключателя «только лексическая ветвь», и через HTTP нельзя снять
// BM25-базу, не меняя API. Это ограничение прогоняющего слоя, а не
// пакета eval: тот честно сравнивает любые два режима, если ему дали две
// разные функции. Пока переключателя нет, снять базу можно только
// офлайн-бэкендом: сначала прогнать инде��с в режиме bm25, затем hybrid.
func (c *searchOptionsClient) search(
	ctx context.Context, q evaluation.Query, _ evaluation.Mode, limit int,
) ([]evaluation.Hit, error) {
	body := searchBody{KBID: c.kbID, Query: q.Text}
	if limit > 0 {
		body.TopK = limit
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("вопрос %s: тело запроса: %w", q.CaseID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/search", strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("вопрос %s: %w", q.CaseID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("вопрос %s: поиск недоступен: %w", q.CaseID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("вопрос %s: поиск вернул %s: %s",
			q.CaseID, resp.Status, strings.TrimSpace(string(snippet)))
	}
	var decoded searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("вопрос %s: не разобрался ответ поиска: %w", q.CaseID, err)
	}

	hits := make([]evaluation.Hit, 0, len(decoded.Hits))
	for _, h := range decoded.Hits {
		// Один документ приходит несколькими чанками. Метрики считаются по
		// документам, и без склейки документ занимал бы несколько позиций
		// выдачи и дважды попал бы в Recall.
		if h.DocumentID == "" {
			continue
		}
		hits = append(hits, evaluation.Hit{DocID: h.DocumentID})
	}
	return hits, nil
}

// --- бэкенд fixture ---------------------------------------------------------

// fixtureHits — снятые выдачи: режим → вопрос → документы в порядке
// релевантности. Это ровно тот JSON, который пишет -api, плюс обёртка для
// прогонов без сети.
type fixtureHits struct {
	// Modes: {"hybrid": {"<case-id>": ["doc", ...]}, "bm25_only": {...}}
	Modes map[string]map[string][]string `json:"modes"`
	// Description — необязательное поле для человека, читающего артефакт.
	Description string `json:"description,omitempty"`
}

// fixtureBackend отдаёт заранее снятые выдачи.
type fixtureBackend struct {
	hits map[string]map[string][]string
}

// newFixtureSearch читает файл со снятыми выдачами.
func newFixtureSearch(path string) (evaluation.SearchFunc, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("файл выдач %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	var parsed fixtureHits
	dec := json.NewDecoder(bufio.NewReader(file))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("файл выдач %q: %w", path, err)
	}
	if len(parsed.Modes) == 0 {
		return nil, fmt.Errorf("файл выдач %q: нет ни одного режима — "+
			"ожидался объект {\"modes\": {...}}", path)
	}

	// Каталог вопросов проверяем сразу: снятая выдача, в которой вопрос
	// переименован, даст «промах» там, где на самом деле ошибка разметки.
	backend := &fixtureBackend{hits: make(map[string]map[string][]string, len(parsed.Modes))}
	for mode, byCase := range parsed.Modes {
		if _, err := evaluation.ParseMode(mode); err != nil {
			return nil, fmt.Errorf("файл выдач %q: %w", path, err)
		}
		backend.hits[mode] = byCase
	}
	return backend.search, nil
}

// search отдаёт снятую выдачу по режиму и вопросу.
func (f *fixtureBackend) search(
	_ context.Context, q evaluation.Query, mode evaluation.Mode, limit int,
) ([]evaluation.Hit, error) {
	byCase, ok := f.hits[string(mode)]
	if !ok {
		return nil, fmt.Errorf("вопрос %s: в файле выдач нет режима %q — снимите его тем же -api",
			q.CaseID, mode)
	}
	docs, ok := byCase[q.CaseID]
	if !ok {
		// Отсутствие вопроса в снятой выдаче — ошибка артефакта, а не
		// «ничего не нашлось»: система не может молча превратить устаревший
		// снимок в 141 промах и покраснеть в отчёте.
		return nil, fmt.Errorf("вопрос %s: в файле выдач нет выдачи для режима %q — "+
			"снимок устарел, перезапишите его", q.CaseID, mode)
	}
	hits := make([]evaluation.Hit, 0, len(docs))
	for i, doc := range docs {
		if limit > 0 && i == limit {
			break
		}
		hits = append(hits, evaluation.Hit{DocID: doc, Rank: i})
	}
	return hits, nil
}

// humanSummary печатает короткую сводку для человека: что прогнали, какие
// цифры получились и открыты ли ворота. Машина читает -out, человек —
// это; смешивать их в одном потоке означало бы, что CI-лог придётся
// парсить регулярками.
func humanSummary(result *evaluation.RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "вопросов: %d, срез nDCG@%d, Recall@%d\n",
		result.Cases, result.K, result.RecallK)
	for _, m := range result.Modes {
		r := m.Report
		fmt.Fprintf(&b, "  %-9s nDCG@%d=%.4f Recall@%d=%.4f MRR=%.4f",
			m.Mode, result.K, r.NDCG, result.RecallK, r.Recall, r.MRR)
		if r.TrapCases > 0 {
			fmt.Fprintf(&b, " ловушек=%d ложных=%d", r.TrapCases, r.FalsePositives)
		}
		b.WriteString("\n")
		if len(m.FailedQueries) > 0 {
			fmt.Fprintf(&b, "    ретривер упал на %d вопросах: %s\n",
				len(m.FailedQueries), strings.Join(firstN(m.FailedQueries, 5), ", "))
		}
		if len(m.EmptyQueries) > 0 {
			fmt.Fprintf(&b, "    без выдачи: %d вопросов\n", len(m.EmptyQueries))
		}
	}
	if result.Gate == nil {
		b.WriteString("гейт §9 не выносился: нужен прогон в обоих режимах (-mode=compare)\n")
		return b.String()
	}
	gate := result.Gate
	fmt.Fprintf(&b, "гейт §9: %+.4f (%.4f → %.4f, допуск %.4f), просело %d, улучшилось %d\n",
		gate.NDCG.Delta, gate.NDCG.Baseline, gate.NDCG.Candidate,
		gate.Tolerance, gate.NDCG.RegressedCases, gate.NDCG.ImprovedCases)
	for _, kd := range gate.KindRegressions {
		// Предупреждение, а не вердикт: §9 требует одно число, но рост
		// на одной категории не должен прятать провал на другой.
		fmt.Fprintf(&b, "  внимание: категория %s просела %.4f → %.4f (%+.4f, %d вопросов)\n",
			kd.Kind, kd.Baseline, kd.Candidate, kd.Delta, kd.Cases)
	}
	if gate.Passed {
		b.WriteString("ворота открыты: гибрид не хуже BM25-базы\n")
	} else {
		b.WriteString("ворота закрыты\n")
	}
	return b.String()
}

// writeReport кладёт машинный отчёт в файл.
func writeReport(path string, result *evaluation.RunResult) error {
	payload := struct {
		evaluation.RunResult
		Passed bool `json:"passed"`
	}{
		RunResult: *result,
		Passed:    result.Gate == nil || result.Gate.Passed,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("сериализация отчёта: %w", err)
	}
	// Файл создаётся целиком и только потом заменяет прежний: обрезанный
	// артефакт CI хуже отсутствующего.
	var buf bytes.Buffer
	buf.Write(encoded)
	buf.WriteString("\n")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("запись отчёта %q: %w", path, err)
	}
	return nil
}

// firstN показывает первые n элементов для короткой диагностики.
func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := append([]string(nil), items[:n]...)
	return append(out, fmt.Sprintf("… ещё %d", len(items)-n))
}
