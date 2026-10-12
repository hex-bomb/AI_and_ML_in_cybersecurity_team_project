package evaluation

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Режим работы ретривера, который сравнивает гейт §9.
//
// Тип, а не флаг bool: режимов станет три, когда к BM25-базе добавятся
// vector-only и RRF-гибрид, и пара булевых параметров
// (`hybrid=true, bm25=true`) неизбежно разрешается в состояние, которого
// не существует.
type Mode string

const (
	// ModeHybrid — BM25 + kNN, слитые RRF в Go-коде (раздел 5, шаг 3).
	ModeHybrid Mode = "hybrid"
	// ModeBM25Only — только лексическая ветвь. Это база, с которой
	// сравнивается гибрид: в EnterpriseRAG-Bench (§2) именно BM25 даёт
	// 68,8% против 51,4% у чистого вектора, поэтому «не хуже BM25» — это
	// не проходная цифра, а нижняя планка, ниже которой прирост от
	// вектора не окупает его стоимость.
	ModeBM25Only Mode = "bm25_only"
)

// Valid сообщает, что режим известен прогону.
func (m Mode) Valid() bool {
	return m == ModeHybrid || m == ModeBM25Only
}

func (m Mode) String() string { return string(m) }

// ParseMode разбирает имя режима из командной строки.
func ParseMode(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hybrid", "rrf", "hybrid_rrf":
		return ModeHybrid, nil
	case "bm25", "bm25_only", "lexical":
		return ModeBM25Only, nil
	default:
		return "", fmt.Errorf("неизвестный режим поиска %q: ожидались %q или %q",
			raw, ModeHybrid, ModeBM25Only)
	}
}

// NDCGTolerance — допустимое падение nDCG, при котором гейт всё равно зелёный.
//
// ВАЖНО, значение равно нулю намеренно и менять его надо с обоснованием.
//
// Гейт, который всегда зелёный, не стоит ничего: он не ловит регрессию и
// при этом создаёт ощущение, будто качество измеряется. Ненулевой допуск —
// это ровно тот «шум», который и позволяет плохому ранжированию проехать
// под зелёной крышкой. Если допуск всё-таки понадобится (допустим, набор
// пересобирают и 0.4% среднего уезжает на изменение корпуса), он должен
// стать именованной константой с числом, обоснованием и датой ревизии —
// «поставим 0.01, чтобы гейт не мигал» — это отключение контроля, а не
// его настройка. Правило §9 сформулировано как «не ниже», и мы его
// выполняем буквально.
const NDCGTolerance = 0.0

// deltaEpsilon отделяет реальную регрессию вопроса от шума представления
// float64: 1e-12 — примерно 1e6 раз меньше различимой глазом разницы.
//
// Это НЕ допуск гейта. Он влияет только на то, попадёт ли вопрос в
// список «просело» для диагностики; вердикт целиком считает NDCGTolerance.
const deltaEpsilon = 1e-12

// maxReportedRegressions — сколько худших вопросов печатает ошибка гейта.
// Больше двадцати строк никто не прочитает, а полный список остаётся в
// Gate.Regressions и в JSON-отчёте.
const maxReportedRegressions = 5

// GateOptions — параметры сравнения. Нулевые значения заменяются
// умолчаниями, поэтому вызывающий код может оставить структуру пустой.
type GateOptions struct {
	// K — срез для nDCG. По умолчанию DefaultK (10), как в воротах §9.
	K int
	// Tolerance — допуск падения nDCG. По умолчанию NDCGTolerance (0).
	Tolerance float64
}

func (o GateOptions) normalized() GateOptions {
	if o.K <= 0 {
		o.K = DefaultK
	}
	if o.Tolerance == 0 {
		o.Tolerance = NDCGTolerance
	}
	if o.K < 0 {
		o.K = DefaultK
	}
	if o.Tolerance < 0 {
		// Отрицательный допуск — это замаскированное требование «лучше»,
		// о котором никто не договаривался. Молча превращать его в ноль
		// нельзя: получится правило, отличающееся от того, что задали.
		o.Tolerance = -o.Tolerance
	}
	return o
}

