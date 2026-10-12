package evaluation

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// reportFrom собирает отчёт из готовых результатов вопросов.
func reportFrom(results ...CaseResult) Report {
	return Aggregate(results)
}

func TestGatePassesOnEqualScores(t *testing.T) {
	// Ровно те же вопросы с теми же метриками в обоих режимах: гибрид
	// ничего не изменил, и §9 такой случай пропускает — «не ниже», а не
	// «строго выше».
	base := reportFrom(
		CaseResult{CaseID: "q1", Kind: "single_hop", NDCG: 0.8, Recall: 1, MRR: 1},
		CaseResult{CaseID: "q2", Kind: "multi_hop", NDCG: 0.4, Recall: 0.5, MRR: 0.5},
	)
	cand := reportFrom(
		CaseResult{CaseID: "q1", Kind: "single_hop", NDCG: 0.8, Recall: 1, MRR: 1},
		CaseResult{CaseID: "q2", Kind: "multi_hop", NDCG: 0.4, Recall: 0.5, MRR: 0.5},
	)
	gate := CompareModes(base, cand, GateOptions{})
	if !gate.Passed {
		t.Fatalf("равные метрики не прошли гейт: %v", gate.Failures)
	}
	if err := gate.Err(); err != nil {
		t.Fatalf("Err() вернул ошибку при равных метриках: %v", err)
	}
}

func TestGateFailsWhenHybridWorseThanBM25(t *testing.T) {
	base := reportFrom(CaseResult{CaseID: "q1", Kind: "single_hop", NDCG: 0.8})
	cand := reportFrom(CaseResult{CaseID: "q1", Kind: "single_hop", NDCG: 0.5})

	gate := CompareModes(base, cand, GateOptions{})
	if gate.Passed {
		t.Fatal("гибрид хуже базы, но гейт прошёл — ворота §9 не работают")
	}
	err := gate.Err()
	if err == nil {
		t.Fatal("Err() вернул nil при провале гейта")
	}
	var gateErr *GateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("ожидалась *GateError, получено %T", err)
	}
	if gateErr.Gate.NDCG.RegressedCases != 1 {
		t.Fatalf("в ошибке %d просевших вопросов, ожидался 1", gateErr.Gate.NDCG.RegressedCases)
	}
}

func TestGateStrictBoundary(t *testing.T) {
	// Граница §9 — «не ниже». Ровно ноль дельты проходит; любое падение,
	// даже на четвёртом знаке, закрывает ворота. Специального допуска нет:
	// см. комментарий к NDCGTolerance.
	cases := []struct {
		name     string
		candNDCG float64
		wantPass bool
	}{
		{"ровно равно", 0.8, true},
		{"лучше на 0.0001", 0.8001, true},
		{"хуже на 0.0001", 0.7999, false},
		{"хуже на 0.001", 0.799, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := CompareModes(
				reportFrom(CaseResult{CaseID: "q1", Kind: "single_hop", NDCG: 0.8}),
				reportFrom(CaseResult{CaseID: "q1", Kind: "single_hop", NDCG: tc.candNDCG}),
				GateOptions{},
			)
			if gate.Passed != tc.wantPass {
				t.Fatalf("nDCG %.4f против 0.8000: passed=%v, ожидалось %v (%v)",
					tc.candNDCG, gate.Passed, tc.wantPass, gate.Failures)
			}
		})
	}
}

func TestGateToleranceIsZeroByDefault(t *testing.T) {
	// Константа обязана быть нулём: ненулевой допуск — это гейт, который
	// перестаёт ловить регрессию. Тест фиксирует именно ноль, а не
	// «достаточно малое число», чтобы следующий редактор увидел цену правки.
	if NDCGTolerance != 0 {
		t.Fatalf("NDCGTolerance = %v, ожидался 0: ненулевой допуск отключает гейт", NDCGTolerance)
	}
	// Fallback в GateOptions тоже обязан давать ноль при незаданном поле.
	gate := CompareModes(
		reportFrom(CaseResult{CaseID: "q1", NDCG: 0.8}),
		reportFrom(CaseResult{CaseID: "q1", NDCG: 0.79}),
		GateOptions{},
	)
	if gate.Tolerance != 0 {
		t.Fatalf("допуск прогона %.4f, ожидался 0", gate.Tolerance)
	}
	if gate.Passed {
		t.Fatal("падение на 0.01 прошло при нулевом допуске")
	}
}

