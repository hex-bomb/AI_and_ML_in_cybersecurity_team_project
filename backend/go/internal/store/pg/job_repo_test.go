package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Тесты очереди. Каждый комментарий «Порт test_jobs.py:NNN» указывает
// Python-тест, который перенесён, — так сверяется семантика двух реализаций.

func newTestJobs() *JobRepository {
	return NewJobRepository(ports.SystemClock{})
}

func newTestDocuments() *DocumentRepository {
	return NewDocumentRepository(ports.SystemClock{})
}

// claimInTx оборачивает claim в транзакцию — так его вызывает воркер.
func claimInTx(
	t *testing.T, ctx context.Context, repo *JobRepository, workerID string, stages []string,
) *domain.JobLease {
	t.Helper()
	lease, err := repo.Claim(ctx, testDB, workerID, stages)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return lease
}

// ── claim ──────────────────────────────────────────────────────────

// TestClaimTakesQueuedJobAndIssuesLease — порт test_jobs.py:172.
func TestClaimTakesQueuedJobAndIssuesLease(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if lease == nil {
		t.Fatal("Claim вернул nil на непустой очереди")
	}
	if lease.JobID != jobID {
		t.Errorf("job_id = %s, ожидался %s", lease.JobID, jobID)
	}
	if lease.TenantID != tenantID || lease.DocumentID != docID {
		t.Errorf("идентификаторы в lease не совпали: %+v", lease)
	}
	if lease.VersionID == nil || *lease.VersionID != versionID {
		t.Errorf("version_id = %v, ожидался %s", lease.VersionID, versionID)
	}
	if lease.Kind != domain.KindIngest || lease.Stage != domain.StageConvert {
		t.Errorf("kind/stage = %s/%s, ожидалось ingest/convert", lease.Kind, lease.Stage)
	}
	// Попытка увеличивается при выдаче, а не при успехе: счётчик должен
	// пережить падение процесса (Python jobs.py:93).
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d, ожидалась 1", lease.Attempt)
	}
	if lease.LeaseToken == "" {
		t.Error("lease_token пуст")
	}
	// Lease = domain.LeaseDuration (90 с). В Python 120 с — расхождение
	// зафиксировано в отчёте, здесь проверяется константа домена.
	ttl := time.Until(lease.LeaseExpiresAt)
	if ttl < domain.LeaseDuration-time.Minute || ttl > domain.LeaseDuration {
		t.Errorf("срок lease = %v, ожидалось около %v", ttl, domain.LeaseDuration)
	}

	record := getJob(t, ctx, repo)
	if record.Status != domain.JobStatusRunning {
		t.Errorf("status = %s, ожидался running", record.Status)
	}
	if record.WorkerID == nil || *record.WorkerID != "worker-1" {
		t.Errorf("worker_id = %v, ожидался worker-1", record.WorkerID)
	}
}

// TestClaimReturnsNilWhenQueueEmpty — порт test_jobs.py:195.
func TestClaimReturnsNilWhenQueueEmpty(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if lease != nil {
		t.Fatalf("Claim вернул %+v на пустой очереди, ожидался nil", lease)
	}
}

// TestClaimSkipsStageNotRequested — порт test_jobs.py:200.
// Критично для совместной работы двух воркеров: Go-воркер обслуживает
// стадии convert, index и delete, а cleanup остаётся за отдельным потребителем.
// Чужую стадию Claim обязан пропустить, иначе задание уборки забрал бы
// Go-воркер и убрал версию из-под работающего с ней поиска.
//
// Эталонной чужой стадией здесь cleanup, а не delete: удаление обслуживает этот
// же воркер (domain.GoStages содержит StageDelete), и проверять его надо на
// заборе, а не на отказе.
func TestClaimSkipsStageNotRequested(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{stage: domain.StageCleanup})
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", domain.GoStages())
	if lease != nil {
		t.Fatalf("Go-воркер забрал задание стадии %s, а её обслуживает другой воркер", lease.Stage)
	}
}