// MetricDelta — изменение одной метрики между базой и кандидатом.
type MetricDelta struct {
	Name      string  `json:"name"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
	Delta     float64 `json:"delta"`
	// RegressedCases / ImprovedCases — сколько вопросов стало хуже и
	// сколько лучше. Без них среднее обманывает: минус 0.01 может
	// означать один провалившийся вопрос или просадку половины набора.
	RegressedCases int `json:"regressed_cases"`
	ImprovedCases  int `json:"improved_cases"`
	// Gate — попадает ли метрика в вердикт. У nDCG@10 true; Recall и MRR
	// считаются и показываются, но §9 требует именно nDCG, и три
	// обязательных правила в одном гейте означают невнятную ошибку в CI.
	Gate bool `json:"gate"`
}

// CaseDelta — как изменился один вопрос.
type CaseDelta struct {
	CaseID          string  `json:"case_id"`
	Kind            string  `json:"kind,omitempty"`
	Query           string  `json:"query,omitempty"`
	BaselineNDCG    float64 `json:"baseline_ndcg"`
	CandidateNDCG   float64 `json:"candidate_ndcg"`
	Delta           float64 `json:"delta"`
	BaselineRecall  float64 `json:"baseline_recall"`
	CandidateRecall float64 `json:"candidate_recall"`
}

// KindDelta — просадка внутри одной категории вопросов.
type KindDelta struct {
	Kind      string  `json:"kind"`
	Cases     int     `json:"cases"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
	Delta     float64 `json:"delta"`
}

// Gate — результат сравнения гибрида с BM25-базой.
type Gate struct {
	K         int         `json:"k"`
	Tolerance float64     `json:"tolerance"`
	NDCG      MetricDelta `json:"ndcg"`
	Recall    MetricDelta `json:"recall"`
	MRR       MetricDelta `json:"mrr"`
	// BaselineCases / CandidateCases — размеры сопоставленных наборов.
	BaselineCases  int `json:"baseline_cases"`
	CandidateCases int `json:"candidate_cases"`
	// Regressions — просевшие вопросы, от худших к лучшим, детерминированно.
	Regressions []CaseDelta `json:"regressions,omitempty"`
	// KindRegressions — просадки по категориям. Диагностика, а не вердикт:
	// §9 ставит одно число, и маскирующая рост на другой категории
	// просадка должна быть видна в отчёте. Строгий покомпонентный контроль
	// категорий делает Compare в golden.go — у него есть допуск на шум.
	KindRegressions []KindDelta `json:"kind_regressions,omitempty"`
	// Failures — список причин, по которым ворота закрыты.
	Failures []string `json:"failures,omitempty"`
	Passed   bool     `json:"passed"`
}

