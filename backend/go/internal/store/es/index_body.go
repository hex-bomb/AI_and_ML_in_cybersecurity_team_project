// Package elasticsearch реализует порт ports.SearchIndex поверх
// Elasticsearch (ТЗ 6.4). Вендор сменён с OpenSearch без смены семантики.
package elasticsearch

import (
	"encoding/json"

	"github.com/kbsearch/kb/internal/envelope"
)

// russianAnalyzer — встроенный русский анализатор со стеммингом (своего словаря не ведём).
const russianAnalyzer = "russian"

// bulkBatchSize — размер батча bulk-запроса (ТЗ 6.3 п.6).
const bulkBatchSize = 500

// indexBody собирает тело запроса создания индекса (реплик 0 — один узел;
// index_options не задаём — тип HNSW выбирает кластер).
//
// Про версии индекса (§5: «переиндексация идёт новым полем vec_v2 и
// переключением alias», §4: «индексы Elasticsearch на базе alias»). Имя
// индекса — kb-chunks-v1-<profile_id>, и profile_id считается по шести полям
// векторного профиля (IndexingConfig.ProfileID), а не по mapping'у. Поэтому
// добавление space_id и acl НЕ меняет имя индекса и не ломает схему алиасов:
// это обычное аддитивное изменение mapping'а, попадающее в ту же v1-схему.
//
// Но есть эксплуатационная деталь, о которой стоит знать: EnsureIndex создаёт
// индекс, только если его нет, и для существующего НЕ применяет mapping. Старый
// индекс, созданный до этих полей, их не получит и продолжит отдавать пустую
// выдачу всем — то есть ровно тот отказ, который закрывает эта правка. Лечение
// то же, что у §5 для смены модели: новый индекс через новый профиль плюс
// переключение алиаса, либо пересоздание индекса из lake (lake — источник
// правды, §1). Ни то, ни другое не требует правок в этом файле.
func indexBody(dimension int) map[string]any {
	return map[string]any{
		"settings": map[string]any{"index": map[string]any{
			"number_of_shards":   1,
			"number_of_replicas": 0,
		}},
		"mappings": map[string]any{
			"dynamic": "strict",
			"properties": map[string]any{
				"schema_version": map[string]any{"type": "keyword"},
				"chunk_id":       map[string]any{"type": "keyword"},
				"tenant_id":      map[string]any{"type": "keyword"},
				"kb_id":          map[string]any{"type": "keyword"},
				"document_id":    map[string]any{"type": "keyword"},
				"version_id":     map[string]any{"type": "keyword"},
				// Поля прав §6. Имена и тип берутся из констант запроса
				// (acl_filter.go): сторона запроса и сторона индекса обязаны
				// называть поля одинаково. Тип обязан быть keyword, а не text:
				// анализатор разобьёт «user:anna» на части, exact-совпадение
				// перестанет работать, и фильтр прав откажется видеть документ
				// своему же владельцу — молча, без ошибки.
				aclFieldSpace:          map[string]any{"type": "keyword"},
				aclFieldPrincipals:     map[string]any{"type": "keyword"},
				"embedding_profile_id": map[string]any{"type": "keyword"},
				"chunk_number":         map[string]any{"type": "integer"},
				"title":                map[string]any{"type": "text", "analyzer": russianAnalyzer},
				"text":                 map[string]any{"type": "text", "analyzer": russianAnalyzer},
				"locator": map[string]any{
					"properties": map[string]any{
						"block_ids":    map[string]any{"type": "keyword"},
						"section_path": map[string]any{"type": "keyword"},
						"page_start":   map[string]any{"type": "integer"},
						"page_end":     map[string]any{"type": "integer"},
					},
				},
				"embedding": map[string]any{
					"type":       "dense_vector",
					"dims":       dimension,
					"index":      true,
					"similarity": "cosine",
				},
			},
		},
	}
}

// docSource — документ индекса в _source.
//
// SpaceID и ACL — вторая половина контракта §6, первая половина живёт в
// acl_filter.go и строит фильтр из запроса. Здесь они просто едут вместе с
// документом (§4, стадия index: «Upsert в Elasticsearch вместе с ACL»).
//
// Про пустой ACL: отсутствующие исключения пишутся пустым массивом, а не null и
// не «прочим доступом». Документ без исключений виден ровно по своему
// space_id, и это fail-closed: terms по пустому массиву не совпадает ни с чем,
// а ветка запроса по space_id остаётся единственной, которая вообще может
// совпасть. Наоборот, «прочий доступ всем» здесь означал бы утечку всего
// тенанта — тот же класс ошибки, что document_id: null в filterClauses.
//
// Про document-level security Elasticsearch: он здесь намеренно не
// используется, и объяснение этому дано в шапке acl_filter.go — не дублируем
// его. Коротко: наши пользователи не пользователи ES, terms-lookup в ролевых
// запросах не поддерживается, статистика релевантности считается без учёта
// фильтра роли, а агрегации могут раскрывать термины из недоступных документов.
// Не «упрощайте» это переездом на DLS.
type docSource struct {
	SchemaVersion      string     `json:"schema_version"`
	ChunkID            string     `json:"chunk_id"`
	TenantID           string     `json:"tenant_id"`
	KBID               string     `json:"kb_id"`
	DocumentID         string     `json:"document_id"`
	VersionID          string     `json:"version_id"`
	SpaceID            string     `json:"space_id"`
	ACL                []string   `json:"acl"`
	ChunkNumber        int        `json:"chunk_number"`
	Title              string     `json:"title"`
	Text               string     `json:"text"`
	Locator            docLocator `json:"locator"`
	EmbeddingProfileID string     `json:"embedding_profile_id"`
	Embedding          []float32  `json:"embedding"`
}