// TestSecondWorkerCannotClaimJobWithLiveLease — порт test_jobs.py:206.
func TestSecondWorkerCannotClaimJobWithLiveLease(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	first := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if first == nil {
		t.Fatal("первый воркер не получил задание")
	}

	// Второй claim идёт на пуле (autocommit) — то есть в отдельной неявной
	// транзакции, ровно как у другого процесса.
	second := claimInTx(t, ctx, repo, "worker-2", []string{domain.StageConvert})
	if second != nil {
		t.Fatalf("второй воркер забрал задание с живым lease: %+v", second)
	}
}

// TestClaimIsAtomicUnderConcurrentWorkers — проверка без Python-теста.
// Выдача задания обязана быть атомарной даже без явной транзакции: если
func TestClaimIsAtomicUnderConcurrentWorkers(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	const workers = 10
	results := make(chan *domain.JobLease, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func(n int) {
			<-start
			lease, err := repo.Claim(ctx, testDB, "worker", []string{domain.StageConvert})
			if err != nil {
				t.Errorf("воркер %d: %v", n, err)
				results <- nil
				return
			}
			results <- lease
		}(i)
	}
	close(start)

	claimed := 0
	for i := 0; i < workers; i++ {
		if lease := <-results; lease != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("задание выдано %d воркерам, ожидался ровно один", claimed)
	}
}

// TestClaimSkipsJobAwaitingRetryDelay — порт test_jobs.py:217.
func TestClaimSkipsJobAwaitingRetryDelay(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	// Задержка ретрая в будущем: задание не должно выдаваться.
	execRaw(t, ctx, "UPDATE jobs SET next_attempt_at = now() + interval '1 hour' WHERE id = $1", jobID)

	if lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert}); lease != nil {
		t.Fatalf("задание с будущей задержкой выдано: %+v", lease)
	}
}

// ── heartbeat ──────────────────────────────────────────────────────

// TestHeartbeatExtendsLease — порт test_jobs.py:228.
func TestHeartbeatExtendsLease(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if lease == nil {
		t.Fatal("задание не выдано")
	}
	before := getJob(t, ctx, repo).LeaseExpiresAt
	if before == nil {
		t.Fatal("после claim lease_expires_at должен быть заполнен")
	}

	renewed, err := repo.Heartbeat(ctx, testDB, jobID.String(), lease.LeaseToken)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !renewed {
		t.Fatal("Heartbeat вернул false для действующего lease")
	}

	after := getJob(t, ctx, repo).LeaseExpiresAt
	if after == nil || !after.After(*before) {
		t.Fatalf("срок lease не продлён: было %v, стало %v", before, after)
	}
}

// TestHeartbeatDoesNotResurrectExpiredLease — проверка без Python-теста.
// Heartbeat не должен продлевать уже истёкший lease: иначе воскресший
func TestHeartbeatDoesNotResurrectExpiredLease(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if lease == nil {
		t.Fatal("задание не выдано")
	}
	execRaw(t, ctx, "UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1", jobID)

	renewed, err := repo.Heartbeat(ctx, testDB, jobID.String(), lease.LeaseToken)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if renewed {
		t.Fatal("истёкший lease был продлён")
	}
}

// TestHeartbeatWithWrongTokenReturnsFalse — порт test_jobs.py:239.
func TestHeartbeatWithWrongTokenReturnsFalse(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	if lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert}); lease == nil {
		t.Fatal("задание не выдано")
	}
	renewed, err := repo.Heartbeat(ctx, testDB, jobID.String(), "wrong-token")
	if err != nil {
		t.Fatalf("чужой токен должен давать false без ошибки, получено: %v", err)
	}
	if renewed {
		t.Fatal("Heartbeat с чужим токеном вернул true")
	}
}

