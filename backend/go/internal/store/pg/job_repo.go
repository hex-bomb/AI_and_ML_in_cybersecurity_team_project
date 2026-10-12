package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// Политика повторов этапа (ТЗ 2.4).
const (
	// MaxAttempts — сколько раз воркер в сумме пытается выполнить этап.
	MaxAttempts = 3

	// Паузы перед второй и третьей попыткой.
	retryDelay1 = 5 * time.Second
	retryDelay2 = 30 * time.Second
)

// RetryDelays — задержки перед повторами (индекс — номер попытки минус один).
var RetryDelays = [MaxAttempts - 1]time.Duration{retryDelay1, retryDelay2}

// claimOrderBy — порядок очереди: created_at, id.
const claimOrderBy = "ORDER BY created_at, id"

// claimSQL — захват задания одним UPDATE с CTE (SELECT + UPDATE отдал бы одно
// задание двум воркерам). Фильтр stage — разделение с Python-воркером (ТЗ 3.6);
// attempt растёт при выдаче, чтобы пережить падение процесса.
const claimSQL = `
WITH candidate AS (
	SELECT id
	FROM jobs
	WHERE stage = ANY($2::text[])
	  AND (
		    (status = 'queued' AND (next_attempt_at IS NULL OR next_attempt_at <= $1))
		 OR (status = 'running' AND lease_expires_at <= $1)
		  )
	` + claimOrderBy + `
	LIMIT 1
	FOR UPDATE SKIP LOCKED
)
UPDATE jobs AS j
SET status          = 'running',
	attempt          = j.attempt + 1,
	lease_token      = gen_random_uuid()::text,
	lease_expires_at = $3,
	next_attempt_at  = NULL,
	worker_id        = $4,
	updated_at       = $1
FROM candidate
WHERE j.id = candidate.id
RETURNING j.id, j.tenant_id, j.document_id, j.version_id, j.kind, j.stage,
          j.attempt, j.lease_token, j.lease_expires_at`

// selectJobByID — полная выборка задания (и для Get, и под FOR UPDATE).
const selectJobByID = `
	SELECT id, tenant_id, document_id, version_id, kind, stage, status, attempt,
	       next_attempt_at, lease_token, lease_expires_at, worker_id,
	       error_code, error_message, processed_chunks, total_chunks
	FROM jobs
	WHERE id = $1`

// JobRecord — состояние задания (NULL-колонки — указатели).
type JobRecord struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	DocumentID     uuid.UUID
	VersionID      *uuid.UUID
	Kind           string
	Stage          string
	Status         string
	Attempt        int
	NextAttemptAt  *time.Time
	LeaseToken     *string
	LeaseExpiresAt *time.Time
	WorkerID       *string
	ErrorCode      *string
	ErrorMessage   *string
	ProcessedCount int
	TotalChunks    *int
}

// JobRepository — реализация ports.JobRepository (только стадия index, ТЗ 3.6).
type JobRepository struct {
	clock ports.Clock
}

var _ ports.JobRepository = (*JobRepository)(nil)

// NewJobRepository создаёт репозиторий очереди (clock — ради детерминированных тестов).
func NewJobRepository(clock ports.Clock) *JobRepository {
	return &JobRepository{clock: clock}
}

// Claim берёт задание под lease (nil без ошибки — очередь пуста).
func (r *JobRepository) Claim(
	ctx context.Context, q ports.Queryer, workerID string, stages []string,
) (*domain.JobLease, error) {
	if len(stages) == 0 {
		return nil, domain.ConfigurationInvalid("Список стадий задания пуст", nil)
	}
	now := r.clock.Now()

	row := q.QueryRow(ctx, claimSQL, now, stages, now.Add(domain.LeaseDuration), workerID)
	var (
		id         uuid.UUID
		tenantID   uuid.UUID
		documentID uuid.UUID
		versionID  *uuid.UUID
		kind       string
		stage      string
		attempt    int
		leaseToken *string
		expiresAt  *time.Time
	)
	err := row.Scan(&id, &tenantID, &documentID, &versionID, &kind, &stage,
		&attempt, &leaseToken, &expiresAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}

	return &domain.JobLease{
		JobID:          id,
		TenantID:       tenantID,
		DocumentID:     documentID,
		VersionID:      versionID,
		Kind:           kind,
		Stage:          stage,
		Attempt:        attempt,
		LeaseToken:     deref(leaseToken),
		LeaseExpiresAt: timeOrZero(expiresAt, now),
	}, nil
}

