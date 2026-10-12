package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kbsearch/kb/internal/platform/config"
)

// timeoutAfterSeconds — короткая ожидалка для проверки «сервис ответил».
func timeoutAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}

// TestUnmatchedPathReturns404Fast — регрессия на монтирование роутера в
// самого себя.
//
// Сборка была r.Mount("/", HandlerWithOptions(..., BaseRouter: r)).
// Сгенерированный обработчик регистрирует маршруты прямо на переданном
// роутере, поэтому при несовпадении пути запрос повторно попадал в тот же
// mount и уходил в бесконечную рекурсию: клиент висел до своего таймаута,
// а в логе не появлялось ни строки.
//
// Тест ловит именно зависание, а не код ответа: без таймаунера зависший
// тест завис бы сам.
func TestUnmatchedPathReturns404Fast(t *testing.T) {
	handler := Handler(config.Config{}, Services{})

	// Только пути, которых нет в контракте. Путь, существующий с другим
	// методом, должен давать 405, а не 404.
	paths := []string{
		"/api/v1/нет-такого",
		"/",
		"/api/v2/kbs",
		"/api/v1/documents/22222222-2222-4222-8222-222222222222/несуществует",
	}
	for _, path := range paths {
		done := make(chan int, 1)
		go func() {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			done <- w.Code
		}()
		select {
		case code := <-done:
			if code != http.StatusNotFound {
				t.Errorf("%s: код %d, ожидался 404", path, code)
			}
		case <-timeoutAfterSeconds(3):
			t.Fatalf("%s: запрос не вернулся — рекурсия в роутере вернулась", path)
		}
	}
}

// TestKnownPathsStillRouted — после починки роутера маршруты из контракта
// должны продолжать работать: тест выше проходит и при полностью
func TestKnownPathsStillRouted(t *testing.T) {
	handler := Handler(config.Config{}, Services{})

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/health/live", http.StatusOK},
		{http.MethodGet, "/api/v1/me", http.StatusServiceUnavailable},
		// Сценарий не подключён — внятный 503, а не 404: маршрут есть.
		{http.MethodGet, "/api/v1/kbs", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Errorf("%s %s: код %d, ожидался %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
}

// TestErrorEnvelopeOnNotFound — ТЗ 2.3 требует единый формат ошибки и на
// неизвестный путь тоже: клиент различает «нет такого маршрута» по коду
func TestErrorEnvelopeOnNotFound(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(config.Config{}, Services{}).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/нет", nil))

	body := w.Body.String()
	for _, want := range []string{`"code":"NOT_FOUND"`, `"request_id":`} {
		if !strings.Contains(body, want) {
			t.Errorf("в ответе 404 нет %s: %s", want, body)
		}
	}
}
