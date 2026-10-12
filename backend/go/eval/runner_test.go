package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSearch — заглушка ретривера: отдаёт заранее заданную выдачу на
// вопрос. Ключ — «режим/вопрос», чтобы один прогон мог дать разные выдачи
// в разных режимах.
type fakeSearch struct {
	results map[string][]Hit
	errs    map[string]error
	calls   int
	// limits фиксирует, какой срез просили: ретривер, которому заказали
	// top-10, не может честно посчитать Recall@50.
	limits []int
}

func (f *fakeSearch) search(_ context.Context, q Query, mode Mode, limit int) ([]Hit, error) {
	f.calls++
	f.limits = append(f.limits, limit)
	key := string(mode) + "/" + q.CaseID
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	return f.results[key], nil
}

func newFakeSearch(pairs map[string][]Hit) *fakeSearch {
	return &fakeSearch{results: pairs}
}

// memGolden собирает маленький набор в памяти — чтобы тест гейта не
// зависел от файла на диске и его правок.
func memGolden(t *testing.T) []Case {
	t.Helper()
	input := strings.Join([]string{
		`{"id":"q-exact","query":"какой диаметр","relevant":[{"doc_id":"d1","grade":3}],"kind":"single_hop"}`,
		`{"id":"q-two","query":"цена и срок","relevant":[{"doc_id":"d2","grade":3},{"doc_id":"d3","grade":2}],"kind":"multi_hop"}`,
		`{"id":"q-miss","query":"чего нет","relevant":[{"doc_id":"d4","grade":3}],"kind":"temporal"}`,
		`{"id":"q-trap","query":"несуществующее","relevant":[],"kind":"single_hop","expected_empty":true}`,
	}, "\n")
	cases, err := LoadGolden(strings.NewReader(input))
	if err != nil {
		t.Fatalf("разбор тестового набора: %v", err)
	}
	return cases
}

func resultByID(t *testing.T, report Report, id string) CaseResult {
	t.Helper()
	for _, r := range report.Details {
		if r.CaseID == id {
			return r
		}
	}
	t.Fatalf("вопрос %q отсутствует в отчёте", id)
	return CaseResult{}
}

func TestRunScoresEveryCasePerMode(t *testing.T) {
	cases := memGolden(t)
	fake := newFakeSearch(map[string][]Hit{
		"bm25_only/q-exact": {{DocID: "d1"}, {DocID: "noise"}},
		"bm25_only/q-two":   {{DocID: "noise"}, {DocID: "d2"}},
		"bm25_only/q-miss":  nil,
		"bm25_only/q-trap":  nil,
		"hybrid/q-exact":    {{DocID: "d1"}},
		"hybrid/q-two":      {{DocID: "d2"}, {DocID: "d3"}},
		"hybrid/q-miss":     nil,
		"hybrid/q-trap":     nil,
	})

	result, err := Run(context.Background(), cases, fake.search, RunnerConfig{})
	if err != nil {
		t.Fatalf("прогон: %v", err)
	}
	if len(result.Modes) != 2 {
		t.Fatalf("прогнано %d режимов, ожидалось 2", len(result.Modes))
	}
	// Порядок режимов по умолчанию: сначала база, потом кандидат.
	if result.Modes[0].Mode != ModeBM25Only || result.Modes[1].Mode != ModeHybrid {
		t.Fatalf("порядок режимов %v, ожидался [bm25_only hybrid]", result.Modes)
	}

	base, ok := result.Report(ModeBM25Only)
	if !ok {
		t.Fatal("нет отчёта по базовому режиму")
	}
	if base.Cases != 4 {
		t.Fatalf("в базовом отчёте %d вопросов, ожидалось 4", base.Cases)
	}
	if base.TrapCases != 1 {
		t.Fatalf("ловушек %d, ожидалась 1", base.TrapCases)
	}
	if base.FalsePositives != 0 {
		t.Fatalf("ложных ответов %d, ожидалось 0: ловушка без выдачи — это успех", base.FalsePositives)
	}
	// Промах (q-miss) обязан портить средний балл нулём, иначе система
	// выигрывает набор, просто ничего не возвращая.
	if got := resultByID(t, base, "q-miss"); got.NDCG != 0 || got.Recall != 0 || got.MRR != 0 {
		t.Fatalf("вопрос без выдачи получил nDCG %.4f / Recall %.4f / MRR %.4f, "+
			"ожидались нули", got.NDCG, got.Recall, got.MRR)
	}
	// Ловушка не должна попадать в средние.
	for _, d := range base.Details {
		if d.Trap && d.NDCG != 0 {
			t.Fatalf("ловушка %s повлияла на nDCG: %.4f", d.CaseID, d.NDCG)
		}
	}
	// Первым в Details идёт худший вопрос (так Aggregate его сортирует):
	// в отчёте регрессия обязана читаться раньше удачи.
	if base.Details[0].CaseID != "q-miss" {
		t.Fatalf("первым в Details идёт %q, ожидался худший вопрос q-miss",
			base.Details[0].CaseID)
	}
}