// CompareModes сравнивает кандидата (гибрид) с базой (BM25) по §9:
//
//	nDCG@10 гибрида не ниже BM25-базы; бусты включаются только после этого замера.
//
// Решающих метрик ровно две, обе обязаны быть из одного прогона по одному
// набору: если наборы различаются, сравнение бессмысленно, и это ошибка, а
// не «почти нулевая дельта» — разные вопросы дают разный средний балл
// независимо от ранжирования.
func CompareModes(baseline, candidate Report, opts GateOptions) Gate {
	opts = opts.normalized()

	gate := Gate{
		K:              opts.K,
		Tolerance:      opts.Tolerance,
		BaselineCases:  baseline.Cases,
		CandidateCases: candidate.Cases,
	}
	gate.NDCG = diffMetric(
		fmt.Sprintf("nDCG@%d", opts.K), baseline.NDCG, candidate.NDCG, true)
	gate.Recall = diffMetric(
		fmt.Sprintf("Recall@%d", maxInt(DefaultRecallK, opts.K)),
		baseline.Recall, candidate.Recall, false)
	gate.MRR = diffMetric("MRR", baseline.MRR, candidate.MRR, false)

	baseByID := indexResults(baseline.Details)
	candByID := indexResults(candidate.Details)

	// Общие вопросы — только они сопоставимы. Отсутствующий вопрос означает,
	// что один из прогонов потерял или добавил вопрос: считать дельту по
	// усечённому набору — значит приписать гибриду качество чужого набора.
	shared := make([]string, 0, len(baseByID))
	for id := range baseByID {
		if _, ok := candByID[id]; ok {
			shared = append(shared, id)
		}
	}
	sort.Strings(shared)

	if len(baseByID) != len(candByID) || len(shared) != len(baseByID) {
		gate.Failures = append(gate.Failures, fmt.Sprintf(
			"наборы разошлись: в базе %d уникальных вопросов, в кандидате %d, "+
				"совпали %d — дельта посчитана только по совпавшим",
			len(baseByID), len(candByID), len(shared)))
	}

	regressions := make([]CaseDelta, 0, len(shared))
	improved := 0
	unchanged := 0
	recallRegressed := 0
	for _, id := range shared {
		b, c := baseByID[id], candByID[id]
		d := CaseDelta{
			CaseID:          id,
			Kind:            c.Kind,
			Query:           c.Query,
			BaselineNDCG:    b.NDCG,
			CandidateNDCG:   c.NDCG,
			Delta:           c.NDCG - b.NDCG,
			BaselineRecall:  b.Recall,
			CandidateRecall: c.Recall,
		}
		if d.Kind == "" {
			d.Kind = b.Kind
		}
		switch {
		case d.Delta < -deltaEpsilon:
			regressions = append(regressions, d)
		case d.Delta > deltaEpsilon:
			improved++
		default:
			unchanged++
		}
		if d.CandidateRecall < d.BaselineRecall-deltaEpsilon {
			recallRegressed++
		}
	}
	// Худшие первыми: в отчёте и в ошибке регрессия должна читаться раньше,
	// чем список улучшений, который никто не ищет.
	sort.SliceStable(regressions, func(i, j int) bool {
		if regressions[i].Delta != regressions[j].Delta {
			return regressions[i].Delta < regressions[j].Delta
		}
		return regressions[i].CaseID < regressions[j].CaseID
	})
	gate.Regressions = regressions
	gate.NDCG.RegressedCases = len(regressions)
	gate.NDCG.ImprovedCases = improved
	gate.Recall.RegressedCases = recallRegressed

	gate.KindRegressions = kindDeltas(baseline, candidate)

	// Нечисловые метрики обязаны закрывать ворота, а не открывать их:
	// любое сравнение с NaN даёт false, то есть «кандидат не хуже базы».
	// Сломанный расчёт, который тихо проходит гейт, — худший вид поломки.
	for _, m := range []MetricDelta{gate.NDCG, gate.Recall, gate.MRR} {
		if !isFinite(m.Baseline) || !isFinite(m.Candidate) {
			gate.Failures = append(gate.Failures, fmt.Sprintf(
				"%s посчитана как нечисло (база %.4f, кандидат %.4f) — "+
					"прогон сломан, вердикт по нему невозможен",
				m.Name, m.Baseline, m.Candidate))
		}
	}
	for i, r := range gate.Regressions {
		if !isFinite(r.Delta) {
			gate.Failures = append(gate.Failures, fmt.Sprintf(
				"вопрос %s [%s] дал нечисловую дельту nDCG (%.4f → %.4f)",
				r.CaseID, r.Kind, r.BaselineNDCG, r.CandidateNDCG))
			gate.Regressions = gate.Regressions[:i]
			break
		}
	}

	// Вердикт §9: nDCG@10 гибрида не ниже BM25-базы. Не «в среднем не ниже
	// с допуском», а ровно не ниже.
	if gate.NDCG.Candidate < gate.NDCG.Baseline-gate.Tolerance {
		gate.Failures = append(gate.Failures, gate.ndcgFailure())
	}
	gate.Passed = len(gate.Failures) == 0
	return gate
}

