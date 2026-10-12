package documents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Подставные порты: правила сценария проверяются на всех сочетаниях прав,
// отказов и повторов, а не на тех, что случайно оказались в тестовой базе.

// fakeRepo — репозиторий документов на одной карте.
type fakeRepo struct {
	records map[string]Record
	created []CreateParams

	createErr error
	listErr   error
	getErr    error
	deleteErr error
	retryErr  error

	createCalls int
	deletedIDs  []string
	retriedIDs  []string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{records: map[string]Record{}}
}

func (f *fakeRepo) Create(
	_ context.Context, _ ports.Queryer, _ domain.Principal, in CreateParams,
) (CreateResult, error) {
	f.createCalls++
	if f.createErr != nil {
		return CreateResult{}, f.createErr
	}
	f.created = append(f.created, in)

	docID := in.DocumentID
	if docID == uuid.Nil {
		docID = uuid.New()
	}
	version := Version{
		ID: in.VersionID.String(), Filename: in.Filename, Title: in.Title,
		Status: domain.VersionStatusUploaded, SourceURL: in.SourceURL,
		ContentSHA256: in.ContentSHA256, OriginalKey: in.OriginalKey,
		Metadata: in.Metadata, CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	}
	record, ok := f.records[docID.String()]
	if !ok {
		record = Record{
			DocumentID: docID.String(), KBID: in.KBID.String(),
			CreatedAt: time.Date(2026, 10, 9, 11, 0, 0, 0, time.UTC),
		}
	}
	record.Latest = &version
	f.records[docID.String()] = record
	return CreateResult{
		DocumentID: docID, VersionID: in.VersionID, JobID: uuid.New(),
	}, nil
}

func (f *fakeRepo) List(
	_ context.Context, _ ports.Queryer, _ domain.Principal, _ string, _ int, _ string,
) ([]Record, string, error) {
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	items := make([]Record, 0, len(f.records))
	for _, record := range f.records {
		items = append(items, record)
	}
	return items, "", nil
}

func (f *fakeRepo) Get(
	_ context.Context, _ ports.Queryer, _ domain.Principal, documentID string,
) (Record, error) {
	if f.getErr != nil {
		return Record{}, f.getErr
	}
	record, ok := f.records[documentID]
	if !ok {
		return Record{}, domain.NotFound("Документ не найден")
	}
	return record, nil
}

func (f *fakeRepo) MarkDeleted(
	_ context.Context, _ ports.Queryer, _ domain.Principal, documentID string,
) (string, error) {
	f.deletedIDs = append(f.deletedIDs, documentID)
	if f.deleteErr != nil {
		return "", f.deleteErr
	}
	return uuid.NewString(), nil
}

func (f *fakeRepo) Retry(
	_ context.Context, _ ports.Queryer, _ domain.Principal, documentID string,
) (RetryResult, error) {
	f.retriedIDs = append(f.retriedIDs, documentID)
	if f.retryErr != nil {
		return RetryResult{}, f.retryErr
	}
	record := f.records[documentID]
	if record.Latest != nil {
		version := *record.Latest
		version.Status = domain.VersionStatusUploaded
		record.Latest = &version
		f.records[documentID] = record
	}
	return RetryResult{VersionID: uuid.New(), JobID: uuid.New()}, nil
}

// fakeIdem — хранилище идемпотентности на одной карте.
type fakeIdem struct {
	records map[IdempotencyKey]IdempotencyRecord

	lookupErr error
	storeErr  error
	storeBody map[string]any
	lookups   int
	stores    int
}

func newFakeIdem() *fakeIdem {
	return &fakeIdem{records: map[IdempotencyKey]IdempotencyRecord{}}
}

func (f *fakeIdem) Lookup(
	_ context.Context, _ ports.Queryer, key IdempotencyKey,
) (*IdempotencyRecord, error) {
	f.lookups++
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	record, ok := f.records[key]
	if !ok {
		return nil, nil
	}
	return &record, nil
}

func (f *fakeIdem) Store(
	_ context.Context, _ ports.Queryer, key IdempotencyKey, status int, body map[string]any,
) error {
	f.stores++
	if f.storeErr != nil {
		return f.storeErr
	}
	f.storeBody = body
	f.records[key] = IdempotencyRecord{Status: status, Body: body}
	return nil
}

// fakeStorage — хранилище файлов в памяти.
type fakeStorage struct {
	files map[string][]byte

	putErr   error
	readErr  error
	putCalls int
	delCalls int
	deleted  []string
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{files: map[string][]byte{}}
}

func (f *fakeStorage) Put(_ context.Context, key string, data []byte) error {
	f.putCalls++
	if f.putErr != nil {
		return f.putErr
	}
	f.files[key] = data
	return nil
}

