package evaluation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Разметка набора хранится в JSON Lines: один вопрос — одна строка.
//
// Почему не JSON-массив: файл правят руками по одному вопросу, и при
// array-формате правка одной строки ломает diff на всём файле. JSONL
// даёт построчный diff и позволяет склеивать наборы из разных источников.

// LoadGolden читает набор из io.Reader и проверяет каждую запись.
//
// Проверка строгая по умыслу: молча пропущенная битая строка в наборе из
// 100 вопросов превращается в «регрессию», которую неделю ищут в коде.
// Пустой набор — ошибка, а не «нулевой результат».
func LoadGolden(r io.Reader) ([]Case, error) {
	var cases []Case
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "//") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, fmt.Errorf("строка %d: не разобрался JSON: %w", line, err)
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("строка %d: %w", line, err)
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("чтение набора: %w", err)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("набор пуст: без размеченных вопросов метрики не считаются")
	}
	return cases, nil
}

// LoadGoldenFile читает набор из файла.
func LoadGoldenFile(path string) ([]Case, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("набор %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	return LoadGolden(file)
}

// Validate проверяет инварианты одного вопроса.
func (c Case) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("вопрос без id")
	}
	if strings.TrimSpace(c.Query) == "" {
		return fmt.Errorf("вопрос %q: пустой текст запроса", c.ID)
	}
	if c.ExpectedEmpty && len(c.Relevant) > 0 {
		return fmt.Errorf("вопрос %q: помечен как ловушка, но размечены документы — "+
			"либо разметка, либо флаг неверен", c.ID)
	}
	if !c.ExpectedEmpty && len(c.Relevant) == 0 {
		return fmt.Errorf("вопрос %q: нет размеченных документов — по нему нельзя посчитать метрики; "+
			"если правильный ответ «ничего не найдено», поставьте expected_empty", c.ID)
	}
	seen := make(map[string]struct{}, len(c.Relevant))
	for _, rel := range c.Relevant {
		if strings.TrimSpace(rel.DocID) == "" {
			return fmt.Errorf("вопрос %q: пустой doc_id в разметке", c.ID)
		}
		if rel.Grade <= 0 {
			return fmt.Errorf("вопрос %q: оценка документа %q должна быть больше нуля, "+
				"нерелевантные документы в разметке не перечисляются", c.ID, rel.DocID)
		}
		if _, dup := seen[rel.DocID]; dup {
			return fmt.Errorf("вопрос %q: документ %q размечен дважды", c.ID, rel.DocID)
		}
		seen[rel.DocID] = struct{}{}
	}
	return nil
}

// WriteGolden сохраняет набор в JSONL (по строке на вопрос).
func WriteGolden(w io.Writer, cases []Case) error {
	enc := json.NewEncoder(w)
	for _, c := range cases {
		if err := enc.Encode(c); err != nil {
			return fmt.Errorf("запись вопроса %q: %w", c.ID, err)
		}
	}
	return nil
}

// Retriever возвращает выдачу по запросу: идентификаторы документов в
// порядке убывания релевантности.
//
// Это единственная точка сцепления с реальным поиском: пакет evaluation
// не знает ни про Elasticsearch, ни про HTTP, поэтому один и тот же
// набор гоняется и против боевого индекса, и против заглушки в тесте.
type Retriever func(query string, limit int) ([]Hit, error)

// Runner прогоняет набор и возвращает отчёт.
type Runner struct {
	// K — срез для nDCG (по умолчанию DefaultK).
	K int
	// RecallK — срез для Recall (по умолчанию DefaultRecallK).
	RecallK int
	// MinGrade — порог релевантности (по умолчанию DefaultMinGrade).
	MinGrade int
}

// Run считает метрики по набору. Ошибка ретривера на одном вопросе не
// роняет прогон: она записывается как провал (нулевые метрики), иначе
// один упавший Elasticsearch тихо превращает 99 вопросов в «нет данных».
func (r Runner) Run(cases []Case, retrieve Retriever) Report {
	k, recallK, minGrade := r.K, r.RecallK, r.MinGrade
	if k <= 0 {
		k = DefaultK
	}
	if recallK <= 0 {
		recallK = DefaultRecallK
	}
	if minGrade <= 0 {
		minGrade = DefaultMinGrade
	}
	// Recall@50 не имеет смысла при выдаче короче k: сужаем срез,
	// иначе Recall заведомо ниже nDCG и сравнение бессмысленно.
	if recallK < k {
		recallK = k
	}

	results := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		hits, err := retrieve(c.Query, recallK)
		if err != nil {
			hits = nil
		}
		results = append(results, ScoreCase(c, normalizeRanks(hits), k, recallK, minGrade))
	}
	return Aggregate(results)
}

// normalizeRanks проставляет ранги по порядку и отбрасывает документы с
// пустым идентификатором: такие не сопоставимы с разметкой и только
// раздувают выдачу.
func normalizeRanks(hits []Hit) []Hit {
	out := make([]Hit, 0, len(hits))
	for _, hit := range hits {
		if strings.TrimSpace(hit.DocID) == "" {
			continue
		}
		out = append(out, Hit{DocID: hit.DocID, Rank: len(out)})
	}
	return out
}

// Compare проверяет, что кандидат не хуже базовой выдачи.
//
// Возвращает список нарушений ворота: пустой список означает «можно
// выкатывать». Порог задаётся как допустимое падение nDCG, потому что
// случайный шум на наборе из 100 вопросов даёт доли процента сам по себе;
// ноль делает гейт непроходимым.
func Compare(base, candidate Report, maxNDCGDrop float64) []string {
	var violations []string
	if candidate.Cases != base.Cases {
		violations = append(violations, fmt.Sprintf(
			"разный размер набора: база %d вопросов, кандидат %d — сравнивать нельзя",
			base.Cases, candidate.Cases))
	}
	if candidate.NDCG < base.NDCG-maxNDCGDrop {
		violations = append(violations, fmt.Sprintf(
			"nDCG@%d упал: %.4f → %.4f (допустимо падение %.4f)",
			DefaultK, base.NDCG, candidate.NDCG, maxNDCGDrop))
	}
	// Ложный ответ на вопрос-ловушку — не «шум», а прямой вред: система
	// уверенно предлагает неверное вместо честного «не знаю».
	if candidate.FalsePositives > base.FalsePositives {
		violations = append(violations, fmt.Sprintf(
			"ложных ответов на вопросы-ловушки стало больше: %d → %d",
			base.FalsePositives, candidate.FalsePositives))
	}
	for kind, baseKind := range base.ByKind {
		candKind, ok := candidate.ByKind[kind]
		if !ok {
			violations = append(violations, fmt.Sprintf(
				"категория вопросов %q исчезла из отчёта кандидата", kind))
			continue
		}
		if candKind.Cases != baseKind.Cases {
			violations = append(violations, fmt.Sprintf(
				"категория %q: разное число вопросов (%d против %d)",
				kind, baseKind.Cases, candKind.Cases))
			continue
		}
		if candKind.NDCG < baseKind.NDCG-maxNDCGDrop {
			violations = append(violations, fmt.Sprintf(
				"категория %q: nDCG упал %.4f → %.4f", kind, baseKind.NDCG, candKind.NDCG))
		}
	}
	return violations
}
