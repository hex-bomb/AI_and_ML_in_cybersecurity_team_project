// Package search — ядро гибридного поиска: запросы к Elasticsearch и слияние RRF.
// Без I/O и глобального состояния: всё принимается параметрами, результат — значения.
package search

import "sort"

const (
	// rrfK — постоянная сглаживания RRF; 60 — значение из оригинальной статьи.
	rrfK = 60
	// candidates — минимум кандидатов для knn, чтобы recall не падал на коротких запросах.
	candidates = 100
)

// Hit — один фрагмент в ответе наружу.
type Hit struct {
	ChunkID    string `json:"chunk_id"`
	DocumentID string `json:"document_id"`
	// DocumentRef — внутренний идентификатор документа. По нему answer
	// сохраняет цитаты: внешний ключ не годится для ссылок в БД.
	DocumentRef string  `json:"document_ref"`
	VersionID   string  `json:"version_id"`
	Title       string  `json:"title"`
	Text        string  `json:"text"`
	Locator     Locator `json:"locator"`
	SourceURL   *string `json:"source_url"`
	DownloadURL string  `json:"download_url"`
	Score       float64 `json:"score"`
}

// Locator — где фрагмент лежит в исходном документе.
type Locator struct {
	BlockIDs    []string `json:"block_ids"`
	SectionPath []string `json:"section_path"`
	PageStart   *int     `json:"page_start"`
	PageEnd     *int     `json:"page_end"`
}

// Scope — каналы и группы principal'а, которым разрешён поиск.
type Scope struct {
	Channels []string
	Groups   []string
}

// focusBoost поднимает фрагменты документа, к которому привязана страница.
// Значение умеренное: подсказка не должна перебивать прямое совпадение по теме.
const focusBoost = 3.0

// Size — сколько кандидатов просить у каждой ветки: с запасом на слияние
// и не меньше 10.
func Size(topK int) int {
	return max(topK*3, 10)
}

// scopeFilter — один фильтр на обе ветки: иначе лексика и kNN видят разные
// документы.
//
// Условия отражают правило доступа сервиса documents: документ виден, если
// опубликован, его канал входит в каналы principal'а и либо он открыт всем
// в канале, либо его группа есть у principal'а.
func scopeFilter(s Scope) []any {
	groups := s.Groups
	if groups == nil {
		// nil сериализуется в null, а terms с null Elasticsearch отвергает.
		groups = []string{}
	}
	return []any{
		map[string]any{"term": map[string]any{"published": true}},
		map[string]any{"terms": map[string]any{"channels": s.Channels}},
		map[string]any{"bool": map[string]any{
			"minimum_should_match": 1,
			"should": []any{
				map[string]any{"term": map[string]any{"open_to_all": true}},
				map[string]any{"terms": map[string]any{"group_keys": groups}},
			},
		}},
	}
}

// LexicalBody — тело BM25-запроса по заголовку и тексту. title^3 поднимает
// заголовок над текстом; embedding из _source не тянем — наружу он не нужен.
//
// focus — документ, к которому привязана страница виджета. Это повышение, а не
// фильтр: остальные документы по-прежнему участвуют в выдаче, а доступ от
// подсказки не меняется.
func LexicalBody(s Scope, query string, size int, focus string) map[string]any {
	boolQuery := map[string]any{
		"must": []any{map[string]any{"multi_match": map[string]any{
			"query":  query,
			"fields": []string{"title^3", "text"},
			"type":   "best_fields",
		}}},
		"filter": scopeFilter(s),
	}
	if focus != "" {
		boolQuery["should"] = []any{map[string]any{"term": map[string]any{
			"document_ref": map[string]any{"value": focus, "boost": focusBoost},
		}}}
	}
	return map[string]any{
		"size":    size,
		"_source": map[string]any{"excludes": []string{"embedding"}},
		"query":   map[string]any{"bool": boolQuery},
	}
}

// KnnBody — тело kNN-запроса по вектору. num_candidates задаёт глубину обхода
// HNSW: больше кандидатов — выше recall и дороже запрос.
func KnnBody(s Scope, vector []float64, size int) map[string]any {
	return map[string]any{
		"size":    size,
		"_source": map[string]any{"excludes": []string{"embedding"}},
		"knn": map[string]any{
			"field":          "embedding",
			"query_vector":   vector,
			"k":              size,
			"num_candidates": max(candidates, size*10),
			"filter":         map[string]any{"bool": map[string]any{"filter": scopeFilter(s)}},
		},
	}
}

// Fused — результат слияния до отображения в Hit.
type Fused struct {
	ID    string
	Score float64
}

// RRF сливает ранжировки по формуле reciprocal rank fusion: вклад документа —
// 1/(rrfK+rank). tie-break — по порядку первого появления, чтобы выдача была стабильной.
func RRF(rankings [][]string, limit int) []Fused {
	scores := map[string]float64{}
	firstSeen := map[string]int{}

	for _, ranking := range rankings {
		for i, id := range ranking {
			scores[id] += 1.0 / float64(rrfK+i+1)
			if _, ok := firstSeen[id]; !ok {
				firstSeen[id] = len(firstSeen)
			}
		}
	}

	out := make([]Fused, 0, len(scores))
	for id, score := range scores {
		out = append(out, Fused{ID: id, Score: score})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return firstSeen[out[i].ID] < firstSeen[out[j].ID]
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ToHit отображает документ Elasticsearch в наружный Hit. BlockIDs остаётся
// пустым: в _source нет block_ids, но в JSON нужно [], а не null.
func ToHit(src map[string]any, score float64) Hit {
	documentID := str(src["document_id"])
	return Hit{
		ChunkID:     str(src["chunk_id"]),
		DocumentID:  documentID,
		DocumentRef: str(src["document_ref"]),
		VersionID:   str(src["version_id"]),
		Title:       str(src["title"]),
		Text:        str(src["text"]),
		Locator: Locator{
			BlockIDs:    []string{},
			SectionPath: stringsOf(src["section_path"]),
			PageStart:   intPtr(src["page_start"]),
			PageEnd:     intPtr(src["page_end"]),
		},
		DownloadURL: "/api/v1/documents/" + documentID + "/source",
		Score:       score,
	}
}

// MergeSources сливает источники из нескольких выдач: позже переопределяет раньше.
func MergeSources(groups ...map[string]map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, group := range groups {
		for id, doc := range group {
			out[id] = doc
		}
	}
	return out
}

// Rankings собирает только непустые ранжировки: пустая не должна тянуть score.
func Rankings(groups ...[]string) [][]string {
	out := make([][]string, 0, len(groups))
	for _, group := range groups {
		if len(group) > 0 {
			out = append(out, group)
		}
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func stringsOf(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// intPtr переводит число из _source: encoding/json отдаёт числа как float64.
func intPtr(v any) *int {
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	n := int(f)
	return &n
}