func (f *fakeStorage) Read(_ context.Context, key string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	data, ok := f.files[key]
	if !ok {
		return nil, domain.NotFound("файл не найден в хранилище")
	}
	return data, nil
}

func (f *fakeStorage) Delete(_ context.Context, key string) error {
	f.delCalls++
	f.deleted = append(f.deleted, key)
	delete(f.files, key)
	return nil
}

// fakeAuth — авторизация с настраиваемым отказом.
type fakeAuth struct {
	adminErr error
	readErr  error

	adminCalls int
}

func (f *fakeAuth) RequireAdmin(domain.Principal) error {
	f.adminCalls++
	return f.adminErr
}

func (f *fakeAuth) RequireRead(domain.Principal, domain.ACL) error { return f.readErr }

// fakeJobs — очередь, считающая постановки уборки.
type fakeJobs struct {
	cleanup    []string
	cleanupErr error
}

func (f *fakeJobs) Claim(
	context.Context, ports.Queryer, string, []string,
) (*domain.JobLease, error) {
	return nil, nil
}

func (f *fakeJobs) Heartbeat(context.Context, ports.Queryer, string, string) (bool, error) {
	return true, nil
}

func (f *fakeJobs) FinishIndex(
	context.Context, ports.Queryer, string, string, domain.IndexingResult,
) error {
	return nil
}

// FinishConvert стадии convert: сценарий документов её не вызывает.
func (f *fakeJobs) FinishConvert(
	context.Context, ports.Queryer, string, string, domain.ConversionResult,
) error {
	return nil
}

func (f *fakeJobs) Fail(
	context.Context, ports.Queryer, string, string, string, string, bool,
) error {
	return nil
}

func (f *fakeJobs) EnqueueCleanup(
	_ context.Context, _ ports.Queryer, _, _, versionID string,
) (string, error) {
	if f.cleanupErr != nil {
		return "", f.cleanupErr
	}
	f.cleanup = append(f.cleanup, versionID)
	return uuid.NewString(), nil
}

// fakeKbs — базы знаний: одна существующая, остальные не найдены.
type fakeKbs struct {
	kbs  map[string]domain.KB
	acls map[string]domain.ACL

	requireCalls int
	aclOverride  *domain.ACL
}

func newFakeKbs(kbID string) *fakeKbs {
	return &fakeKbs{
		kbs:  map[string]domain.KB{kbID: {KBID: kbID, Name: "ВЕКТОР"}},
		acls: map[string]domain.ACL{},
	}
}

func (f *fakeKbs) Create(
	context.Context, ports.Queryer, domain.Principal, domain.CreateKBInput,
) (domain.KB, error) {
	return domain.KB{}, errors.New("не используется")
}

func (f *fakeKbs) List(
	context.Context, ports.Queryer, domain.Principal, int, string,
) ([]domain.KB, string, error) {
	return nil, "", nil
}

func (f *fakeKbs) Require(
	_ context.Context, _ ports.Queryer, _ domain.Principal, kbID string,
) (domain.KB, error) {
	f.requireCalls++
	kb, ok := f.kbs[kbID]
	if !ok {
		return domain.KB{}, domain.NotFound("База знаний не найдена")
	}
	return kb, nil
}

func (f *fakeKbs) SetACL(
	context.Context, ports.Queryer, string, domain.ACL,
) error {
	return nil
}

func (f *fakeKbs) SetDocumentACL(
	_ context.Context, _ ports.Queryer, _ string, override *domain.ACL,
) error {
	f.aclOverride = override
	return nil
}

func (f *fakeKbs) EmbeddingProfileID(context.Context, ports.Queryer, string) (string, error) {
	return "", nil
}

func (f *fakeKbs) ACL(_ context.Context, _ ports.Queryer, kbID string) (domain.ACL, error) {
	return f.acls[kbID], nil
}

// fakeTx — транзакция на том же подставном Queryer.
type fakeTx struct{ calls int }

func (f *fakeTx) WithTx(ctx context.Context, fn func(ports.Queryer) error) error {
	f.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(f)
}

func (f *fakeTx) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, errors.New("сценарий не читает напрямую")
}

func (f *fakeTx) QueryRow(context.Context, string, ...any) ports.Row { return nil }

func (f *fakeTx) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, errors.New("сценарий не пишет напрямую")
}

// fakeClock — фиксированное время, чтобы даты в ответе были предсказуемыми.
type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

