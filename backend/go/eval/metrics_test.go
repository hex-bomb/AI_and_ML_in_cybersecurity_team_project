package evaluation

import (
	"math"
	"strings"
	"testing"
)

func hits(ids ...string) []Hit {
	out := make([]Hit, len(ids))
	for i, id := range ids {
		out[i] = Hit{DocID: id, Rank: i}
	}
	return out
}

func TestNDCGPerfectRankingIsOne(t *testing.T) {
	relevant := map[string]int{"a": 3, "b": 2}
	got := NDCGAtK(hits("a", "b", "c"), relevant, 3, 10)
	if math.Abs(got-1) > 1e-9 {
		t.Fatalf("идеальная выдача даёт %.6f, ожидалась 1", got)
	}
}

func TestNDCGReversedIsBelowOne(t *testing.T) {
	relevant := map[string]int{"a": 3, "b": 2}
	forward := NDCGAtK(hits("a", "b"), relevant, 3, 10)
	reverse := NDCGAtK(hits("b", "a"), relevant, 3, 10)
	if reverse >= forward {
		t.Fatalf("обратный порядок (%.6f) не хуже прямого (%.6f)", reverse, forward)
	}
}

func TestNDCGEmptyHitsIsZero(t *testing.T) {
	// Пустой ответ обязан портить средний балл: иначе система выигрывает
	// набор, просто ничего не возвращая.
	if got := NDCGAtK(nil, map[string]int{"a": 3}, 3, 10); got != 0 {
		t.Fatalf("пустая выдача даёт %.6f, ожидался 0", got)
	}
}

func TestNDCGNormalizesByNumberOfRelevantDocs(t *testing.T) {
	// Один релевантный документ на первой позиции — идеальный результат
	// независимо от длины выдачи.
	relevant := map[string]int{"only": 3}
	if got := NDCGAtK(hits("only", "x", "y", "z"), relevant, 3, 10); math.Abs(got-1) > 1e-9 {
		t.Fatalf("один релевантный наверху даёт %.6f, ожидалась 1", got)
	}
}

func TestNDCGRespectsK(t *testing.T) {
	relevant := map[string]int{"a": 3}
	// Второй документ релевантен, но стоит за срезом k=1.
	if got := NDCGAtK(hits("x", "a"), relevant, 3, 1); got != 0 {
		t.Fatalf("попадание за срезом повлияло на nDCG@1: %.6f", got)
	}
}

func TestRecallCountsOnlyAboveThreshold(t *testing.T) {
	// «weak» (оценка 1) не тянет знаменатель при minGrade=2: частичная
	// релевантность не должна стоить столько же, сколько этал��ный ответ.
	relevant := map[string]int{"a": 3, "b": 2, "weak": 1}
	if got := RecallAtK(hits("a"), relevant, 2, 50); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("Recall при minGrade=2 = %.4f, ожидалось 0.5", got)
	}
	if got := RecallAtK(hits("a", "b", "weak"), relevant, 1, 50); math.Abs(got-1) > 1e-9 {
		t.Fatalf("Recall при minGrade=1 = %.4f, ожидалась 1", got)
	}
}

func TestRecallEmptyHitsIsZero(t *testing.T) {
	if got := RecallAtK(nil, map[string]int{"a": 3}, 1, 50); got != 0 {
		t.Fatalf("пустая выдача даёт Recall %.4f, ожидался 0", got)
	}
}

func TestMRRFirstRelevant(t *testing.T) {
	relevant := map[string]int{"b": 3}
	if got := MRR(hits("a", "b"), relevant, 1); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("MRR = %.4f, ожидалось 0.5", got)
	}
	if got := MRR(hits("a"), relevant, 1); got != 0 {
		t.Fatalf("промах даёт MRR %.4f, ожидался 0", got)
	}
}

func TestTrapQuestionCountsFalsePositive(t *testing.T) {
	trap := Case{ID: "t1", Query: "чего не существует", ExpectedEmpty: true}
	results := []CaseResult{
		ScoreCase(trap, hits("doc"), 10, 50, 1),
		ScoreCase(trap, nil, 10, 50, 1),
	}
	report := Aggregate(results)
	if report.TrapCases != 2 || report.FalsePositives != 1 {
		t.Fatalf("ловушек %d, ложных ответов %d; ожидалось 2 и 1",
			report.TrapCases, report.FalsePositives)
	}
}

func TestTrapsDoNotPolluteAverages(t *testing.T) {
	// Ловушка с идеально пустой выдачей не должна ронять средние:
	// усреднять её с обычными вопросами бессмысленно.
	results := []CaseResult{
		{CaseID: "q1", Kind: "single_hop", NDCG: 1, Recall: 1, MRR: 1},
		{CaseID: "t1", Kind: "trap", Trap: true, NDCG: 0, Recall: 0, MRR: 0},
	}
	report := Aggregate(results)
	if report.NDCG != 1 {
		t.Fatalf("средний nDCG %.4f, ожидалась 1 — ловушка исказила среднее", report.NDCG)
	}
}

