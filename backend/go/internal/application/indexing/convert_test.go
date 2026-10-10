package indexing_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/indexing"
	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// convertHarness собирает стадию convert на подделках: очередь, хранилище и
// репозиторий версий. Проверяется порядок и содержимое записи, а не то, что
// «Convert вернул nil».
type convertHarness struct {
	converter *indexing.Converter
	documents *stubDocuments
	jobs      *stubJobs
	storage   *memoryStorage
	tx        *recordingTransactor
	lease     domain.JobLease
	version   domain.ConvertVersion
}

// convertVersion — версия в базе: оригинал на диске плюс атрибуты разбора.
func convertVersion(filename string, original []byte) domain.ConvertVersion {
	source := "https://portal.example/guide"
	return domain.ConvertVersion{
		Filename:      filename,
		Title:         "Инструкция по выгрузке",
		SourceURL:     &source,
		Metadata:      map[string]any{"product": "portal"},
		ContentSHA256: "",
		Content:       original,
	}
}

func newConvertHarness(t *testing.T, filename string, original []byte) *convertHarness {
	t.Helper()

	version := convertVersion(filename, original)
	docs := &stubDocuments{convert: version}
	jobs := &stubJobs{}
	storage := &memoryStorage{}
	tx := &recordingTransactor{}

	h := &convertHarness{
		documents: docs, jobs: jobs, storage: storage, tx: tx, version: version,
	}
	// Идентификаторы берутся у эталонного ParsedDocument нарезки: он уже
	// зашит в тесты стадии index, и общий источник исключает расхождение
	// tenant/document/version между двумя наборами тестов.
	ids := document()
	h.lease = domain.JobLease{
		JobID:      uuid.New(),
		TenantID:   ids.TenantID,
		DocumentID: ids.DocumentID,
		VersionID:  &ids.VersionID,
		KBID:       ids.KBID,
		Kind:       domain.KindIngest,
		Stage:      domain.StageConvert,
		Attempt:    1,
		LeaseToken: "lease-token",
	}
	h.converter = indexing.NewConverter(indexing.Deps{
		Documents:  h.documents,
		Jobs:       h.jobs,
		Storage:    h.storage,
		Queryer:    stubQueryer{},
		Transactor: h.tx,
		ParsedKey:  parsedKeyFor,
	})
	return h
}

func parsedKeyFor(tenantID, versionID string) string {
	return tenantID + "/" + versionID + "/parsed.json"
}

// versionKey — ключ parsed.json этой версии. Вынесен, потому что тест сверяет
// его в нескольких местах, и разъезжающиеся литералы читались бы как ошибка
// хранилища, а не как ошибка теста.
func (h *convertHarness) versionKey() string {
	versionID, _ := h.lease.VersionIDValue()
	return parsedKeyFor(h.lease.TenantID.String(), versionID.String())
}

// TestConverterParsesAndAdvancesJob — полный успешный путь стадии convert:
// оригинал прочитан, документ разобран, JSON записан в хранилище, а задание
// переведено на index одной транзакцией вместе с записью parsed_key.
func TestConverterParsesAndAdvancesJob(t *testing.T) {
	h := newConvertHarness(t, "export-guide.md", []byte("# Раздел\n\nТекст документа.\n"))

	if err := h.converter.Convert(context.Background(), h.lease); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if h.jobs.convertCalls != 1 {
		t.Fatalf("FinishConvert вызван %d раз, ожидался 1", h.jobs.convertCalls)
	}
	if h.jobs.parsedKey != h.versionKey() {
		t.Errorf("parsed_key %q вместо %q", h.jobs.parsedKey, h.versionKey())
	}
	if h.jobs.blockCount != 2 {
		t.Errorf("в результате %d блоков, ожидалось 2", h.jobs.blockCount)
	}
	if h.jobs.leaseToken != h.lease.LeaseToken {
		t.Error("задание завершено с чужим lease token")
	}
	if h.tx.calls != 1 {
		t.Errorf("транзакций %d, ожидалась 1: parsed_key и перевод задания должны идти вместе",
			h.tx.calls)
	}
	if h.storage.puts != 1 {
		t.Errorf("записей в хранилище %d, ожидалась 1", h.storage.puts)
	}

	parsed, ok := loadStored(t, h.storage, h.versionKey())
	if !ok {
		t.Fatal("parsed.json не записан в хранилище")
	}
	if len(parsed.Blocks) != 2 {
		t.Errorf("в parsed.json %d блоков, ожидалось 2", len(parsed.Blocks))
	}
	if parsed.SchemaVersion != "1.0" {
		t.Errorf("schema_version %q вместо 1.0", parsed.SchemaVersion)
	}
	if parsed.Title != "Инструкция по выгрузке" || parsed.Filename != "export-guide.md" {
		t.Errorf("title/filename не перенесены из версии: %q/%q", parsed.Title, parsed.Filename)
	}
	if parsed.SourceURL == nil || *parsed.SourceURL != "https://portal.example/guide" {
		t.Error("source_url версии не перенесён в ParsedDocument")
	}
	if parsed.Metadata["product"] != "portal" {
		t.Errorf("metadata версии не перенесена: %v", parsed.Metadata)
	}
}

