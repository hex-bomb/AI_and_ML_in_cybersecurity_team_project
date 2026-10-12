package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/elastic/go-elasticsearch/v9"
	"github.com/elastic/go-elasticsearch/v9/esapi"
	"github.com/google/uuid"
	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Config — параметры подключения к Elasticsearch (из окружения, ТЗ 6.1).
type Config struct {
	// Addresses — список URL Elasticsearch.
	Addresses []string
	// Username и Password — базовая аутентификация. Пустые значения означают
	// кластер без безопасности: так на стенде, а политику продакшена задаёт
	// оператор, и проверять её здесь нечем.
	Username string
	Password string
}

// maxErrorBodyBytes — в лог и Details уходит максимум 2 КБ тела ошибки.
const maxErrorBodyBytes = 2048

// indexWildcard — все индексы профилей (чанки могут лежать в чужом индексе).
const indexWildcard = domain.IndexPrefix + "-*"

// Index — реализация ports.SearchIndex поверх Elasticsearch (без собственного состояния).
type Index struct {
	client *elasticsearch.Client
}

// New создаёт адаптер индекса (ошибка клиента — ServiceUnavailable).
func New(cfg Config) (*Index, error) {
	if len(cfg.Addresses) == 0 {
		return nil, domain.ServiceUnavailable("адрес Elasticsearch не задан").
			WithCause(errors.New(" Addresses пуст"))
	}
	options := []elasticsearch.Option{elasticsearch.WithAddresses(cfg.Addresses...)}
	if cfg.Username != "" || cfg.Password != "" {
		options = append(options, elasticsearch.WithBasicAuth(cfg.Username, cfg.Password))
	}
	client, err := elasticsearch.New(options...)
	if err != nil {
		return nil, domain.ServiceUnavailable("не удалось создать клиент Elasticsearch").WithCause(err)
	}
	return &Index{client: client}, nil
}

// NewWithClient создаёт адаптер поверх готового клиента (тестам и сборке с своим транспортом).
func NewWithClient(client *elasticsearch.Client) (*Index, error) {
	if client == nil {
		return nil, domain.ServiceUnavailable("клиент Elasticsearch не задан").
			WithCause(errors.New("NewWithClient(nil)"))
	}
	return &Index{client: client}, nil
}

// EnsureIndex создаёт индекс профиля или проверяет размерность вектора (ТЗ 6.3 п.6).
func (i *Index) EnsureIndex(ctx context.Context, cfg domain.IndexingConfig) (string, error) {
	name := cfg.IndexName()

	exists, err := i.indexExists(ctx, name)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := i.createIndex(ctx, name, cfg.EmbeddingDimension); err != nil {
			return "", err
		}
		return name, nil
	}

	dimension, err := i.indexDimension(ctx, name)
	if err != nil {
		return "", err
	}
	if dimension != cfg.EmbeddingDimension {
		return "", domain.IndexProfileMismatch("Размерность индекса не совпадает с профилем").
			WithDetails(map[string]any{"expected": cfg.EmbeddingDimension, "actual": dimension})
	}
	return name, nil
}

// indexExists спрашивает HEAD /<index> (без ignore_unavailable — иначе 404 пропадёт).
func (i *Index) indexExists(ctx context.Context, name string) (bool, error) {
	res, err := i.client.Indices.Exists([]string{name}, i.client.Indices.Exists.WithContext(ctx))
	if err != nil {
		return false, domain.IndexUnavailable("Elasticsearch недоступен").WithCause(err)
	}
	defer closeBody(res)
	switch {
	case res.StatusCode == http.StatusNotFound:
		return false, nil
	case res.IsError():
		return false, esError("проверка существования индекса не выполнена", res)
	default:
		return true, nil
	}
}