func TestByTagBreaksDownPerDomain(t *testing.T) {
	report := Aggregate([]CaseResult{
		{CaseID: "q1", Kind: "single_hop", Tags: []string{"welding"}, NDCG: 0.9},
		{CaseID: "q2", Kind: "single_hop", Tags: []string{"welding"}, NDCG: 0.3},
		{CaseID: "q3", Kind: "single_hop", Tags: []string{"service"}, NDCG: 1},
	})
	if report.ByTag["welding"].Cases != 2 {
		t.Fatalf("в метке welding %d вопросов, ожидалось 2", report.ByTag["welding"].Cases)
	}
	if math.Abs(report.ByTag["welding"].NDCG-0.6) > 1e-9 {
		t.Fatalf("nDCG метки welding %.4f, ожидалось 0.6", report.ByTag["welding"].NDCG)
	}
}

func TestCompareFailsOnNewFalsePositives(t *testing.T) {
	base := Aggregate([]CaseResult{{CaseID: "t1", Kind: "trap", Relevant: 0}})
	candidate := Aggregate([]CaseResult{{CaseID: "t1", Kind: "trap", Relevant: 0, FalsePositive: true}})
	if violations := Compare(base, candidate, 0.01); len(violations) == 0 {
		t.Fatal("ложный ответ на ловушку не замечен")
	}
}

func TestAggregateAveragesPerCaseNotPerDocument(t *testing.T) {
	results := []CaseResult{
		{CaseID: "q1", Kind: "single_hop", NDCG: 1, Recall: 1, MRR: 1},
		{CaseID: "q2", Kind: "single_hop", NDCG: 0, Recall: 0, MRR: 0},
		{CaseID: "q3", Kind: "multi_hop", NDCG: 0.5, Recall: 0.5, MRR: 0.5},
	}
	report := Aggregate(results)
	if report.Cases != 3 {
		t.Fatalf("в отчёте %d вопросов, ожидалось 3", report.Cases)
	}
	if math.Abs(report.NDCG-0.5) > 1e-9 {
		t.Fatalf("средний nDCG %.4f, ожидалось 0.5", report.NDCG)
	}
	if got := report.ByKind["single_hop"].NDCG; math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("nDCG категории single_hop %.4f, ожидалось 0.5", got)
	}
	if report.ByKind["multi_hop"].Cases != 1 {
		t.Fatalf("в категории multi_hop %d вопросов, ожидался 1", report.ByKind["multi_hop"].Cases)
	}
}

func TestAggregateOrdersDetailsWorstFirst(t *testing.T) {
	report := Aggregate([]CaseResult{
		{CaseID: "good", NDCG: 0.9},
		{CaseID: "bad", NDCG: 0.1},
	})
	if report.Details[0].CaseID != "bad" {
		t.Fatalf("первым идёт %q, ожидался худший вопрос", report.Details[0].CaseID)
	}
}

func TestAggregateDoesNotAliasInputSlice(t *testing.T) {
	results := []CaseResult{{CaseID: "q1", NDCG: 0.5}}
	report := Aggregate(results)
	report.Details[0].CaseID = "mutated"
	if results[0].CaseID != "q1" {
		t.Fatal("Aggregate переписал входной срез — вызывающий код изменился из ниоткуда")
	}
}

