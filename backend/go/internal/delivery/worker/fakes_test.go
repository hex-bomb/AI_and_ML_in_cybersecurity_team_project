package worker_test

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/application/ports"
	"github.com/kbsearch/backend-go/internal/domain"
)

// maxAttempts повторяет ТЗ 2.4 — то же число стоит в
// infrastructure/postgres (job_repo.go). Подделка queue намеренно повторяет
const (
	maxAttempts  = 3
	testWorkerID = "worker-test-1"
)

var (
	testTenantID   = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	testDocumentID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	testVersionID  = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

// queuedJob — состояние задания в подделке очереди.
type queuedJob struct {
	id        uuid.UUID
	stage     string
	status    string
	attempt   int
	nextAt    time.Time
	failCode  string
	failMsg   string
	retryable bool
	failCount int
}

// fakeJobs — очередь заданий в памяти.
// Моделирует ровно то, что видно циклу: выдачу под lease, продление, запись
type fakeJobs struct {
	mu sync.Mutex

	job    *queuedJob
	claims int
	// delays записывает паузы перед повторами: их значения задаёт
	// репозиторий, и цикл не должен их ни вычислять, ни обходить.
	delays []time.Duration

	// base и offset — время подделки. Ожидание next_attempt_at не
	// проигрывается в реальности, а проматывает часы подделки: тест
	base   time.Time
	offset time.Duration

	stages   []string
	workerID string

	// heartbeatOK — результат Heartbeat: false означает «lease уже чужой».
	heartbeatOK   bool
	heartbeatSeen int
	// heartbeats сигналит каждый вызов Heartbeat, чтобы тест дождался нужного
	// тика, а не уснул наугад.
	heartbeats chan struct{}
}

func newFakeJobs() *fakeJobs {
	return &fakeJobs{
		heartbeatOK: true,
		heartbeats:  make(chan struct{}, 64),
		base:        time.Now(),
	}
}

func (f *fakeJobs) withIndexJob() *fakeJobs {
	f.job = &queuedJob{id: uuid.New(), stage: domain.StageIndex, status: domain.JobStatusQueued}
	return f
}

// withConvertJob кладёт задание стадии convert — её ставит загрузка документа.
func (f *fakeJobs) withConvertJob() *fakeJobs {
	f.job = &queuedJob{id: uuid.New(), stage: domain.StageConvert, status: domain.JobStatusQueued}
	return f
}

func (f *fakeJobs) now() time.Time { return f.base.Add(f.offset) }

// available сообщает, может ли задание быть выдано. Сроки повторов считаются по
// времени подделки; живой lease второй раз не выдаётся. Истёкший lease
func (f *fakeJobs) available() bool {
	if f.job.status != domain.JobStatusQueued {
		return false
	}
	if f.job.nextAt.After(f.now()) {
		f.offset += f.job.nextAt.Sub(f.now())
	}
	return true
}

func (f *fakeJobs) Claim(
	_ context.Context, _ ports.Queryer, workerID string, stages []string,
) (*domain.JobLease, error) {
	f.mu.Lock()
	f.claims++
	f.stages = stages
	f.workerID = workerID
	if f.job == nil || !f.available() {
		f.mu.Unlock()
		return nil, nil
	}
	f.job.status = domain.JobStatusRunning
	f.job.attempt++
	version := testVersionID
	lease := &domain.JobLease{
		JobID:          f.job.id,
		TenantID:       testTenantID,
		DocumentID:     testDocumentID,
		VersionID:      &version,
		Kind:           domain.KindIngest,
		Stage:          f.job.stage,
		Attempt:        f.job.attempt,
		LeaseToken:     "lease-" + uuid.NewString()[:8],
		LeaseExpiresAt: f.now().Add(domain.LeaseDuration),
	}
	f.mu.Unlock()
	return lease, nil
}

func (f *fakeJobs) Heartbeat(
	_ context.Context, _ ports.Queryer, _, _ string,
) (bool, error) {
	f.mu.Lock()
	f.heartbeatSeen++
	ok := f.heartbeatOK
	f.mu.Unlock()

	select {
	case f.heartbeats <- struct{}{}:
	default:
	}
	return ok, nil
}

func (f *fakeJobs) FinishIndex(
	_ context.Context, _ ports.Queryer, _, _ string, _ domain.IndexingResult,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.job.status = domain.JobStatusSucceeded
	return nil
}

// FinishConvert завершает задание convert и переводит его на стадию index —
// повторяет advance_to_index из ТЗ 3.6.
func (f *fakeJobs) FinishConvert(
	_ context.Context, _ ports.Queryer, _, _ string, _ domain.ConversionResult,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.job.status = domain.JobStatusSucceeded
	return nil
}

// Fail повторяет политику репозитория: постоянная ошибка завершает задание,
// временная возвращает его в очередь с паузой, пока не исчерпан потолок попыток.
func (f *fakeJobs) Fail(
	_ context.Context, _ ports.Queryer, _, _, code, message string, retryable bool,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.job.failCount++
	f.job.failCode = code
	f.job.failMsg = message
	f.job.retryable = retryable

	f.job.status = domain.JobStatusFailed
	f.job.nextAt = time.Time{}
	if retryable && f.job.attempt < maxAttempts {
		f.job.status = domain.JobStatusQueued
		f.job.nextAt = f.now().Add(delayFor(f.job.attempt))
		f.delays = append(f.delays, delayFor(f.job.attempt))
	}
	return nil
}

func delayFor(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 5 * time.Second
	case 2:
		return 30 * time.Second
	default:
		return 0
	}
}