// ndcgFailure собирает текст нарушения: метрика, оба значения, дельта и
// число просевших вопросов. Числа без вопросов бесполезны — «−0.012» не
// говорит, что чинить; идентификаторы вопросов говорят.
func (g Gate) ndcgFailure() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s кандидата %.4f ниже базы %.4f (дельта %+.4f, допуск %.4f)",
		g.NDCG.Name, g.NDCG.Candidate, g.NDCG.Baseline, g.NDCG.Delta, g.Tolerance)
	fmt.Fprintf(&b, "; просело %d вопросов, улучшилось %d",
		g.NDCG.RegressedCases, g.NDCG.ImprovedCases)
	if len(g.Regressions) == 0 {
		fmt.Fprintf(&b,
			"\n  подозрение на подмену набора: ни один вопрос не ухудшился, "+
				"значит сравниваются разные вопросы, а не разные ранжирования")
		return b.String()
	}
	b.WriteString("\n  худшие просадки:")
	for i, r := range g.Regressions {
		if i >= maxReportedRegressions {
			fmt.Fprintf(&b, "\n    … ещё %d (полный список в Gate.Regressions)",
				len(g.Regressions)-i)
			break
		}
		fmt.Fprintf(&b, "\n    %s [%s] %.4f → %.4f (%+.4f) %q",
			r.CaseID, r.Kind, r.BaselineNDCG, r.CandidateNDCG, r.Delta, truncate(r.Query, 90))
	}
	return b.String()
}

// Err возвращает ошибку с описанием нарушения или nil, если ворота открыты.
//
// Ошибка, а не bool: CI обязан уметь показать, что именно сломалось, и
// одним этой строкой. Возвращаемый тип — *GateError, чтобы вызывающий код
// мог отличить провал качества от падения прогонки (разные коды выхода).
func (g Gate) Err() error {
	if g.Passed {
		return nil
	}
	return &GateError{Gate: g}
}

// GateError — ворота §9 закрыты.
type GateError struct {
	Gate Gate
}

func (e *GateError) Error() string {
	var b strings.Builder
	b.WriteString("гейт §9 не пройден (гибрид не должен быть хуже BM25-базы)")
	for _, f := range e.Gate.Failures {
		b.WriteString("\n  ")
		b.WriteString(f)
	}
	return b.String()
}

// diffMetric считает одно изменение метрики.
func diffMetric(name string, base, candidate float64, isGate bool) MetricDelta {
	return MetricDelta{
		Name:      name,
		Baseline:  base,
		Candidate: candidate,
		Delta:     candidate - base,
		Gate:      isGate,
	}
}

// indexResults строит карту «вопрос → результат».
func indexResults(details []CaseResult) map[string]CaseResult {
	out := make(map[string]CaseResult, len(details))
	for _, r := range details {
		// Дубликат в Details — баг выше по стеку; оставляем худший, чтобы
		// один и тот же вопрос не учитывался дважды.
		if prev, ok := out[r.CaseID]; ok && prev.NDCG <= r.NDCG {
			continue
		}
		out[r.CaseID] = r
	}
	return out
}

// kindDeltas собирает просадки по категориям вопросов, детерминированно по имени.
func kindDeltas(baseline, candidate Report) []KindDelta {
	names := make([]string, 0, len(baseline.ByKind))
	for name := range baseline.ByKind {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]KindDelta, 0, len(names))
	for _, name := range names {
		b, okB := baseline.ByKind[name]
		c, okC := candidate.ByKind[name]
		if !okB || !okC {
			// Категория есть только в одной выдаче: здесь причина в прогоне,
			// а не в ранжировании, и она уже попала в Failures как расхождение наборов.
			continue
		}
		delta := c.NDCG - b.NDCG
		if delta >= -deltaEpsilon {
			continue
		}
		out = append(out, KindDelta{
			Kind:      name,
			Cases:     c.Cases,
			Baseline:  b.NDCG,
			Candidate: c.NDCG,
			Delta:     delta,
		})
	}
	return out
}

// maxInt — локальный помощник, чтобы не тянуть sort.Slice ради двух чисел.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// truncate обрезает текст до limit символов, не разрывая UTF-8 по байтам
// в середине руны: обрезанный вопрос в ошибке гейта обязан оставаться
// читаемым.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return strings.TrimRight(string(runes[:limit]), " ") + "…"
}

// isFinite отсекает NaN и ±Inf: они не должны попасть в отчёт ни при каких
// обстоятельствах, потому что сравнение с ними всегда даёт «прошло».
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