// docLocator — локатор в _source.
type docLocator struct {
	BlockIDs    []string `json:"block_ids"`
	SectionPath []string `json:"section_path"`
	PageStart   *int     `json:"page_start"`
	PageEnd     *int     `json:"page_end"`
}

// toDocSource переводит доменный чанк в документ индекса (слайсы копируются).
//
// Права берутся из chunk.Rights и копируются: чанк принадлежит сценарию
// индексации и может быть переиспользован, а общий срез в _source — это
// гонка данных между стадиями. ExceptionTerms() к тому же возвращает новый
// срез и никогда не nil, так что acl не может сериализоваться как null.
func toDocSource(chunk domain.Chunk) docSource {
	blockIDs := make([]string, 0, len(chunk.Locator.BlockIDs))
	for _, id := range chunk.Locator.BlockIDs {
		blockIDs = append(blockIDs, id.String())
	}
	sectionPath := make([]string, len(chunk.Locator.SectionPath))
	copy(sectionPath, chunk.Locator.SectionPath)
	embedding := make([]float32, len(chunk.Embedding))
	copy(embedding, chunk.Embedding)

	return docSource{
		SchemaVersion: chunk.SchemaVersion,
		ChunkID:       chunk.ChunkID,
		TenantID:      chunk.TenantID,
		KBID:          chunk.KBID,
		DocumentID:    chunk.DocumentID,
		VersionID:     chunk.VersionID,
		SpaceID:       chunk.Rights.SpaceID,
		ACL:           chunk.Rights.ExceptionTerms(),
		ChunkNumber:   chunk.ChunkNumber,
		Title:         chunk.Title,
		Text:          chunk.Text,
		Locator: docLocator{
			BlockIDs:    blockIDs,
			SectionPath: sectionPath,
			PageStart:   copyInt(chunk.Locator.PageStart),
			PageEnd:     copyInt(chunk.Locator.PageEnd),
		},
		EmbeddingProfileID: chunk.EmbeddingProfileID,
		Embedding:          embedding,
	}
}

func copyInt(v *int) *int {
	if v == nil {
		return nil
	}
	value := *v
	return &value
}

// filterClauses — обязательные фильтры тенанта, базы, профиля и документов (ТЗ 2.1, 6.4).
func filterClauses(f domain.SearchFilter) []any {
	allowed := f.AllowedDocumentIDs
	if allowed == nil {
		// null в terms означал бы «не ограничивать».
		allowed = []string{}
	}
	return []any{
		map[string]any{"term": map[string]any{"tenant_id": f.TenantID}},
		map[string]any{"term": map[string]any{"kb_id": f.KBID}},
		map[string]any{"term": map[string]any{"embedding_profile_id": f.EmbeddingProfileID}},
		map[string]any{"terms": map[string]any{"document_id": allowed}},
	}
}

// keywordQuery — тело полнотекстового поиска (multi_match без fuzziness).
func keywordQuery(text string, f domain.SearchFilter, limit int) map[string]any {
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
				"filter": filterClauses(f),
			},
		},
	}
}

// vectorQuery — тело поиска по вектору (k = max(limit, 10) ради фильтрации).
func vectorQuery(vec []float32, f domain.SearchFilter, limit int) map[string]any {
	k := limit
	if k < minKNNK {
		k = minKNNK
	}
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
						},
					},
				},
				"filter": filterClauses(f),
			},
		},
	}
}

// minKNNK — нижняя граница k (max(limit, 10)).
const minKNNK = 10

// numCandidates — размер множества кандидатов HNSW.
func numCandidates(k int) int {
	n := k * 10
	if n < minNumCandidates {
		n = minNumCandidates
	}
	return n
}

const minNumCandidates = 100

// deleteQuery — запрос удаления чанков версии или документа (ТЗ 4.2).
func deleteQuery(tenantID, documentID, versionID string) map[string]any {
	clauses := []any{
		map[string]any{"term": map[string]any{"tenant_id": tenantID}},
		map[string]any{"term": map[string]any{"document_id": documentID}},
	}
	if versionID != "" {
		clauses = append(clauses, map[string]any{"term": map[string]any{"version_id": versionID}})
	}
	return map[string]any{"query": map[string]any{"bool": map[string]any{"filter": clauses}}}
}

// searchResponse — ответ _search в виде, нужном адаптеру.
type searchResponse struct {
	Hits struct {
		Hits []struct {
			Score  float64   `json:"_score"`
			Source docSource `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// deleteResponse — ответ _delete_by_query.
type deleteResponse struct {
	Deleted int `json:"deleted"`
}

// mappingProperties — свойства полей в mapping'е.
type mappingProperties map[string]struct {
	Type       string `json:"type"`
	Dims       int    `json:"dims"`
	Similarity string `json:"similarity"`
	Analyzer   string `json:"analyzer"`
	// IndexOptions — выбранный кластером тип HNSW для dense_vector.
	IndexOptions struct {
		Type string `json:"type"`
	} `json:"index_options"`
}

// indexMapping — mapping одного индекса.
type indexMapping struct {
	Mappings struct {
		Properties mappingProperties `json:"properties"`
	} `json:"mappings"`
}

// mappingResponse — ответ _mapping: индекс -> mapping.
type mappingResponse map[string]indexMapping

// marshalBody сериализует тело запроса (ошибка — баг кода, не недоступность).
func marshalBody(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, domain.IndexUnavailable("не удалось собрать тело запроса к индексу").
			WithCause(err)
	}
	return data, nil
}