func TestGateHonoursExplicitTolerance(t *testing.T) {
	// Явный допуск — осознанное решение вызывающего кода, а не значение по
	// умолчанию: падение внутри него гейт пропускает.
	base := reportFrom(CaseResult{CaseID: "q1", NDCG: 0.8})
	cand := reportFrom(CaseResult{CaseID: "q1", NDCG: 0.795})

	if gate := CompareModes(base, cand, GateOptions{}); gate.Passed {
		t.Fatal("без допуска падение 0.005 должно закрывать ворота")
	}
	gate := CompareModes(base, cand, GateOptions{Tolerance: 0.01})
	if !gate.Passed {
		t.Fatalf("падение 0.005 внутри допуска 0.01 должно проходить: %v", gate.Failures)
	}
	// Выход за допуск по-прежнему ловится.
	if CompareModes(base, reportFrom(CaseResult{CaseID: "q1", NDCG: 0.7}),
		GateOptions{Tolerance: 0.01}).Passed {
		t.Fatal("падение 0.1 внутри допуска 0.01 прошло")
	}
}

func TestGateReportsDeltaAndRegressedCases(t *testing.T) {
	base := reportFrom(
		CaseResult{CaseID: "good", Kind: "single_hop", NDCG: 0.9, Query: "сколько стоит"},
		CaseResult{CaseID: "bad1", Kind: "multi_hop", NDCG: 0.7, Query: "кто владелец"},
		CaseResult{CaseID: "bad2", Kind: "temporal", NDCG: 0.6, Query: "когда ввели"},
		CaseResult{CaseID: "bad3", Kind: "single_hop", NDCG: 0.5, Query: "какой диаметр"},
	)
	cand := reportFrom(
		CaseResult{CaseID: "good", Kind: "single_hop", NDCG: 0.9, Query: "сколько стоит"},
		CaseResult{CaseID: "bad1", Kind: "multi_hop", NDCG: 0.4, Query: "кто владелец"},
		CaseResult{CaseID: "bad2", Kind: "temporal", NDCG: 0.5, Query: "когда ввели"},
		CaseResult{CaseID: "bad3", Kind: "single_hop", NDCG: 0.55, Query: "какой диаметр"},
	)

	gate := CompareModes(base, cand, GateOptions{})
	if gate.Passed {
		t.Fatal("гибрид хуже базы, но гейт прошёл")
	}
	// Два вопроса просели (bad1: -0.30, bad2: -0.10), один улучшился
	// (bad3: +0.05), один не изменился (good: 0).
	if gate.NDCG.RegressedCases != 2 {
		t.Fatalf("просевших %d, ожидалось 2", gate.NDCG.RegressedCases)
	}
	if gate.NDCG.ImprovedCases != 1 {
		t.Fatalf("улучшившихся %d, ожидался 1", gate.NDCG.ImprovedCases)
	}
	wantDelta := cand.NDCG - base.NDCG
	if math.Abs(gate.NDCG.Delta-wantDelta) > 1e-12 {
		t.Fatalf("дельта %.6f, ожидалась %.6f", gate.NDCG.Delta, wantDelta)
	}

	// Худшая просадка идёт первой — её и чинят первой.
	if len(gate.Regressions) != 2 {
		t.Fatalf("в списке %d вопросов, ожидалось 2", len(gate.Regressions))
	}
	if gate.Regressions[0].CaseID != "bad1" {
		t.Fatalf("первой идёт %q, ожидался bad1 (просадка -0.30)", gate.Regressions[0].CaseID)
	}
	if gate.Regressions[1].CaseID != "bad2" {
		t.Fatalf("второй идёт %q, ожидался bad2", gate.Regressions[1].CaseID)
	}
	if gate.Regressions[0].Query != "кто владелец" {
		t.Fatalf("в просадке потерялся текст вопроса: %q", gate.Regressions[0].Query)
	}
}

func TestGateErrorNamesMetricValuesDeltaAndCases(t *testing.T) {
	base := reportFrom(
		CaseResult{CaseID: "gk-007", Kind: "multi_hop", NDCG: 0.9, Query: "кто владелец лицензии?"},
		CaseResult{CaseID: "gk-008", Kind: "single_hop", NDCG: 0.6, Query: "какой цена?"},
	)
	cand := reportFrom(
		CaseResult{CaseID: "gk-007", Kind: "multi_hop", NDCG: 0.3, Query: "кто владелец лицензии?"},
		CaseResult{CaseID: "gk-008", Kind: "single_hop", NDCG: 0.5, Query: "какой цена?"},
	)

	err := CompareModes(base, cand, GateOptions{}).Err()
	if err == nil {
		t.Fatal("ожидалась ошибка гейта")
	}
	msg := err.Error()
	// Метрика, оба значения, дельта, число просевших, худшие вопросы.
	// Средние: база (0.9+0.6)/2 = 0.75, кандидат (0.3+0.5)/2 = 0.40.
	for _, want := range []string{
		"nDCG@10", "0.4000", "0.7500", "-0.3500",
		"просело 2 вопросов", "gk-007", "gk-008", "кто владелец лицензии?",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("в ошибке гейта нет %q:\n%s", want, msg)
		}
	}
}

