package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/delivery/worker"
	"github.com/kbsearch/backend-go/internal/domain"
)

// newTestWorker собирает цикл с подделками и заглушенным журналом: тесты
// проверяют состояние очереди, а не текст логов.
func newTestWorker(jobs *fakeJobs, stage worker.Stage, clock ports.Clock) *worker.Worker {
	return worker.New(worker.Deps{
		Jobs:       jobs,
		Queryer:    fakeQueryer{},
		Transactor: &fakeTransactor{},
		Stage:      stage,
		Clock:      clock,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, worker.Options{
		WorkerID:          testWorkerID,
		PollInterval:      time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
	})
}

// waitStage дожидается, пока стадия начнёт работу.
func waitStage(t *testing.T, stage *fakeStage) {
	t.Helper()
	select {
	case <-stage.started:
	case <-time.After(2 * time.Second):
		t.Fatal("стадия не была вызвана")
	}
}

func waitHeartbeat(t *testing.T, jobs *fakeJobs) {
	t.Helper()
	select {
	case <-jobs.heartbeats:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat не был выполнен")
	}
}

// TestWorkerClaimsIndexStageWithWorkerID — цикл забирает задание под lease и
// передаёт воркера и список стадий, закреплённых за Go (ТЗ 3.6).
func TestWorkerClaimsIndexStageWithWorkerID(t *testing.T) {
	jobs := newFakeJobs().withIndexJob()
	stage := newFakeStage()

	newTestWorker(jobs, stage, &fakeClock{stopAfter: 1}).Run(context.Background())

	if stage.calls != 1 {
		t.Fatalf("стадия вызвана %d раз, ожидался 1", stage.calls)
	}
	if stage.lease.Stage != domain.StageIndex {
		t.Errorf("обработана стадия %q", stage.lease.Stage)
	}
	if jobs.workerID != testWorkerID {
		t.Errorf("в очередь ушёл worker_id %q вместо %q", jobs.workerID, testWorkerID)
	}
	if len(jobs.stages) != len(domain.GoStages()) {
		t.Errorf("запрошены стадии %v вместо %v", jobs.stages, domain.GoStages())
	}
	for _, got := range jobs.stages {
		if got != domain.StageIndex {
			t.Errorf("в списке стадий есть чужая %q: её обслуживает Python-воркер", got)
		}
	}
}

// TestWorkerDoesNothingOnEmptyQueue — пустая очередь не сбой: воркер ждёт и
// берёт следующее задание, а не падает.
func TestWorkerDoesNothingOnEmptyQueue(t *testing.T) {
	jobs := newFakeJobs()
	stage := newFakeStage()

	newTestWorker(jobs, stage, &fakeClock{stopAfter: 3}).Run(context.Background())

	if stage.calls != 0 {
		t.Errorf("стадия вызвана на пустой очереди %d раз", stage.calls)
	}
}

// TestWorkerHeartbeatsWhileWorking — долгая работа продлевает lease сама.
// Эталонный срок lease 90 секунд (domain.LeaseDuration), heartbeat идёт каждые
func TestWorkerHeartbeatsWhileWorking(t *testing.T) {
	jobs := newFakeJobs().withIndexJob()
	stage := newFakeStage()
	stage.block = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		newTestWorker(jobs, stage, &fakeClock{stopAfter: 1_000_000}).Run(ctx)
	}()

	waitStage(t, stage)
	waitHeartbeat(t, jobs)
	waitHeartbeat(t, jobs)

	if got := jobs.heartbeatsTaken(); got < 2 {
		t.Errorf("выполнено %d продлений, ожидалось не меньше двух", got)
	}
	cancel()
	<-done
}

// TestWorkerStopsWorkWhenLeaseLost — утрата lease останавливает работу: ни
// Fail, ни попытки дописать чужое задание (ТЗ 7.7).
func TestWorkerStopsWorkWhenLeaseLost(t *testing.T) {
	jobs := newFakeJobs().withIndexJob()
	jobs.heartbeatOK = false
	stage := newFakeStage()
	stage.block = true

	done := make(chan struct{})
	go func() {
		defer close(done)
		newTestWorker(jobs, stage, &fakeClock{stopAfter: 1_000_000}).Run(context.Background())
	}()

	waitStage(t, stage)
	waitHeartbeat(t, jobs)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("цикл не завершился после утраты lease")
	}

	if stage.seenErr == nil {
		t.Error("стадия не была прервана отменой контекста")
	}
	if got := jobs.snapshot().failCount; got != 0 {
		t.Errorf("после утраты lease выполнено %d записей об ошибке, ожидался 0", got)
	}
}

// TestWorkerGivesUpOnLeaseLostError — тот же исход, когда сценарий сам вернул
// JOB_LEASE_LOST: чужое задание нельзя помечать ошибкой.
func TestWorkerGivesUpOnLeaseLostError(t *testing.T) {
	jobs := newFakeJobs().withIndexJob()
	stage := newFakeStage()
	stage.err = domain.JobLeaseLost("Задание перехвачено")

	newTestWorker(jobs, stage, &fakeClock{stopAfter: 1}).Run(context.Background())

	if got := jobs.snapshot().failCount; got != 0 {
		t.Errorf("выполнено %d записей об ошибке, ожидался 0", got)
	}
}