// TestConverterStoredDocumentMatchesLease — файл на диске пишет конвертер по
// lease конкретного документа: подмена чужим содержимым осиротила бы чанки
// этой версии, а чужой tenant — ещё и нарушила бы изоляцию.
func TestConverterStoredDocumentMatchesLease(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("# Раздел\n\nтекст\n"))

	if err := h.converter.Convert(context.Background(), h.lease); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	parsed, ok := loadStored(t, h.storage, h.versionKey())
	if !ok {
		t.Fatal("parsed.json не записан")
	}
	if parsed.TenantID != h.lease.TenantID {
		t.Errorf("tenant_id %s вместо %s", parsed.TenantID, h.lease.TenantID)
	}
	if parsed.DocumentID != h.lease.DocumentID {
		t.Errorf("document_id %s вместо %s", parsed.DocumentID, h.lease.DocumentID)
	}
	if parsed.KBID != h.lease.KBID {
		t.Errorf("kb_id %s вместо %s", parsed.KBID, h.lease.KBID)
	}
	if versionID, _ := h.lease.VersionIDValue(); parsed.VersionID != versionID {
		t.Errorf("version_id %s вместо %s", parsed.VersionID, versionID)
	}
}

// TestConverterEnqueuesIndexExactlyOnceOnRetry — упавшая попытка не должна
// оставить задание на индексацию: успешным считается только переход,
// подтверждённый базой, иначе упавшая попытка выглядела бы как потерянное
// задание индексации.
func TestConverterEnqueuesIndexExactlyOnceOnRetry(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("# Раздел\n\nтекст\n"))

	// Первая попытка падает после разбора: транзакция не прошла.
	h.jobs.convertErr = errors.New("нет соединения")
	if err := h.converter.Convert(context.Background(), h.lease); err == nil {
		t.Fatal("ожидалась ошибка транзакции")
	}
	if h.jobs.convertCalls != 1 {
		t.Fatalf("FinishConvert вызван %d раз, ожидался 1", h.jobs.convertCalls)
	}
	if h.jobs.indexEnqueues != 0 {
		t.Fatalf("упавшая попытка перевела задание на индексацию %d раз",
			h.jobs.indexEnqueues)
	}

	// Вторая попытка: база отвечает, работа завершается ровно один раз.
	h.jobs.convertErr = nil
	if err := h.converter.Convert(context.Background(), h.lease); err != nil {
		t.Fatalf("повтор Convert: %v", err)
	}
	if h.jobs.indexEnqueues != 1 {
		t.Fatalf("заданий на индексацию поставлено %d, ожидалось 1", h.jobs.indexEnqueues)
	}
}

// TestConverterDoesNotRequeueAfterLeaseSpent — повторный запуск стадии с уже
// израсходованным lease обязан быть отвергнут, а не поставить второе задание:
// перевод задания делается UPDATE-ом той же строки jobs, и второй переход
// означал бы, что документ встал на индексацию дважды.
func TestConverterDoesNotRequeueAfterLeaseSpent(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("# Раздел\n\nтекст\n"))

	if err := h.converter.Convert(context.Background(), h.lease); err != nil {
		t.Fatalf("первый Convert: %v", err)
	}

	// Тот же израсходованный lease — как при обработке задублированного
	// задания после таймаута первой попытки.
	err := h.converter.Convert(context.Background(), h.lease)
	if err == nil {
		t.Fatal("повтор с израсходованным lease обязан быть отвергнут")
	}
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Errorf("получено %v вместо JOB_LEASE_LOST", err)
	}
	if h.jobs.indexEnqueues != 1 {
		t.Errorf("заданий на индексацию %d, ожидалось 1", h.jobs.indexEnqueues)
	}
}