// TestExpiredLeaseCanBeReclaimedAndOldTokenRejected — порт test_jobs.py:249.
func TestExpiredLeaseCanBeReclaimedAndOldTokenRejected(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	stale := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if stale == nil {
		t.Fatal("первое задание не выдано")
	}
	execRaw(t, ctx, "UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1", jobID)

	fresh := claimInTx(t, ctx, repo, "worker-2", []string{domain.StageConvert})
	if fresh == nil {
		t.Fatal("истёкший lease не был перевыдан")
	}
	if fresh.LeaseToken == stale.LeaseToken {
		t.Fatal("после перевыдачи токен не изменился")
	}

	// Старый обработчик больше не владеет заданием (ТЗ 7.7).
	if renewed, err := repo.Heartbeat(ctx, testDB, jobID.String(), stale.LeaseToken); err != nil || renewed {
		t.Fatalf("старый токен продлил lease: renewed=%v err=%v", renewed, err)
	}
	err := repo.FinishIndex(ctx, testDB, jobID.String(), stale.LeaseToken,
		domain.NewIndexingResult(docID.String(), versionID.String(), profileID, 7))
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("FinishIndex со старым токеном: %v, ожидался ErrJobLeaseLost", err)
	}
	if renewed, err := repo.Heartbeat(ctx, testDB, jobID.String(), fresh.LeaseToken); err != nil || !renewed {
		t.Fatalf("новый токен не продлил lease: renewed=%v err=%v", renewed, err)
	}
}

// TestFinishIndexWithWrongLeaseTokenReturnsJobLeaseLost — порт test_jobs.py:270.
func TestFinishIndexWithWrongLeaseTokenReturnsJobLeaseLost(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()
	lease := advanceToIndex(t, ctx, repo)

	err := repo.FinishIndex(ctx, testDB, jobID.String(), "wrong-token",
		domain.NewIndexingResult(docID.String(), versionID.String(), profileID, 7))
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("FinishIndex с чужим токеном: %v, ожидался ErrJobLeaseLost", err)
	}

	// Задание не изменилось: осталось running с прежним токеном (ТЗ 3.6).
	record := getJob(t, ctx, repo)
	if record.Status != domain.JobStatusRunning {
		t.Errorf("status = %s, ожидался running", record.Status)
	}
	if record.LeaseToken == nil || *record.LeaseToken != lease.LeaseToken {
		t.Errorf("lease_token изменился: %v", record.LeaseToken)
	}
}

// TestFinishIndexRejectsJobOnWrongStage — проверка без Python-теста.
// Задание на стадии convert закрывать как проиндексированное нельзя:
func TestFinishIndexRejectsJobOnWrongStage(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if lease == nil {
		t.Fatal("задание не выдано")
	}
	err := repo.FinishIndex(ctx, testDB, jobID.String(), lease.LeaseToken,
		domain.NewIndexingResult(docID.String(), versionID.String(), profileID, 3))
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("FinishIndex на стадии convert: %v, ожидался ErrJobLeaseLost", err)
	}
}

// ── повторы этапа ──────────────────────────────────────────────────

// TestFailRetriesThreeTimesWithFiveAndThirtySecondDelays — порт test_jobs.py:354.
func TestFailRetriesThreeTimesWithFiveAndThirtySecondDelays(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	var delays []int
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
		if lease == nil {
			t.Fatalf("попытка %d: задание не выдано", attempt)
		}
		if lease.Attempt != attempt {
			t.Fatalf("attempt = %d на попытке %d", lease.Attempt, attempt)
		}

		err := repo.Fail(ctx, testDB, jobID.String(), lease.LeaseToken,
			domain.CodeIndexUnavailable, "индекс недоступен", true)
		if err != nil {
			t.Fatalf("Fail: %v", err)
		}

		record := getJob(t, ctx, repo)
		if attempt < MaxAttempts {
			if record.Status != domain.JobStatusQueued {
				t.Fatalf("попытка %d: status = %s, ожидался queued", attempt, record.Status)
			}
			if record.NextAttemptAt == nil {
				t.Fatalf("попытка %d: next_attempt_at не задан", attempt)
			}
			delays = append(delays, int(time.Until(*record.NextAttemptAt).Round(time.Second)/time.Second))
			// Имитация истечения задержки перед следующей попыткой.
			execRaw(t, ctx, "UPDATE jobs SET next_attempt_at = now() - interval '1 second' WHERE id = $1", jobID)
			continue
		}
		if record.Status != domain.JobStatusFailed {
			t.Fatalf("попытка %d: status = %s, ожидался failed", attempt, record.Status)
		}
		if record.NextAttemptAt != nil {
			t.Fatalf("попытка %d: next_attempt_at = %v, ожидался NULL", attempt, record.NextAttemptAt)
		}
	}

	want := []int{int(RetryDelays[0] / time.Second), int(RetryDelays[1] / time.Second)}
	if len(delays) != len(want) {
		t.Fatalf("задержек %d, ожидалось %d: %v", len(delays), len(want), delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("задержки = %v, ожидалось %v", delays, want)
		}
	}
}

