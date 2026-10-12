package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/queue"
	"github.com/kbsearch/kb/internal/ports"
)

var (
	localTenant   = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	foreignTenant = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	documentID    = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	versionID     = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

// stubQueryer — заглушка доступа к SQL: сценарий только передаёт его порту.
type stubQueryer struct{}

func (stubQueryer) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, context.Canceled
}

func (stubQueryer) QueryRow(context.Context, string, ...any) ports.Row { return nil }

func (stubQueryer) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, context.Canceled
}

// fakeRepo отдаёт заранее заданную запись и запоминает tenant, под которым
// сценарий её запросил.
type fakeRepo struct {
	record     jobs.Record
	err        error
	askedFor   string
	queryCount int
}

func (f *fakeRepo) GetJob(
	_ context.Context, _ ports.Queryer, tenantID, jobID string,
) (jobs.Record, error) {
	f.askedFor = tenantID
	f.queryCount++
	if f.err != nil {
		return jobs.Record{}, f.err
	}
	if f.record.ID.String() != jobID {
		return jobs.Record{}, domain.NotFound("Задание не найдено")
	}
	return f.record, nil
}

func localPrincipal() domain.Principal {
	return domain.Principal{TenantID: localTenant, UserID: "local", Roles: []string{"admin"}}
}

func sampleRecord() jobs.Record {
	code := "INDEX_UNAVAILABLE"
	message := "Elasticsearch недоступен"
	total := 12
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	return jobs.Record{
		ID:              uuid.New(),
		TenantID:        localTenant,
		DocumentID:      documentID,
		VersionID:       &versionID,
		Kind:            domain.KindIngest,
		Stage:           domain.StageIndex,
		Status:          domain.JobStatusFailed,
		Attempt:         2,
		ProcessedChunks: 12,
		TotalChunks:     &total,
		ErrorCode:       &code,
		ErrorMessage:    &message,
		CreatedAt:       now.Add(-time.Minute),
		UpdatedAt:       now,
	}
}

// TestGetReturnsJobState — ответ собирается из записи очереди целиком.
func TestGetReturnsJobState(t *testing.T) {
	record := sampleRecord()
	repo := &fakeRepo{record: record}

	view, err := jobs.New(repo).Get(context.Background(), stubQueryer{}, localPrincipal(), record.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if view.ID != record.ID {
		t.Errorf("id %s вместо %s", view.ID, record.ID)
	}
	if view.DocumentID != documentID.String() {
		t.Errorf("document_id %s вместо %s", view.DocumentID, documentID)
	}
	if view.VersionID != versionID.String() {
		t.Errorf("version_id %s вместо %s", view.VersionID, versionID)
	}
	if view.Stage != domain.StageIndex || view.Status != domain.JobStatusFailed {
		t.Errorf("стадия/статус %q/%q", view.Stage, view.Status)
	}
	if view.Attempts != 2 || view.Chunks != 12 {
		t.Errorf("попыток %d, чанков %d", view.Attempts, view.Chunks)
	}
	if view.ErrorCode != "INDEX_UNAVAILABLE" || view.ErrorText == "" {
		t.Errorf("ошибка %q / %q", view.ErrorCode, view.ErrorText)
	}
	if view.CreatedAt == "" || view.UpdatedAt == "" {
		t.Error("время создания или обновления пустое")
	}
}

// TestGetPassesTenantToRepository — tenant обязателен в самом запросе: сценарий,
// который полагался бы на фильтр «в адаптере», пропустил бы чужое задание при
// первой же правке SQL.
func TestGetPassesTenantToRepository(t *testing.T) {
	record := sampleRecord()
	repo := &fakeRepo{record: record}

	if _, err := jobs.New(repo).Get(
		context.Background(), stubQueryer{}, localPrincipal(), record.ID,
	); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if repo.askedFor != localTenant.String() {
		t.Errorf("запрошен tenant %q вместо %q", repo.askedFor, localTenant)
	}
}

// TestGetHidesForeignTenantJob — задание чужого tenant не выдаётся даже если
// адаптер по ошибке его вернул: посторонний не должен узнать, что такой id
// существует.
func TestGetHidesForeignTenantJob(t *testing.T) {
	record := sampleRecord()
	record.TenantID = foreignTenant
	repo := &fakeRepo{record: record}

	_, err := jobs.New(repo).Get(context.Background(), stubQueryer{}, localPrincipal(), record.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("получено %v вместо NOT_FOUND", err)
	}
}

// TestGetPropagatesNotFound — отсутствующее задание остаётся 404, а не
// превращается в 500.
func TestGetPropagatesNotFound(t *testing.T) {
	record := sampleRecord()
	repo := &fakeRepo{record: record}

	_, err := jobs.New(repo).Get(
		context.Background(), stubQueryer{}, localPrincipal(), uuid.New(),
	)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("получено %v вместо NOT_FOUND", err)
	}
}

// TestGetRejectsNilID — пустой идентификатор не должен уходить в базу: он
// означал бы «первое задание», а это не то, чего ждёт клиент.
func TestGetRejectsNilID(t *testing.T) {
	repo := &fakeRepo{record: sampleRecord()}

	_, err := jobs.New(repo).Get(context.Background(), stubQueryer{}, localPrincipal(), uuid.Nil)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("получено %v вместо NOT_FOUND", err)
	}
	if repo.queryCount != 0 {
		t.Error("база опрошена для пустого идентификатора")
	}
}

// TestGetRespectsCancelledContext — отменённый запрос не идёт в базу.
func TestGetRespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record := sampleRecord()
	repo := &fakeRepo{record: record}

	_, err := jobs.New(repo).Get(ctx, stubQueryer{}, localPrincipal(), record.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("получено %v вместо отмены контекста", err)
	}
	if repo.queryCount != 0 {
		t.Error("база опрошена после отмены контекста")
	}
}
