package domain

import (
	"errors"
	"net/http"
	"testing"
)

func TestErrorRegistryMatchesPython(t *testing.T) {
	cases := []struct {
		code   string
		status int
	}{
		{CodeInternalError, http.StatusInternalServerError},
		{CodeNotFound, http.StatusNotFound},
		{CodeInvalidMetadata, http.StatusUnprocessableEntity},
		{CodeConflict, http.StatusConflict},
		{CodeDocumentBusy, http.StatusConflict},
		{CodeDocumentExists, http.StatusConflict},
		{CodeIdempotencyConflict, http.StatusConflict},
		{CodeConfigurationMissing, http.StatusConflict},
		{CodeConfigurationInvalid, http.StatusConflict},
		{CodeEmbeddingProfileNeedsRebuild, http.StatusConflict},
		{CodeServiceUnavailable, http.StatusServiceUnavailable},
		{CodeLLMUnavailable, http.StatusServiceUnavailable},
		{CodeLLMResponseInvalid, http.StatusBadGateway},
		{CodeJobLeaseLost, http.StatusConflict},
		{CodeProviderUnavailable, http.StatusServiceUnavailable},

		{CodeIndexUnavailable, http.StatusServiceUnavailable},
		{CodeIndexProfileMismatch, http.StatusConflict},

		{CodeEmbeddingResponseInvalid, http.StatusBadGateway},
		{CodeEmbeddingDimensionMismatch, http.StatusConflict},

		{CodeUnsupportedFormat, http.StatusUnsupportedMediaType},
		{CodeFileTooLarge, http.StatusRequestEntityTooLarge},
		{CodeTooManyRequests, http.StatusTooManyRequests},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			err := newAppError(tc.code, "")
			if err.Status != tc.status {
				t.Errorf("код %s: HTTP %d, ожидалось %d", tc.code, err.Status, tc.status)
			}
			if err.Code != tc.code {
				t.Errorf("получен код %s, ожидалось %s", err.Code, tc.code)
			}
			if err.Message == "" {
				t.Errorf("код %s: пустое сообщение по умолчанию", tc.code)
			}
		})
	}
}

func TestErrorRegistryHasNoOrphans(t *testing.T) {
	allCodes := []string{
		CodeInternalError, CodeNotFound, CodeMethodNotAllowed, CodeInvalidMetadata,
		CodeConflict,
		CodeDocumentBusy, CodeDocumentExists, CodeIdempotencyConflict,
		CodeConfigurationMissing, CodeConfigurationInvalid,
		CodeEmbeddingProfileNeedsRebuild, CodeServiceUnavailable,
		CodeLLMUnavailable, CodeLLMResponseInvalid, CodeJobLeaseLost,
		CodeProviderUnavailable, CodeIndexUnavailable, CodeIndexProfileMismatch,
		CodeEmbeddingResponseInvalid, CodeEmbeddingDimensionMismatch,
		CodeUnsupportedFormat, CodeFileTooLarge, CodeTooManyRequests,
		CodeUnauthorized,
	}

	for _, code := range allCodes {
		if _, ok := errorRegistry[code]; !ok {
			t.Errorf("код %s есть среди констант, но отсутствует в реестре", code)
		}
	}
	if len(errorRegistry) != len(allCodes) {
		t.Errorf("в реестре %d кодов, среди констант %d — лишние записи в реестре",
			len(errorRegistry), len(allCodes))
	}
	for code := range permanentCodes {
		if _, ok := errorRegistry[code]; !ok {
			t.Errorf("permanentCodes содержит код %s, которого нет в реестре", code)
		}
	}
}