// Heartbeat продлевает живой lease (false — lease чужой, не ошибка).
func (r *JobRepository) Heartbeat(ctx context.Context, q ports.Queryer, jobID, leaseToken string) (bool, error) {
	job, err := parseUUID("job_id", jobID)
	if err != nil {
		return false, err
	}
	now := r.clock.Now()
	const query = `
		UPDATE jobs
		SET lease_expires_at = $3, updated_at = $1
		WHERE id = $2
		  AND status = 'running'
		  AND lease_token = $4
		  AND lease_expires_at > $1
		RETURNING id`
	var renewed uuid.UUID
	if err := q.QueryRow(ctx, query, now, job, now.Add(domain.LeaseDuration), leaseToken).Scan(&renewed); err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FinishConvert завершает стадию convert: помечает версию разобранной и
// переводит задание на стадию index (ТЗ 3.6 advance_to_index).
//
// Перевод делается UPDATE-ом той же строки jobs, а не вставкой нового задания.
// Это и есть механизм идемпотентности: у задания одна строка, второй перевод
// невозможен физически, а unique-индекс uq_jobs_active_ingest (одно активное
// ingest-задание на документ) не может быть нарушен. Новая строка конфликтовала
// бы с текущим заданием и упиралась в этот индекс.
//
// Обе записи — в одной транзакции вызывающего. Расходятся они лишь в одном
// сценарии: версия в статусе parsed, а задание всё ещё висит на convert.
// Повтор тогда не помогает — parsed_key уже записан, и стадия convert решит,
// что работа сделана.
func (r *JobRepository) FinishConvert(
	ctx context.Context, tx ports.Queryer, jobID, leaseToken string, res domain.ConversionResult,
) error {
	job, err := parseUUID("job_id", jobID)
	if err != nil {
		return err
	}
	version, err := parseUUID("version_id", res.VersionID)
	if err != nil {
		return err
	}

	record, err := r.lockRunningJob(ctx, tx, job, leaseToken)
	if err != nil {
		return err
	}
	// Чужая стадия — ошибка вызывающего, молча закрывать нельзя.
	if record.Stage != domain.StageConvert {
		return domain.JobLeaseLost("Задание не на этапе convert")
	}
	if record.VersionID == nil || *record.VersionID != version {
		return domain.Conflict("Задание convert ссылается на другую версию")
	}
	// Порядок блокировок как в Fail (задание -> версия).
	if err := lockVersion(ctx, tx, version, record.DocumentID); err != nil {
		return err
	}

	const versionQuery = `
		UPDATE document_versions
		SET parsed_key = $3,
			status     = 'parsed',
			failed_stage  = NULL,
			error_code    = NULL,
			error_message = NULL
		WHERE id = $1 AND document_id = $2`
	if _, err := tx.Exec(ctx, versionQuery, version, record.DocumentID, res.ParsedKey); err != nil {
		return err
	}

	// Счётчик попыток обнуляется: стадия сменилась, и её попытки должны
	// считаться заново — иначе версия, пережившая две неудачи на convert,
	// получила бы на index ноль повторов вместо трёх.
	const jobQuery = `
		UPDATE jobs
		SET status          = 'queued',
			stage           = $3,
			attempt         = 0,
			next_attempt_at = $4,
			lease_token     = NULL,
			lease_expires_at = NULL,
			error_code      = NULL,
			error_message   = NULL,
			processed_chunks = $5,
			total_chunks     = $5,
			updated_at       = $4
		WHERE id = $1 AND lease_token = $2`
	if _, err := tx.Exec(ctx, jobQuery, job, leaseToken,
		domain.StageIndex, r.clock.Now(), res.BlockCount); err != nil {
		return err
	}
	return nil
}

// FinishIndex завершает задание в одной транзакции с ActivateVersion (ТЗ 6.3 п.7).
func (r *JobRepository) FinishIndex(
	ctx context.Context, tx ports.Queryer, jobID, leaseToken string, res domain.IndexingResult,
) error {
	job, err := parseUUID("job_id", jobID)
	if err != nil {
		return err
	}
	record, err := r.lockRunningJob(ctx, tx, job, leaseToken)
	if err != nil {
		return err
	}
	// Чужая стадия — ошибка вызывающего, молча закрывать нельзя.
	if record.Stage != domain.StageIndex {
		return domain.JobLeaseLost("Задание не на этапе index")
	}

	const query = `
		UPDATE jobs
		SET status          = 'succeeded',
			processed_chunks = $3,
			total_chunks     = $3,
			lease_token      = NULL,
			lease_expires_at = NULL,
			next_attempt_at  = NULL,
			error_code       = NULL,
			error_message    = NULL,
			updated_at       = $4
		WHERE id = $1 AND lease_token = $2`
	if _, err := tx.Exec(ctx, query, job, leaseToken, res.ChunkCount, r.clock.Now()); err != nil {
		return err
	}
	return nil
}

// Fail фиксирует ошибку этапа и решает о повторе (ТЗ 2.4): временные — до
// MaxAttempts с RetryDelays; код ошибки пишется всегда (ТЗ 4.1).
func (r *JobRepository) Fail(
	ctx context.Context, tx ports.Queryer, jobID, leaseToken, code, message string, retryable bool,
) error {
	job, err := parseUUID("job_id", jobID)
	if err != nil {
		return err
	}
	record, err := r.lockRunningJob(ctx, tx, job, leaseToken)
	if err != nil {
		return err
	}

	now := r.clock.Now()
	willRetry := retryable && record.Attempt < MaxAttempts

	if record.VersionID != nil {
		// Порядок блокировок как в ActivateVersion (задание -> версия).
		if err := lockVersion(ctx, tx, *record.VersionID, record.DocumentID); err != nil {
			return err
		}
		// Статус — в том же UPDATE, без промежутка несогласованности.
		const versionQuery = `
			UPDATE document_versions
			SET failed_stage  = $3,
				error_code    = $4,
				error_message = $5,
				status        = CASE WHEN $6::boolean THEN status ELSE 'failed' END
			WHERE id = $1 AND document_id = $2`
		if _, err := tx.Exec(ctx, versionQuery,
			*record.VersionID, record.DocumentID, record.Stage, code, message, willRetry); err != nil {
			return err
		}
	}

	status := domain.JobStatusFailed
	var nextAttemptAt *time.Time
	if willRetry {
		delay, ok := retryDelay(record.Attempt)
		if !ok {
			return domain.Conflict("Номер попытки задания вне диапазона 1..3")
		}
		status = domain.JobStatusQueued
		next := now.Add(delay)
		nextAttemptAt = &next
	}

	const query = `
		UPDATE jobs
		SET status          = $5,
			next_attempt_at  = $6,
			lease_token      = NULL,
			lease_expires_at = NULL,
			error_code       = $3,
			error_message    = $4,
			updated_at       = $7
		WHERE id = $1 AND lease_token = $2`
	if _, err := tx.Exec(ctx, query,
		job, leaseToken, code, message, status, nextAttemptAt, now); err != nil {
		return err
	}
	return nil
}

// EnqueueCleanup ставит задачу уборки версии (идемпотентна: повтор возвращает прежний id).
func (r *JobRepository) EnqueueCleanup(
	ctx context.Context, tx ports.Queryer, tenantID, documentID, versionID string,
) (string, error) {
	tenant, err := parseUUID("tenant_id", tenantID)
	if err != nil {
		return "", err
	}
	docUUID, err := parseUUID("document_id", documentID)
	if err != nil {
		return "", err
	}
	versionUUID, err := parseUUID("version_id", versionID)
	if err != nil {
		return "", err
	}

	now := r.clock.Now()
	const insert = `
		INSERT INTO jobs (id, tenant_id, document_id, version_id, kind, stage, status,
		                  attempt, next_attempt_at, lease_token, lease_expires_at,
		                  worker_id, processed_chunks, total_chunks,
		                  error_code, error_message, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 'cleanup_version', 'cleanup', 'queued',
		        0, NULL, NULL, NULL, NULL, 0, NULL, NULL, NULL, $4, $4)
		ON CONFLICT (tenant_id, document_id, version_id) WHERE kind = 'cleanup_version'
		DO NOTHING`
	if _, err := tx.Exec(ctx, insert, tenant, docUUID, versionUUID, now); err != nil {
		return "", err
	}

	const selectExisting = `
		SELECT id FROM jobs
		WHERE tenant_id = $1 AND document_id = $2 AND version_id = $3
		  AND kind = 'cleanup_version'`
	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, selectExisting, tenant, docUUID, versionUUID).Scan(&jobID); err != nil {
		return "", noRows(err, "Задача уборки не найдена после вставки")
	}
	return jobID.String(), nil
}

