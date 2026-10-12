package elasticsearch

import (
	"context"
	"strings"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Файл собирает два тела запроса, в которых права едут внутрь (§5 шаг 2, §6),
// и реализует адаптерную часть порта ports.SecuredSearchIndex.
//
// Старый путь (SearchFilter.AllowedDocumentIDs) остаётся в index_body.go: им
// пользуется internal/search/service, который переезжает на этот порт отдельной
// задачей. Пока переезд не случился, оба пути сосуществуют, и их нельзя
// смешивать: список разрешённых документов — это ровно та материализация, которую
// §6 запрещает.

// scopedFilterClauses — обязательные фильтры запроса: тенант, база, профиль
// эмбеддинга и права.
//
// Клоза прав здесь ВСЕГДА присутствует, даже когда не разрешает ничего: пустая
// область превращается в match_none, а не в «фильтр не задан». Забытый или
// выброшенный фильтр читается как «доступны все документы тенанта», поэтому
// список клоз фиксирован и не зависит от данных.
//
// Термин по тенанту — первая клоза и он не зависит от области доступа: даже
// scope с полными правами другого тенанта обязана остаться внутри своего
// tenant_id.
func scopedFilterClauses(scope domain.AccessScope, cfg domain.IndexingConfig) []any {
	return []any{
		map[string]any{"term": map[string]any{"tenant_id": scope.TenantID}},
		map[string]any{"term": map[string]any{"kb_id": scope.KBID}},
		map[string]any{"term": map[string]any{"embedding_profile_id": cfg.ProfileID()}},
		aclAccessClause(scope),
	}
}

// scopedKeywordQuery — тело текстовой ветви: тот же multi_match без fuzziness,
// что и у keywordQuery, но с правами вместо списка документов.
func scopedKeywordQuery(text string, scope domain.AccessScope, cfg domain.IndexingConfig, limit int) map[string]any {
	return map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"multi_match": map[string]any{
							"query":    text,
							"fields":   []string{"text", "title"},
							"analyzer": russianAnalyzer,
						},
					},
				},
				"filter": scopedFilterClauses(scope, cfg),
			},
		},
	}
}

// scopedVectorQuery — тело векторной ветви. Фильтр кладётся ВНУТРЬ knn.filter.
//
// Это не украшение: kNN отбирает кандидатов по векторной близости, и фильтр,
// стоящий только на верхнем уровне запроса, сужает уже отобранную выдачу, но не
// сам поиск. На практике это означает «прочитать чужие соседи и выбросить их»:
// HNSW вернёт k ближайших чанков корпуса, из них отфильтруются чужие, и
// пользователю приедет меньше результата, чем могло быть, а при небольшом числе
// совпадений — пусто. §5 шаг 2 и говорит прямо: «фильтр передаётся внутрь
// knn.filter».
//
// Тот же фильтр продублирован на верхнем уровне bool: kNN.filter сужает
// выбор кандидатов, а эта клоза гарантирует, что в ответе не окажется ничего
// вне прав при любой будущей правке формы запроса. Оба раза берётся один и тот
// же срез — он только читается при сериализации.
func scopedVectorQuery(vec []float32, cfg domain.IndexingConfig, scope domain.AccessScope, limit int) map[string]any {
	k := limit
	if k < minKNNK {
		k = minKNNK
	}
	clauses := scopedFilterClauses(scope, cfg)
	return map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"knn": map[string]any{
							"field":          "embedding",
							"query_vector":   vec,
							"k":              k,
							"num_candidates": numCandidates(k),
							"filter":         map[string]any{"bool": map[string]any{"filter": clauses}},
						},
					},
				},
				"filter": clauses,
			},
		},
	}
}

// KeywordSearchScoped — текстовая ветвь порта ports.SecuredSearchIndex: права
// приходят аргументом и уезжают внутрь запроса.
//
// Пустая область — пустой результат без обращения к индексу. Это дешевле и
// безопаснее, чем спрашивать кластер, который всё равно не сможет ответить
// «покажи мне то, что мне нельзя».
func (i *Index) KeywordSearchScoped(
	ctx context.Context,
	query string,
	scope domain.AccessScope,
	cfg domain.IndexingConfig,
	limit int,
) ([]domain.SearchCandidate, error) {
	if scope.AllowsNothing() || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	body := scopedKeywordQuery(query, scope, cfg, clampLimit(limit))
	return i.search(ctx, cfg.IndexName(), body)
}

// VectorSearchScoped — векторная ветвь порта ports.SecuredSearchIndex.
//
// Порядок проверок повторяет VectorSearch: права, затем размерность вектора.
// Обе проверки выполняются до сети, потому что обе означают «этот запрос не
// может быть выполнен», а не «ответьте пустым списком».
func (i *Index) VectorSearchScoped(
	ctx context.Context,
	vec []float32,
	cfg domain.IndexingConfig,
	scope domain.AccessScope,
	limit int,
) ([]domain.SearchCandidate, error) {
	if scope.AllowsNothing() {
		return nil, nil
	}
	// Размерность — постоянная ошибка профиля, а не недоступность индекса.
	if len(vec) != cfg.EmbeddingDimension {
		return nil, domain.EmbeddingDimensionMismatch(
			"Размерность вектора не совпала с профилем", len(vec), cfg.EmbeddingDimension)
	}
	body := scopedVectorQuery(vec, cfg, scope, clampLimit(limit))
	return i.search(ctx, cfg.IndexName(), body)
}

// Адаптер реализует оба порта: старый SearchIndex (список документов) и новый
// SecuredSearchIndex (права в запросе). Проверка компиляцией — единственная
// защита от расхождения подписи с портом.
var (
	_ ports.SearchIndex        = (*Index)(nil)
	_ ports.SecuredSearchIndex = (*Index)(nil)
)