// TestWorkerPermanentErrorIsNotRetried — постоянные коды повторов не получают
// (ТЗ 2.4). Признак берётся из AppError, а не разбирается по тексту ошибки.
func TestWorkerPermanentErrorIsNotRetried(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"неверная настройка", domain.ConfigurationInvalid("overlap больше лимита", nil), domain.CodeConfigurationInvalid},
		{"несовпадение профиля", domain.IndexProfileMismatch("профиль другой"), domain.CodeIndexProfileMismatch},
		{"неверная размерность", domain.EmbeddingDimensionMismatch("размер", 1, 2), domain.CodeEmbeddingDimensionMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := newFakeJobs().withIndexJob()
			stage := newFakeStage()
			stage.err = tc.err

			newTestWorker(jobs, stage, &fakeClock{stopAfter: 1}).Run(context.Background())

			job := jobs.snapshot()
			if job.failCount != 1 {
				t.Fatalf("записей об ошибке %d, ожидалась 1", job.failCount)
			}
			if job.retryable {
				t.Error("постоянная ошибка помечена повторяемой")
			}
			if job.failCode != tc.code {
				t.Errorf("код %q вместо %q", job.failCode, tc.code)
			}
			if job.status != domain.JobStatusFailed {
				t.Errorf("статус %q вместо failed", job.status)
			}
		})
	}
}

// TestWorkerRetriesTemporaryErrorUntilCeiling — временная ошибка повторяется
// до потолка попыток. Цикл не должен ни остановиться раньше, ни продолжать
func TestWorkerRetriesTemporaryErrorUntilCeiling(t *testing.T) {
	jobs := newFakeJobs().withIndexJob()
	stage := newFakeStage()
	stage.err = domain.ServiceUnavailable("Elasticsearch недоступен")

	newTestWorker(jobs, stage, &fakeClock{stopAfter: 20}).Run(context.Background())

	job := jobs.snapshot()
	if job.failCount != maxAttempts {
		t.Errorf("записей об ошибке %d, ожидалось %d", job.failCount, maxAttempts)
	}
	if stage.calls != maxAttempts {
		t.Errorf("стадия выполнена %d раз, ожидалось %d", stage.calls, maxAttempts)
	}
	if !job.retryable {
		t.Error("временная ошибка помечена постоянной")
	}
	if job.status != domain.JobStatusFailed {
		t.Errorf("после потолка попыток статус %q вместо failed", job.status)
	}
	if job.failCode != domain.CodeServiceUnavailable {
		t.Errorf("код %q вместо SERVICE_UNAVAILABLE", job.failCode)
	}
	// Паузы между попытками — дело репозитория; цикл проверяет лишь, что он
	// их не переопределяет.
	want := []time.Duration{5 * time.Second, 30 * time.Second}
	if got := jobs.recordedDelays(); !slices.Equal(got, want) {
		t.Errorf("паузы перед повторами %v вместо %v", got, want)
	}
}

// TestWorkerRecordsInternalErrorForUnknownFailure — ошибка не из приложения не
// должна уехать в очередь как своя: клиент увидел бы код, которого нет в
func TestWorkerRecordsInternalErrorForUnknownFailure(t *testing.T) {
	jobs := newFakeJobs().withIndexJob()
	stage := newFakeStage()
	stage.err = errors.New("соединение разорвано")

	newTestWorker(jobs, stage, &fakeClock{stopAfter: 1}).Run(context.Background())

	job := jobs.snapshot()
	if job.failCode != domain.CodeInternalError {
		t.Errorf("код %q вместо INTERNAL_ERROR", job.failCode)
	}
	if !job.retryable {
		t.Error("внутренняя ошибка обязана быть повторяемой")
	}
	if job.failMsg == "соединение разорвано" {
		t.Error("текст внутренней ошибки не должен попадать в очередь как есть")
	}
}

// TestWorkerReleasesLeaseOnShutdown — остановка посреди задания не теряет
// его: запись об ошибке не пишется, lease истекает, и Claim отдаёт задание
func TestWorkerReleasesLeaseOnShutdown(t *testing.T) {
	jobs := newFakeJobs().withIndexJob()
	stage := newFakeStage()
	stage.block = true

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		newTestWorker(jobs, stage, &fakeClock{stopAfter: 1_000_000}).Run(ctx)
	}()

	waitStage(t, stage)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("цикл не завершился после отмены контекста")
	}

	job := jobs.snapshot()
	if job.failCount != 0 {
		t.Errorf("при остановке выполнено %d записей об ошибке, ожидался 0", job.failCount)
	}
	if job.status != domain.JobStatusRunning {
		t.Errorf("статус задания %q: lease должен истечь, а задание остаться в работе",
			job.status)
	}
}

// TestWorkerStopsOnContextCancel — отменённый контекст останавливает цикл даже
// на пустой очереди.
func TestWorkerStopsOnContextCancel(t *testing.T) {
	jobs := newFakeJobs()
	stage := newFakeStage()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		newTestWorker(jobs, stage, &fakeClock{stopAfter: 1_000_000}).Run(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("цикл не завершился после отмены контекста")
	}
}

// TestWorkerRejectsStageItDoesNotOwn — чужая стадия не исполняется как index.
// Через очередь это недостижимо (Claim фильтрует), но если фильтр сломается,
func TestWorkerRejectsStageItDoesNotOwn(t *testing.T) {
	jobs := newFakeJobs()
	jobs.job = &queuedJob{id: testTenantID, stage: domain.StageConvert, status: domain.JobStatusQueued}
	stage := newFakeStage()

	newTestWorker(jobs, stage, &fakeClock{stopAfter: 1}).Run(context.Background())

	if stage.calls != 0 {
		t.Error("стадия convert выполнена Go-воркером: её обслуживает Python")
	}
	if jobs.snapshot().retryable {
		t.Error("отказ по чужой стадии не должен повторяться")
	}
}