func TestGateReportsOnlyWorstRegressionsInError(t *testing.T) {
	results := make([]CaseResult, 0, maxReportedRegressions+5)
	baseResults := make([]CaseResult, 0, maxReportedRegressions+5)
	for i := range maxReportedRegressions + 5 {
		id := string(rune('a' + i))
		baseResults = append(baseResults, CaseResult{CaseID: id, NDCG: 0.9})
		// Просадка растёт с индексом: худший — последний.
		results = append(results, CaseResult{CaseID: id, NDCG: 0.5 - float64(i)*0.01})
	}

	err := CompareModes(reportFrom(baseResults...), reportFrom(results...), GateOptions{}).Err()
	if err == nil {
		t.Fatal("ожидалась ошибка гейта")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ещё 5") {
		t.Fatalf("в ошибке нет указания на усечение списка:\n%s", msg)
	}
	// Худший (последний по индексу) обязан быть назван, лучший из худших —
	// тоже, а самый слабый из напечатанных — нет.
	if !strings.Contains(msg, string(rune('a'+maxReportedRegressions-1))) {
		t.Fatalf("в ошибке нет пятой худшей просадки:\n%s", msg)
	}
	if strings.Contains(msg, "  a [") {
		t.Fatalf("в ошибку попала шестая просадка — список должен усекаться:\n%s", msg)
	}
}

func TestGateFlagsMismatchedSets(t *testing.T) {
	// Разные вопросы дают разный средний балл независимо от ранжирования:
	// сравнивать их бессмысленно, и это ошибка прогона, а не «почти нулевая
	// дельта».
	base := reportFrom(
		CaseResult{CaseID: "q1", NDCG: 0.9},
		CaseResult{CaseID: "q2", NDCG: 0.9},
	)
	cand := reportFrom(CaseResult{CaseID: "q1", NDCG: 0.95})

	gate := CompareModes(base, cand, GateOptions{})
	if gate.Passed {
		t.Fatal("разные наборы вопросов прошли гейт — сравнение бессмысленно")
	}
	if !strings.Contains(gate.Err().Error(), "наборы разошлись") {
		t.Fatalf("в ошибке нет упоминания расхождения наборов:\n%s", gate.Err().Error())
	}
}

func TestGateSurvivesMismatchedSetsWhenCandidateLooksBetter(t *testing.T) {
	// Обратный случай: гибрид «выглядит лучше», но на других вопросах.
	// Если бы правило было «candidate >= base», такое расхождение прошло бы
	// и замаскировало бы подмену набора.
	base := reportFrom(
		CaseResult{CaseID: "q1", NDCG: 0.2},
		CaseResult{CaseID: "q2", NDCG: 0.2},
	)
	cand := reportFrom(CaseResult{CaseID: "q1", NDCG: 0.2})
	if CompareModes(base, cand, GateOptions{}).Passed {
		t.Fatal("подмена набора прошла гейт")
	}
}

func TestGateTreatsNaNAsFailure(t *testing.T) {
	// Любое сравнение с NaN даёт false. Без явной проверки провалившийся
	// расчёт прошёл бы как «не хуже базы» — худший вид поломки, потому что
	// он выглядит как успех.
	nan := math.NaN()
	gate := CompareModes(
		reportFrom(CaseResult{CaseID: "q1", NDCG: 0.8}),
		reportFrom(CaseResult{CaseID: "q1", NDCG: nan}),
		GateOptions{},
	)
	if gate.Passed {
		t.Fatal("NaN в метрике прошёл гейт")
	}
	if !strings.Contains(gate.Err().Error(), "нечисло") {
		t.Fatalf("в ошибке нет упоминания нечисловой метрики:\n%s", gate.Err().Error())
	}
}

func TestGateIsDeterministic(t *testing.T) {
	baseResults := []CaseResult{
		{CaseID: "b", Kind: "multi_hop", NDCG: 0.9},
		{CaseID: "a", Kind: "single_hop", NDCG: 0.9},
		{CaseID: "c", Kind: "temporal", NDCG: 0.9},
	}
	candResults := []CaseResult{
		{CaseID: "b", Kind: "multi_hop", NDCG: 0.1},
		{CaseID: "a", Kind: "single_hop", NDCG: 0.1},
		{CaseID: "c", Kind: "temporal", NDCG: 0.1},
	}

	first := CompareModes(reportFrom(baseResults...), reportFrom(candResults...), GateOptions{})
	for range 20 {
		got := CompareModes(reportFrom(baseResults...), reportFrom(candResults...), GateOptions{})
		if len(got.Regressions) != len(first.Regressions) {
			t.Fatalf("разное число просадок: %d и %d", len(got.Regressions), len(first.Regressions))
		}
		for i := range first.Regressions {
			if got.Regressions[i].CaseID != first.Regressions[i].CaseID {
				t.Fatalf("порядок просадок изменился: %v против %v", got.Regressions, first.Regressions)
			}
		}
	}
}