// TestConverterIsIdempotentOnSameInput — повторный запуск на том же оригинале
// даёт тот же JSON побайтово: иначе повтор после упавшей транзакции заменил бы
// parsed.json и осиротил бы чанки прежней попытки.
func TestConverterIsIdempotentOnSameInput(t *testing.T) {
	original := []byte("# Раздел\n\nтекст документа\n")
	h := newConvertHarness(t, "doc.md", original)
	key := h.versionKey()

	if err := h.converter.Convert(context.Background(), h.lease); err != nil {
		t.Fatalf("первый Convert: %v", err)
	}
	first := h.storage.data[key]

	h.storage.puts = 0
	// Повтор в реальности приходит с новым lease: первый истёк, задание
	// вернулось в очередь и его снова забрали. Старый токен отверг бы
	// повтор по JOB_LEASE_LOST — это проверяет отдельный тест.
	h.jobs.leaseSpent = false
	if err := h.converter.Convert(context.Background(), h.lease); err != nil {
		t.Fatalf("повторный Convert: %v", err)
	}
	if h.storage.data[key] != first {
		t.Error("повторный разбор дал другой JSON: block_id не детерминированы")
	}
	if h.jobs.indexEnqueues != 2 {
		t.Errorf("после повтора заданий на индексацию %d, ожидалось 2", h.jobs.indexEnqueues)
	}
}

// TestConverterReadsVersionOnce — оригинал читается ровно один раз: повторное
// чтение означало бы либо двойной разбор файла, либо разбор уже сменившейся
// версии вместо той, за которую отдан lease.
func TestConverterReadsVersionOnce(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("# Раздел\n\nтекст\n"))

	if err := h.converter.Convert(context.Background(), h.lease); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if h.documents.versionReads != 1 {
		t.Errorf("версия прочитана %d раз, ожидался 1", h.documents.versionReads)
	}
}

// TestConverterRequiresParsedKeyBuilder — без keyBuilder стадия обязана
// отказаться, а не подставить запасной формат: ключ должен совпадать с тем,
// по которому инфраструктура читает файл, иначе parsed.json ляжет туда, откуда
// его никто не прочитает.
func TestConverterRequiresParsedKeyBuilder(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("# Раздел\n\nтекст\n"))
	h.converter = indexing.NewConverter(indexing.Deps{
		Documents:  h.documents,
		Jobs:       h.jobs,
		Storage:    h.storage,
		Queryer:    stubQueryer{},
		Transactor: h.tx,
	})

	err := h.converter.Convert(context.Background(), h.lease)
	if !errors.Is(err, domain.ErrConfigurationInvalid) {
		t.Fatalf("получено %v вместо CONFIGURATION_INVALID", err)
	}
	if h.storage.puts != 0 || h.jobs.convertCalls != 0 {
		t.Error("работа выполнена без построителя ключа")
	}
}

// TestConverterRejectsJobWithoutVersion — задание без версии обработать
// нечем: молча пропустить его нельзя, иначе документ навсегда остался бы
// в состоянии parsing.
func TestConverterRejectsJobWithoutVersion(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("текст"))
	lease := h.lease
	lease.VersionID = nil

	err := h.converter.Convert(context.Background(), lease)
	if err == nil {
		t.Fatal("ожидался отказ по заданию без версии")
	}
	if h.jobs.convertCalls != 0 || h.storage.puts != 0 {
		t.Error("хранилище или очередь затронуты для задания без версии")
	}
}

// TestConverterFailsCleanlyOnMissingVersion — версии нет в базе: стадия
// возвращает ошибку и ничего не пишет. Молчаливый успех оставил бы документ
// в parsing навсегда, а запись в чужой parsed.json — ещё и утечку.
func TestConverterFailsCleanlyOnMissingVersion(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("текст"))
	h.documents.convertErr = domain.NotFound("Версия документа не найдена")

	err := h.converter.Convert(context.Background(), h.lease)
	if err == nil {
		t.Fatal("ожидалась ошибка чтения версии")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("получено %v вместо NOT_FOUND", err)
	}
	if h.jobs.convertCalls != 0 {
		t.Error("задание завершено, хотя версия не найдена")
	}
	if h.jobs.indexEnqueues != 0 {
		t.Error("задание на индексацию поставлено без версии")
	}
	if h.storage.puts != 0 {
		t.Error("parsed.json записан без версии")
	}
	if h.tx.calls != 0 {
		t.Errorf("транзакций %d, ожидался 0: писать нечего", h.tx.calls)
	}
}

