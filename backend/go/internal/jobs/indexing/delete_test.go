package indexing_test

// Тесты стадии delete — надгробия удаления (§4, «удаление — через
// событие-надгробие (tombstone)»; §9, «документ, удалённый в источнике,
// исчезает из выдачи»).
//
// Проверяются четыре вещи, каждая отвечает на свой вопрос:
//
//	ПОРЯДОК ШАГОВ — lake, потом индекс, потом задание. Обратный порядок оставил
//	бы окно, в котором документа нет в индексе, но нет и в lake: пересборка
//	вернула бы его в выдачу, то есть ровно то нарушение §9, которое стадия чинит.
//
//	ИДЕМПОТЕНТНОСТЬ — повтор того же задания даёт тот же event_id, и lake
//	отбрасывает дубль реестром raw_event_ids. «Удалённого документа нет» и
//	«удаление уже записано» — это нормальный успех, а не ошибка.
//
//	ИЗОЛЯЦИЯ ТЕНАНТА — удаление ограничено парой (tenant_id, document_id). Один
//	document_id у двух тенантов — штатная ситуация (doc_id §4 хэширует tenant,
//	но рукописные и загруженные id исторически не хэшировались).
//
//	ЖИЗНЕННЫЙ ЦИКЛ ЗАДАНИЯ — закрытие происходит под своим lease и только своей
//	стадии; чужой lease даёт JobLeaseLost, и цикл не считает это отказом.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
	"github.com/kbsearch/kb/internal/ports"
)

var (
	deleteTenantA = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	deleteTenantB = uuid.MustParse("22222222-2222-4222-8222-222222222222")
)

// lakeKey ограничение ck_raw_events_content_hash из миграции 0002: форма
// «<алгоритм>:[0-9a-f]+». Проверяется в тесте, а не только комментарием:
// ключ, который форма не проходит, lake не запишет.
var lakeKey = regexp.MustCompile(`^[a-z0-9]+:[0-9a-f]+$`)

// deleteClock — часы подделки: occurred_at надгробия обязан быть предсказуемым.
type deleteClock struct{ now time.Time }

func (c deleteClock) Now() time.Time { return c.now }