// Get возвращает состояние задания (для диагностики и тестов).
func (r *JobRepository) Get(ctx context.Context, q ports.Queryer, jobID string) (*JobRecord, error) {
	job, err := parseUUID("job_id", jobID)
	if err != nil {
		return nil, err
	}
	record, err := scanJob(q.QueryRow(ctx, selectJobByID, job))
	if err != nil {
		if isNoRows(err) {
			return nil, domain.NotFound("Задание не найдено")
		}
		return nil, err
	}
	return record, nil
}

func (r *JobRepository) lockRunningJob(
	ctx context.Context, tx ports.Queryer, jobID uuid.UUID, leaseToken string,
) (*JobRecord, error) {
	return lockRunningJob(ctx, r.clock, tx, jobID, leaseToken)
}

// lockRunningJob блокирует строку задания и проверяет lease (ТЗ 3.6).
// Общая для очереди и ActivateVersion — проверка lease в приложении единственная.
func lockRunningJob(
	ctx context.Context, clock ports.Clock, tx ports.Queryer, jobID uuid.UUID, leaseToken string,
) (*JobRecord, error) {
	record, err := scanJob(tx.QueryRow(ctx, selectJobByID+" FOR UPDATE", jobID))
	if err != nil {
		if isNoRows(err) {
			return nil, domain.JobLeaseLost("Задание не найдено или уже разобрано")
		}
		return nil, err
	}
	now := clock.Now()
	if record.Status != domain.JobStatusRunning ||
		record.LeaseToken == nil || *record.LeaseToken != leaseToken ||
		record.LeaseExpiresAt == nil ||
		!record.LeaseExpiresAt.After(now) {
		return nil, domain.JobLeaseLost("Lease задания утрачен")
	}
	return record, nil
}

