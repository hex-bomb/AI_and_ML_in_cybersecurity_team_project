package evaluation

import (
	"context"
	"fmt"
	"strings"
)

// Query — запрос к ретриверу с идентификатором вопроса.
//
// Идентификатор входит в запрос намеренно: без него невозможно сопоставить
// выдачу с вопросом в отчёте, а сопоставление по тексту ломается на двух
// одинаковых вопросах — и тогда пропавший результат выглядит как «вопрос
// никогда не возвращался».
type Query struct {
	CaseID string
	Text   string
}

// SearchFunc — единственная точка, через которую eval достаёт выдачу.
//
// Почему функция, а не интерфейс с методами и не прямой вызов
// internal/search: пакет evaluation не должен зависеть от внутренних
// слоёв. Иначе `go test ./eval/...` требует живой Elasticsearch и Postgres,
// юнит-тест гейта невозможно написать, а правило «любое изменение
// ранжирования проходит eval-набор» невозможно проверить в CI до выката —
// то есть ровно там, где оно нужно.
//
// Реализация для боевого стенда живёт снаружи пакета (cmd/kbeval или
// тестовый набор в internal) и подставляется сюда. Функция обязана вернуть
// документы в порядке убывания релевантности: ранги проставляются прогоном,
// поле Hit.Rank на входе игнорируется.
type SearchFunc func(ctx context.Context, q Query, mode Mode, limit int) ([]Hit, error)

// RunnerConfig — параметры прогона по набору.
type RunnerConfig struct {
	// K — срез для nDCG. Ноль означает DefaultK (10, как в §9).
	K int
	// RecallK — срез для Recall. Ноль означает DefaultRecallK (50).
	RecallK int
	// MinGrade — порог релевантности. Ноль означает DefaultMinGrade (1).
	MinGrade int
	// Modes — какие режимы прогнать. Пустой список означает оба:
	// без базы сравнивать не с чем, а без кандидата нечего выкатывать.
	Modes []Mode
}

// Нормализованная конфигурация прогона: значения по умолчанию подставлены,
// режимы проверены и дедуплицированы в порядке Modes.
func (c RunnerConfig) normalized() (RunnerConfig, error) {
	if c.K <= 0 {
		c.K = DefaultK
	}
	if c.RecallK <= 0 {
		c.RecallK = DefaultRecallK
	}
	if c.MinGrade <= 0 {
		c.MinGrade = DefaultMinGrade
	}
	// Recall@50 при выдаче короче k бессмысленен: Recall заведомо ниже nDCG,
	// и «улучшение Recall» в гейте окажется сравнением несравнимого.
	if c.RecallK < c.K {
		c.RecallK = c.K
	}
	if len(c.Modes) == 0 {
		c.Modes = []Mode{ModeBM25Only, ModeHybrid}
	}
	seen := make(map[Mode]struct{}, len(c.Modes))
	ordered := make([]Mode, 0, len(c.Modes))
	for _, m := range c.Modes {
		if !m.Valid() {
			return RunnerConfig{}, fmt.Errorf("неизвестный режим поиска %q", m)
		}
		if _, dup := seen[m]; dup {
			// Повтор режима не ошибочен сам по себе, но почти всегда означает
			// ошибку в скрипте прогона, и молчаливый пропуск сделал бы
			// результат невоспроизводимым по построению.
			return RunnerConfig{}, fmt.Errorf("режим %q указан дважды", m)
		}
		seen[m] = struct{}{}
		ordered = append(ordered, m)
	}
	c.Modes = ordered
	return c, nil
}

// ModeResult — отчёт по одному режиму плюс диагностика прогона.
type ModeResult struct {
	Mode   Mode   `json:"mode"`
	Report Report `json:"report"`
	// FailedQueries — вопросы, на которых ретривер вернул ошибку.
	// Отчёт по ним всё равно содержит нули (см. runMode), но список
	// виден, чтобы «качество упало» и «индекс был недоступен» не
	// выглядели одним и тем же.
	FailedQueries []string `json:"failed_queries,omitempty"`
	// EmptyQueries — вопросы без размеченной выдачи. Их ноль в среднем —
	// честный промах, а не ошибка прогона.
	EmptyQueries []string `json:"empty_queries,omitempty"`
}

// RunResult — полный прогон: отчёт по каждому режиму и вердикт гейта.
type RunResult struct {
	K        int          `json:"k"`
	RecallK  int          `json:"recall_k"`
	MinGrade int          `json:"min_grade"`
	Cases    int          `json:"cases"`
	Modes    []ModeResult `json:"modes"`
	// Gate заполнен только когда прогнаны оба режима: сравнивать не с чем.
	Gate *Gate `json:"gate,omitempty"`
}

// Report возвращает отчёт по режиму.
func (r *RunResult) Report(mode Mode) (Report, bool) {
	for _, m := range r.Modes {
		if m.Mode == mode {
			return m.Report, true
		}
	}
	return Report{}, false
}

// RunResult.Err возвращает ошибку прогона: ворота §9 закрыты, наборы для
// сравнения разошлись или ретривер упал. Прогон, где всё зелено, ошибки
// не даёт.
func (r *RunResult) Err() error {
	if r.Gate == nil {
		return nil
	}
	return r.Gate.Err()
}

