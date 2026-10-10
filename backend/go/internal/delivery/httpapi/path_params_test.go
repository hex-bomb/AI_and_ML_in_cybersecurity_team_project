package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kbsearch/backend-go/internal/config"
)

// TestPathParamsMatchContractNames — параметры пути читаются по именам из
// контракта.
//
// Регрессия: обработчики искали «kbId», «documentId», «jobId», а oapi-
// codegen регистрирует маршруты по именам из спецификации — «kb_id»,
// «document_id», «job_id». chi.URLParam возвращает по точному имени, и все
// одиннадцать маршрутов с идентификатором в пути отдавали 422
// «Некорректный идентификатор в пути» с ПУСТЫМ значением.
//
// Тест ловит рассинхрон целиком: имя проверяется на каждом пути с
// параметром, а не выборочно.
func TestPathParamsMatchContractNames(t *testing.T) {
	const (
		kbID       = "22222222-2222-4222-8222-222222222222"
		documentID = "33333333-3333-4333-8333-333333333333"
		jobID      = "44444444-4444-4444-8444-444444444444"
	)
	handler := Handler(config.Config{}, Services{})

	paths := []string{
		"/api/v1/kbs/" + kbID + "/documents",
		"/api/v1/documents/" + documentID,
		"/api/v1/documents/" + documentID + "/content",
		"/api/v1/documents/" + documentID + "/reindex",
		"/api/v1/documents/" + documentID + "/retry",
		"/api/v1/documents/" + documentID + "/source",
		"/api/v1/documents/" + documentID + "/acl",
		"/api/v1/jobs/" + jobID,
	}

	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut,
			http.MethodPatch, http.MethodDelete} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))

			// Признак рассинхрона — сообщение с ПУСТЫМ идентификатором:
			// параметр в пути есть, но прочитать его не удалось.
			if body := w.Body.String(); strings.Contains(body, "идентификатор в пути: \"\"") {
				t.Errorf("%s %s: параметр пути не прочитан", method, path)
			}
			if w.Code == http.StatusNotFound {
				t.Errorf("%s %s: маршрут не зарегистрирован", method, path)
			}
		}
	}
}

// TestPathUUIDRejectsUnknownParam — сам pathUUID обязан отличать «параметр не
// найден» от «параметр найден, но значение не UUID». Оба случая дают 422, но
func TestPathUUIDRejectsUnknownParam(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/kbs/1", nil)

	if _, err := pathUUID(r, "kb_id"); err == nil {
		t.Fatal("параметр, которого нет в маршруте, прочитан")
	}
}

// TestQueryParamMissingIsError — отсутствующий kb_id в query отвергается:
// сценарий без него работал бы по всей базе.
func TestQueryParamMissingIsError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/documents", nil)
	if _, err := uuidParam(r, "kb_id"); err == nil {
		t.Fatal("отсутствующий kb_id принят")
	}

	r = httptest.NewRequest(http.MethodGet, "/api/v1/documents?kb_id="+uuid.NewString(), nil)
	if _, err := uuidParam(r, "kb_id"); err != nil {
		t.Fatalf("корректный kb_id отвергнут: %v", err)
	}
}