func (i *Index) createIndex(ctx context.Context, name string, dimension int) error {
	body, err := marshalBody(indexBody(dimension))
	if err != nil {
		return err
	}
	res, err := i.client.Indices.Create(name,
		i.client.Indices.Create.WithContext(ctx),
		i.client.Indices.Create.WithBody(bytes.NewReader(body)),
	)
	if err != nil {
		return domain.IndexUnavailable("Не удалось создать индекс").WithCause(err)
	}
	defer closeBody(res)
	if !res.IsError() {
		return nil
	}
	// Тело читается один раз на двоих с responseError.
	payload, _ := readErrorBody(res)
	// Гонка двух создателей индекса — не сбой (resource_already_exists_exception).
	if isAlreadyExists(payload) {
		return nil
	}
	return responseError("не удалось создать индекс", res.StatusCode, payload)
}

// isAlreadyExists распознаёт resource_already_exists_exception по типу (не тексту).
func isAlreadyExists(payload []byte) bool {
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return false
	}
	return envelope.Error.Type == "resource_already_exists_exception"
}

// indexDimension читает размерность вектора из mapping'а.
func (i *Index) indexDimension(ctx context.Context, name string) (int, error) {
	res, err := i.client.Indices.GetMapping(
		i.client.Indices.GetMapping.WithContext(ctx),
		i.client.Indices.GetMapping.WithIndex(name),
	)
	if err != nil {
		return 0, domain.IndexUnavailable("Elasticsearch недоступен").WithCause(err)
	}
	defer closeBody(res)
	if res.IsError() {
		return 0, esError("чтение mapping индекса не выполнено", res)
	}
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, domain.IndexUnavailable("чтение mapping индекса не выполнено").WithCause(err)
	}
	var parsed mappingResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return 0, domain.IndexUnavailable("mapping индекса не разобран").WithCause(err)
	}
	index, ok := parsed[name]
	if !ok {
		// Индекс удалён между exists и get_mapping — «профиля нет».
		return 0, nil
	}
	return index.Mappings.Properties["embedding"].Dims, nil
}

// validateChunks проверяет чанки против профиля (чужой профиль vs размерность — разные ошибки).
func validateChunks(cfg domain.IndexingConfig, profileID string, chunks []domain.Chunk) error {
	for _, chunk := range chunks {
		if chunk.EmbeddingProfileID != profileID {
			return domain.IndexProfileMismatch("Чанк чужого embedding-профиля").
				WithDetails(map[string]any{
					"chunk_id": chunk.ChunkID,
					"expected": profileID,
					"actual":   chunk.EmbeddingProfileID,
				})
		}
		if len(chunk.Embedding) != cfg.EmbeddingDimension {
			return domain.EmbeddingDimensionMismatch(
				"Размерность чанка не совпала с профилем",
				len(chunk.Embedding), cfg.EmbeddingDimension,
			).WithDetails(map[string]any{
				"chunk_id": chunk.ChunkID,
				"got":      len(chunk.Embedding),
				"want":     cfg.EmbeddingDimension,
			})
		}
	}
	return nil
}

// UpsertChunks пишет чанки батчем (валидация всех — до первого обращения к сети).
func (i *Index) UpsertChunks(ctx context.Context, cfg domain.IndexingConfig, chunks []domain.Chunk) (int, error) {
	if len(chunks) == 0 {
		return 0, nil
	}
	profileID := cfg.ProfileID()

	// Валидация всех чанков до сети (иначе частичная запись под видом сетевой ошибки).
	if err := validateChunks(cfg, profileID, chunks); err != nil {
		return 0, err
	}

	name, err := i.EnsureIndex(ctx, cfg)
	if err != nil {
		return 0, err
	}

	confirmed := 0
	for start := 0; start < len(chunks); start += bulkBatchSize {
		batch := chunks[start:min(start+bulkBatchSize, len(chunks))]

		var payload bytes.Buffer
		for _, chunk := range batch {
			source, err := marshalBody(toDocSource(chunk))
			if err != nil {
				return 0, err
			}
			// _id = chunk_id: повтор перезаписывает, а не плодит дубли (ТЗ 6.6).
			payload.WriteString(`{"index":{"_index":`)
			payload.WriteString(strconv.Quote(name))
			payload.WriteString(`,"_id":`)
			payload.WriteString(strconv.Quote(chunk.ChunkID))
			payload.WriteString("}}\n")
			payload.Write(source)
			payload.WriteByte('\n')
		}

		if err := i.sendBulk(ctx, bytes.NewReader(payload.Bytes()), len(batch)); err != nil {
			return 0, err
		}
		confirmed += len(batch)
	}

	// Refresh обязателен: иначе поиск не увидит чанки до пометки ready (ТЗ 6.3 п.7).
	if err := i.refresh(ctx, name); err != nil {
		return 0, err
	}
	return confirmed, nil
}