// TestFailNonRetryableMarksVersionFailed — порт test_jobs.py:375.
func TestFailNonRetryableMarksVersionFailed(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if lease == nil {
		t.Fatal("задание не выдано")
	}
	err := repo.Fail(ctx, testDB, jobID.String(), lease.LeaseToken, "OCR_REQUIRED", "нет текста", false)
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}

	record := getJob(t, ctx, repo)
	if record.Status != domain.JobStatusFailed {
		t.Errorf("status = %s, ожидался failed", record.Status)
	}
	if record.NextAttemptAt != nil {
		t.Errorf("next_attempt_at = %v, ожидался NULL", record.NextAttemptAt)
	}

	var (
		status      string
		failedStage *string
		errorCode   *string
	)
	err = testDB.QueryRow(ctx,
		"SELECT status, failed_stage, error_code FROM document_versions WHERE id = $1", versionID,
	).Scan(&status, &failedStage, &errorCode)
	if err != nil {
		t.Fatalf("чтение версии: %v", err)
	}
	if status != domain.VersionStatusFailed {
		t.Errorf("status версии = %s, ожидался failed", status)
	}
	if failedStage == nil || *failedStage != domain.StageConvert {
		t.Errorf("failed_stage = %v, ожидался convert", failedStage)
	}
	if errorCode == nil || *errorCode != "OCR_REQUIRED" {
		t.Errorf("error_code = %v, ожидался OCR_REQUIRED", errorCode)
	}
}

// TestFailRetryableKeepsVersionAliveUntilLastAttempt — проверка без Python-теста.
// Промежуточный статус версии сохраняется, пока будет ещё попытка: версия
func TestFailRetryableKeepsVersionAliveUntilLastAttempt(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if lease == nil {
		t.Fatal("задание не выдано")
	}
	if err := repo.Fail(ctx, testDB, jobID.String(), lease.LeaseToken,
		domain.CodeServiceUnavailable, "временно", true); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	var status string
	if err := testDB.QueryRow(ctx,
		"SELECT status FROM document_versions WHERE id = $1", versionID).Scan(&status); err != nil {
		t.Fatalf("чтение версии: %v", err)
	}
	if status == domain.VersionStatusFailed {
		t.Fatal("версия помечена failed при наличии ещё одной попытки")
	}

	// Код ошибки при этом записан: по документу должно быть видно, что
	// попытка была неуспешной (ТЗ 4.1).
	var errorCode *string
	if err := testDB.QueryRow(ctx,
		"SELECT error_code FROM document_versions WHERE id = $1", versionID).Scan(&errorCode); err != nil {
		t.Fatalf("чтение версии: %v", err)
	}
	if errorCode == nil || *errorCode != domain.CodeServiceUnavailable {
		t.Fatalf("error_code = %v, ожидался %s", errorCode, domain.CodeServiceUnavailable)
	}
}

// TestFailWithWrongTokenReturnsJobLeaseLost — проверка без Python-теста.
func TestFailWithWrongTokenReturnsJobLeaseLost(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	lease := claimInTx(t, ctx, repo, "worker-1", []string{domain.StageConvert})
	if lease == nil {
		t.Fatal("задание не выдано")
	}
	err := repo.Fail(ctx, testDB, jobID.String(), "wrong-token", domain.CodeInternalError, "x", true)
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("Fail с чужим токеном: %v, ожидался ErrJobLeaseLost", err)
	}
	// Состояние не тронуто.
	if record := getJob(t, ctx, repo); record.Status != domain.JobStatusRunning {
		t.Fatalf("status = %s, ожидался running", record.Status)
	}
}

// ── cleanup ────────────────────────────────────────────────────────