// TestConverterPropagatesParseError — негодный файл обязан дойти до очереди
// своим кодом: оператор чинит документ по коду, а не по общему «внутренняя
// ошибка».
func TestConverterPropagatesParseError(t *testing.T) {
	h := newConvertHarness(t, "broken.docx", []byte("PK\x03\x04 не архив"))

	err := h.converter.Convert(context.Background(), h.lease)
	if !errors.Is(err, domain.InvalidMetadata("")) {
		t.Fatalf("получено %v вместо INVALID_METADATA", err)
	}
	if h.jobs.convertCalls != 0 {
		t.Error("задание завершено при ошибке разбора")
	}
}

// TestConverterPropagatesCancelledContext — отмена процесса не должна приводить
// к записи в очередь.
func TestConverterPropagatesCancelledContext(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("текст"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.converter.Convert(ctx, h.lease); !errors.Is(err, context.Canceled) {
		t.Fatalf("получено %v вместо context.Canceled", err)
	}
	if h.jobs.convertCalls != 0 {
		t.Error("задание завершено при отменённом контексте")
	}
}

// TestConverterDoesNotAdvanceWhenStorageWriteFails — не записанный
// parsed.json не должен переводить задание на index: стадия index взяла бы
// пустой файл и падала бы, а документ числился бы разобранным.
func TestConverterDoesNotAdvanceWhenStorageWriteFails(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("# Раздел\n\nтекст\n"))
	h.storage.putErr = domain.ServiceUnavailable("диск недоступен")

	err := h.converter.Convert(context.Background(), h.lease)
	if err == nil {
		t.Fatal("ожидалась ошибка записи в хранилище")
	}
	if h.jobs.convertCalls != 0 || h.jobs.indexEnqueues != 0 {
		t.Error("задание переведено на индексацию без записанного parsed.json")
	}
}

// TestConverterReportsUnmarshalableMetadata — значение metadata, которое не
// кодируется в JSON, обязано дать внутреннюю ошибку, а не упасть с чужой
// ошибкой кодировщика: очередь решает по коду, повторяет ли она задание.
func TestConverterReportsUnmarshalableMetadata(t *testing.T) {
	h := newConvertHarness(t, "doc.md", []byte("# Раздел\n\nтекст\n"))
	// Метаданные правятся у подделки репозитория, а не у копии версии в
	// harness: LoadVersion отдаёт значение, и правка локальной копии не была
	// бы видна стадии.
	h.documents.convert.Metadata = map[string]any{"handler": strings.Contains}

	err := h.converter.Convert(context.Background(), h.lease)
	if err == nil {
		t.Fatal("ожидалась ошибка кодирования metadata")
	}
	appErr, ok := domain.AsAppError(err)
	if !ok {
		t.Fatalf("ошибка не AppError: %v", err)
	}
	if appErr.Code != domain.CodeInternalError {
		t.Errorf("код %s вместо INTERNAL_ERROR", appErr.Code)
	}
	if appErr.Cause == nil {
		t.Error("причина кодировщика не сохранена в Cause")
	}
	if h.jobs.convertCalls != 0 {
		t.Error("задание переведено при ошибке кодирования")
	}
}

// loadStored достаёт записанный ParsedDocument: проверяется ровно то, что
// прочитает стадия index, а не то, что вернул конвертер в памяти.
func loadStored(t *testing.T, store *memoryStorage, key string) (domain.ParsedDocument, bool) {
	t.Helper()
	raw, ok := store.data[key]
	if !ok {
		return domain.ParsedDocument{}, false
	}
	var parsed domain.ParsedDocument
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("parsed.json не разбирается: %v", err)
	}
	return parsed, true
}

// memoryStorage — хранилище файлов в памяти: стадия convert пишет в него
// parsed.json, и тест проверяет байты, которые потом прочитает стадия index.
type memoryStorage struct {
	data map[string]string
	puts int
	// putErr имитирует сбой диска: незаписанный parsed.json не должен
	// приводить к переводу задания на индексацию.
	putErr error
}

func (s *memoryStorage) Put(_ context.Context, key string, data []byte) error {
	if s.putErr != nil {
		return s.putErr
	}
	if s.data == nil {
		s.data = map[string]string{}
	}
	s.puts++
	s.data[key] = string(data)
	return nil
}

func (s *memoryStorage) Read(_ context.Context, key string) ([]byte, error) {
	raw, ok := s.data[key]
	if !ok {
		return nil, domain.NotFound("файл не найден в хранилище")
	}
	return []byte(raw), nil
}

func (s *memoryStorage) Delete(_ context.Context, key string) error {
	delete(s.data, key)
	return nil
}

var _ ports.FileStorage = (*memoryStorage)(nil)