func (c fakeClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func testPrincipal() domain.Principal { return domain.LocalPrincipal() }

// testKey — ключ оригинала в тестовом хранилище: тот же формат, что у
// storage.OriginalKey, иначе тесты проверяли бы не тот путь.
func testKey(tenantID, versionID string) string {
	return tenantID + "/" + versionID + "/original.bin"
}

type harness struct {
	svc   *Service
	repo  *fakeRepo
	idem  *fakeIdem
	store *fakeStorage
	auth  *fakeAuth
	jobs  *fakeJobs
	kbs   *fakeKbs
	tx    *fakeTx
	kbID  uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		repo: newFakeRepo(), idem: newFakeIdem(), store: newFakeStorage(),
		auth: &fakeAuth{}, jobs: &fakeJobs{}, tx: &fakeTx{}, kbID: uuid.New(),
	}
	h.kbs = newFakeKbs(h.kbID.String())
	clock := fakeClock{now: time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)}
	h.svc = New(
		h.repo, h.kbs, h.jobs, h.idem, h.store, h.auth, h.tx, h.tx, clock, testKey,
	)
	return h
}

// validDoc — пригодный для приёма файл: расширение поддерживается и
// сигнатура совпадает (ТЗ 5.5 п.2).
func validDoc() []byte {
	return []byte("# Муфта\n\nМомент затяжки 45 Нм.\n")
}

func uploadInput(kbID uuid.UUID) UploadInput {
	return UploadInput{
		KBID: kbID, Filename: "муфта.md", Title: "Муфта",
		Content: validDoc(), Metadata: map[string]any{"источник": "испытания"},
		IdempotencyKey: "11111111-1111-4111-8111-111111111111",
	}
}

// ── Загрузка ────────────────────────────────────────────────────────

func TestUploadСоздаётДокументВерсиюИЗадание(t *testing.T) {
	h := newHarness(t)

	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if created.DocumentID == uuid.Nil || created.JobID == uuid.Nil || created.VersionID == uuid.Nil {
		t.Fatalf("ожидались ненулевые идентификаторы: %+v", created)
	}
	if h.repo.createCalls != 1 {
		t.Fatalf("ожидался ровно один вызов Create, получено %d", h.repo.createCalls)
	}

	params := h.repo.created[0]
	if params.ContentSHA256 != contentDigest(validDoc()) {
		t.Fatalf("отпечаток содержимого не совпал: %q", params.ContentSHA256)
	}
	// Ключ оригинала строится по правилу хранилища, а не склеивается здесь:
	// второй экземпляр правила разошёлся бы с первым при первой правке.
	expected := testKey(domain.LocalTenantID.String(), params.VersionID.String())
	if params.OriginalKey != expected {
		t.Fatalf("ключ оригинала %q, ожидался %q", params.OriginalKey, expected)
	}
	if _, ok := h.store.files[params.OriginalKey]; !ok {
		t.Fatal("оригинал должен быть сохранён в хранилище")
	}
	if h.idem.stores != 1 {
		t.Fatalf("ответ должен сохраняться по ключу идемпотентности, получено %d", h.idem.stores)
	}
}

func TestUploadПовторСТемЖеКлючомНеСоздаётВторуюВерсию(t *testing.T) {
	h := newHarness(t)
	ctx := testCtx(t)
	in := uploadInput(h.kbID)

	first, err := h.svc.Upload(ctx, testPrincipal(), in)
	if err != nil {
		t.Fatalf("первая загрузка: %v", err)
	}
	second, err := h.svc.Upload(ctx, testPrincipal(), in)
	if err != nil {
		t.Fatalf("повтор той же загрузки должен быть успешным, получено: %v", err)
	}

	// ТЗ 5.4: повтор той же загрузки возвращает исходный ответ и не плодит
	// версии. Вторая версия здесь означала бы двойное индексирование файла.
	if h.repo.createCalls != 1 {
		t.Fatalf("ожидался один вызов Create, получено %d — создана лишняя версия", h.repo.createCalls)
	}
	if first != second {
		t.Fatalf("повтор должен вернуть исходные идентификаторы: %+v против %+v", first, second)
	}
}

func TestUploadСДругимКлючомСоздаётНовыйДокумент(t *testing.T) {
	h := newHarness(t)
	ctx := testCtx(t)

	if _, err := h.svc.Upload(ctx, testPrincipal(), uploadInput(h.kbID)); err != nil {
		t.Fatalf("первая загрузка: %v", err)
	}
	second := uploadInput(h.kbID)
	second.IdempotencyKey = "22222222-2222-4222-8222-222222222222"

	if _, err := h.svc.Upload(ctx, testPrincipal(), second); err != nil {
		t.Fatalf("вторая загрузка: %v", err)
	}
	// Новый ключ — новая операция: это осознанный повтор, а не ретрай.
	if h.repo.createCalls != 2 {
		t.Fatalf("ожидалось два вызова Create, получено %d", h.repo.createCalls)
	}
}