func TestRunGatePassesWhenHybridBeatsBM25(t *testing.T) {
	cases := memGolden(t)
	fake := newFakeSearch(map[string][]Hit{
		// База знает один документ из двух размеченных.
		"bm25_only/q-exact": {{DocID: "d1"}},
		"bm25_only/q-two":   {{DocID: "d3"}},
		"bm25_only/q-miss":  nil,
		"bm25_only/q-trap":  nil,
		// Гибрид находит оба и ставит эталонный документ первым.
		"hybrid/q-exact": {{DocID: "d1"}},
		"hybrid/q-two":   {{DocID: "d2"}, {DocID: "d3"}},
		"hybrid/q-miss":  nil,
		"hybrid/q-trap":  nil,
	})

	result, err := Run(context.Background(), cases, fake.search, RunnerConfig{})
	if err != nil {
		t.Fatalf("прогон: %v", err)
	}
	if result.Gate == nil {
		t.Fatal("гейт не построен при двух режимах")
	}
	if !result.Gate.Passed {
		t.Fatalf("гибрид лучше базы, но гейт закрыт: %v", result.Gate.Failures)
	}
	if result.Gate.NDCG.Candidate <= result.Gate.NDCG.Baseline {
		t.Fatalf("кандидат %.4f не лучше базы %.4f — тест не проверяет нужное",
			result.Gate.NDCG.Candidate, result.Gate.NDCG.Baseline)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("Err() вернул ошибку при зелёном гейте: %v", err)
	}
}

func TestRunGateFailsEndToEndWhenCaseRegresses(t *testing.T) {
	cases := memGolden(t)
	fake := newFakeSearch(map[string][]Hit{
		// База: идеальный порядок, оба документа найдены.
		"bm25_only/q-exact": {{DocID: "d1"}},
		"bm25_only/q-two":   {{DocID: "d2"}, {DocID: "d3"}},
		"bm25_only/q-miss":  nil,
		"bm25_only/q-trap":  nil,
		// Гибрид: на q-two отодвинул эталонный документ за частично
		// релевантный — типичная регрессия от переусложнения слияния.
		"hybrid/q-exact": {{DocID: "d1"}},
		"hybrid/q-two":   {{DocID: "d3"}, {DocID: "d2"}},
		"hybrid/q-miss":  nil,
		"hybrid/q-trap":  nil,
	})

	result, err := Run(context.Background(), cases, fake.search, RunnerConfig{})
	if err != nil {
		t.Fatalf("прогон не должен падать на провале качества: %v", err)
	}
	if result.Gate == nil || result.Gate.Passed {
		t.Fatal("просадка nDCG не закрыла гейт")
	}

	err = result.Err()
	if err == nil {
		t.Fatal("Err() вернул nil при закрытых воротах")
	}
	var gateErr *GateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("ожидалась *GateError, получено %T", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "q-two") {
		t.Fatalf("в ошибке нет просевшего вопроса:\n%s", msg)
	}
	if !strings.Contains(msg, "nDCG@10") {
		t.Fatalf("в ошибке нет имени метрики:\n%s", msg)
	}
	// Просадка ровно одного вопроса — остальные не изменились.
	if got := gateErr.Gate.NDCG.RegressedCases; got != 1 {
		t.Fatalf("просевших %d, ожидался 1", got)
	}
}

