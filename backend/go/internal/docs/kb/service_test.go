package kb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Подставной репозиторий: проверять правила сценария нужно на всех
// сочетаниях прав и отказов, а не на тех, что случайно оказались в тестовой
// базе. Интеграционные тесты адаптера лежат в
// internal/store/pg.

// fakeRepo — репозиторий баз знаний из фиксированных данных.
type fakeRepo struct {
	kbs     map[string]domain.KB
	acls    map[string]domain.ACL
	created []domain.CreateKBInput

	createErr error
	listErr   error
	getErr    error
	setACLErr error

	// listCalls и listLimit фиксируют, что сценарий ограничивает страницу
	// сам, а не полагается на адаптер: сценарий — публичная точка входа, и
	// значение больше 100 не должно проходить даже в обход транспорта.
	listCalls int
	listLimit int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{kbs: map[string]domain.KB{}, acls: map[string]domain.ACL{}}
}

func (f *fakeRepo) Create(
	_ context.Context, _ ports.Queryer, _ domain.Principal, in domain.CreateKBInput,
) (domain.KB, error) {
	if f.createErr != nil {
		return domain.KB{}, f.createErr
	}
	f.created = append(f.created, in)
	kb := domain.KB{
		KBID:      uuid.NewString(),
		Name:      in.Name,
		CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	}
	f.kbs[kb.KBID] = kb
	return kb, nil
}

func (f *fakeRepo) List(
	_ context.Context, _ ports.Queryer, _ domain.Principal, limit int, _ string,
) ([]domain.KB, string, error) {
	f.listCalls++
	f.listLimit = limit
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	items := make([]domain.KB, 0, len(f.kbs))
	for _, kb := range f.kbs {
		items = append(items, kb)
	}
	return items, "", nil
}

func (f *fakeRepo) Require(
	_ context.Context, _ ports.Queryer, _ domain.Principal, kbID string,
) (domain.KB, error) {
	if f.getErr != nil {
		return domain.KB{}, f.getErr
	}
	kb, ok := f.kbs[kbID]
	if !ok {
		return domain.KB{}, domain.NotFound("База знаний не найдена")
	}
	return kb, nil
}

func (f *fakeRepo) SetACL(
	_ context.Context, _ ports.Queryer, kbID string, in domain.ACL,
) error {
	if f.setACLErr != nil {
		return f.setACLErr
	}
	f.acls[kbID] = in
	return nil
}

func (f *fakeRepo) SetDocumentACL(
	_ context.Context, _ ports.Queryer, _ string, _ *domain.ACL,
) error {
	return nil
}

func (f *fakeRepo) EmbeddingProfileID(_ context.Context, _ ports.Queryer, _ string) (string, error) {
	return "", nil
}

func (f *fakeRepo) ACL(_ context.Context, _ ports.Queryer, kbID string) (domain.ACL, error) {
	if f.getErr != nil {
		return domain.ACL{}, f.getErr
	}
	if _, ok := f.kbs[kbID]; !ok {
		return domain.ACL{}, domain.NotFound("База знаний не найдена")
	}
	return f.acls[kbID], nil
}

// fakeAuth — авторизация, которая умеет отказывать по заданному правилу.
type fakeAuth struct {
	adminErr error
	readErr  error

	adminCalls int
	readCalls  int
}

func (f *fakeAuth) RequireAdmin(domain.Principal) error {
	f.adminCalls++
	return f.adminErr
}

func (f *fakeAuth) RequireRead(domain.Principal, domain.ACL) error {
	f.readCalls++
	return f.readErr
}

// fakeTx — транзакция, выполняющая fn на том же подставном Queryer.
type fakeTx struct {
	calls int
}

func (f *fakeTx) WithTx(ctx context.Context, fn func(ports.Queryer) error) error {
	f.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(f)
}

func (f *fakeTx) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, errors.New("сценарий не должен читать напрямую")
}

