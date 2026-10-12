package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// IdempotencyTTL — срок хранения ключа идемпотентности (ТЗ 2.4).
const IdempotencyTTL = 24 * time.Hour

// MaxIdempotencyKeyLength — предел длины ключа (CHECK ck_api_idempotency_key_length).
const MaxIdempotencyKeyLength = 128

// IdempotencyKey — идентичность запроса (ТЗ 2.4); RequestHash сравнивается, не ищется.
type IdempotencyKey struct {
	TenantID    uuid.UUID
	UserID      string
	Method      string
	Path        string
	Key         string
	RequestHash string
}

// IdempotencyRecord — сохранённый исходный ответ.
type IdempotencyRecord struct {
	ID             uuid.UUID
	ResponseStatus int
	ResponseBody   map[string]any
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// IdempotencyRepository — порт идемпотентности (в domain его нет — TODO: перенести в порты).
type IdempotencyRepository interface {
	// Lookup возвращает сохранённый ответ.
	// nil без ошибки — ключ не использован (либо истёк и был удалён).
	// Тот же ключ с другим телом — ErrIdempotencyConflict.
	Lookup(ctx context.Context, q ports.Queryer, key IdempotencyKey) (*IdempotencyRecord, error)

	// Store сохраняет исходный ответ. Повторная запись того же ключа
	// безопасна: первый ответ остаётся действующим.
	Store(ctx context.Context, q ports.Queryer, key IdempotencyKey, status int, body map[string]any) error

	// PurgeExpired удаляет истёкшие записи и возвращает их число.
	PurgeExpired(ctx context.Context, q ports.Queryer) (int64, error)
}

// idempotencyRepo — реализация поверх PostgreSQL.
type idempotencyRepo struct {
	clock ports.Clock
	ttl   time.Duration
}

var _ IdempotencyRepository = (*idempotencyRepo)(nil)

// NewIdempotencyRepository создаёт репозиторий (нулевой ttl — IdempotencyTTL).
func NewIdempotencyRepository(clock ports.Clock, ttl time.Duration) IdempotencyRepository {
	if ttl <= 0 {
		ttl = IdempotencyTTL
	}
	return &idempotencyRepo{clock: clock, ttl: ttl}
}

// Lookup ищет сохранённый ответ по ключу.
func (r *idempotencyRepo) Lookup(
	ctx context.Context, q ports.Queryer, key IdempotencyKey,
) (*IdempotencyRecord, error) {
	if err := validateIdempotencyKey(key.Key); err != nil {
		return nil, err
	}
	now := r.clock.Now()

	const query = `
		SELECT id, request_hash, response_status, response_body, created_at, expires_at
		FROM api_idempotency
		WHERE tenant_id = $1 AND user_id = $2 AND method = $3 AND path = $4 AND key = $5`
	var (
		record      IdempotencyRecord
		requestHash string
		rawBody     []byte
		createdAt   time.Time
		expiresAt   time.Time
	)
	err := q.QueryRow(ctx, query,
		key.TenantID, key.UserID, key.Method, key.Path, key.Key,
	).Scan(&record.ID, &requestHash, &record.ResponseStatus, &rawBody, &createdAt, &expiresAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}

	// Истёкший ключ — как отсутствующий, запись удаляется.
	if !expiresAt.After(now) {
		const deleteExpired = `
			DELETE FROM api_idempotency
			WHERE tenant_id = $1 AND user_id = $2 AND method = $3 AND path = $4 AND key = $5`
		if _, err := q.Exec(ctx, deleteExpired,
			key.TenantID, key.UserID, key.Method, key.Path, key.Key); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if requestHash != key.RequestHash {
		return nil, domain.IdempotencyConflict(
			"Ключ идемпотентности уже использован с другими данными")
	}

	body := map[string]any{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		// Тело пишет сам сервис — нечитаемый JSON здесь дефект кода.
		return nil, (&domain.AppError{
			Code:    domain.CodeInternalError,
			Status:  500,
			Message: "Сохранённый ответ идемпотентности не читается",
			Cause:   err,
		})
	}
	record.ResponseBody = body
	record.CreatedAt = createdAt.UTC()
	record.ExpiresAt = expiresAt.UTC()
	return &record, nil
}

// Store сохраняет исходный ответ (ON CONFLICT DO NOTHING — первый ответ побеждает).
func (r *idempotencyRepo) Store(
	ctx context.Context, q ports.Queryer, key IdempotencyKey, status int, body map[string]any,
) error {
	if err := validateIdempotencyKey(key.Key); err != nil {
		return err
	}
	if body == nil {
		body = map[string]any{}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return (&domain.AppError{
			Code:    domain.CodeInternalError,
			Status:  500,
			Message: "Тело ответа не сериализуется в JSON",
			Cause:   err,
		})
	}

	now := r.clock.Now()
	const query = `
		INSERT INTO api_idempotency (id, tenant_id, user_id, method, path, key,
		                              request_hash, response_status, response_body,
		                              created_at, expires_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)
		ON CONFLICT (tenant_id, user_id, method, path, key) DO NOTHING`
	_, err = q.Exec(ctx, query,
		key.TenantID, key.UserID, key.Method, key.Path, key.Key,
		key.RequestHash, status, payload, now, now.Add(r.ttl))
	return err
}

// PurgeExpired удаляет истёкшие записи.
func (r *idempotencyRepo) PurgeExpired(ctx context.Context, q ports.Queryer) (int64, error) {
	const query = `DELETE FROM api_idempotency WHERE expires_at <= $1`
	tag, err := q.Exec(ctx, query, r.clock.Now())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// validateIdempotencyKey проверяет длину ключа (400 Python здесь 422 INVALID_METADATA).
func validateIdempotencyKey(key string) error {
	if len(key) == 0 || len(key) > MaxIdempotencyKeyLength {
		return &domain.AppError{
			Code:    domain.CodeInvalidMetadata,
			Status:  http.StatusUnprocessableEntity,
			Message: "Idempotency-Key должен содержать от 1 до 128 символов",
		}
	}
	return nil
}

// RequestHash — отпечаток тела запроса: SHA-256 файла + канонический JSON метаданных.
func RequestHash(content []byte, metadata map[string]any) (string, error) {
	digest := sha256.New()

	contentDigest := sha256.Sum256(content)
	digest.Write(contentDigest[:])

	canonical, err := canonicalMetadataJSON(metadata)
	if err != nil {
		return "", err
	}
	digest.Write(canonical)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// canonicalMetadataJSON — канонический JSON как Python sort_keys/separators/ensure_ascii=False.
func canonicalMetadataJSON(metadata map[string]any) ([]byte, error) {
	if metadata == nil {
		metadata = map[string]any{}
	}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalizeJSONNumbers(metadata)); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// IdempotencyStoreAdapter отдаёт хранилище в виде, нужном сценарию документов.
type IdempotencyStoreAdapter struct{ repo IdempotencyRepository }

// NewIdempotencyStoreAdapter оборачивает репозиторий для сценария.
func NewIdempotencyStoreAdapter(repo IdempotencyRepository) IdempotencyStoreAdapter {
	return IdempotencyStoreAdapter{repo: repo}
}

// Lookup отдаёт сохранённый ответ сценарию.
func (a IdempotencyStoreAdapter) Lookup(
	ctx context.Context, q ports.Queryer, key domain.DocumentIdempotencyKey,
) (*domain.DocumentIdempotencyRecord, error) {
	saved, err := a.repo.Lookup(ctx, q, IdempotencyKey{
		TenantID: key.TenantID, UserID: key.UserID, Method: key.Method,
		Path: key.Path, Key: key.Key, RequestHash: key.RequestHash,
	})
	if err != nil || saved == nil {
		return nil, err
	}
	return &domain.DocumentIdempotencyRecord{
		Status: saved.ResponseStatus,
		Body:   saved.ResponseBody,
	}, nil
}

// Store сохраняет ответ под ключом.
func (a IdempotencyStoreAdapter) Store(
	ctx context.Context, q ports.Queryer,
	key domain.DocumentIdempotencyKey, status int, body map[string]any,
) error {
	return a.repo.Store(ctx, q, IdempotencyKey{
		TenantID: key.TenantID, UserID: key.UserID, Method: key.Method,
		Path: key.Path, Key: key.Key, RequestHash: key.RequestHash,
	}, status, body)
}