func (c deleteClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// recordedLake — EventBus, который помнит всё написанное и ведёт себя как
// lake в части идемпотентности: повтор того же event_id не добавляет записи.
//
// Именно так работает raw_event_ids (миграция 0002, ON CONFLICT DO NOTHING), и
// подделка повторяет это, чтобы тест проверял ПОВЕДЕНИЕ стадии, а не то,
// сколько раз она позвала порт.
type recordedLake struct {
	events []domain.Event
	seen   map[uuid.UUID]int
	err    error
	calls  int
}

func newRecordedLake() *recordedLake {
	return &recordedLake{seen: map[uuid.UUID]int{}}
}

func (l *recordedLake) Publish(_ context.Context, event domain.Event) error {
	l.calls++
	if l.err != nil {
		return l.err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	l.seen[event.EventID]++
	if l.seen[event.EventID] == 1 {
		l.events = append(l.events, event)
	}
	return nil
}

func (l *recordedLake) PublishBatch(ctx context.Context, events []domain.Event) error {
	for _, event := range events {
		if err := l.Publish(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func (l *recordedLake) Close() error { return nil }

// записанные — события, дошедшие до lake (без отброшенных дублей).
func (l *recordedLake) записанные() []domain.Event { return slices.Clone(l.events) }

// deleteIndex — индекс, помнящий документы и удаления.
//
// Хранит документы парами (tenant, document), потому что именно этой парой
// ограничено удаление: парадокс «один document_id у двух тенантов» и есть то,
// что изоляция обязана отсечь.
type deleteIndex struct {
	chunks  map[string]int
	deleted []deleteCall
	delErr  error
	delFrom map[string]error
}

// deleteCall — состояние индекса на момент удаления (для проверки порядка).
type deleteCall struct {
	tenantID   string
	documentID string
}

func newDeleteIndex() *deleteIndex {
	return &deleteIndex{chunks: map[string]int{}, delFrom: map[string]error{}}
}

func chunkKey(tenantID, documentID string) string { return tenantID + "/" + documentID }

func (i *deleteIndex) seed(tenantID, documentID string, chunks int) {
	i.chunks[chunkKey(tenantID, documentID)] = chunks
}

func (i *deleteIndex) EnsureIndex(context.Context, domain.IndexingConfig) (string, error) {
	return "kb-chunks-test", nil
}

func (i *deleteIndex) UpsertChunks(
	context.Context, domain.IndexingConfig, []domain.Chunk,
) (int, error) {
	return 0, nil
}

func (i *deleteIndex) DeleteVersion(context.Context, string, string, string) (int, error) {
	return 0, nil
}

func (i *deleteIndex) DeleteDocument(
	_ context.Context, tenantID, documentID string,
) (int, error) {
	i.deleted = append(i.deleted, deleteCall{tenantID: tenantID, documentID: documentID})
	if i.delErr != nil {
		return 0, i.delErr
	}
	if err, ok := i.delFrom[chunkKey(tenantID, documentID)]; ok && err != nil {
		return 0, err
	}
	key := chunkKey(tenantID, documentID)
	removed := i.chunks[key]
	delete(i.chunks, key)
	return removed, nil
}

func (i *deleteIndex) KeywordSearch(
	context.Context, string, domain.SearchFilter, int,
) ([]domain.SearchCandidate, error) {
	return nil, nil
}

func (i *deleteIndex) VectorSearch(
	context.Context, []float32, domain.IndexingConfig, domain.SearchFilter, int,
) ([]domain.SearchCandidate, error) {
	return nil, nil
}

func (i *deleteIndex) Ping(context.Context) error { return nil }

// осталось — документы, ещё лежащие в индексе, с их размерами.
func (i *deleteIndex) осталось() map[string]int { return mapsClone(i.chunks) }

func mapsClone(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// stubDeleteJobs — подделка DeleteJobs, повторяющая FinishDelete из delete.go:
// чужая стадия или уже потраченный lease дают JobLeaseLost, как это делает
// настоящая реализация условиями UPDATE.
//
// Именно «потраченный lease», а не «задание закрыто»: настоящий FinishDelete
// отвергает закрытие по lease_token и статусу running, и повтор с ТОТ ЖЕ
// подписью lease — это повтор стадии на той же выдаче, то есть LeaseLost.
// Следующая выдача приходит с новым токеном, и её закрытие проходит.
type stubDeleteJobs struct {
	calls   []domain.JobLease
	removed []int
	err     error
	spent   map[string]bool
}

func newStubDeleteJobs() *stubDeleteJobs {
	return &stubDeleteJobs{spent: map[string]bool{}}
}

func (j *stubDeleteJobs) FinishDelete(
	_ context.Context, _ ports.Queryer, lease domain.JobLease, removed int,
) error {
	j.calls = append(j.calls, lease)
	j.removed = append(j.removed, removed)
	if j.err != nil {
		return j.err
	}
	if lease.Stage != domain.StageDelete {
		return domain.JobLeaseLost("Задание не на стадии delete")
	}
	if j.spent[lease.LeaseToken] {
		return domain.JobLeaseLost("Задание уже разобрано")
	}
	j.spent[lease.LeaseToken] = true
	return nil
}

// deleteHarness собирает стадию удаления на подделках.
type deleteHarness struct {
	deleter *indexing.Deleter
	lake    *recordedLake
	index   *deleteIndex
	jobs    *stubDeleteJobs
	tx      *recordingTransactor
	lease   domain.JobLease
}

func newDeleteHarness() *deleteHarness {
	h := &deleteHarness{
		lake:  newRecordedLake(),
		index: newDeleteIndex(),
		jobs:  newStubDeleteJobs(),
		tx:    &recordingTransactor{},
	}
	h.lease = domain.JobLease{
		JobID:          uuid.New(),
		TenantID:       deleteTenantA,
		DocumentID:     deleteTenantB,
		Kind:           domain.KindDelete,
		Stage:          domain.StageDelete,
		Attempt:        1,
		LeaseToken:     "lease-delete",
		LeaseExpiresAt: time.Now().Add(domain.LeaseDuration),
	}
	// Версии у задания удаления нет: MarkDeleted кладёт version_id NULL, и
	// стадия обязана это выносить.
	h.lease.DocumentID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	h.deleter = indexing.NewDeleter(indexing.DeleteDeps{
		Index:      h.index,
		Lake:       h.lake,
		Jobs:       h.jobs,
		Transactor: h.tx,
		Clock:      deleteClock{now: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)},
	})
	return h
}

// TestDeleteStageПишетНадгробиеИУбираетИзИндекса — полный успешный путь.
//
// Шаги проверяются по порядку, а не по конечному состоянию: порядок здесь и
// есть содержание правила (§1, принцип 1).
func TestDeleteStageПишетНадгробиеИУбираетИзИндекса(t *testing.T) {
	h := newDeleteHarness()
	h.index.seed(h.lease.TenantID.String(), h.lease.DocumentID.String(), 4)

	if err := h.deleter.Delete(context.Background(), h.lease); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if h.lake.calls != 1 {
		t.Fatalf("lake вызван %d раз, ожидался 1", h.lake.calls)
	}
	recorded := h.lake.записанные()
	if len(recorded) != 1 {
		t.Fatalf("в lake записано %d событий, ожидалось 1", len(recorded))
	}
	event := recorded[0]
	if event.Stage != domain.DeleteStage {
		t.Errorf("стадия события %q вместо delete", event.Stage)
	}
	if event.TenantID != h.lease.TenantID || event.DocID != h.lease.DocumentID.String() {
		t.Errorf("надгробие адресовано %s/%s вместо %s/%s", event.TenantID, event.DocID,
			h.lease.TenantID, h.lease.DocumentID)
	}
	if len(h.index.deleted) != 1 {
		t.Fatalf("индекс вызван %d раз, ожидался 1", len(h.index.deleted))
	}
	call := h.index.deleted[0]
	if call.tenantID != h.lease.TenantID.String() || call.documentID != h.lease.DocumentID.String() {
		t.Errorf("удалено %s/%s вместо %s/%s", call.tenantID, call.documentID,
			h.lease.TenantID, h.lease.DocumentID)
	}
	if len(h.jobs.calls) != 1 {
		t.Fatalf("задание закрыто %d раз, ожидался 1", len(h.jobs.calls))
	}
	if got := h.jobs.removed[0]; got != 4 {
		t.Errorf("закрытие задания сообщило об удалении %d чанков, ожидалось 4", got)
	}
	if h.tx.calls != 1 {
		t.Errorf("транзакций %d, ожидалась 1: закрытие задания обязано быть транзакционным", h.tx.calls)
	}
}

// TestТелоНадгробияСогласуетсяСШапкой — тело и шапка события обязаны называть
// одно и то же.
//
// Пересборка индекса берёт поля из тела (lakePayload в internal/arch), и тело с
// чужим doc_id собрало бы документ из чужого содержимого — то есть прошло бы
// проверку и выдало неверный индекс.
func TestТелоНадгробияСогласуетсяСШапкой(t *testing.T) {
	lease := domain.JobLease{
		JobID:      uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		TenantID:   deleteTenantA,
		DocumentID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		Stage:      domain.StageDelete,
	}
	occurredAt := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	event, err := indexing.TombstoneEvent(lease, occurredAt)
	if err != nil {
		t.Fatalf("TombstoneEvent: %v", err)
	}

	var body struct {
		SchemaVersion string `json:"schema_version"`
		DocID         string `json:"doc_id"`
		TenantID      string `json:"tenant_id"`
		Stage         string `json:"stage"`
		Reason        string `json:"reason"`
		DeleteJobID   string `json:"delete_job_id"`
		DeletedAt     string `json:"deleted_at"`
	}
	if err := json.Unmarshal(event.Payload, &body); err != nil {
		t.Fatalf("разбор тела надгробия: %v", err)
	}

	cases := []struct {
		name      string
		got, want string
	}{
		{"doc_id", body.DocID, event.DocID},
		{"tenant_id", body.TenantID, event.TenantID.String()},
		{"stage", body.Stage, event.Stage.String()},
		{"delete_job_id", body.DeleteJobID, lease.JobID.String()},
		{"reason", body.Reason, indexing.TombstoneReasonSourceDeleted},
		{"schema_version", body.SchemaVersion, "1.0"},
		{"deleted_at", body.DeletedAt, occurredAt.Format(time.RFC3339Nano)},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("в теле надгробия %s = %q, ожидалось %q", c.name, c.got, c.want)
		}
	}
	if !strings.Contains(event.ContentHash, lease.JobID.String()[:8]) &&
		!strings.HasPrefix(event.ContentHash, "job:") {
		t.Errorf("ключ надгробия %q не привязан к заданию удаления", event.ContentHash)
	}
}

// TestКлючНадгробияДетерминирован — таблица правил ключа идемпотентности §4.
//
// Ключ обязан быть детерминирован для одного удаления (повтор стадии — no-op) и
// разным для двух (иначе пересборка индекса потеряла бы второе удаление), и
// обязан проходить форму ограничения content_hash.
func TestКлючНадгробияДетерминирован(t *testing.T) {
	first := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	second := uuid.MustParse("66666666-6666-4666-8666-666666666666")

	cases := []struct {
		name     string
		jobID    uuid.UUID
		equals   uuid.UUID
		wantSame bool
	}{
		{"повтор того же удаления даёт тот же ключ", first, first, true},
		{"второе удаление документа даёт другой ключ", second, first, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := indexing.TombstoneKey(c.jobID)
			if other := indexing.TombstoneKey(c.equals); (got == other) != c.wantSame {
				t.Errorf("TombstoneKey(%s) = %q, у другого задания %q: одинаковость %v, ожидалось %v",
					c.jobID, got, other, got == other, c.wantSame)
			}
			if !lakeKey.MatchString(got) {
				t.Errorf("ключ %q не проходит ck_raw_events_content_hash "+
					"(«<алгоритм>:[0-9a-f]+»): lake его не запишет", got)
			}
		})
	}
}

// TestDeleteStageИдемпотентна — повтор одного и того же задания.
//
// Один и тот же факт об одном и том же удалении обязан давать один и тот же
// event_id (lake отбрасывает дубль реестром raw_event_ids), ноль чанков при
// повторном удалении и отсутствие ошибки. «Ничего не изменилось» — это успех:
// §4 требует идемпотентности стадий, а не обязательного изменения состояния.
func TestDeleteStageИдемпотентна(t *testing.T) {
	h := newDeleteHarness()
	documentID := h.lease.DocumentID.String()
	h.index.seed(h.lease.TenantID.String(), documentID, 3)

	if err := h.deleter.Delete(context.Background(), h.lease); err != nil {
		t.Fatalf("первый Delete: %v", err)
	}

	// Повтор стадии приходит с НОВОЙ выдачей: очередь не отдаёт задание со
	// старым lease, поэтому токен другой, а факт удаления — тот же самый.
	retry := h.lease
	retry.LeaseToken = "lease-delete-retry"
	retry.Attempt = 2
	if err := h.deleter.Delete(context.Background(), retry); err != nil {
		t.Fatalf("повтор Delete: %v", err)
	}

	if got := h.lake.записанные(); len(got) != 1 {
		t.Errorf("в lake записано %d событий, ожидалось 1: повторное удаление не должно "+
			"быть вторым фактом", len(got))
	}
	if len(h.index.deleted) != 2 {
		t.Fatalf("индекс вызван %d раз, ожидалось 2", len(h.index.deleted))
	}
	if got := h.jobs.removed[1]; got != 0 {
		t.Errorf("повторное удаление сообщило %d чанков, ожидался 0", got)
	}
	// Закрытие по УЖЕ ПОТРАЧЕННОМУ lease — это LeaseLost, а не ошибка стадии:
	// цикл на неё не пишет отказ и не возвращает задание в очередь (см.
	// worker.process).
	if err := h.jobs.FinishDelete(
		context.Background(), stubQueryer{}, h.lease, 0,
	); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Errorf("повторное закрытие вернуло %v, ожидался ErrJobLeaseLost", err)
	}
}

// TestDeleteStageНаДокументеКоторогоНетВИндексе — удаление, которому нечего
// убирать, обязано быть успехом.
//
// Это не экзотика, а норма: документ мог не дойти до индекса (загрузка
// упала), и тогда DeleteDocument вернёт 0 без ошибки. Ошибка здесь означала бы,
// что удалённый документ без индекса снова и снова занимает очередь.
func TestDeleteStageНаДокументеКоторогоНетВИндексе(t *testing.T) {
	h := newDeleteHarness()
	// Ничего не сеем: документа в индексе нет.

	if err := h.deleter.Delete(context.Background(), h.lease); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := len(h.lake.записанные()); got != 1 {
		t.Errorf("в lake %d надгробий, ожидалось 1: удаление документа всё равно состоялось", got)
	}
	if len(h.jobs.calls) != 1 {
		t.Errorf("задание закрыто %d раз, ожидался 1", len(h.jobs.calls))
	}
}

// TestDeleteStageИзолируетТенанта — надгробие одного тенанта не трогает чужой
// документ с тем же document_id.
//
// Пара «одинаковый document_id у двух тенантов» не экзотика: doc_id §4 хэширует
// tenant, но рукописные и загруженные id исторически не хэшировались, поэтому
// term только по document_id снёс бы чужой документ.
func TestDeleteStageИзолируетТенанта(t *testing.T) {
	// Оба документа известны индексу под ОДНИМ document_id, но принадлежат
	// разным организациям. Идентификатор берётся из самого задания: стадия
	// удаляет ровно тот документ, который названо в её lease.
	sharedID := uuid.MustParse("33333333-3333-4333-8333-333333333333").String()
	cases := []struct {
		name   string
		tenant uuid.UUID
		other  uuid.UUID
	}{
		{"надгробие тенанта A не трогает одноимённый документ тенанта B", deleteTenantA, deleteTenantB},
		{"надгробие тенанта B не трогает одноимённый документ тенанта A", deleteTenantB, deleteTenantA},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			index := newDeleteIndex()
			index.seed(c.tenant.String(), sharedID, 2)
			index.seed(c.other.String(), sharedID, 5)
			deleter := indexing.NewDeleter(indexing.DeleteDeps{
				Index: index, Lake: newRecordedLake(), Jobs: newStubDeleteJobs(),
				Transactor: &recordingTransactor{},
				Clock:      deleteClock{now: time.Now().UTC()},
			})

			lease := domain.JobLease{
				JobID:      uuid.New(),
				TenantID:   c.tenant,
				DocumentID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
				Kind:       domain.KindDelete,
				Stage:      domain.StageDelete,
				LeaseToken: "lease-" + c.tenant.String()[:8],
			}
			if err := deleter.Delete(context.Background(), lease); err != nil {
				t.Fatalf("Delete: %v", err)
			}

			// Запрос удаления ограничен парой: тенант задания плюс его документ.
			if len(index.deleted) != 1 {
				t.Fatalf("запросов удаления %d, ожидался 1", len(index.deleted))
			}
			if index.deleted[0].tenantID != c.tenant.String() {
				t.Errorf("удаление ограничено тенантом %q вместо %q",
					index.deleted[0].tenantID, c.tenant)
			}

			left := index.осталось()
			if _, ok := left[chunkKey(c.tenant.String(), sharedID)]; ok {
				t.Errorf("собственный документ тенанта %s остался в индексе", c.tenant)
			}
			if chunks, ok := left[chunkKey(c.other.String(), sharedID)]; !ok || chunks != 5 {
				t.Errorf("документ тенанта %s пострадал: осталось %d из 5 чанков — "+
					"надгробие вышло за пределы своего тенанта", c.other, chunks)
			}
		})
	}
}