// Run гоняет набор в каждом запрошенном режиме и, если прогнаны оба,
// выносит вердикт §9.
//
// Ошибка ретривера на одном вопросе НЕ роняет прогон и превращается в
// нулевые метрики по этому вопросу: упавший Elasticsearch не должен
// превращать 99 остальных вопросов в «нет данных». При этом прогон
// считается испорченным, если ретривер ошибался на части вопросов или на
// всех сразу — иначе зелёный гейт означал бы «индекс лежал», и именно
// такую тихую поломку §5 и запрещает («любое изменение ранжирования
// проходит через этот набор»).
func Run(ctx context.Context, cases []Case, search SearchFunc, cfg RunnerConfig) (*RunResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("контекст обязателен: без него отмена прогона невозможна")
	}
	if search == nil {
		return nil, fmt.Errorf("не задан SearchFunc: пакет eval не знает, как ходить в индекс — " +
			"функцию подставляет вызывающий код")
	}
	if len(cases) == 0 {
		// Пустой набор — ошибка наравне с пустой выдачей: «ноль вопросов»
		// и «ноль качества» не должны выглядеть одинаково.
		return nil, fmt.Errorf("набор пуст: без размеченных вопросов метрики не считаются")
	}
	normalized, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	for _, c := range cases {
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("набор повреждён: %w", err)
		}
	}

	result := &RunResult{
		K:        normalized.K,
		RecallK:  normalized.RecallK,
		MinGrade: normalized.MinGrade,
		Cases:    len(cases),
		Modes:    make([]ModeResult, 0, len(normalized.Modes)),
	}
	for _, mode := range normalized.Modes {
		result.Modes = append(result.Modes, runMode(ctx, cases, search, mode, normalized))
	}

	if err := checkFailures(result); err != nil {
		return nil, err
	}
	if gate := buildGate(result, normalized); gate != nil {
		result.Gate = gate
	}
	return result, nil
}

// runMode прогоняет один режим.
func runMode(ctx context.Context, cases []Case, search SearchFunc, mode Mode, cfg RunnerConfig) ModeResult {
	out := ModeResult{Mode: mode}
	results := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		q := Query{CaseID: c.ID, Text: c.Query}
		hits, err := search(ctx, q, mode, cfg.RecallK)
		if err != nil {
			// Провал запроса — нулевая метрика по этому вопросу, а не
			// пропуск: выкинуть вопрос из среднего значит завысить оценку
			// ровно тогда, когда индекс недоступен.
			hits = nil
			out.FailedQueries = append(out.FailedQueries, c.ID)
		}
		hits = sanitizeHits(hits, cfg.RecallK)
		scored := ScoreCase(c, hits, cfg.K, cfg.RecallK, cfg.MinGrade)
		if len(hits) == 0 && !c.ExpectedEmpty {
			// Легальный промах: система честно не нашла ответ. Ноль в
			// среднем — правильное значение, паникой здесь быть нечем.
			out.EmptyQueries = append(out.EmptyQueries, c.ID)
		}
		results = append(results, scored)
	}
	out.Report = Aggregate(results)
	return out
}

// sanitizeHits приводит выдачу к виду, на котором метрики считаются
// однозначно.
//
// Три вещи, каждая из которых иначе тихо портит числа:
//
//  1. Пустой DocID отбрасывается — он несопоставим с разметкой, и его
//     нельзя отличить от «система вернула мусор».
//  2. Дубликаты по DocID схлопываются: один документ, пришедший двумя
//     чанками, не должен занимать две позиции выдачи и дважды попадать в
//     DCG и Recall. Порядок первой встречи сохраняется — это и есть лучший
//     ранг документа.
//  3. Ранги проставляются заново по фактической позиции: поле Rank на входе
//     от поискового движка может быть чем угодно, а метрики читают позицию в
//     срезе.
func sanitizeHits(hits []Hit, limit int) []Hit {
	if len(hits) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(hits))
	out := make([]Hit, 0, len(hits))
	for _, hit := range hits {
		docID := strings.TrimSpace(hit.DocID)
		if docID == "" {
			continue
		}
		if _, dup := seen[docID]; dup {
			continue
		}
		seen[docID] = struct{}{}
		out = append(out, Hit{DocID: docID, Rank: len(out)})
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// checkFailures отличает «качество упало» от «прогон не состоялся».
func checkFailures(result *RunResult) error {
	var errs []string
	for _, m := range result.Modes {
		if len(m.FailedQueries) > 0 {
			errs = append(errs, fmt.Sprintf("режим %s: ретривер вернул ошибку на %d из %d вопросов (%s)",
				m.Mode, len(m.FailedQueries), result.Cases, sampleIDs(m.FailedQueries)))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &RunError{Errors: errs}
}

// RunError — прогон не состоялся, вердикт о качестве не выносится.
type RunError struct {
	Errors []string
}

func (e *RunError) Error() string {
	var b strings.Builder
	b.WriteString("прогон eval не состоялся, качество не измерено:")
	for _, s := range e.Errors {
		b.WriteString("\n  ")
		b.WriteString(s)
	}
	b.WriteString("\n  зелёный гейт на таком прогоне означал бы «индекс был недоступен», " +
		"а не «ранжирование не хуже базы»")
	return b.String()
}

// buildGate строит вердикт, если есть оба режима.
func buildGate(result *RunResult, cfg RunnerConfig) *Gate {
	base, hasBase := result.Report(ModeBM25Only)
	cand, hasCand := result.Report(ModeHybrid)
	if !hasBase || !hasCand {
		return nil
	}
	gate := CompareModes(base, cand, GateOptions{K: cfg.K})
	return &gate
}

// sampleIDs показывает несколько идентификаторов для диагностики.
func sampleIDs(ids []string) string {
	const limit = 5
	if len(ids) <= limit {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:limit], ", ") + fmt.Sprintf(", … ещё %d", len(ids)-limit)
}