func TestPermanentCodesMatchPython(t *testing.T) {
	wantCodes := map[string]struct{}{
		CodeConfigurationInvalid:         {},
		CodeConfigurationMissing:         {},
		CodeEmbeddingDimensionMismatch:   {},
		CodeEmbeddingProfileNeedsRebuild: {},
		CodeEmbeddingResponseInvalid:     {},
		CodeIndexProfileMismatch:         {},
		CodeUnsupportedFormat:            {},
		CodeInvalidMetadata:              {},
	}

	if len(permanentCodes) != len(wantCodes) {
		t.Errorf("в permanentCodes %d кодов, в Python %d", len(permanentCodes), len(wantCodes))
	}
	for code := range wantCodes {
		if _, ok := permanentCodes[code]; !ok {
			t.Errorf("код %s есть в PERMANENT_CODES Python, но отсутствует здесь", code)
		}
	}

	for code := range wantCodes {
		if newAppError(code, "").Retryable() {
			t.Errorf("код %s должен быть без ретрая, но Retryable() == true", code)
		}
	}
	for _, code := range []string{
		CodeServiceUnavailable, CodeIndexUnavailable, CodeLLMUnavailable,
		CodeProviderUnavailable, CodeNotFound,
	} {
		if !newAppError(code, "").Retryable() {
			t.Errorf("код %s должен быть повторяемым, но Retryable() == false", code)
		}
	}
}

func TestErrorsIsComparesByCode(t *testing.T) {
	if !errors.Is(NotFound("Ресурс не найден"), ErrNotFound) {
		t.Error("NotFound не совпал с ErrNotFound через errors.Is")
	}
	cause := errors.New("dial tcp: refused")
	wrapped := ServiceUnavailable("поиск недоступен").WithCause(cause)
	if !errors.Is(wrapped, ErrServiceUnavailable) {
		t.Error("обёрнутая ошибка потеряла код при errors.Is")
	}
	if !errors.Is(wrapped, cause) {
		t.Error("исходная ошибка должна оставаться в цепочке")
	}
}

func TestAsAppErrorFindsWrappedError(t *testing.T) {
	inner := LLMUnavailable("ollama не отвечает")
	got, ok := AsAppError(inner.WithCause(errors.New("dial tcp")))
	if !ok {
		t.Fatal("AsAppError не нашёл AppError")
	}
	if got.Code != CodeLLMUnavailable {
		t.Errorf("код %s, ожидалось %s", got.Code, CodeLLMUnavailable)
	}

	if _, ok := AsAppError(errors.New("чужой пакет")); ok {
		t.Error("AsAppError не должен опознавать постороннюю ошибку как AppError")
	}
}

func TestUnknownCodeDegradesToInternalError(t *testing.T) {
	err := newAppError("TYPO_IN_CODE", "что-то сломалось")
	if err.Code != CodeInternalError {
		t.Errorf("код %s, ожидалось %s", err.Code, CodeInternalError)
	}
	if err.Status != http.StatusInternalServerError {
		t.Errorf("HTTP %d, ожидалось 500", err.Status)
	}
	if err.Message != "Внутренняя ошибка" {
		t.Errorf("сообщение %q: текст не должен утекать наружу вместе с опечаткой в коде", err.Message)
	}
}

func TestWithMessageDoesNotMutateSentinel(t *testing.T) {
	before := ErrNotFound.Message
	custom := NotFound("База знаний не найдена")

	if custom.Message == before {
		t.Error("WithMessage/конструктор не задали собственное сообщение")
	}
	if ErrNotFound.Message != before {
		t.Errorf("sentinel изменился: %q -> %q", before, ErrNotFound.Message)
	}
}

func TestEmbeddingDimensionMismatchCarriesDetails(t *testing.T) {
	err := EmbeddingDimensionMismatch("размерность не совпала", 768, 384)
	if err.Details["got"] != 768 || err.Details["want"] != 384 {
		t.Errorf("details = %v, ожидалось got=768 want=384", err.Details)
	}
}

func TestErrorMessageOrDefault(t *testing.T) {
	err := &AppError{Code: CodeNotFound, Status: http.StatusNotFound}
	if got := err.MessageOrDefault(); got != "Ресурс не найден" {
		t.Errorf("MessageOrDefault() = %q, ожидалось %q", got, "Ресурс не найден")
	}
	unknown := &AppError{Code: "NOPE"}
	if got := unknown.MessageOrDefault(); got != "Внутренняя ошибка" {
		t.Errorf("MessageOrDefault() для неизвестного кода = %q", got)
	}
}