func (f *fakeTx) QueryRow(context.Context, string, ...any) ports.Row {
	return nil
}

func (f *fakeTx) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, errors.New("сценарий не должен писать напрямую")
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func testPrincipal() domain.Principal {
	return domain.LocalPrincipal()
}

func newService(repo *fakeRepo, auth ports.Authorizer) (*Service, *fakeTx) {
	tx := &fakeTx{}
	return New(repo, auth, tx, tx), tx
}

// ── Создание ────────────────────────────────────────────────────────

func TestCreateKBСоздаётБазу(t *testing.T) {
	repo := newFakeRepo()
	auth := &fakeAuth{}
	svc, _ := newService(repo, auth)

	id, err := svc.CreateKB(testCtx(t), testPrincipal(), "  ВЕКТОР  ")
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("ожидался ненулевой идентификатор созданной базы")
	}
	// Пробелы по краям срезаются: имя из формы приходит как есть, а в базе
	// оно должно совпадать с тем, что показывает список.
	if len(repo.created) != 1 || repo.created[0].Name != "ВЕКТОР" {
		t.Fatalf("ожидалось имя \"ВЕКТОР\", получено: %+v", repo.created)
	}
}

func TestCreateKBТребуетАдминистратора(t *testing.T) {
	repo := newFakeRepo()
	auth := &fakeAuth{adminErr: domain.NotFound("Действие доступно только администратору tenant'а")}
	svc, _ := newService(repo, auth)

	_, err := svc.CreateKB(testCtx(t), testPrincipal(), "ВЕКТОР")
	if err == nil {
		t.Fatal("без прав администратора создание должно отклоняться")
	}
	if len(repo.created) != 0 {
		t.Fatal("при отказе по правам база создаваться не должна")
	}
}

func TestCreateKBОтклоняетПустоеИмя(t *testing.T) {
	repo := newFakeRepo()
	svc, _ := newService(repo, &fakeAuth{})

	_, err := svc.CreateKB(testCtx(t), testPrincipal(), "   ")
	if appErr, ok := domain.AsAppError(err); !ok || appErr.Code != domain.CodeInvalidMetadata {
		t.Fatalf("ожидался INVALID_METADATA, получено: %v", err)
	}
}

func TestCreateKBУважаетОтменуКонтекста(t *testing.T) {
	repo := newFakeRepo()
	auth := &fakeAuth{}
	svc, _ := newService(repo, auth)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.CreateKB(ctx, testPrincipal(), "ВЕКТОР"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидалась отмена контекста, получено: %v", err)
	}
	// Отмена до обращения к репозиторию: запись в базу после отмены запроса
	// оставила бы базу, на которую клиент уже не получит ответа.
	if len(repo.created) != 0 {
		t.Fatal("при отмене контекста репозиторий вызываться не должен")
	}
}

// ── Список ──────────────────────────────────────────────────────────

func TestListKBsВозвращаетПустойСрезАНеNil(t *testing.T) {
	repo := newFakeRepo()
	svc, _ := newService(repo, &fakeAuth{})

	items, next, err := svc.ListKBs(testCtx(t), testPrincipal(), 10, "")
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if items == nil {
		t.Fatal("items должен быть пустым срезом, а не nil: в JSON он обязан стать []")
	}
	if len(items) != 0 {
		t.Fatalf("ожидался пустой список, получено %d элементов", len(items))
	}
	if next != "" {
		t.Fatalf("пустой курсор означает «дальше нечего», получено: %q", next)
	}
}

func TestListKBsПередаётКурсорАдаптеру(t *testing.T) {
	repo := newFakeRepo()
	svc, _ := newService(repo, &fakeAuth{})

	// Курсор непрозрачен и разбирается адаптером: сценарий обязан передать
	// его как есть, иначе пагинация поедет по другому набору строк.
	if _, _, err := svc.ListKBs(testCtx(t), testPrincipal(), 10, "MjAyNi0xMC0wOXx1dWlk"); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if repo.listLimit != 10 {
		t.Fatalf("ожидался limit=10, передан %d", repo.listLimit)
	}
}