func TestUploadБезКлючаИдемпотентностиРаботаетНоНеЗапоминаетОтвет(t *testing.T) {
	h := newHarness(t)
	in := uploadInput(h.kbID)
	in.IdempotencyKey = ""

	if _, err := h.svc.Upload(testCtx(t), testPrincipal(), in); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	// Заголовок необязателен в контракте, и ответа 400 там нет: требовать
	// его значило бы вернуть код, которого в контракте не существует.
	if h.idem.lookups != 0 || h.idem.stores != 0 {
		t.Fatalf("без ключа хранилище не трогается: %d/%d", h.idem.lookups, h.idem.stores)
	}
}

func TestUploadОтклоняетНеподдерживаемыйФормат(t *testing.T) {
	h := newHarness(t)
	in := uploadInput(h.kbID)
	in.Filename = "отчёт.rtf"
	in.Content = []byte("не md")

	_, err := h.svc.Upload(testCtx(t), testPrincipal(), in)
	if !errors.Is(err, domain.ErrNotFound) {
		if appErr, ok := domain.AsAppError(err); !ok || appErr.Code != domain.CodeUnsupportedFormat {
			t.Fatalf("ожидался UNSUPPORTED_FORMAT, получено: %v", err)
		}
	}
	// Отказ до записи: негодный файл не должен ни занимать версию, ни
	// оставлять оригинал в хранилище.
	if h.repo.createCalls != 0 || h.store.putCalls != 0 {
		t.Fatalf("негодный файл не должен доходить до записи: create=%d put=%d",
			h.repo.createCalls, h.store.putCalls)
	}
}

func TestUploadОтклоняетСлишкомБольшойФайл(t *testing.T) {
	h := newHarness(t)
	in := uploadInput(h.kbID)
	in.Content = make([]byte, MaxFileBytes+1)

	_, err := h.svc.Upload(testCtx(t), testPrincipal(), in)
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeFileTooLarge {
		t.Fatalf("ожидался FILE_TOO_LARGE, получено: %v", err)
	}
	if h.store.putCalls != 0 {
		t.Fatal("файл больше лимита не должен попадать в хранилище")
	}
}

func TestUploadУдаляетОригиналПриПровалТранзакции(t *testing.T) {
	h := newHarness(t)
	h.repo.createErr = domain.Conflict("Документ с таким external_id уже существует")

	_, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	if err == nil {
		t.Fatal("ожидалась ошибка от репозитория")
	}
	// Оригинал без версии в базе — мусор на диске: хранилище чистится по
	// ключам версий из таблицы, и незаписанной версии у него нет.
	if h.store.delCalls != 1 {
		t.Fatalf("ожидался один вызов удаления файла, получено %d", h.store.delCalls)
	}
	if len(h.store.files) != 0 {
		t.Fatalf("в хранилище остался файл: %v", h.store.files)
	}
}

func TestUploadТребуетАдминистратора(t *testing.T) {
	h := newHarness(t)
	h.auth.adminErr = domain.NotFound("Действие доступно только администратору")

	_, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	if err == nil {
		t.Fatal("загрузка без прав администратора должна отклоняться")
	}
	if h.store.putCalls != 0 {
		t.Fatal("при отказе по правам оригинал сохраняться не должен")
	}
}

func TestUploadНеНаходитБазуЗнаний(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(uuid.New()))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
	if h.store.putCalls != 0 {
		t.Fatal("при несуществующей базе оригинал сохраняться не должен")
	}
}

func TestUploadУважаетОтменуКонтекста(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.svc.Upload(ctx, testPrincipal(), uploadInput(h.kbID)); !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидалась отмена контекста, получено: %v", err)
	}
	if h.store.putCalls != 0 || h.repo.createCalls != 0 {
		t.Fatal("при отмене контекста не должно быть ни файла, ни версии")
	}
}

// ── Список и чтение ─────────────────────────────────────────────────

func TestListВозвращаетПустойСрезАНеNil(t *testing.T) {
	h := newHarness(t)

	out, err := h.svc.List(testCtx(t), testPrincipal(), ListInput{KBID: h.kbID, Limit: 10})
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if out.Items == nil {
		t.Fatal("Items должен быть пустым срезом, а не nil: в JSON он обязан стать []")
	}
	if out.NextCursor != "" {
		t.Fatalf("пустой курсор означает «дальше нечего», получено: %q", out.NextCursor)
	}
}