func TestGateReportsKindRegressionsWithoutBlocking(t *testing.T) {
	// Просадка внутри категории — диагностика: §9 требует одно число, но
	// рост на другой категории не должен прятать провал на multi_hop.
	// Суммарный nDCG растёт (0.6333 → 0.6667), multi_hop при этом падает
	// вдвое — ровно тот случай, который суммарная цифра прячет.
	base := reportFrom(
		CaseResult{CaseID: "q1", Kind: "single_hop", NDCG: 0.9},
		CaseResult{CaseID: "q2", Kind: "multi_hop", NDCG: 0.5},
		CaseResult{CaseID: "q3", Kind: "single_hop", NDCG: 0.5},
	)
	cand := reportFrom(
		CaseResult{CaseID: "q1", Kind: "single_hop", NDCG: 1.0},
		CaseResult{CaseID: "q2", Kind: "multi_hop", NDCG: 0.1},
		CaseResult{CaseID: "q3", Kind: "single_hop", NDCG: 0.9},
	)

	gate := CompareModes(base, cand, GateOptions{})
	if !gate.Passed {
		t.Fatalf("суммарный nDCG не упал, гейт закрыт напрасно: %v", gate.Failures)
	}
	if len(gate.KindRegressions) != 1 {
		t.Fatalf("в отчёте %d просадок по категориям, ожидалась 1", len(gate.KindRegressions))
	}
	kd := gate.KindRegressions[0]
	if kd.Kind != "multi_hop" {
		t.Fatalf("просела категория %q, ожидалась multi_hop", kd.Kind)
	}
	if math.Abs(kd.Delta-(-0.4)) > 1e-9 {
		t.Fatalf("дельта категории %.4f, ожидалась -0.4000", kd.Delta)
	}
}

func TestParseMode(t *testing.T) {
	ok := map[string]Mode{
		"hybrid":    ModeHybrid,
		"rrf":       ModeHybrid,
		"HYBRID":    ModeHybrid,
		" bm25 ":    ModeBM25Only,
		"bm25_only": ModeBM25Only,
		"lexical":   ModeBM25Only,
	}
	for raw, want := range ok {
		got, err := ParseMode(raw)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseMode(%q) = %q, ожидался %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "vector", "bm25+knn", "нет"} {
		if _, err := ParseMode(raw); err == nil {
			t.Fatalf("ParseMode(%q) не вернул ошибку", raw)
		}
	}
}

func TestModeValidity(t *testing.T) {
	if !ModeHybrid.Valid() || !ModeBM25Only.Valid() {
		t.Fatal(" штатные режимы объявлены невалидными")
	}
	if Mode("vector_only").Valid() {
		t.Fatal("неизвестный режим прошёл проверку")
	}
	if ModeHybrid.String() != "hybrid" || ModeBM25Only.String() != "bm25_only" {
		t.Fatal("String() режима не совпадает с его значением")
	}
}

func TestGateUsesDefaultKFromOptions(t *testing.T) {
	// Срез по умолчанию — 10, как в воротах §9; при K=1 падение за пределами
	// первого документа не влияет на метрику, и гейт это не ловит.
	base := reportFrom(
		CaseResult{CaseID: "q1", NDCG: 0.5},
		CaseResult{CaseID: "q2", NDCG: 0.5},
	)
	cand := reportFrom(
		CaseResult{CaseID: "q1", NDCG: 0.9},
		CaseResult{CaseID: "q2", NDCG: 0.1},
	)
	gate := CompareModes(base, cand, GateOptions{K: 1})
	if gate.K != 1 {
		t.Fatalf("K = %d, ожидался 1", gate.K)
	}
	// Средние отчётов посчитаны заранее с K=10, поэтому проверяем только то,
	// что срез доехал до вердикта и попал в имя метрики.
	if !strings.HasPrefix(gate.NDCG.Name, "nDCG@1") {
		t.Fatalf("метрика названа %q, ожидался срез из K", gate.NDCG.Name)
	}
}

func TestTruncateKeepsTextReadable(t *testing.T) {
	if got := truncate("короткий вопрос", 90); got != "короткий вопрос" {
		t.Fatalf("короткий текст изменён: %q", got)
	}
	// Многострочный кириллический вопрос не должен превратиться в мусор из
	// половины руны.
	long := strings.Repeat("я", 200)
	got := truncate(long, 90)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("длинный текст не обрезан: %q", got)
	}
	if !strings.HasPrefix(long[:], strings.TrimSuffix(got, "…")) {
		t.Fatalf("обрезанный текст не является префиксом исходного: %q", got)
	}
}