func TestRunFailsWhenRetrieverErrors(t *testing.T) {
	// Зелёный гейт на прогоне, где индекс лежал, означал бы «ранжирование
	// не хуже базы», а на деле — «мы ничего не знаем». Такой прогон обязан
	// быть ошибкой, а не предупреждением.
	cases := memGolden(t)
	fake := newFakeSearch(nil)
	fake.errs = map[string]error{
		"hybrid/q-exact": assertError{},
		"hybrid/q-two":   assertError{},
		"hybrid/q-miss":  assertError{},
		"hybrid/q-trap":  assertError{},
	}

	_, err := Run(context.Background(), cases, fake.search, RunnerConfig{})
	if err == nil {
		t.Fatal("прогон с упавшим ретривером завершился успехом")
	}
	var runErr *RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("ожидалась *RunError, получено %T (%v)", err, err)
	}
	if !strings.Contains(err.Error(), "индекс был недоступен") {
		t.Fatalf("в ошибке нет предупреждения о недостоверности вердикта:\n%s", err.Error())
	}
	// База отработала, кандидат нет — в ошибке назван именно он.
	if !strings.Contains(err.Error(), string(ModeHybrid)) {
		t.Fatalf("в ошибке не назван упавший режим:\n%s", err.Error())
	}
}

func TestRunKeepsFailedCaseAsZeroButRefusesToPass(t *testing.T) {
	// Ошибка на одном вопросе обнуляет его метрики (выкидывать вопрос из
	// среднего значит завысить оценку ровно тогда, когда индекс недоступен),
	// но прогон в целом признаётся испорченным.
	cases := memGolden(t)
	fake := newFakeSearch(map[string][]Hit{
		"bm25_only/q-exact": {{DocID: "d1"}},
		"bm25_only/q-two":   {{DocID: "d2"}, {DocID: "d3"}},
		"bm25_only/q-miss":  nil,
		"bm25_only/q-trap":  nil,
		"hybrid/q-exact":    {{DocID: "d1"}},
		"hybrid/q-two":      {{DocID: "d2"}, {DocID: "d3"}},
		"hybrid/q-miss":     nil,
		"hybrid/q-trap":     nil,
	})
	fake.errs = map[string]error{"hybrid/q-miss": assertError{}}

	_, err := Run(context.Background(), cases, fake.search, RunnerConfig{})
	if err == nil {
		t.Fatal("частичный провал ретривера прошёл как успех")
	}
	if !strings.Contains(err.Error(), "q-miss") {
		t.Fatalf("в ошибке нет упавшего вопроса:\n%s", err.Error())
	}
}

func TestRunAcceptsEmptyHitsOnTrapAndRecordsEmptyOnRealCase(t *testing.T) {
	// Различие, которое обязано быть явным:
	//   — пустая выдача на ловушке — правильный ответ, вопрос не попадает
	//     в EmptyQueries и не портит средние;
	//   — пустая выдача на обычном вопросе — честный промах, это ноль в
	//     среднем и запись в EmptyQueries для разбора.
	cases := memGolden(t)
	fake := newFakeSearch(map[string][]Hit{
		"hybrid/q-exact": {{DocID: "d1"}},
		"hybrid/q-two":   {{DocID: "d2"}, {DocID: "d3"}},
		"hybrid/q-miss":  nil,
		"hybrid/q-trap":  nil,
	})

	result, err := Run(context.Background(), cases, fake.search,
		RunnerConfig{Modes: []Mode{ModeHybrid}})
	if err != nil {
		t.Fatalf("прогон: %v", err)
	}
	if len(result.Modes[0].EmptyQueries) != 1 ||
		result.Modes[0].EmptyQueries[0] != "q-miss" {
		t.Fatalf("пустые выдачи %v, ожидался только q-miss",
			result.Modes[0].EmptyQueries)
	}
	if result.Gate != nil {
		t.Fatal("гейт построен при одном режиме — сравнивать не с чем")
	}
	if err := result.Err(); err != nil {
		t.Fatalf("Err() без гейта вернул ошибку: %v", err)
	}
}