// lockVersion блокирует строку версии (порядок задание -> версия един здесь).
func lockVersion(ctx context.Context, tx ports.Queryer, versionID, documentID uuid.UUID) error {
	const query = `
		SELECT id FROM document_versions
		WHERE id = $1 AND document_id = $2
		FOR UPDATE`
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, query, versionID, documentID).Scan(&locked); err != nil {
		return noRows(err, "Версия документа не найдена")
	}
	return nil
}

// retryDelay отдаёт задержку по номеру попытки (false — повторять нельзя).
func retryDelay(attempt int) (time.Duration, bool) {
	switch attempt {
	case 1:
		return retryDelay1, true
	case 2:
		return retryDelay2, true
	default:
		return 0, false
	}
}

// scanJob читает полную строку задания в порядке selectJobByID.
func scanJob(row ports.Row) (*JobRecord, error) {
	var record JobRecord
	err := row.Scan(
		&record.ID,
		&record.TenantID,
		&record.DocumentID,
		&record.VersionID,
		&record.Kind,
		&record.Stage,
		&record.Status,
		&record.Attempt,
		&record.NextAttemptAt,
		&record.LeaseToken,
		&record.LeaseExpiresAt,
		&record.WorkerID,
		&record.ErrorCode,
		&record.ErrorMessage,
		&record.ProcessedCount,
		&record.TotalChunks,
	)
	if err != nil {
		return nil, err
	}
	if record.LeaseExpiresAt != nil {
		record.LeaseExpiresAt = ptr(record.LeaseExpiresAt.UTC())
	}
	if record.NextAttemptAt != nil {
		record.NextAttemptAt = ptr(record.NextAttemptAt.UTC())
	}
	return &record, nil
}

// timeOrZero подставляет fallback для NULL lease (нулевое время роняло бы lease).
func timeOrZero(value *time.Time, fallback time.Time) time.Time {
	if value == nil {
		return fallback
	}
	return value.UTC()
}

// ptr возвращает ссылку на копию времени в UTC.
func ptr(value time.Time) *time.Time {
	return &value
}