func TestLoadGoldenParsesJSONLAndSkipsComments(t *testing.T) {
	input := strings.Join([]string{
		"# комментарий",
		`{"id":"q1","query":"как развернуть","relevant":[{"doc_id":"d1","grade":3}],"kind":"single_hop"}`,
		``,
		`{"id":"q2","query":"кто владелец","relevant":[{"doc_id":"d2","grade":2}],"kind":"multi_hop"}`,
	}, "\n")

	cases, err := LoadGolden(strings.NewReader(input))
	if err != nil {
		t.Fatalf("разбор набора: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("разобрано %d вопросов, ожидалось 2", len(cases))
	}
	if cases[0].Kind != "single_hop" {
		t.Fatalf("категория %q, ожидалась single_hop", cases[0].Kind)
	}
}

func TestLoadGoldenRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"пустой набор":       "\n# только комментарии\n",
		"битый JSON":         `{"id":"q1",`,
		"вопрос без запроса": `{"id":"q1","query":"","relevant":[{"doc_id":"d","grade":1}]}`,
		"без разметки":       `{"id":"q1","query":"что","relevant":[]}`,
		"нулевая оценка":     `{"id":"q1","query":"что","relevant":[{"doc_id":"d","grade":0}]}`,
		"дубль документа":    `{"id":"q1","query":"что","relevant":[{"doc_id":"d","grade":1},{"doc_id":"d","grade":2}]}`,
		"без идентификатора": `{"query":"что","relevant":[{"doc_id":"d","grade":1}]}`,
		"ловушка с разметкой": `{"id":"t1","query":"чего нет","expected_empty":true,` +
			`"relevant":[{"doc_id":"d","grade":1}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadGolden(strings.NewReader(input)); err == nil {
				t.Fatal("ожидалась ошибка разбора, её не было")
			}
		})
	}
}

func TestRoundTripGolden(t *testing.T) {
	original := []Case{{
		ID: "q1", Query: "как развернуть", Kind: "single_hop",
		Relevant: []Scored{{DocID: "d1", Grade: 3}},
	}}
	var buf strings.Builder
	if err := WriteGolden(&buf, original); err != nil {
		t.Fatalf("запись набора: %v", err)
	}
	parsed, err := LoadGolden(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("разбор набора: %v", err)
	}
	if len(parsed) != 1 || parsed[0].ID != "q1" || parsed[0].Relevant[0].Grade != 3 {
		t.Fatalf("набор не пережил круг: %+v", parsed)
	}
}

func TestRunnerRetrieverErrorCountsAsFailureNotSkip(t *testing.T) {
	cases := []Case{{
		ID: "q1", Query: "что", Relevant: []Scored{{DocID: "d1", Grade: 3}},
	}}
	report := Runner{}.Run(cases, func(string, int) ([]Hit, error) {
		return nil, assertError{}
	})
	if report.Cases != 1 {
		t.Fatalf("в отчёте %d вопросов, ожидался 1 — прогон не должен терять вопросы", report.Cases)
	}
	if report.NDCG != 0 {
		t.Fatalf("упавший ретривер дал nDCG %.4f, ожидался 0", report.NDCG)
	}
}

func TestRunnerDropsHitsWithoutIDs(t *testing.T) {
	cases := []Case{{
		ID: "q1", Query: "что", Relevant: []Scored{{DocID: "d1", Grade: 3}},
	}}
	report := Runner{}.Run(cases, func(string, int) ([]Hit, error) {
		return []Hit{{DocID: ""}, {DocID: "d1"}}, nil
	})
	if report.Details[0].Hits != 1 {
		t.Fatalf("в выдаче %d документов, ожидался 1 — пустые id должны отбрасываться", report.Details[0].Hits)
	}
}

func TestRunnerWidensRecallToK(t *testing.T) {
	var requestedLimit int
	cases := []Case{{
		ID: "q1", Query: "что", Relevant: []Scored{{DocID: "d1", Grade: 3}},
	}}
	Runner{K: 10, RecallK: 5}.Run(cases, func(_ string, limit int) ([]Hit, error) {
		requestedLimit = limit
		return hits("d1"), nil
	})
	if requestedLimit != 10 {
		t.Fatalf("ретриверу запрошен лимит %d, ожидалось не меньше k=10", requestedLimit)
	}
}

func TestCompareDetectsRegression(t *testing.T) {
	base := Aggregate([]CaseResult{
		{CaseID: "q1", Kind: "single_hop", NDCG: 0.8},
	})
	candidate := Aggregate([]CaseResult{
		{CaseID: "q1", Kind: "single_hop", NDCG: 0.5},
	})
	violations := Compare(base, candidate, 0.01)
	if len(violations) == 0 {
		t.Fatal("падение nDCG не замечено — гейт пропустил бы регрессию")
	}
}

func TestCompareAcceptsNoiseWithinThreshold(t *testing.T) {
	base := Aggregate([]CaseResult{{CaseID: "q1", Kind: "single_hop", NDCG: 0.8}})
	candidate := Aggregate([]CaseResult{{CaseID: "q1", Kind: "single_hop", NDCG: 0.795}})
	if violations := Compare(base, candidate, 0.01); len(violations) != 0 {
		t.Fatalf("шум в допуске считается нарушением: %v", violations)
	}
}

func TestCompareRejectsDifferentSetSizes(t *testing.T) {
	base := Aggregate([]CaseResult{{CaseID: "q1", Kind: "single_hop", NDCG: 0.8}})
	candidate := Aggregate([]CaseResult{
		{CaseID: "q1", Kind: "single_hop", NDCG: 0.9},
		{CaseID: "q2", Kind: "single_hop", NDCG: 0.9},
	})
	if violations := Compare(base, candidate, 0.01); len(violations) == 0 {
		t.Fatal("разный размер набора не замечен — сравнение бессмысленно")
	}
}

type assertError struct{}

func (assertError) Error() string { return "индекс недоступен" }
