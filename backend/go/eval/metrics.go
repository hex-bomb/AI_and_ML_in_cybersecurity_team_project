// Package evaluation — метрики качества поиска и прогон по размеченному набору.
//
// WHY: в разделе 9 документа «Confluence нового поколения» стоит правило
// «любое изменение ранжирования проходит eval-набор до выката», а ворота MVP
// требуют «eval-набор не меньше 100 реальных вопросов; nDCG@10 гибрида не
// ниже BM25-базы». Значит метрики и формат набора должны существовать раньше
// первого буста, иначе «улучшение» нечем будет отличить от регрессии.
//
// Пакет ничего не знает ни про Elasticsearch, ни про HTTP: он берёт готовый
// список документов из функции Retriever и считает по нему метрики. Так его
// можно прогнать и против боевого индекса, и против заглушки в юнит-тесте.
package evaluation

import (
	"math"
	"sort"
)

// Hit — один документ в выдаче: идентификатор и ранг (с нуля).
//
// Идентификатор сравнивается строкой намеренно: он приходит из разных
// источников (uuid из Postgres, внешний id коннектора) и не должен
// приводиться к общему типу ради сравнения.
type Hit struct {
	DocID string `json:"doc_id"`
	Rank  int    `json:"rank"`
}

// Scored — разметка одного вопроса: релевантные документы с оценками.
//
// Оценка graded: 0 — нерелевантен, 1 — частично, 2 — релевантен, 3 —
// эталонный ответ. nDCG это различает, а «релевантен/нет» — нет.
type Scored struct {
	DocID string `json:"doc_id"`
	Grade int    `json:"grade"`
}

// Case — один размеченный вопрос набора.
type Case struct {
	ID    string `json:"id"`
	Query string `json:"query"`
	// Relevant — размеченные релевантные документы. Документы с оценкой 0
	// не перечисляются: «нерелевантен» по набору не значит «не должен
	// находиться», иначе разметка раздувается до всех документов корпуса.
	Relevant []Scored `json:"relevant"`
	// Kind — категория вопроса (single_hop, multi_hop, temporal,
	// contradiction, completeness). Нужна, чтобы видеть, на какой класс
	// вопросов регрессия, а не только общую цифру.
	Kind string `json:"kind"`
	// Tags — доменные метки (welding, service, onboarding). Считаются так
	// же, как Kind, и позволяют увидеть просадку на одном участке знаний,
	// замаскированную ростом на другом.
	Tags []string `json:"tags,omitempty"`
	// ExpectedEmpty — вопрос-ловушка, где правильный ответ «ничего не
	// найдено». Для них считается false-positive rate: непустая выдача
	// здесь хуже пустой, а не лучше.
	ExpectedEmpty bool `json:"expected_empty,omitempty"`
}

// relevantSet — карта документов в оценки для быстрого слияния.
func (c Case) relevantSet() map[string]int {
	set := make(map[string]int, len(c.Relevant))
	for _, rel := range c.Relevant {
		set[rel.DocID] = rel.Grade
	}
	return set
}

// MaxGrade — максимальная оценка в разметке; nDCG нормируется по ней.
func (c Case) MaxGrade() int {
	maxGrade := 0
	for _, rel := range c.Relevant {
		if rel.Grade > maxGrade {
			maxGrade = rel.Grade
		}
	}
	return maxGrade
}

// CaseResult — метрики одного вопроса. Пустой срез означает «ничего не
// нашли»: это провал, а не «не измерялось».
type CaseResult struct {
	CaseID   string   `json:"case_id"`
	Kind     string   `json:"kind"`
	Query    string   `json:"query"`
	Tags     []string `json:"tags,omitempty"`
	NDCG     float64  `json:"ndcg"`
	Recall   float64  `json:"recall"`
	MRR      float64  `json:"mrr"`
	Hits     int      `json:"hits"`
	Relevant int      `json:"relevant"`
	FirstHit string   `json:"first_hit,omitempty"`
	// Trap — вопрос-ловушка: правильный ответ «ничего не найдено».
	// Для него nDCG/Recall нулевые по построению, в средние он не входит.
	Trap bool `json:"trap,omitempty"`
	// FalsePositive — на вопрос-ловушку система ответила непустой выдачей.
	// Это хуже промаха: агент получит неверный ответ уверенно.
	FalsePositive bool `json:"false_positive,omitempty"`
}

