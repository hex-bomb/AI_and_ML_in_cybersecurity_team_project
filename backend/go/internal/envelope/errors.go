package domain

import (
	"errors"
	"fmt"
	"net/http"
)

// AppError is the single application error shape.
// Details go to HTTP as-is and must not hold secrets; Cause stays server-side.
type AppError struct {
	Code    string
	Status  int
	Message string
	Details map[string]any
	Cause   error
}

type errorSpec struct {
	status  int
	message string
}

// errorRegistry is the single code -> HTTP status source; order and values
// mirror errors.py + opensearch_index.py + embeddings.py.
var errorRegistry = map[string]errorSpec{
	"INTERNAL_ERROR": {status: http.StatusInternalServerError, message: "Внутренняя ошибка"},
	"NOT_FOUND":      {status: http.StatusNotFound, message: "Ресурс не найден"},
	// Текст ответа не различает «токена нет», «токен невалиден» и «токен
	// отозван»: различие дало бы переборщику оракул о том, какие токены
	// вообще бывают. Подробности — только в логе сервера.
	"UNAUTHORIZED": {status: http.StatusUnauthorized, message: "Требуется аутентификация"},
	"METHOD_NOT_ALLOWED": {
		status:  http.StatusMethodNotAllowed,
		message: "Метод не поддерживается",
	},
	"INVALID_METADATA": {
		status:  http.StatusUnprocessableEntity,
		message: "Некорректные данные",
	},
	"CONFLICT": {status: http.StatusConflict, message: "Конфликт состояния"},
	"DOCUMENT_BUSY": {
		status:  http.StatusConflict,
		message: "Документ уже обрабатывается",
	},
	"DOCUMENT_EXISTS": {status: http.StatusConflict, message: "Документ с таким external_id уже существует"},
	"IDEMPOTENCY_CONFLICT": {
		status:  http.StatusConflict,
		message: "Ключ идемпотентности использован с другими данными",
	},
	"CONFIGURATION_MISSING": {
		status:  http.StatusConflict,
		message: "Не заполнены обязательные настройки",
	},
	"CONFIGURATION_INVALID": {
		status:  http.StatusConflict,
		message: "Настройки имеют неверное значение",
	},
	"EMBEDDING_PROFILE_CHANGE_REQUIRES_REBUILD": {
		status:  http.StatusConflict,
		message: "Смена embedding-профиля требует переиндексации",
	},
	"SERVICE_UNAVAILABLE": {status: http.StatusServiceUnavailable, message: "Сервис временно недоступен"},
	"LLM_UNAVAILABLE":     {status: http.StatusServiceUnavailable, message: "Генерация ответа временно недоступна"},
	"LLM_RESPONSE_INVALID": {
		status:  http.StatusBadGateway,
		message: "Некорректный ответ модели",
	},
	"JOB_LEASE_LOST":       {status: http.StatusConflict, message: "Lease задания утрачен"},
	"PROVIDER_UNAVAILABLE": {status: http.StatusServiceUnavailable, message: "Провайдер временно недоступен"},
	"INDEX_UNAVAILABLE":    {status: http.StatusServiceUnavailable, message: "Индекс поиска временно недоступен"},
	"INDEX_PROFILE_MISMATCH": {
		status:  http.StatusConflict,
		message: "Индекс не соответствует embedding-профилю",
	},
	"EMBEDDING_RESPONSE_INVALID": {
		status:  http.StatusBadGateway,
		message: "Некорректный ответ embedding-провайдера",
	},
	"EMBEDDING_DIMENSION_MISMATCH": {
		status:  http.StatusConflict,
		message: "Размерность embedding не совпадает с профилем",
	},
	"UNSUPPORTED_FORMAT": {
		status:  http.StatusUnsupportedMediaType,
		message: "Формат файла не поддерживается",
	},
	"FILE_TOO_LARGE":    {status: http.StatusRequestEntityTooLarge, message: "Файл слишком большой"},
	"TOO_MANY_REQUESTS": {status: http.StatusTooManyRequests, message: "Слишком много запросов"},
}

// permanentCodes mirrors PERMANENT_CODES in app/worker.py.
var permanentCodes = map[string]struct{}{
	"CONFIGURATION_INVALID":                     {},
	"CONFIGURATION_MISSING":                     {},
	"EMBEDDING_DIMENSION_MISMATCH":              {},
	"EMBEDDING_PROFILE_CHANGE_REQUIRES_REBUILD": {},
	"EMBEDDING_RESPONSE_INVALID":                {},
	"INDEX_PROFILE_MISMATCH":                    {},
	"UNSUPPORTED_FORMAT":                        {},
	"INVALID_METADATA":                          {},
}