func TestRunPropagatesFalsePositiveOnTrap(t *testing.T) {
	cases := memGolden(t)
	fake := newFakeSearch(map[string][]Hit{
		"bm25_only/q-exact": {{DocID: "d1"}},
		"bm25_only/q-two":   {{DocID: "d2"}, {DocID: "d3"}},
		"bm25_only/q-miss":  nil,
		"bm25_only/q-trap":  nil,
		"hybrid/q-exact":    {{DocID: "d1"}},
		"hybrid/q-two":      {{DocID: "d2"}, {DocID: "d3"}},
		"hybrid/q-miss":     nil,
		// Уверенный неверный ответ на вопрос-ловушку.
		"hybrid/q-trap": {{DocID: "d1"}},
	})

	result, err := Run(context.Background(), cases, fake.search, RunnerConfig{})
	if err != nil {
		t.Fatalf("прогон: %v", err)
	}
	hybrid, _ := result.Report(ModeHybrid)
	if hybrid.FalsePositives != 1 {
		t.Fatalf("ложных ответов %d, ожидался 1", hybrid.FalsePositives)
	}
	// Ложный ответ не должен проваливать гейт §9 молча: он виден в отчёте,
	// а ненулевое число ложных ответов — повод разбираться.
	if result.Gate == nil {
		t.Fatal("гейт не построен")
	}
}

func TestRunRejectsEmptySetAndNilSearch(t *testing.T) {
	// Пустой набор — ошибка наравне с пустой выдачей: «ноль вопросов» и
	// «ноль качества» не должны выглядеть одинаково.
	if _, err := Run(context.Background(), nil, newFakeSearch(nil).search,
		RunnerConfig{}); err == nil {
		t.Fatal("пустой набор принят как успешный прогон")
	} else if !strings.Contains(err.Error(), "набор пуст") {
		t.Fatalf("неожиданный текст ошибки пустого набора: %v", err)
	}
	if _, err := Run(context.Background(), memGolden(t), nil,
		RunnerConfig{}); err == nil {
		t.Fatal("прогон без SearchFunc прошёл — пакет eval не умеет ходить в индекс")
	}
}

func TestRunRejectsBrokenSet(t *testing.T) {
	// Повреждённый набор обязан падать до обращения к индексу: иначе
	// «качество» считается по вопросу без разметки и выглядит правдой.
	broken := []Case{{ID: "q1", Query: "что-то"}}
	_, err := Run(context.Background(), broken, newFakeSearch(nil).search,
		RunnerConfig{Modes: []Mode{ModeHybrid}})
	if err == nil {
		t.Fatal("вопрос без разметки принят")
	}
	if !strings.Contains(err.Error(), "набор повреждён") {
		t.Fatalf("неожиданный текст ошибки: %v", err)
	}
}

func TestRunNormalisesConfigAndRejectsBadModes(t *testing.T) {
	cases := memGolden(t)
	fake := newFakeSearch(map[string][]Hit{
		"hybrid/q-exact": {{DocID: "d1"}},
		"hybrid/q-two":   {{DocID: "d2"}, {DocID: "d3"}},
		"hybrid/q-miss":  nil,
		"hybrid/q-trap":  nil,
	})
	result, err := Run(context.Background(), cases, fake.search,
		RunnerConfig{Modes: []Mode{ModeHybrid}, K: 0, RecallK: 0, MinGrade: 0})
	if err != nil {
		t.Fatalf("прогон: %v", err)
	}
	if result.K != DefaultK {
		t.Fatalf("K = %d, ожидался %d", result.K, DefaultK)
	}
	if result.MinGrade != DefaultMinGrade {
		t.Fatalf("MinGrade = %d, ожидался %d", result.MinGrade, DefaultMinGrade)
	}
	// Ретриверу заказан срез не меньше Recall@50.
	for _, limit := range fake.limits {
		if limit < DefaultRecallK {
			t.Fatalf("ретриверу запрошен срез %d, ожидалось не меньше %d", limit, DefaultRecallK)
		}
	}

	bad := []RunnerConfig{
		{Modes: []Mode{Mode("vector_only")}},
		{Modes: []Mode{ModeHybrid, ModeHybrid}},
	}
	for i, cfg := range bad {
		if _, err := Run(context.Background(), cases, fake.search, cfg); err == nil {
			t.Fatalf("конфигурация %d принята: %+v", i, cfg)
		}
	}
}