// Report — агрегаты по набору плюс ворогая подробность по каждому вопросу.
//
// Средние по вопросам, а не по документам: вопрос с одним релевантным
// документом и вопрос с пятью весят одинаково — иначе набор из вопросов
// про один и тот же документ перекосит результат.
type Report struct {
	Cases  int     `json:"cases"`
	NDCG   float64 `json:"ndcg"`
	Recall float64 `json:"recall"`
	MRR    float64 `json:"mrr"`
	// TrapCases / FalsePositives — вопросы-ловушки и ответы на них.
	// Пустая выдача здесь правильна, поэтому эти числа не входят в
	// средние выше: усреднять их с обычными вопросами бессмысленно.
	TrapCases      int             `json:"trap_cases"`
	FalsePositives int             `json:"false_positives"`
	ByKind         map[string]Kind `json:"by_kind"`
	ByTag          map[string]Kind `json:"by_tag,omitempty"`
	Details        []CaseResult    `json:"details"`
}

// Kind — агрегаты по категории вопросов.
type Kind struct {
	Cases  int     `json:"cases"`
	NDCG   float64 `json:"ndcg"`
	Recall float64 `json:"recall"`
	MRR    float64 `json:"mrr"`
}

// NDCGAtK — нормализованный дисконтированный накопленный выигрыш.
//
// Логарифм берётся от базы 2 (не ln) — стандартный выбор для NDCG: при
// grade=1 и позиции 1 скидка равна 1, а не 0.63.
//
// Пустой срез возвращает 0, а не 1: вопрос, на который ничего не нашли,
// обязан портить средний балл, иначе система может «выиграть» набор,
// просто ничего не возвращая.
func NDCGAtK(hits []Hit, relevant map[string]int, maxGrade, k int) float64 {
	if k <= 0 || maxGrade <= 0 {
		return 0
	}
	dcg := 0.0
	for i, hit := range hits {
		if i >= k {
			break
		}
		grade, ok := relevant[hit.DocID]
		if !ok {
			continue
		}
		dcg += gain(grade) / math.Log2(float64(i+2))
	}
	idcg := 0.0
	// Идеальная выдача короче реальной: её длина — число размеченных
	// документов, ограниченное k. Иначе вопрос с одним релевантным
	// документом невозможно выиграть в принципе.
	//
	// Нормируем по реальным оценкам, отсортированным по убыванию, а не по
	// повторению maxGrade: иначе вопрос, где один документ — эталон, а
	// второй лишь частично релевантен, не мог бы получить 1.0 даже при
	// идеальном порядке, и его вклад занижался бы навсегда.
	grades := make([]int, 0, len(relevant))
	for _, grade := range relevant {
		grades = append(grades, grade)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(grades)))
	for i := range min(len(grades), k) {
		idcg += gain(grades[i]) / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// gain — экспоненциальный выигрыш оценки: 0 → 0, 1 → 0.5, 2 → 1.5, 3 → 3.5.
// Именно экспонента, а не сама оценка, делает попадание эталонного документа
// на первую позицию заметно весомее попадания частично релевантного.
func gain(grade int) float64 {
	if grade <= 0 {
		return 0
	}
	return (math.Pow(2, float64(grade)) - 1) / 2
}

// RecallAtK — доля размеченных релевантных документов, попавших в топ-k.
//
// Документы с оценкой ниже порога релевантности не считаются: иначе
// «частично релевантен» (grade 1) обязан попасть в выдачу так же строго,
// как эталонный ответ.
func RecallAtK(hits []Hit, relevant map[string]int, minGrade, k int) float64 {
	total := 0
	for _, grade := range relevant {
		if grade >= minGrade {
			total++
		}
	}
	if total == 0 {
		return 0
	}
	found := 0
	for i, hit := range hits {
		if i >= k {
			break
		}
		if grade, ok := relevant[hit.DocID]; ok && grade >= minGrade {
			found++
		}
	}
	return float64(found) / float64(total)
}

// MRR — средняя обратная ранга первого релевантного документа.
//
// minGrade задаёт, что считается попаданием; тот же порог, что и у RecallAtK.
func MRR(hits []Hit, relevant map[string]int, minGrade int) float64 {
	for i, hit := range hits {
		if grade, ok := relevant[hit.DocID]; ok && grade >= minGrade {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// Defaults — значения по умолчанию: те самые, что названы в воротах MVP
// (nDCG@10, Recall@50) плюс обычный порог релевантности 1.
const (
	DefaultK        = 10
	DefaultRecallK  = 50
	DefaultMinGrade = 1
)

// ScoreCase считает метрики одного вопроса.
func ScoreCase(c Case, hits []Hit, k, recallK, minGrade int) CaseResult {
	relevant := c.relevantSet()
	res := CaseResult{
		CaseID:   c.ID,
		Kind:     c.Kind,
		Query:    c.Query,
		Tags:     c.Tags,
		Hits:     len(hits),
		Relevant: len(c.Relevant),
		NDCG:     NDCGAtK(hits, relevant, c.MaxGrade(), k),
		Recall:   RecallAtK(hits, relevant, minGrade, recallK),
		MRR:      MRR(hits, relevant, minGrade),
	}
	for _, hit := range hits {
		if grade, ok := relevant[hit.DocID]; ok && grade >= minGrade {
			res.FirstHit = hit.DocID
			break
		}
	}
	// Ловушка считается отдельно: у неё нет размеченных документов,
	// поэтому nDCG/Recall для неё нулевые по построению.
	if c.ExpectedEmpty {
		res.Trap = true
		res.FalsePositive = len(hits) > 0
	}
	return res
}

// Aggregate сводит результаты по вопросам в отчёт.
//
// Разделение на «средние по вопросам» и «по категориям» сделано потому,
// что без него нельзя увидеть главное: просадку на одном классе вопросов
// компенсирует рост на другом, и суммарная цифра остаётся зелёной.
func Aggregate(results []CaseResult) Report {
	report := Report{
		Cases:  len(results),
		ByKind: map[string]Kind{},
	}
	if len(results) == 0 {
		return report
	}
	byKind := map[string][]CaseResult{}
	byTag := map[string][]CaseResult{}
	for _, r := range results {
		// Ловушки не идут в обычные средние: у них нет разметки, и
		// нулевой nDCG от пустой выдачи — это успех, а не провал.
		if !r.Trap {
			report.NDCG += r.NDCG
			report.Recall += r.Recall
			report.MRR += r.MRR
		}
		if r.FalsePositive {
			report.FalsePositives++
		}
		byKind[r.Kind] = append(byKind[r.Kind], r)
		for _, tag := range r.Tags {
			byTag[tag] = append(byTag[tag], r)
		}
	}

	graded := 0
	for _, r := range results {
		if !r.Trap {
			graded++
		}
	}
	if graded > 0 {
		n := float64(graded)
		report.NDCG /= n
		report.Recall /= n
		report.MRR /= n
	}
	for _, r := range results {
		if r.Trap {
			report.TrapCases++
		}
	}

	report.ByKind = aggregateGroups(byKind)
	if len(byTag) > 0 {
		report.ByTag = aggregateGroups(byTag)
	}

	// Подробность — от худших вопросов к лучшим: первой в отчёте должна
	// быть видна регрессия, а не удача.
	report.Details = append([]CaseResult(nil), results...)
	sort.SliceStable(report.Details, func(i, j int) bool {
		return report.Details[i].NDCG < report.Details[j].NDCG
	})
	return report
}

// aggregateGroups считает средние по каждой группе вопросов.
func aggregateGroups(groups map[string][]CaseResult) map[string]Kind {
	out := make(map[string]Kind, len(groups))
	for key, group := range groups {
		agg := Kind{Cases: len(group)}
		graded := 0
		for _, r := range group {
			if r.Trap {
				continue
			}
			agg.NDCG += r.NDCG
			agg.Recall += r.Recall
			agg.MRR += r.MRR
			graded++
		}
		if graded == 0 {
			out[key] = agg
			continue
		}
		n := float64(graded)
		agg.NDCG /= n
		agg.Recall /= n
		agg.MRR /= n
		out[key] = agg
	}
	return out
}