const (
	CodeInternalError                = "INTERNAL_ERROR"
	CodeNotFound                     = "NOT_FOUND"
	CodeUnauthorized                 = "UNAUTHORIZED"
	CodeMethodNotAllowed             = "METHOD_NOT_ALLOWED"
	CodeInvalidMetadata              = "INVALID_METADATA"
	CodeConflict                     = "CONFLICT"
	CodeDocumentBusy                 = "DOCUMENT_BUSY"
	CodeDocumentExists               = "DOCUMENT_EXISTS"
	CodeIdempotencyConflict          = "IDEMPOTENCY_CONFLICT"
	CodeConfigurationMissing         = "CONFIGURATION_MISSING"
	CodeConfigurationInvalid         = "CONFIGURATION_INVALID"
	CodeEmbeddingProfileNeedsRebuild = "EMBEDDING_PROFILE_CHANGE_REQUIRES_REBUILD"
	CodeServiceUnavailable           = "SERVICE_UNAVAILABLE"
	CodeLLMUnavailable               = "LLM_UNAVAILABLE"
	CodeLLMResponseInvalid           = "LLM_RESPONSE_INVALID"
	CodeJobLeaseLost                 = "JOB_LEASE_LOST"
	CodeProviderUnavailable          = "PROVIDER_UNAVAILABLE"
	CodeIndexUnavailable             = "INDEX_UNAVAILABLE"
	CodeIndexProfileMismatch         = "INDEX_PROFILE_MISMATCH"
	CodeEmbeddingResponseInvalid     = "EMBEDDING_RESPONSE_INVALID"
	CodeEmbeddingDimensionMismatch   = "EMBEDDING_DIMENSION_MISMATCH"
	CodeUnsupportedFormat            = "UNSUPPORTED_FORMAT"
	CodeFileTooLarge                 = "FILE_TOO_LARGE"
	CodeTooManyRequests              = "TOO_MANY_REQUESTS"
)

var (
	ErrNotFound                     = newAppError(CodeNotFound, "")
	ErrUnauthorized                 = newAppError(CodeUnauthorized, "")
	ErrConfigurationMissing         = newAppError(CodeConfigurationMissing, "")
	ErrConfigurationInvalid         = newAppError(CodeConfigurationInvalid, "")
	ErrConflict                     = newAppError(CodeConflict, "")
	ErrDocumentBusy                 = newAppError(CodeDocumentBusy, "")
	ErrDocumentExists               = newAppError(CodeDocumentExists, "")
	ErrIdempotencyConflict          = newAppError(CodeIdempotencyConflict, "")
	ErrServiceUnavailable           = newAppError(CodeServiceUnavailable, "")
	ErrLLMUnavailable               = newAppError(CodeLLMUnavailable, "")
	ErrLLMResponseInvalid           = newAppError(CodeLLMResponseInvalid, "")
	ErrJobLeaseLost                 = newAppError(CodeJobLeaseLost, "")
	ErrIndexUnavailable             = newAppError(CodeIndexUnavailable, "")
	ErrIndexProfileMismatch         = newAppError(CodeIndexProfileMismatch, "")
	ErrEmbeddingResponseInvalid     = newAppError(CodeEmbeddingResponseInvalid, "")
	ErrEmbeddingDimensionMismatch   = newAppError(CodeEmbeddingDimensionMismatch, "")
	ErrEmbeddingProfileNeedsRebuild = newAppError(CodeEmbeddingProfileNeedsRebuild, "")
)

// newAppError builds from code and message.
func newAppError(code, message string) *AppError {
	spec, ok := errorRegistry[code]
	if !ok {
		return &AppError{
			Code:    CodeInternalError,
			Status:  http.StatusInternalServerError,
			Message: "Внутренняя ошибка",
			Cause:   fmt.Errorf("неизвестный код ошибки %q", code),
		}
	}
	if message == "" {
		message = spec.message
	}
	return &AppError{Code: code, Status: spec.status, Message: message}
}

func NotFound(message string) *AppError { return newAppError(CodeNotFound, message) }