func TestSanitizeHitsDropsBlanksAndDuplicates(t *testing.T) {
	// Один документ двумя чанками обязан занимать одну позицию: иначе он
	// дважды попадёт в DCG и Recall, а топ-10 забьётся одним документом.
	in := []Hit{
		{DocID: "  "},
		{DocID: "d1", Rank: 99},
		{DocID: "d1", Rank: 3},
		{DocID: "d2", Rank: 0},
		{DocID: "d3", Rank: 7},
	}
	got := sanitizeHits(in, 0)
	want := []Hit{{DocID: "d1", Rank: 0}, {DocID: "d2", Rank: 1}, {DocID: "d3", Rank: 2}}
	if len(got) != len(want) {
		t.Fatalf("после нормализации %d документов, ожидалось %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("позиция %d: %+v, ожидалось %+v", i, got[i], want[i])
		}
	}
	if sanitizeHits(nil, 10) != nil {
		t.Fatal("пустая выдача превратилась в непустую")
	}
	if got := sanitizeHits(in, 2); len(got) != 2 {
		t.Fatalf("срез 2 дал %d документов, ожидалось 2", len(got))
	}
}

func TestRunIsDeterministicAcrossRepeats(t *testing.T) {
	// Один и тот же прогон должен давать один и тот же отчёт: CI сравнивает
	// числа, а не «примерно те же» числа.
	cases := memGolden(t)
	fake := newFakeSearch(map[string][]Hit{
		"bm25_only/q-exact": {{DocID: "noise"}, {DocID: "d1"}},
		"bm25_only/q-two":   {{DocID: "d3"}, {DocID: "d2"}},
		"bm25_only/q-miss":  nil,
		"bm25_only/q-trap":  nil,
		"hybrid/q-exact":    {{DocID: "d1"}},
		"hybrid/q-two":      {{DocID: "d2"}, {DocID: "d3"}},
		"hybrid/q-miss":     nil,
		"hybrid/q-trap":     nil,
	})

	first, err := Run(context.Background(), cases, fake.search, RunnerConfig{})
	if err != nil {
		t.Fatalf("прогон: %v", err)
	}
	for range 10 {
		again, err := Run(context.Background(), cases, fake.search, RunnerConfig{})
		if err != nil {
			t.Fatalf("повторный прогон: %v", err)
		}
		if again.Gate == nil || first.Gate == nil {
			t.Fatal("гейт то появляется, то нет")
		}
		if again.Gate.NDCG != first.Gate.NDCG ||
			again.Gate.Recall != first.Gate.Recall ||
			again.Gate.MRR != first.Gate.MRR {
			t.Fatalf("метрики изменились между прогонами: %+v против %+v", first.Gate, again.Gate)
		}
		if len(again.Gate.Regressions) != len(first.Gate.Regressions) {
			t.Fatal("список просадок изменился между прогонами")
		}
	}
}

// --- защита поставляемого набора -------------------------------------------
//
// Ворота §9 бессмысленны на битом наборе: 40 вопросов вместо 141 дают
// «замер», который никто не увидит, а дубликат id склеит два разных
// вопроса в один и незаметно изменит среднее. Этот тест — единственная
// защита от правки файла, который никто не перечитывает.

const (
	shippedSetPath = "golden/golden.jsonl"
	// minShippedCases — нижняя граница из §9: «eval-набор не меньше 100».
	minShippedCases = 100
)

// validKinds — категории вопросов, описанные в Case.Kind. Неизвестная
// категория не ломает расчёт, но прячет просадку внутри ByKind, поэтому
// набор без прав на выдуманные значения не проходит.
var validKinds = map[string]struct{}{
	"single_hop":    {},
	"multi_hop":     {},
	"temporal":      {},
	"contradiction": {},
	"completeness":  {},
}

func loadShippedSet(t *testing.T) []Case {
	t.Helper()
	cases, err := LoadGoldenFile(shippedSetPath)
	if err != nil {
		t.Fatalf("поставляемый набор не читается: %v", err)
	}
	return cases
}

func TestShippedGoldenSetMeetsMVPGate(t *testing.T) {
	cases := loadShippedSet(t)
	if len(cases) < minShippedCases {
		t.Fatalf("в наборе %d вопросов, §9 требует не меньше %d", len(cases), minShippedCases)
	}
}

