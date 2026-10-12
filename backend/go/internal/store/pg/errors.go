package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// SQLSTATE-классы: важен класс (повтор имеет смысл или нет), а не каждый код.
const (
	sqlStateClassConnection    = "08" // connection_exception
	sqlStateClassTransaction   = "40" // transaction_rollback
	sqlStateClassAdminShutdown = "57" // operator_intervention
)

// wrapDBError превращает ошибку драйвера в ошибку приложения: отмена контекста —
// как есть; ошибка связи — SERVICE_UNAVAILABLE; остальное — INTERNAL_ERROR с Cause.
func wrapDBError(err error) error {
	if err == nil {
		return nil
	}
	// ErrNoRows — штатный исход, доходит до вызывающего неизменённым.
	if errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if isConnectionError(err) {
		return domain.ServiceUnavailable("PostgreSQL временно недоступен").WithCause(err)
	}
	return (&domain.AppError{
		Code:    domain.CodeInternalError,
		Status:  500,
		Message: "Ошибка обращения к PostgreSQL",
		Cause:   err,
	})
}

// isConnectionError сообщает, что ошибка лечится повтором.
func isConnectionError(err error) bool {
	if errors.Is(err, pgx.ErrTxClosed) {
		// Транзакция уже закрыта: повтор внутри неё невозможен, но и
		// «сервис недоступен» тут неверно — это ошибка вызывающего.
		return false
	}
	if pgconn.SafeToRetry(err) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch classOf(pgErr.SQLState()) {
		case sqlStateClassConnection, sqlStateClassTransaction, sqlStateClassAdminShutdown:
			return true
		}
	}
	return false
}

// classOf отдаёт первые две цифры SQLSTATE или пустую строку.
func classOf(sqlState string) string {
	if len(sqlState) < 2 {
		return ""
	}
	return sqlState[:2]
}

// isNoRows сообщает, что SELECT не вернул строк.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// noRows превращает pgx.ErrNoRows в доменную ошибку с текстом вызывающего.
func noRows(err error, message string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound(message)
	}
	return err
}

// errDocumentNotFound — единый NOT_FOUND для чужого/удалённого/отсутствующего
// документа: наружу случаи не различаются (иначе перебор id), текст один.
func errDocumentNotFound() error { return domain.NotFound("Документ не найден") }

// parseUUID разбирает строковый идентификатор порта (ошибка — дефект вызывающего).
func parseUUID(name, value string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s: некорректный UUID %q: %w", name, value, err)
	}
	return parsed, nil
}
