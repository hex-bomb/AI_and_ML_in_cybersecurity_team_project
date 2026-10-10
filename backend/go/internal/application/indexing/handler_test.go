package indexing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/indexing"
	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

var (
	otherTenantID = uuid.MustParse("99999999-9999-4999-8999-999999999999")
)

// stubQueryer — заглушка доступа к SQL: сценарий только передаёт его портам,
// которые в тесте подменены.
type stubQueryer struct{}

func (stubQueryer) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, context.Canceled
}

func (stubQueryer) QueryRow(context.Context, string, ...any) ports.Row { return nil }

func (stubQueryer) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, context.Canceled
}

// stubStorage — хранилище файлов: сценарий индексации читает разобранный
// документ через порт и ничего в хранилище не меняет.
type stubStorage struct{}

func (s *stubStorage) Put(context.Context, string, []byte) error { return nil }

func (s *stubStorage) Read(context.Context, string) ([]byte, error) {
	return []byte("{}"), nil
}

func (s *stubStorage) Delete(context.Context, string) error { return nil }

// stubSettings отдаёт заранее заданный профиль.
type stubSettings struct {
	cfg domain.IndexingConfig
	err error
	kb  string
}

func (s *stubSettings) IndexingConfig(
	_ context.Context, _ ports.Queryer, _, kbID string,
) (domain.IndexingConfig, error) {
	s.kb = kbID
	if s.err != nil {
		return domain.IndexingConfig{}, s.err
	}
	return s.cfg, nil
}

// stubDocuments отдаёт готовый ParsedDocument и записывает активацию.
type stubDocuments struct {
	parsed    domain.ParsedDocument
	loadErr   error
	previous  string
	activate  domain.ActivateParams
	activated bool
}

func (s *stubDocuments) GetAllowedActiveVersions(
	context.Context, ports.Queryer, domain.Principal, string,
) ([]domain.ActiveVersion, error) {
	return nil, nil
}

func (s *stubDocuments) LoadParsed(
	context.Context, ports.Queryer, ports.FileStorage, string, string,
) (domain.ParsedDocument, error) {
	if s.loadErr != nil {
		return domain.ParsedDocument{}, s.loadErr
	}
	return s.parsed, nil
}

func (s *stubDocuments) VersionSources(
	context.Context, ports.Queryer, domain.Principal, string, map[string]string,
) (map[string]string, error) {
	return nil, nil
}

func (s *stubDocuments) ActivateVersion(
	_ context.Context, _ ports.Queryer, p domain.ActivateParams,
) (string, error) {
	s.activate = p
	s.activated = true
	return s.previous, nil
}

func (s *stubDocuments) IngestBusy(context.Context, ports.Queryer, string) (bool, error) {
	return false, nil
}

func (s *stubDocuments) IsVersionActive(
	context.Context, ports.Queryer, string, string,
) (bool, error) {
	return false, nil
}

// stubEmbedder считает вектора фиксированной размерности по числу текстов.
type stubEmbedder struct {
	dimension int
	err       error
	// shortCount заставляет вернуть меньше векторов, чем текстов.
	shortCount bool
	texts      []string
}

func (s *stubEmbedder) Embed(
	_ context.Context, texts []string, _ domain.IndexingConfig,
) ([][]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.texts = texts
	n := len(texts)
	if s.shortCount && n > 0 {
		n--
	}
	out := make([][]float32, n)
	for i := range out {
		out[i] = make([]float32, s.dimension)
		out[i][0] = 1
	}
	return out, nil
}

func (s *stubEmbedder) EmbedQuery(
	context.Context, string, domain.IndexingConfig,
) ([]float32, error) {
	return make([]float32, s.dimension), nil
}

func (s *stubEmbedder) Dimensions() int            { return s.dimension }
func (s *stubEmbedder) Ping(context.Context) error { return nil }
func (s *stubEmbedder) Close() error               { return nil }

// stubIndex записывает порядок обращений к индексу: решение об идемпотентности
// проверяется по последовательности вызовов, а не по конечному состоянию.
type stubIndex struct {
	calls  []string
	chunks []domain.Chunk
	upsert error
	delErr error
	// shortWrite имитирует частичный успех bulk: подтверждено меньше
	// записей, чем отправлено.
	shortWrite bool
}