func TestShippedGoldenSetHasUniqueIDs(t *testing.T) {
	// Дубликат id схлопывает два разных вопроса в один в карте сопоставления
	// и делает средний балл невоспроизводимым.
	seen := make(map[string]int, len(loadShippedSet(t)))
	for _, c := range loadShippedSet(t) {
		seen[c.ID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("идентификатор %q встречается %d раз", id, count)
		}
	}
}

func TestShippedGoldenSetHasMarkedRelevantDocs(t *testing.T) {
	// Validate в LoadGolden уже ловит отсутствие разметки, но этот тест
	// проверяет итог явно: набор из ловушек формально валиден и метрики по
	// нему посчитать нельзя.
	cases := loadShippedSet(t)
	traps := 0
	for _, c := range cases {
		if c.ExpectedEmpty {
			traps++
			continue
		}
		if len(c.Relevant) == 0 {
			t.Errorf("вопрос %q без размеченных документов", c.ID)
		}
	}
	if traps == 0 {
		t.Error("в наборе нет ни одного вопроса-ловушки: система, которая всегда " +
			"отвечает, будет выглядеть идеальной")
	}
	// Ловушки — малая доля: если их большинство, набор измеряет отказ
	// отвечать, а не качество поиска.
	if traps*2 > len(cases) {
		t.Errorf("ловушки %d из %d вопросов — набор измеряет не качество поиска",
			traps, len(cases))
	}
}

func TestShippedGoldenSetHasValidKindsAndGrades(t *testing.T) {
	for _, c := range loadShippedSet(t) {
		if _, ok := validKinds[c.Kind]; !ok {
			t.Errorf("вопрос %q: неизвестная категория %q", c.ID, c.Kind)
		}
		for _, rel := range c.Relevant {
			if rel.Grade < 1 || rel.Grade > 3 {
				t.Errorf("вопрос %q: оценка %d вне шкалы 1..3", c.ID, rel.Grade)
			}
		}
	}
}

func TestShippedGoldenSetCoversQuestionClasses(t *testing.T) {
	// §2 и §5 называют сложные классы явно (противоречивая информация,
	// полнота). Набор, где всё single_hop, не способен поймать регрессию
	// векторной ветви — ровно ту, ради которой набор и делается.
	kinds := map[string]int{}
	for _, c := range loadShippedSet(t) {
		kinds[c.Kind]++
	}
	for _, required := range []string{"single_hop", "multi_hop", "temporal", "contradiction"} {
		if kinds[required] == 0 {
			t.Errorf("в наборе нет вопросов категории %q", required)
		}
	}
}

func TestShippedGoldenSetMatchesFixtureCorpus(t *testing.T) {
	// Идентификаторы документов в разметке должны существовать в корпусе
	// фикстур, иначе «золотой» набор измеряет индекс, в котором этих
	// документов нет, и все метрики уходят в ноль по построению.
	root := filepath.Join("..", "..", "..", "contracts", "fixtures", "vektor")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("фикстуры корпуса недоступны (%v) — проверка пропущена", err)
	}
	// Идентификатор документа в наборе — имя файла фикстуры без .md,
	// с одной исключением: README.md корпуса индексируется как
	// vektor-readme, потому что «readme» само по себе не различает
	// каталог и конкретный документ.
	known := map[string]struct{}{"manual": {}, "vektor-readme": {}}
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".md") {
			known[strings.TrimSuffix(name, ".md")] = struct{}{}
		}
	}
	for _, c := range loadShippedSet(t) {
		for _, rel := range c.Relevant {
			if _, ok := known[rel.DocID]; !ok {
				t.Errorf("вопрос %q ссылается на документ %q, которого нет в корпусе фикстур",
					c.ID, rel.DocID)
			}
		}
	}
}