// TestEnqueueCleanupReturnsSameIDForSameVersion — порт test_jobs.py:398.
func TestEnqueueCleanupReturnsSameIDForSameVersion(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	var first string
	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		id, err := repo.EnqueueCleanup(ctx, tx, tenantID.String(), docID.String(), versionID.String())
		first = id
		return err
	})
	if err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	var second string
	err = testDB.WithTx(ctx, func(tx ports.Queryer) error {
		id, err := repo.EnqueueCleanup(ctx, tx, tenantID.String(), docID.String(), versionID.String())
		second = id
		return err
	})
	if err != nil {
		t.Fatalf("повторный EnqueueCleanup: %v", err)
	}
	if first != second {
		t.Fatalf("id задачи уборки изменился: %s -> %s", first, second)
	}

	// Задача должна быть в очереди на стадии cleanup, а не быть съедена
	// конфликтом.
	record, err := repo.Get(ctx, testDB, first)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.Kind != domain.KindCleanupVersion || record.Stage != domain.StageCleanup {
		t.Errorf("kind/stage = %s/%s, ожидались cleanup_version/cleanup", record.Kind, record.Stage)
	}
	if record.Status != domain.JobStatusQueued {
		t.Errorf("status = %s, ожидался queued", record.Status)
	}
}

// TestEnqueueCleanupIsNotClaimedByGoWorker — проверка без Python-теста.
// Cleanup обслуживает Python-воркер, поэтому Go-воркер со своим списком
func TestEnqueueCleanupIsNotClaimedByGoWorker(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	err := testDB.WithTx(ctx, func(tx ports.Queryer) error {
		_, err := repo.EnqueueCleanup(ctx, tx, tenantID.String(), docID.String(), versionID.String())
		return err
	})
	if err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	// Задание загрузки (стадия convert) тоже встанет в очередь, поэтому
	// проверяется именно та задача, которую Claim обязан пропустить: уборка.
	for i := 0; i < 3; i++ {
		lease := claimInTx(t, ctx, repo, "go-worker", domain.GoStages())
		if lease == nil {
			break
		}
		if lease.Stage == domain.StageCleanup {
			t.Fatalf("Go-воркер забрал задачу уборки: %+v", lease)
		}
	}
}

// ── Get ────────────────────────────────────────────────────────────

// TestGetReturnsJobState — проверка без Python-теста: состояние задания
// читается целиком, NULL-поля остаются NULL.
func TestGetReturnsJobState(t *testing.T) {
	requireDB(t)
	ctx := testContext(t)
	seedChain(t, ctx, seedOptions{})
	repo := newTestJobs()

	record, err := repo.Get(ctx, testDB, jobID.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.ID != jobID || record.DocumentID != docID {
		t.Errorf("идентификаторы не совпали: %+v", record)
	}
	if record.VersionID == nil || *record.VersionID != versionID {
		t.Errorf("version_id = %v", record.VersionID)
	}
	if record.LeaseToken != nil {
		t.Errorf("lease_token = %v, ожидался NULL до выдачи", *record.LeaseToken)
	}
	if record.TotalChunks != nil {
		t.Errorf("total_chunks = %v, ожидался NULL", *record.TotalChunks)
	}
	if record.ErrorCode != nil || record.ErrorMessage != nil {
		t.Errorf("ошибки не должны быть заполнены: %v / %v", record.ErrorCode, record.ErrorMessage)
	}

	if _, err := repo.Get(ctx, testDB, uuid.New().String()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get несуществующего задания: %v, ожидался ErrNotFound", err)
	}
}

// ── helpers ────────────────────────────────────────────────────────

// advanceToIndex доводит задание до работающего lease на стадии index.
// Стадия convert обслуживает Python-воркер, поэтому перевод делается напрямую
func advanceToIndex(t *testing.T, ctx context.Context, repo *JobRepository) *domain.JobLease {
	t.Helper()
	execRaw(t, ctx, `
		UPDATE jobs
		SET stage = 'index', status = 'queued', attempt = 0,
		    lease_token = NULL, lease_expires_at = NULL, next_attempt_at = NULL
		WHERE id = $1`, jobID)
	lease := claimInTx(t, ctx, repo, "worker-1", domain.GoStages())
	if lease == nil || lease.Stage != domain.StageIndex {
		t.Fatalf("задание не дошло до стадии index: %+v", lease)
	}
	return lease
}

// getJob читает состояние задания для проверок.
func getJob(t *testing.T, ctx context.Context, repo *JobRepository) *JobRecord {
	t.Helper()
	record, err := repo.Get(ctx, testDB, jobID.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return record
}