// Unauthorized — не хватает действительного токена. Сообщение намеренно
// одинаково для всех причин отказа: см. комментарий у UNAUTHORIZED в
// errorRegistry.
func Unauthorized(message string) *AppError { return newAppError(CodeUnauthorized, message) }

func MethodNotAllowed(message string) *AppError {
	return newAppError(CodeMethodNotAllowed, message)
}

func Conflict(message string) *AppError { return newAppError(CodeConflict, message) }

func newConfigurationInvalid(message string) *AppError {
	return ConfigurationInvalid(message, nil)
}

func ConfigurationMissing(message string, details map[string]any) *AppError {
	return newAppError(CodeConfigurationMissing, message).WithDetails(details)
}

func ConfigurationInvalid(message string, details map[string]any) *AppError {
	return newAppError(CodeConfigurationInvalid, message).WithDetails(details)
}

func ServiceUnavailable(message string) *AppError {
	return newAppError(CodeServiceUnavailable, message)
}

func LLMUnavailable(message string) *AppError { return newAppError(CodeLLMUnavailable, message) }

func LLMResponseInvalid(message string) *AppError {
	return newAppError(CodeLLMResponseInvalid, message)
}

func UnsupportedFormat(message string) *AppError { return newAppError(CodeUnsupportedFormat, message) }

func FileTooLarge(message string) *AppError { return newAppError(CodeFileTooLarge, message) }

func DocumentBusy(message string) *AppError { return newAppError(CodeDocumentBusy, message) }

func DocumentExists(message string) *AppError { return newAppError(CodeDocumentExists, message) }

func IdempotencyConflict(message string) *AppError {
	return newAppError(CodeIdempotencyConflict, message)
}

func JobLeaseLost(message string) *AppError { return newAppError(CodeJobLeaseLost, message) }

func IndexUnavailable(message string) *AppError { return newAppError(CodeIndexUnavailable, message) }

func IndexProfileMismatch(message string) *AppError {
	return newAppError(CodeIndexProfileMismatch, message)
}

func EmbeddingResponseInvalid(message string) *AppError {
	return newAppError(CodeEmbeddingResponseInvalid, message)
}

func EmbeddingDimensionMismatch(message string, got, want int) *AppError {
	return newAppError(CodeEmbeddingDimensionMismatch, message).
		WithDetails(map[string]any{"got": got, "want": want})
}

func EmbeddingProfileNeedsRebuild(message string) *AppError {
	return newAppError(CodeEmbeddingProfileNeedsRebuild, message)
}

func InvalidMetadata(message string) *AppError { return newAppError(CodeInvalidMetadata, message) }

// Internal builds a 500 INTERNAL_ERROR.
func Internal(message string) *AppError { return newAppError(CodeInternalError, message) }

// TooManyRequests builds a 429 TOO_MANY_REQUESTS.
func TooManyRequests(message string) *AppError { return newAppError(CodeTooManyRequests, message) }

// WithMessage returns a copy with a replaced message.
func (e *AppError) WithMessage(message string) *AppError {
	clone := *e
	clone.Message = message
	return &clone
}

// WithDetails returns a copy with a cloned details map.
func (e *AppError) WithDetails(details map[string]any) *AppError {
	clone := *e
	if details == nil {
		clone.Details = nil
		return &clone
	}
	next := make(map[string]any, len(details))
	for k, v := range details {
		next[k] = v
	}
	clone.Details = next
	return &clone
}

func (e *AppError) WithCause(cause error) *AppError {
	clone := *e
	clone.Cause = cause
	return &clone
}

func (e *AppError) Error() string { return e.Code + ": " + e.Message }

func (e *AppError) Unwrap() error { return e.Cause }

// Is compares by code so same-code errors match across constructors.
func (e *AppError) Is(target error) bool {
	other, ok := target.(*AppError)
	return ok && other.Code == e.Code
}

func (e *AppError) Retryable() bool {
	_, permanent := permanentCodes[e.Code]
	return !permanent
}

func (e *AppError) Permanent() bool { return !e.Retryable() }

func AsAppError(err error) (*AppError, bool) {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

func (e *AppError) HTTPStatus() int { return e.Status }

func (e *AppError) MessageOrDefault() string {
	if e.Message != "" {
		return e.Message
	}
	if spec, ok := errorRegistry[e.Code]; ok {
		return spec.message
	}
	return "Внутренняя ошибка"
}