func TestShippedGoldenSetRunsAgainstStubSearchFunc(t *testing.T) {
	// Прогон поставляемого набора целиком на заглушке: проверяем, что
	// метрики считаются по всем 141 вопросам и не падают. Elasticsearch
	// при этом не нужен — ровно ради этого SearchFunc и вынесен наружу.
	cases := loadShippedSet(t)
	// Заглушка знает ответы (это её единственная задача в тесте) и отдаёт их
	// в порядке убывания оценки — единственном порядке, при котором выдача
	// идеальна. Порядок важен: nDCG сравнивает DCG с идеальной DCG, и
	// эталонный документ, поставленный после частично релевантного, даёт
	// меньше 1.0 законно.
	ideal := newFakeSearch(nil)
	base := map[string][]Hit{}
	for _, c := range cases {
		ranked := slices.Clone(c.Relevant)
		slices.SortStableFunc(ranked, func(a, b Scored) int {
			return b.Grade - a.Grade
		})
		hits := make([]Hit, 0, len(ranked))
		for _, rel := range ranked {
			hits = append(hits, Hit{DocID: rel.DocID})
		}
		base[string(ModeBM25Only)+"/"+c.ID] = hits
		base[string(ModeHybrid)+"/"+c.ID] = hits
	}
	ideal.results = base

	result, err := Run(context.Background(), cases, ideal.search, RunnerConfig{})
	if err != nil {
		t.Fatalf("прогон поставляемого набора: %v", err)
	}
	for _, mode := range result.Modes {
		if mode.Report.Cases != len(cases) {
			t.Errorf("режим %s: %d вопросов из %d", mode.Mode, mode.Report.Cases, len(cases))
		}
		// Идеальная выдача по размеченной разметке обязана дать 1.0 —
		// если нет, разметка и метрики не стыкуются.
		if math.Abs(mode.Report.NDCG-1) > 1e-9 {
			t.Errorf("режим %s: идеальная выдача дала nDCG %.6f, ожидалась 1",
				mode.Mode, mode.Report.NDCG)
		}
		if mode.Report.FalsePositives != 0 {
			t.Errorf("режим %s: ложных ответов %d при пустой выдаче на ловушках",
				mode.Mode, mode.Report.FalsePositives)
		}
	}
	if result.Gate == nil || !result.Gate.Passed {
		t.Fatalf("одинаковые идеальные выдачи не прошли гейт: %+v", result.Gate)
	}
}

func TestEvalDoesNotDependOnInternalLayers(t *testing.T) {
	// Пакет eval обязан оставаться независимым от внутренних слоёв, и это
	// не стилистическое предпочтение, а условие работоспособности:
	// только без него `go test ./eval/...` запускается без Elasticsearch и
	// Postgres, и только тогда правило §5 («любое изменение ранжирования
	// проходит через этот набор до выката») проверяется в CI на каждом PR.
	//
	// Проверяем фактические зависимости сборки, а не текст файлов: пакеты,
	// которые Go действительно подтянет, нельзя обойти «нужным» импортом.
	//
	// Проверяется только модуль: у стандартной библиотеки есть свои
	// internal/syscall и подобные, они к архитектуре проекта отношения не
	// имеют.
	const modulePrefix = "github.com/kbsearch/kb/internal/"
	pkgs, err := listPackages(".")
	if err != nil {
		t.Fatalf("список зависимостей пакета: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list вернул пустой список — проверка бессмысленна")
	}
	for _, pkg := range pkgs {
		if strings.Contains(pkg, modulePrefix) {
			t.Errorf("пакет eval тянет внутренний слой %q — метрики перестанут "+
				"тестироваться без кластера", pkg)
		}
	}
}

// listPackages возвращает пакеты, которые пакет eval подтягивает сборкой.
// Список разбирается командой go: держать его руками означало бы забыть
// обновить.
func listPackages(pattern string) ([]string, error) {
	cmd := exec.Command("go", "list", "-deps", pattern)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list -deps %s: %w: %s", pattern, err, stderr.String())
	}
	return strings.Fields(stdout.String()), nil
}

func TestRunResultIsJSONSerializable(t *testing.T) {
	// Отчёт уходит в CI-артефакт, поэтому он обязан быть данными с
	// осмысленными именами полей, а не структурой, у которой в JSON
	// окажется пустота.
	cases := memGolden(t)
	result, err := Run(context.Background(), cases, newFakeSearch(nil).search,
		RunnerConfig{Modes: []Mode{ModeHybrid}})
	if err != nil {
		t.Fatalf("прогон: %v", err)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("отчёт не сериализуется: %v", err)
	}
	for _, key := range []string{`"k": 10`, `"cases": 4`, `"mode": "hybrid"`, `"report"`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("в JSON отчёта нет %s:\n%s", key, encoded)
		}
	}
	if strings.Contains(string(encoded), `"null"`) {
		t.Errorf("в JSON отчёта неразобранные поля (null):\n%s", encoded)
	}
}