func TestListЗаполняетДокументПолностью(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID)); err != nil {
		t.Fatalf("загрузка: %v", err)
	}

	out, err := h.svc.List(testCtx(t), testPrincipal(), ListInput{KBID: h.kbID, Limit: 10})
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("ожидался один документ, получено %d", len(out.Items))
	}

	item := out.Items[0]
	if item.Filename != "муфта.md" || item.Title != "Муфта" {
		t.Fatalf("имя файла и заголовок не перенесены: %+v", item)
	}
	if item.State != domain.VersionStatusUploaded {
		t.Fatalf("состояние должно быть из версии, получено: %q", item.State)
	}
	if item.VersionID == uuid.Nil {
		t.Fatal("идентификатор версии обязателен")
	}
	if item.KBID != h.kbID {
		t.Fatalf("база знаний не совпала: %v", item.KBID)
	}
	// Даты строкой RFC3339: сценарий не разбирает их обратно, транспорт
	// сделает это один раз на границе.
	if item.CreatedAt != "2026-10-09T11:00:00Z" {
		t.Fatalf("дата создания в неверном формате: %q", item.CreatedAt)
	}
	if item.UpdatedAt != "2026-10-09T12:00:00Z" {
		t.Fatalf("дата обновления в неверном формате: %q", item.UpdatedAt)
	}
}

func TestGetВозвращаетДокумент(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}

	view, err := h.svc.Get(testCtx(t), testPrincipal(), docID)
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if view.ID != docID || view.KBID != h.kbID {
		t.Fatalf("получен не тот документ: %+v", view)
	}
}