func (s *stubIndex) EnsureIndex(context.Context, domain.IndexingConfig) (string, error) {
	return "kb-chunks-v1-test", nil
}

func (s *stubIndex) UpsertChunks(
	_ context.Context, _ domain.IndexingConfig, chunks []domain.Chunk,
) (int, error) {
	s.calls = append(s.calls, "upsert")
	s.chunks = chunks
	if s.upsert != nil {
		return 0, s.upsert
	}
	if s.shortWrite {
		return len(chunks) - 1, nil
	}
	return len(chunks), nil
}

func (s *stubIndex) DeleteVersion(
	context.Context, string, string, string,
) (int, error) {
	s.calls = append(s.calls, "delete")
	return 0, s.delErr
}

func (s *stubIndex) DeleteDocument(context.Context, string, string) (int, error) {
	return 0, nil
}

func (s *stubIndex) KeywordSearch(
	context.Context, string, domain.SearchFilter, int,
) ([]domain.SearchCandidate, error) {
	return nil, nil
}

func (s *stubIndex) VectorSearch(
	context.Context, []float32, domain.IndexingConfig, domain.SearchFilter, int,
) ([]domain.SearchCandidate, error) {
	return nil, nil
}

func (s *stubIndex) Ping(context.Context) error { return nil }

// stubJobs записывает завершение задания и постановку уборки.
type stubJobs struct {
	result      domain.IndexingResult
	cleanup     []string
	finishErr   error
	cleanupErr  error
	finishCalls int
}

func (s *stubJobs) Claim(
	context.Context, ports.Queryer, string, []string,
) (*domain.JobLease, error) {
	return nil, nil
}

func (s *stubJobs) Heartbeat(context.Context, ports.Queryer, string, string) (bool, error) {
	return true, nil
}

func (s *stubJobs) FinishIndex(
	_ context.Context, _ ports.Queryer, _, _ string, res domain.IndexingResult,
) error {
	s.finishCalls++
	s.result = res
	return s.finishErr
}

func (s *stubJobs) Fail(
	context.Context, ports.Queryer, string, string, string, string, bool,
) error {
	return nil
}

func (s *stubJobs) EnqueueCleanup(
	_ context.Context, _ ports.Queryer, _, _, versionID string,
) (string, error) {
	if s.cleanupErr != nil {
		return "", s.cleanupErr
	}
	s.cleanup = append(s.cleanup, versionID)
	return uuid.NewString(), nil
}

// recordingTransactor считает транзакции: активация версии, завершение
// задания и постановка уборки обязаны идти в одной (ТЗ 6.3 п.7-8).
type recordingTransactor struct {
	calls int
}

func (t *recordingTransactor) WithTx(
	ctx context.Context, fn func(ports.Queryer) error,
) error {
	t.calls++
	if err := fn(stubQueryer{}); err != nil {
		return err
	}
	return nil
}

// harness собирает сценарий индексации на подделках.
type harness struct {
	indexer   *indexing.Indexer
	documents *stubDocuments
	index     *stubIndex
	jobs      *stubJobs
	settings  *stubSettings
	embedder  *stubEmbedder
	tx        *recordingTransactor
	lease     domain.JobLease
}

func newHarness(t *testing.T, cfg domain.IndexingConfig) *harness {
	t.Helper()

	parsed := document(block(0, []string{"Раздел"}, words(6)), block(1, []string{"Раздел"}, words(5)))
	h := &harness{
		documents: &stubDocuments{parsed: parsed},
		index:     &stubIndex{},
		jobs:      &stubJobs{},
		settings:  &stubSettings{cfg: cfg},
		embedder:  &stubEmbedder{dimension: cfg.EmbeddingDimension},
		tx:        &recordingTransactor{},
	}
	h.lease = domain.JobLease{
		JobID:      uuid.New(),
		TenantID:   parsed.TenantID,
		DocumentID: parsed.DocumentID,
		VersionID:  &parsed.VersionID,
		Kind:       domain.KindIngest,
		Stage:      domain.StageIndex,
		Attempt:    1,
		LeaseToken: "lease-token",
	}
	h.indexer = indexing.NewIndexer(indexing.Deps{
		Documents:  h.documents,
		Jobs:       h.jobs,
		Settings:   h.settings,
		Embedder:   h.embedder,
		Index:      h.index,
		Storage:    &stubStorage{},
		Queryer:    stubQueryer{},
		Transactor: h.tx,
	})
	return h
}