// bulkItem — один элемент ответа bulk.
type bulkItem struct {
	Status int `json:"status"`
	Error  *struct {
		Reason string `json:"reason"`
		Type   string `json:"type"`
	} `json:"error"`
	Index struct {
		ID string `json:"_id"`
	} `json:"index"`
}

// sendBulk отправляет батч; частичный успех — ошибка, а не уменьшенное число (ТЗ 6.3 п.6).
func (i *Index) sendBulk(ctx context.Context, body io.Reader, expected int) error {
	// _index уже в каждой операции, в URL не дублируется.
	res, err := i.client.Bulk(body,
		i.client.Bulk.WithContext(ctx),
		i.client.Bulk.WithRefresh("false"),
	)
	if err != nil {
		return domain.IndexUnavailable("Bulk upsert не выполнен").WithCause(err)
	}
	defer closeBody(res)
	if res.IsError() {
		return esError("Bulk upsert не выполнен", res)
	}

	var envelope struct {
		Errors bool                  `json:"errors"`
		Items  []map[string]bulkItem `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return domain.IndexUnavailable("Ответ bulk не разобран").WithCause(err)
	}
	if len(envelope.Items) != expected {
		return domain.IndexUnavailable("Ответ bulk содержит неожиданное число элементов").
			WithDetails(map[string]any{"expected": expected, "actual": len(envelope.Items)})
	}
	if !envelope.Errors {
		return nil
	}
	failed, sample := collectBulkFailures(envelope.Items)
	return domain.IndexUnavailable("Частичный успех bulk не считаем завершением").
		WithDetails(map[string]any{"failed": failed, "errors": sample})
}

// collectBulkFailures считает неуспешные item'ы (первые три причины — в Details).
func collectBulkFailures(items []map[string]bulkItem) (int, []any) {
	failed := 0
	var sample []any
	for _, item := range items {
		for _, op := range item {
			if op.Error == nil {
				continue
			}
			failed++
			if len(sample) < 3 {
				sample = append(sample, map[string]any{
					"chunk_id": op.Index.ID,
					"type":     op.Error.Type,
					"reason":   op.Error.Reason,
				})
			}
		}
	}
	return failed, sample
}

// DeleteVersion удаляет чанки версии документа (идемпотентна, ТЗ 4.2).
func (i *Index) DeleteVersion(ctx context.Context, tenantID, documentID, versionID string) (int, error) {
	return i.deleteByQuery(ctx, deleteQuery(tenantID, documentID, versionID))
}

// DeleteDocument удаляет все версии документа (идемпотентна, ТЗ 4.2).
func (i *Index) DeleteDocument(ctx context.Context, tenantID, documentID string) (int, error) {
	return i.deleteByQuery(ctx, deleteQuery(tenantID, documentID, ""))
}

func (i *Index) deleteByQuery(ctx context.Context, query map[string]any) (int, error) {
	body, err := marshalBody(query)
	if err != nil {
		return 0, err
	}
	res, err := i.client.DeleteByQuery([]string{indexWildcard}, bytes.NewReader(body),
		i.client.DeleteByQuery.WithContext(ctx),
		i.client.DeleteByQuery.WithIgnoreUnavailable(true),
		i.client.DeleteByQuery.WithRefresh(true),
	)
	if err != nil {
		return 0, domain.IndexUnavailable("Удаление чанков не выполнено").WithCause(err)
	}
	defer closeBody(res)
	if res.StatusCode == http.StatusNotFound {
		// Индексов нет вообще: удалять нечего, и это не ошибка.
		return 0, nil
	}
	if res.IsError() {
		return 0, esError("Удаление чанков не выполнено", res)
	}
	var parsed deleteResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return 0, domain.IndexUnavailable("Ответ удаления не разобран").WithCause(err)
	}
	return parsed.Deleted, nil
}

// KeywordSearch полнотекстовый поиск с обязательными фильтрами.
func (i *Index) KeywordSearch(
	ctx context.Context,
	text string,
	f domain.SearchFilter,
	limit int,
) ([]domain.SearchCandidate, error) {
	// Пустой allows — пустой результат без сети (пустой terms в ES означает «всё»).
	if f.AllowsNothing() || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	index, err := indexNameFor(f.EmbeddingProfileID)
	if err != nil {
		return nil, err
	}
	return i.search(ctx, index, keywordQuery(text, f, clampLimit(limit)))
}

// VectorSearch ищет по вектору; профиль обязателен в фильтре.
func (i *Index) VectorSearch(
	ctx context.Context,
	vec []float32,
	cfg domain.IndexingConfig,
	f domain.SearchFilter,
	limit int,
) ([]domain.SearchCandidate, error) {
	if f.AllowsNothing() {
		return nil, nil
	}
	// Размерность — до сети (постоянная ошибка профиля, не недоступность).
	if len(vec) != cfg.EmbeddingDimension {
		return nil, domain.EmbeddingDimensionMismatch(
			"Размерность вектора не совпала с профилем", len(vec), cfg.EmbeddingDimension)
	}
	index, err := indexNameFor(f.EmbeddingProfileID)
	if err != nil {
		return nil, err
	}
	return i.search(ctx, index, vectorQuery(vec, f, clampLimit(limit)))
}

// clampLimit держит size в границах контракта (1..100).
func clampLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > maxSearchLimit {
		return maxSearchLimit
	}
	return limit
}

// maxSearchLimit — верхняя граница страницы выдачи из контракта.
const maxSearchLimit = 100

// indexNameFor вычисляет имя индекса профиля (без HEAD — отсутствие индекса есть пустой результат).
func indexNameFor(profileID string) (string, error) {
	if strings.TrimSpace(profileID) == "" {
		return "", domain.IndexProfileMismatch("Профиль эмбеддинга не задан")
	}
	return domain.IndexNameForProfile(profileID), nil
}

// search выполняет запрос и разбирает ответ.
func (i *Index) search(ctx context.Context, index string, body map[string]any) ([]domain.SearchCandidate, error) {
	payload, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	res, err := i.client.Search(
		i.client.Search.WithContext(ctx),
		i.client.Search.WithIndex(index),
		i.client.Search.WithBody(bytes.NewReader(payload)),
	)
	if err != nil {
		return nil, domain.IndexUnavailable("Поисковый запрос не выполнен").WithCause(err)
	}
	defer closeBody(res)
	if res.StatusCode == http.StatusNotFound {
		// Индекс профиля ещё не создан — результат пустой, не ошибка.
		return nil, nil
	}
	if res.IsError() {
		return nil, esError("Поисковый запрос не выполнен", res)
	}
	var parsed searchResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, domain.IndexUnavailable("Ответ поиска не разобран").WithCause(err)
	}
	return toCandidates(parsed), nil
}

// toCandidates переводит hits в доменные кандидаты.
func toCandidates(res searchResponse) []domain.SearchCandidate {
	if len(res.Hits.Hits) == 0 {
		return nil
	}
	out := make([]domain.SearchCandidate, 0, len(res.Hits.Hits))
	for _, hit := range res.Hits.Hits {
		out = append(out, toCandidate(hit.Source, hit.Score))
	}
	return out
}

func toCandidate(src docSource, score float64) domain.SearchCandidate {
	sectionPath := make([]string, len(src.Locator.SectionPath))
	copy(sectionPath, src.Locator.SectionPath)
	return domain.SearchCandidate{
		ChunkID:     src.ChunkID,
		TenantID:    src.TenantID,
		KBID:        src.KBID,
		DocumentID:  src.DocumentID,
		VersionID:   src.VersionID,
		ChunkNumber: src.ChunkNumber,
		Title:       src.Title,
		Text:        src.Text,
		Locator: domain.Locator{
			BlockIDs:    parseBlockIDs(src.Locator.BlockIDs),
			SectionPath: sectionPath,
			PageStart:   copyInt(src.Locator.PageStart),
			PageEnd:     copyInt(src.Locator.PageEnd),
		},
		EmbeddingProfileID: src.EmbeddingProfileID,
		Score:              score,
	}
}

// parseBlockIDs разбирает идентификаторы блоков (битые пропускаются).
func parseBlockIDs(raw []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(raw))
	for _, value := range raw {
		parsed, err := uuid.Parse(value)
		if err != nil {
			continue
		}
		out = append(out, parsed)
	}
	return out
}

func (i *Index) refresh(ctx context.Context, name string) error {
	res, err := i.client.Indices.Refresh(
		i.client.Indices.Refresh.WithContext(ctx),
		i.client.Indices.Refresh.WithIndex(name),
		i.client.Indices.Refresh.WithIgnoreUnavailable(true),
	)
	if err != nil {
		return domain.IndexUnavailable("Refresh индекса не выполнен").WithCause(err)
	}
	defer closeBody(res)
	if res.IsError() {
		return esError("Refresh индекса не выполнен", res)
	}
	return nil
}

// Ping проверяет кластер через / (поиск с пустым фильтром соврал бы «доступен»).
func (i *Index) Ping(ctx context.Context) error {
	res, err := i.client.Info(i.client.Info.WithContext(ctx))
	if err != nil {
		return domain.IndexUnavailable("Elasticsearch недоступен").WithCause(err)
	}
	defer closeBody(res)
	if res.IsError() {
		return esError("Elasticsearch не готов", res)
	}
	return nil
}

// Close освобождает соединения клиента (метод адаптера, не порта).
func (i *Index) Close(ctx context.Context) error { return i.client.Close(ctx) }

// esError строит доменную ошибку из ошибочного ответа, прочитав его тело.
func esError(message string, res *esapi.Response) error {
	payload, _ := readErrorBody(res)
	return responseError(message, res.StatusCode, payload)
}

// responseError превращает ответ ES в AppError (текст кластера — в Details, не в Message).
func responseError(message string, status int, payload []byte) error {
	details := map[string]any{"status": status}
	if reason := errorReason(payload); reason != "" {
		details["reason"] = reason
	}
	err := domain.IndexUnavailable(message).WithDetails(details)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Отказ аутентификации повтором не лечится.
		return domain.ServiceUnavailable("Elasticsearch отклонил аутентификацию").WithCause(err)
	}
	return err
}

// errorReason достаёт текст ошибки кластера из тела ответа.
func errorReason(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Error) == 0 {
		return truncate(string(payload))
	}
	return truncate(string(envelope.Error))
}

// readErrorBody читает не больше maxErrorBodyBytes тела ответа.
func readErrorBody(res *esapi.Response) ([]byte, error) {
	if res == nil || res.Body == nil {
		return nil, errors.New("пустой ответ")
	}
	return io.ReadAll(io.LimitReader(res.Body, maxErrorBodyBytes))
}

// closeBody закрывает тело ответа.
func closeBody(res *esapi.Response) {
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
}

func truncate(s string) string {
	if len(s) <= maxErrorBodyBytes {
		return s
	}
	return s[:maxErrorBodyBytes] + "…"
}

var _ ports.SearchIndex = (*Index)(nil)