// TestDeleteStageОстанавливаетсяНаПервойОшибке — что происходит при отказе на
// каждом шаге.
//
// Порядок «lake → индекс → задание» обязан быть виден и по отказам: сорвалось
// надгробие — индекс не трогаем (иначе документ исчез бы из выдачи без следа в
// источнике правды, и пересборка его вернула бы); сорвалось удаление из индекса
// — задание не закрываем, чтобы повтор его вернул в очередь.
func TestDeleteStageОстанавливаетсяНаПервойОшибке(t *testing.T) {
	lakeErr := domain.Internal("lake недоступен")
	indexErr := domain.IndexUnavailable("Elasticsearch недоступен")
	finishErr := domain.JobLeaseLost("Задание уже разобрано")

	cases := []struct {
		name        string
		prepare     func(*deleteHarness)
		wantErr     error
		wantDeleted int
		wantClosed  int
	}{
		{
			name:        "надгробие не записалось — индекс не тронут, задание живо",
			prepare:     func(h *deleteHarness) { h.lake.err = lakeErr },
			wantErr:     lakeErr,
			wantDeleted: 0,
			wantClosed:  0,
		},
		{
			name:        "индекс недоступен — задание не закрыто, повтор его вернёт",
			prepare:     func(h *deleteHarness) { h.index.delErr = indexErr },
			wantErr:     indexErr,
			wantDeleted: 1,
			wantClosed:  0,
		},
		{
			name:        "задание уже разобрано — стадия сообщает об этом, но не молчит",
			prepare:     func(h *deleteHarness) { h.jobs.err = finishErr },
			wantErr:     finishErr,
			wantDeleted: 1,
			wantClosed:  1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newDeleteHarness()
			h.index.seed(h.lease.TenantID.String(), h.lease.DocumentID.String(), 2)
			c.prepare(h)

			err := h.deleter.Delete(context.Background(), h.lease)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Delete вернул %v, ожидалась %v", err, c.wantErr)
			}
			if len(h.index.deleted) != c.wantDeleted {
				t.Errorf("обращений к индексу %d, ожидалось %d", len(h.index.deleted), c.wantDeleted)
			}
			if len(h.jobs.calls) != c.wantClosed {
				t.Errorf("закрытий задания %d, ожидалось %d", len(h.jobs.calls), c.wantClosed)
			}
		})
	}
}

// TestDeleteStageОтвергаетЧужуюСтадию — граница стадии.
//
// Стадия удаления не должна исполняться для задания другой стадии: такой lease
// означает ошибку сборки или подмену, и закрывать его было бы молчаливой
// потерей задания.
func TestDeleteStageОтвергаетЧужуюСтадию(t *testing.T) {
	cases := []struct {
		name  string
		stage string
	}{
		{"convert", domain.StageConvert},
		{"index", domain.StageIndex},
		{"cleanup", domain.StageCleanup},
		{"пустая стадия", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newDeleteHarness()
			lease := h.lease
			lease.Stage = c.stage

			err := h.deleter.Delete(context.Background(), lease)
			if err == nil {
				t.Fatalf("стадия delete приняла задание стадии %q", c.stage)
			}
			if len(h.index.deleted) != 0 {
				t.Error("индекс тронут для чужой стадии")
			}
			if len(h.lake.записанные()) != 0 {
				t.Error("в lake попало надгробие для чужой стадии")
			}
		})
	}
}