// TestIndexerWritesChunksAndPublishes — полный успешный путь: разобранный
// документ нарезан, посчитан вектор, индекс обновлён, версия активирована и
// задание завершено в одной транзакции (ТЗ 6.3 п.1-8).
func TestIndexerWritesChunksAndPublishes(t *testing.T) {
	cfg := testConfig(20, 4)
	h := newHarness(t, cfg)
	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}

	want := expectedChunks(t, cfg)
	if h.jobs.finishCalls != 1 {
		t.Fatalf("FinishIndex вызван %d раз, ожидался 1", h.jobs.finishCalls)
	}
	if h.jobs.result.ChunkCount != want {
		t.Errorf("chunk_count %d вместо %d", h.jobs.result.ChunkCount, want)
	}
	if h.jobs.result.Status != domain.IndexingStatusReady {
		t.Errorf("статус результата %q вместо ready", h.jobs.result.Status)
	}
	if h.jobs.result.EmbeddingProfileID != cfg.ProfileID() {
		t.Error("в результате другой embedding-профиль")
	}
	if h.documents.activate.LeaseToken != h.lease.LeaseToken {
		t.Error("активация выполнена с чужим lease token")
	}
	if h.documents.activate.ChunkCount != want {
		t.Errorf("активация версии с chunk_count %d вместо %d",
			h.documents.activate.ChunkCount, want)
	}
	if h.settings.kb != h.documents.parsed.KBID.String() {
		t.Errorf("профиль запрошен для базы %q вместо %q", h.settings.kb, h.documents.parsed.KBID)
	}
	if h.tx.calls != 1 {
		t.Errorf("транзакций %d, ожидалась 1: активация и завершение должны идти вместе", h.tx.calls)
	}
}

// expectedChunks — сколько чанков даёт нарезка эталонного документа. Считается
// той же нарезкой, чтобы тест не дублировал её ожидания вручную.
func expectedChunks(t *testing.T, cfg domain.IndexingConfig) int {
	t.Helper()
	drafts, err := indexing.Chunk(document(
		block(0, []string{"Раздел"}, words(6)),
		block(1, []string{"Раздел"}, words(5)),
	), cfg)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	return len(drafts)
}

// TestIndexerDeletesVersionBeforeUpsert — идемпотентность повторной
// обработки. chunk_id детерминирован для заданных параметров нарезки, но сами
// параметры берутся из настроек и могут смениться между попытками: без
// удаления чанки прежней попытки остались бы в индексе навсегда.
func TestIndexerDeletesVersionBeforeUpsert(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))

	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(h.index.calls) != 2 || h.index.calls[0] != "delete" || h.index.calls[1] != "upsert" {
		t.Errorf("порядок обращений к индексу %v, ожидался [delete upsert]", h.index.calls)
	}
}

// TestIndexerFailsWhenIndexReportsFewerChunks — частичный успех не считается
// завершением: версия ready с недописанными чанками искалась бы вслепую.
func TestIndexerFailsWhenIndexReportsFewerChunks(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	h.index.shortWrite = true

	err := h.indexer.Index(context.Background(), h.lease)
	if err == nil {
		t.Fatal("ожидалась ошибка частичной записи")
	}
	if h.documents.activated || h.jobs.finishCalls != 0 {
		t.Error("версия активирована при неполной записи чанков")
	}
}

// TestIndexerRollsBackOnFinishFailure — падение FinishIndex откатывает
// активацию: версия без записанных чанков не должна стать видимой.
func TestIndexerRollsBackOnFinishFailure(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	h.jobs.finishErr = errors.New("нет соединения")

	if err := h.indexer.Index(context.Background(), h.lease); err == nil {
		t.Fatal("ожидалась ошибка транзакции")
	}
	if h.jobs.cleanup != nil {
		t.Error("уборка поставлена при откатившейся транзакции")
	}
}