func TestGetНеНаходитДокумент(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Get(testCtx(t), testPrincipal(), uuid.New())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

func TestGetУважаетОтменуКонтекста(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.svc.Get(ctx, testPrincipal(), uuid.New()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидалась отмена контекста, получено: %v", err)
	}
}

// ── Замена содержимого ──────────────────────────────────────────────

func TestReplaceContentСоздаётНовуюВерсиюТогоЖеДокумента(t *testing.T) {
	h := newHarness(t)
	ctx := testCtx(t)
	created, err := h.svc.Upload(ctx, testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}

	jobID, err := h.svc.ReplaceContent(ctx, testPrincipal(), ReplaceContentInput{
		DocumentID: docID, Filename: "муфта-2.md", Title: "Муфта, редакция 2",
		Content:        []byte("# Муфта\n\nМомент затяжки 50 Нм.\n"),
		IdempotencyKey: "33333333-3333-4333-8333-333333333333",
	})
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if jobID == uuid.Nil {
		t.Fatal("ожидался ненулевой идентификатор задания")
	}

	if h.repo.createCalls != 2 {
		t.Fatalf("ожидалось два вызова Create, получено %d", h.repo.createCalls)
	}
	// Новый DocumentID не передаётся: замена содержимого не создаёт второй
	// документ, иначе старый остался бы висеть в базе навсегда.
	if h.repo.created[1].DocumentID != docID {
		t.Fatalf("ожидался документ %v, получено %v", docID, h.repo.created[1].DocumentID)
	}
	if h.repo.created[1].KBID != h.kbID {
		t.Fatalf("база знаний взята не из документа: %v", h.repo.created[1].KBID)
	}
}

func TestReplaceContentПовторСТемЖеКлючомИдемпотентен(t *testing.T) {
	h := newHarness(t)
	ctx := testCtx(t)
	created, err := h.svc.Upload(ctx, testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	in := ReplaceContentInput{
		DocumentID: docID, Filename: "муфта-2.md", Title: "Муфта 2",
		Content: []byte("# Муфта\n\n50 Нм.\n"), IdempotencyKey: "44444444-4444-4444-8444-444444444444",
	}

	if _, err := h.svc.ReplaceContent(ctx, testPrincipal(), in); err != nil {
		t.Fatalf("первая замена: %v", err)
	}
	if _, err := h.svc.ReplaceContent(ctx, testPrincipal(), in); err != nil {
		t.Fatalf("повтор той же замены: %v", err)
	}
	if h.repo.createCalls != 2 {
		t.Fatalf("повтор создал лишнюю версию: вызовов Create %d", h.repo.createCalls)
	}
}

func TestReplaceContentНеНаходитДокумент(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.ReplaceContent(testCtx(t), testPrincipal(), ReplaceContentInput{
		DocumentID: uuid.New(), Filename: "муфта.md", Content: validDoc(),
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

// ── Удаление ────────────────────────────────────────────────────────

func TestDeleteСтавитЗаданиеУборкиДляАктивнойВерсии(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	record := h.repo.records[docID.String()]
	active := *record.Latest
	record.Active = &active
	h.repo.records[docID.String()] = record

	if err := h.svc.Delete(testCtx(t), testPrincipal(), docID); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if len(h.repo.deletedIDs) != 1 || h.repo.deletedIDs[0] != docID.String() {
		t.Fatalf("документ не помечен удалённым: %v", h.repo.deletedIDs)
	}
	// Стадия delete снимет документ с индексации, а задача уборки
	// гарантирует освобождение места даже после исчерпания её попыток.
	if len(h.jobs.cleanup) != 1 || h.jobs.cleanup[0] != active.ID {
		t.Fatalf("ожидалась уборка версии %s, получено: %v", active.ID, h.jobs.cleanup)
	}
}

func TestDeleteБезАктивнойВерсииУбираетПоследнюю(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	record := h.repo.records[docID.String()]
	latest := *record.Latest
	record.Active = nil
	record.Latest = &latest
	h.repo.records[docID.String()] = record

	if err := h.svc.Delete(testCtx(t), testPrincipal(), docID); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	// Чанков в индексе нет, но оригинал и parsed.json последней версии
	// остались на диске, и убрать их больше некому: документ удалён и в
	// список версий больше не попадёт.
	if len(h.jobs.cleanup) != 1 || h.jobs.cleanup[0] != latest.ID {
		t.Fatalf("ожидалась уборка последней версии %s, получено: %v", latest.ID, h.jobs.cleanup)
	}
}

func TestDeleteДокументаБезВерсийНеСтавитУборку(t *testing.T) {
	h := newHarness(t)
	docID := uuid.New()
	h.repo.records[docID.String()] = Record{DocumentID: docID.String(), KBID: h.kbID.String()}

	if err := h.svc.Delete(testCtx(t), testPrincipal(), docID); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	// Убирать нечего: ни чанков, ни файлов у версии, которой не было.
	if len(h.jobs.cleanup) != 0 {
		t.Fatalf("уборка не должна ставиться без версий: %v", h.jobs.cleanup)
	}
}

func TestDeleteПовторИдемпотентен(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	ctx := testCtx(t)

	if err := h.svc.Delete(ctx, testPrincipal(), docID); err != nil {
		t.Fatalf("первое удаление: %v", err)
	}
	if err := h.svc.Delete(ctx, testPrincipal(), docID); err != nil {
		t.Fatalf("повтор удаления должен быть успешным, получено: %v", err)
	}
}

func TestDeleteТребуетАдминистратора(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	h.auth.adminErr = domain.NotFound("Действие доступно только администратору")
	h.tx.calls = 0

	if err := h.svc.Delete(testCtx(t), testPrincipal(), docID); err == nil {
		t.Fatal("удаление без прав администратора должно отклоняться")
	}
	if h.tx.calls != 0 {
		t.Fatal("при отказе по правам транзакция открываться не должна")
	}
}

func TestDeleteНеНаходитДокумент(t *testing.T) {
	h := newHarness(t)

	if err := h.svc.Delete(testCtx(t), testPrincipal(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

// ── Переиндексация ──────────────────────────────────────────────────

func TestReindexСоздаётВерсиюИзСохранённогоОригинала(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}

	jobID, err := h.svc.Reindex(testCtx(t), testPrincipal(), docID)
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if jobID == uuid.Nil {
		t.Fatal("ожидался ненулевой идентификатор задания")
	}

	params := h.repo.created[1]
	if params.DocumentID != docID {
		t.Fatalf("переиндексация обязана оставить тот же документ, получено %v", params.DocumentID)
	}
	if params.Filename != "муфта.md" || params.Title != "Муфта" {
		t.Fatalf("метаданные исходной версии не перенесены: %+v", params)
	}
	// Ключ остаётся прежним: содержимое то же, а новый ключ оставил бы
	// старый оригинал в хранилище навсегда — его уже никто не уберёт.
	if params.OriginalKey == "" || params.OriginalKey != h.repo.created[0].OriginalKey {
		t.Fatalf("ожидался прежний ключ оригинала %q, получено %q",
			h.repo.created[0].OriginalKey, params.OriginalKey)
	}
	// Файл повторно в хранилище не пишется: он уже там и он тот же самый.
	if h.store.putCalls != 1 {
		t.Fatalf("ожидалась одна запись файла, получено %d", h.store.putCalls)
	}
}

func TestReindexБезОригиналаОтказывает(t *testing.T) {
	h := newHarness(t)
	docID := uuid.New()
	h.repo.records[docID.String()] = Record{
		DocumentID: docID.String(), KBID: h.kbID.String(),
		Latest: &Version{ID: uuid.NewString(), Filename: "муфта.md"},
	}

	_, err := h.svc.Reindex(testCtx(t), testPrincipal(), docID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

func TestReindexБезВерсийОтказывает(t *testing.T) {
	h := newHarness(t)
	docID := uuid.New()
	h.repo.records[docID.String()] = Record{DocumentID: docID.String(), KBID: h.kbID.String()}

	_, err := h.svc.Reindex(testCtx(t), testPrincipal(), docID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
	if h.repo.createCalls != 0 {
		t.Fatal("без исходной версии версия создаваться не должна")
	}
}

// ── Повтор ──────────────────────────────────────────────────────────

func TestRetryВозобновляетНеуспешнуюВерсию(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}

	jobID, err := h.svc.Retry(testCtx(t), testPrincipal(), docID)
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if jobID == uuid.Nil {
		t.Fatal("ожидался ненулевой идентификатор задания")
	}
	if len(h.repo.retriedIDs) != 1 || h.repo.retriedIDs[0] != docID.String() {
		t.Fatalf("повтор поставлен не для того документа: %v", h.repo.retriedIDs)
	}
}

func TestRetryПередаётОтказАдаптера(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	h.repo.retryErr = domain.Conflict("Нет неуспешной версии документа")

	if _, err := h.svc.Retry(testCtx(t), testPrincipal(), docID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ожидался CONFLICT, получено: %v", err)
	}
}

func TestRetryТребуетАдминистратора(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	h.auth.adminErr = domain.NotFound("Действие доступно только администратору")
	h.tx.calls = 0

	if _, err := h.svc.Retry(testCtx(t), testPrincipal(), docID); err == nil {
		t.Fatal("повтор без прав администратора должен отклоняться")
	}
	if h.tx.calls != 0 {
		t.Fatal("при отказе по правам транзакция открываться не должна")
	}
}

// ── ACL ─────────────────────────────────────────────────────────────

func TestSetACLЗаписываетСобственныйACLДокумента(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}

	if err := h.svc.SetACL(testCtx(t), testPrincipal(), docID,
		domain.AllowedList{Users: []string{"analyst-1"}}); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if h.kbs.aclOverride == nil {
		t.Fatal("ACL документа должен быть записан")
	}
	if len(h.kbs.aclOverride.Users) != 1 || h.kbs.aclOverride.Users[0] != "analyst-1" {
		t.Fatalf("ACL записан неверно: %+v", h.kbs.aclOverride)
	}
}

func TestSetACLПустойСписокОткрываетВсем(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}

	if err := h.svc.SetACL(testCtx(t), testPrincipal(), docID, domain.AllowedList{}); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	// Подменять пустой список на NULL нельзя: NULL означает «действует ACL
	// базы», а пустой объект — «закрыто для всех, кроме администратора».
	// Это разные состояния, и путаница тихо закрывает документ.
	if h.kbs.aclOverride == nil || !h.kbs.aclOverride.AllowsAny() {
		t.Fatalf("ожидался пустой ACL, а не наследование: %+v", h.kbs.aclOverride)
	}
}

func TestSetACLНеНаходитДокумент(t *testing.T) {
	h := newHarness(t)

	err := h.svc.SetACL(testCtx(t), testPrincipal(), uuid.New(), domain.AllowedList{})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
	if h.kbs.aclOverride != nil {
		t.Fatal("несуществующий документ не должен получать ACL")
	}
}

// ── Скачивание оригинала ────────────────────────────────────────────

func TestSourceОтдаётОригиналАктивнойВерсии(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	record := h.repo.records[docID.String()]
	active := *record.Latest
	active.ID = uuid.NewString()
	record.Active = &active
	h.repo.records[docID.String()] = record

	content, filename, err := h.svc.Source(testCtx(t), testPrincipal(), docID)
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if string(content) != string(validDoc()) {
		t.Fatalf("получено не то содержимое: %q", content)
	}
	// Второе значение уходит в Content-Disposition, поэтому это имя файла.
	if filename != "муфта.md" {
		t.Fatalf("ожидалось имя файла, получено: %q", filename)
	}
}

func TestSourceБезАктивнойВерсииБерётПоследнюю(t *testing.T) {
	h := newHarness(t)
	created, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	docID := created.DocumentID
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}

	if _, _, err := h.svc.Source(testCtx(t), testPrincipal(), docID); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
}

func TestSourceНеНаходитВерсию(t *testing.T) {
	h := newHarness(t)
	docID := uuid.New()
	h.repo.records[docID.String()] = Record{DocumentID: docID.String(), KBID: h.kbID.String()}

	if _, _, err := h.svc.Source(testCtx(t), testPrincipal(), docID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

func TestSourceНеНаходитДокумент(t *testing.T) {
	h := newHarness(t)

	if _, _, err := h.svc.Source(testCtx(t), testPrincipal(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

// ── Идемпотентность: детали ключа ───────────────────────────────────

func TestRequestHashРазличаетСодержимоеПриОдинаковыхМетаданных(t *testing.T) {
	first := requestHash([]byte("один"), map[string]any{"a": "b"})
	second := requestHash([]byte("два"), map[string]any{"a": "b"})

	if first == second {
		t.Fatal("разное содержимое при одинаковых метаданных должно давать разные хеши")
	}
}

func TestRequestHashНеЗависитОтПорядкаПолейМетаданных(t *testing.T) {
	first := requestHash([]byte("файл"), map[string]any{"a": "1", "b": "2"})
	second := requestHash([]byte("файл"), map[string]any{"b": "2", "a": "1"})

	// Порядок полей в JSON объекта не должен менять идентичность запроса:
	// иначе один и тот же вызов с переставленными ключами создал бы вторую
	// версию вместо повтора.
	if first != second {
		t.Fatal("порядок полей не должен влиять на хеш запроса")
	}
}

func TestIdempotencyKeyПустойЗаголовокДаётПустойКлюч(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	principal := testPrincipal()

	if got := h.svc.idempotencyKey(principal, "  ", "POST", "/x", nil, nil); got.Key != "" {
		t.Fatalf("пустой заголовок должен давать пустой ключ, получено: %q", got.Key)
	}
	key := h.svc.idempotencyKey(principal, "abc", "POST", "/x", nil, nil)
	if key.TenantID != domain.LocalTenantID || key.UserID != domain.LocalUserID {
		t.Fatalf("ключ должен содержать tenant и пользователя: %+v", key)
	}
	if key.RequestHash == "" {
		t.Fatal("ключ должен содержать отпечаток тела")
	}
	_ = ctx
}

func TestUploadЧитаетСохранённыйОтветПоКлючу(t *testing.T) {
	h := newHarness(t)
	saved := IdempotencyRecord{
		Status: statusAccepted,
		Body: map[string]any{
			"document_id": uuid.NewString(),
			"version_id":  uuid.NewString(),
			"job_id":      uuid.NewString(),
		},
	}
	key := h.svc.idempotencyKey(
		testPrincipal(), uploadInput(h.kbID).IdempotencyKey, "POST",
		uploadPath(h.kbID), validDoc(), uploadInput(h.kbID).Metadata,
	)
	h.idem.records[key] = saved

	replayed, err := h.svc.Upload(testCtx(t), testPrincipal(), uploadInput(h.kbID))
	if err != nil {
		t.Fatalf("повтор должен вернуть сохранённый ответ, получено: %v", err)
	}
	if replayed.DocumentID.String() != saved.Body["document_id"] {
		t.Fatalf("ожидался сохранённый документ %v, получен %v",
			saved.Body["document_id"], replayed.DocumentID)
	}
	if replayed.JobID.String() != saved.Body["job_id"] {
		t.Fatalf("ожидалось сохранённое задание %v, получено %v",
			saved.Body["job_id"], replayed.JobID)
	}
	// Версия тоже должна прийти из сохранённого ответа: раньше сценарий её
	// терял, и повтор отдавал пустой version_id.
	if replayed.VersionID.String() != saved.Body["version_id"] {
		t.Fatalf("ожидалась сохранённая версия %v, получена %v",
			saved.Body["version_id"], replayed.VersionID)
	}
	// Сохранённый ответ найден до проверки прав и до записи файла: повтор
	// не должен ни создавать версию, ни трогать хранилище.
	if h.repo.createCalls != 0 || h.store.putCalls != 0 {
		t.Fatalf("повтор по сохранённому ключу не должен ничего создавать: create=%d put=%d",
			h.repo.createCalls, h.store.putCalls)
	}
}

func TestUploadВозвращаетОшибкуЗаписанногоОтветаКакВнутреннюю(t *testing.T) {
	h := newHarness(t)
	in := uploadInput(h.kbID)
	key := h.svc.idempotencyKey(
		testPrincipal(), in.IdempotencyKey, "POST", uploadPath(h.kbID), in.Content, in.Metadata,
	)
	// Тело без идентификаторов — дефект кода, а не пользовательский ввод:
	// отдать клиенту исходную запись нельзя, он получил бы 202 с другим
	// содержимым.
	h.idem.records[key] = IdempotencyRecord{Status: statusAccepted, Body: map[string]any{}}

	_, err := h.svc.Upload(testCtx(t), testPrincipal(), in)
	appErr, ok := domain.AsAppError(err)
	if !ok || appErr.Code != domain.CodeInternalError {
		t.Fatalf("ожидался INTERNAL_ERROR, получено: %v", err)
	}
}