func TestListKBsОграничиваетРазмерСтраницы(t *testing.T) {
	repo := newFakeRepo()
	svc, _ := newService(repo, &fakeAuth{})

	if _, _, err := svc.ListKBs(testCtx(t), testPrincipal(), 5000, ""); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	// Больше 100 строк за раз не отдаём: клиент с limit=5000 положил бы
	// процесс, а не узнал что-то новое.
	if repo.listLimit != maxLimit {
		t.Fatalf("ожидался предел %d, передан %d", maxLimit, repo.listLimit)
	}
}

func TestListKBsУважаетОтменуКонтекста(t *testing.T) {
	repo := newFakeRepo()
	svc, _ := newService(repo, &fakeAuth{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := svc.ListKBs(ctx, testPrincipal(), 10, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидалась отмена контекста, получено: %v", err)
	}
	if repo.listCalls != 0 {
		t.Fatal("при отмене контекста репозиторий вызываться не должен")
	}
}

// ── Чтение ──────────────────────────────────────────────────────────

func TestGetKBВозвращаетБазуСACL(t *testing.T) {
	repo := newFakeRepo()
	id := uuid.New()
	repo.kbs[id.String()] = domain.KB{
		KBID:      id.String(),
		Name:      "ВЕКТОР",
		CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	}
	repo.acls[id.String()] = domain.ACL{Users: []string{"local-dev"}}
	auth := &fakeAuth{}
	svc, _ := newService(repo, auth)

	view, err := svc.GetKB(testCtx(t), testPrincipal(), id)
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if view.ID != id || view.Name != "ВЕКТОР" {
		t.Fatalf("получена не та база: %+v", view)
	}
	if view.CreatedAt != "2026-10-09T12:00:00Z" {
		t.Fatalf("дата должна быть RFC3339 в UTC, получено: %q", view.CreatedAt)
	}
	if len(view.ACL.Users) != 1 || view.ACL.Users[0] != "local-dev" {
		t.Fatalf("ACL не передан в ответ: %+v", view.ACL)
	}
	if auth.readCalls != 1 {
		t.Fatalf("ожидался один вызов RequireRead, получено %d", auth.readCalls)
	}
}

func TestGetKBОтказываетПоПравам(t *testing.T) {
	repo := newFakeRepo()
	id := uuid.New()
	repo.kbs[id.String()] = domain.KB{KBID: id.String(), Name: "ВЕКТОР"}
	auth := &fakeAuth{readErr: domain.NotFound("База знаний не найдена")}
	svc, _ := newService(repo, auth)

	_, err := svc.GetKB(testCtx(t), testPrincipal(), id)
	if err == nil {
		t.Fatal("без прав на чтение база отдаваться не должна")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
}

func TestGetKBНеНаходитЧужуюБазу(t *testing.T) {
	repo := newFakeRepo()
	auth := &fakeAuth{}
	svc, _ := newService(repo, auth)

	_, err := svc.GetKB(testCtx(t), testPrincipal(), uuid.New())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
	// Существование проверяется адаптером до правила доступа: иначе по
	// разнице ответов можно было бы отличить «нет» от «запрещено».
	if auth.readCalls != 0 {
		t.Fatal("несуществующая база не должна доходить до проверки прав")
	}
}

func TestGetKBОтклоняетПустойИдентификатор(t *testing.T) {
	svc, _ := newService(newFakeRepo(), &fakeAuth{})

	if _, err := svc.GetKB(testCtx(t), testPrincipal(), uuid.Nil); err == nil {
		t.Fatal("пустой идентификатор должен отклоняться")
	}
}

// ── ACL ─────────────────────────────────────────────────────────────

func TestSetKBACLПишетВТранзакцииПослеПроверкиСуществования(t *testing.T) {
	repo := newFakeRepo()
	id := uuid.New()
	repo.kbs[id.String()] = domain.KB{KBID: id.String(), Name: "ВЕКТОР"}
	auth := &fakeAuth{}
	svc, tx := newService(repo, auth)

	err := svc.SetKBACL(testCtx(t), testPrincipal(), id, domain.AllowedList{
		Users: []string{"analyst-1"},
	})
	if err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	if tx.calls != 1 {
		t.Fatalf("ожидался ровно один вызов транзакции, получено %d", tx.calls)
	}
	saved := repo.acls[id.String()]
	if len(saved.Users) != 1 || saved.Users[0] != "analyst-1" {
		t.Fatalf("ACL не сохранён: %+v", saved)
	}
	// AllowedList не содержит ролей, и в ACL их тоже быть не должно:
	// поля allowed_role_ids в схеме нет, адаптер отверг бы такой ACL.
	if len(saved.Roles) != 0 {
		t.Fatalf("в ACL не должно быть ролей, получено: %+v", saved.Roles)
	}
}

func TestSetKBACLТребуетАдминистратора(t *testing.T) {
	repo := newFakeRepo()
	id := uuid.New()
	repo.kbs[id.String()] = domain.KB{KBID: id.String()}
	auth := &fakeAuth{adminErr: domain.NotFound("Действие доступно только администратору")}
	svc, tx := newService(repo, auth)

	err := svc.SetKBACL(testCtx(t), testPrincipal(), id, domain.AllowedList{})
	if err == nil {
		t.Fatal("правка ACL без прав администратора должна отклоняться")
	}
	if tx.calls != 0 {
		t.Fatal("при отказе по правам транзакция открываться не должна")
	}
}

func TestSetKBACLНеНаходитБазу(t *testing.T) {
	repo := newFakeRepo()
	auth := &fakeAuth{}
	svc, tx := newService(repo, auth)

	err := svc.SetKBACL(testCtx(t), testPrincipal(), uuid.New(), domain.AllowedList{})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ожидался NOT_FOUND, получено: %v", err)
	}
	if tx.calls != 0 {
		t.Fatal("несуществующая база не должна открывать транзакцию")
	}
}

func TestSetKBACLПовторИдемпотентен(t *testing.T) {
	repo := newFakeRepo()
	id := uuid.New()
	repo.kbs[id.String()] = domain.KB{KBID: id.String()}
	svc, _ := newService(repo, &fakeAuth{})
	ctx := testCtx(t)
	acl := domain.AllowedList{Users: []string{"analyst-1"}}

	if err := svc.SetKBACL(ctx, testPrincipal(), id, acl); err != nil {
		t.Fatalf("первая правка: %v", err)
	}
	if err := svc.SetKBACL(ctx, testPrincipal(), id, acl); err != nil {
		t.Fatalf("повтор той же правки должен быть успешным, получено: %v", err)
	}
	if len(repo.acls[id.String()].Users) != 1 {
		t.Fatalf("повтор не должен дописывать записи: %+v", repo.acls[id.String()])
	}
}

func TestSetKBACLПустойСписокОткрываетВсем(t *testing.T) {
	repo := newFakeRepo()
	id := uuid.New()
	repo.kbs[id.String()] = domain.KB{KBID: id.String()}
	svc, _ := newService(repo, &fakeAuth{})

	if err := svc.SetKBACL(testCtx(t), testPrincipal(), id, domain.AllowedList{}); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
	// Пустой список — это «доступ всем пользователям tenant'а»
	// (domain.AllowedList.AllowsAny), а не «закрыть всем».
	if !repo.acls[id.String()].AllowsAny() {
		t.Fatalf("ожидался ACL, открытый всем, получено: %+v", repo.acls[id.String()])
	}
}