func (f *fakeJobs) EnqueueCleanup(
	_ context.Context, _ ports.Queryer, _, _, _ string,
) (string, error) {
	return uuid.NewString(), nil
}

func (f *fakeJobs) snapshot() queuedJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.job
}

func (f *fakeJobs) recordedDelays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.delays...)
}

func (f *fakeJobs) heartbeatsTaken() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeatSeen
}

// fakeStage — стадии convert и index под управлением теста.
type fakeStage struct {
	// err — ошибка, которую вернёт стадия.
	err error
	// block заставляет стадию ждать отмены контекста: так проверяются
	// остановка процесса и утрата lease на середине задания.
	block bool

	calls        int
	convertCalls int
	started      chan struct{}
	lease        domain.JobLease
	seenErr      error
	// jobs — очередь, в которую стадия convert пишет FinishConvert. nil у
	// большинства тестов: там важна сама обработка, а не её итог.
	jobs *fakeJobs
}

func newFakeStage() *fakeStage {
	return &fakeStage{started: make(chan struct{}, 16)}
}

func (s *fakeStage) Index(ctx context.Context, lease domain.JobLease) error {
	s.calls++
	return s.run(ctx, lease)
}

// Convert — стадия разбора. Считается отдельно от Index: тест должен видеть,
// какую именно стадию цикл выполнил, иначе обе проверки проходили бы на
// одном и том же вызове.
// Convert — стадия разбора. Завершает задание через FinishConvert, как это
// делает настоящий конвертер: тест проверяет, что стадия convert дошла до
// перевода задания, а не просто отработала вхолостую.
func (s *fakeStage) Convert(ctx context.Context, lease domain.JobLease) error {
	s.convertCalls++
	// Lease записывается до завершения задания: тест должен видеть, какую
	// стадию цикл отдал, даже когда подделка отвечает в очередь сама.
	s.lease = lease
	if s.jobs != nil {
		if s.err != nil {
			return s.err
		}
		versionID, _ := lease.VersionIDValue()
		return s.jobs.FinishConvert(ctx, fakeQueryer{}, lease.JobID.String(), lease.LeaseToken,
			domain.ConversionResult{
				VersionID: versionID.String(), ParsedKey: "parsed.json", BlockCount: 1,
			})
	}
	return s.run(ctx, lease)
}

func (s *fakeStage) run(ctx context.Context, lease domain.JobLease) error {
	s.lease = lease
	select {
	case s.started <- struct{}{}:
	default:
	}
	if s.block {
		<-ctx.Done()
		s.seenErr = ctx.Err()
		return ctx.Err()
	}
	return s.err
}

// fakeClock не ждёт реального времени: Sleep отменяется контекстом или
// останавливает цикл по достижении лимита опросов.
type fakeClock struct {
	mu        sync.Mutex
	stopAfter int
	sleeps    int
}

func (c *fakeClock) Now() time.Time { return time.Now() }

func (c *fakeClock) Sleep(ctx context.Context, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps++
	stop := c.sleeps >= c.stopAfter
	c.mu.Unlock()
	if stop {
		return context.Canceled
	}
	return nil
}

// fakeQueryer — заглушка Queryer: цикл только передаёт его дальше, SQL не
// выполняется.
type fakeQueryer struct{}

func (fakeQueryer) Query(context.Context, string, ...any) (ports.Rows, error) {
	return nil, context.Canceled
}

func (fakeQueryer) QueryRow(context.Context, string, ...any) ports.Row {
	return nil
}

func (fakeQueryer) Exec(context.Context, string, ...any) (ports.CommandTag, error) {
	return nil, context.Canceled
}

// fakeTransactor повторяет откат: ошибка из fn не должна оставить запись
// «сделанной».
type fakeTransactor struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (t *fakeTransactor) WithTx(_ context.Context, fn func(ports.Queryer) error) error {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if t.err != nil {
		return t.err
	}
	return fn(fakeQueryer{})
}