// TestIndexerEnqueuesCleanupOfPreviousVersion — прежняя активная версия
// убирается отдельной задачей в той же транзакции (ТЗ 6.3 п.8).
func TestIndexerEnqueuesCleanupOfPreviousVersion(t *testing.T) {
	previous := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	h := newHarness(t, testConfig(20, 4))
	h.documents.previous = previous.String()

	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(h.jobs.cleanup) != 1 || h.jobs.cleanup[0] != previous.String() {
		t.Errorf("поставлена уборка %v вместо [%s]", h.jobs.cleanup, previous)
	}
}

// TestIndexerSkipsCleanupWithoutPreviousVersion — у первой версии документа
// убирать нечего, и пустая задача только раздувала бы очередь.
func TestIndexerSkipsCleanupWithoutPreviousVersion(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))

	if err := h.indexer.Index(context.Background(), h.lease); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(h.jobs.cleanup) != 0 {
		t.Errorf("поставлена лишняя уборка %v", h.jobs.cleanup)
	}
}

// TestIndexerRejectsForeignParsedDocument — файл на диске пишет конвертер, и
// читать его должен тот же документ, lease которого держит воркер.
func TestIndexerRejectsForeignParsedDocument(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	h.documents.parsed.TenantID = otherTenantID

	err := h.indexer.Index(context.Background(), h.lease)
	if err == nil {
		t.Fatal("ожидался отказ по чужому разобранному документу")
	}
	if len(h.index.calls) != 0 {
		t.Error("индекс затронут до проверки принадлежности версии")
	}
}

// TestIndexerRejectsJobWithoutVersion — задание стадии index без версии
// невозможно обработать, и молча пропустить его нельзя.
func TestIndexerRejectsJobWithoutVersion(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	lease := h.lease
	lease.VersionID = nil

	if err := h.indexer.Index(context.Background(), lease); err == nil {
		t.Fatal("ожидался отказ по заданию без версии")
	}
	if len(h.index.calls) != 0 {
		t.Error("индекс затронут для задания без версии")
	}
}

// TestIndexerRejectsWrongVectorCount — вектора идут в том же порядке, что и
// тексты; несовпадение молча переставило бы их между чанками.
func TestIndexerRejectsWrongVectorCount(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	h.embedder.shortCount = true

	err := h.indexer.Index(context.Background(), h.lease)
	if err == nil {
		t.Fatal("ожидалась ошибка числа векторов")
	}
	if h.documents.activated {
		t.Error("версия активирована при неверном числе векторов")
	}
}

// TestIndexerPropagatesEmbeddingError — ошибка эмбеддера постоянная, повтор её
// не исправит, и она обязана дойти до очереди как своя.
func TestIndexerPropagatesEmbeddingError(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	h.embedder.err = domain.EmbeddingDimensionMismatch("размерность", 128, 384)

	err := h.indexer.Index(context.Background(), h.lease)
	if !errors.Is(err, domain.ErrEmbeddingDimensionMismatch) {
		t.Fatalf("получена %v вместо EMBEDDING_DIMENSION_MISMATCH", err)
	}
	if len(h.index.calls) != 0 {
		t.Error("индекс затронут при ошибке эмбеддера")
	}
}

// TestIndexerPropagatesSettingsError — неверные настройки не должны выглядеть
// как пустой документ: оператор чинит ключи по коду ошибки.
func TestIndexerPropagatesSettingsError(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	h.settings.err = domain.ConfigurationMissing("", map[string]any{"keys": []string{"chunking.max_tokens"}})

	err := h.indexer.Index(context.Background(), h.lease)
	if !errors.Is(err, domain.ErrConfigurationMissing) {
		t.Fatalf("получена %v вместо CONFIGURATION_MISSING", err)
	}
	if len(h.embedder.texts) != 0 {
		t.Error("эмбеддер вызван без профиля индексации")
	}
}

// TestIndexerLeavesIndexUntouchedOnFailureLoadParsed — без разобранного
// документа версия не активируется, и индекс остаётся в прежнем состоянии.
func TestIndexerLeavesIndexUntouchedOnFailureLoadParsed(t *testing.T) {
	h := newHarness(t, testConfig(20, 4))
	h.documents.loadErr = domain.NotFound("Версия документа не найдена")

	if err := h.indexer.Index(context.Background(), h.lease); err == nil {
		t.Fatal("ожидалась ошибка чтения версии")
	}
	if len(h.index.calls) != 0 || h.documents.activated {
		t.Error("индекс или активная версия изменены при ошибке чтения")
	}
}
