package httpapi

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/platform/config"
)

// TestValidateSourceURLAllowsOnlyHTTP — хранимый source_url не должен уносить
// произвольные URI (javascript:, file:) в выдачу.
func TestValidateSourceURLAllowsOnlyHTTP(t *testing.T) {
	good := []string{
		"http://example.com/doc.pdf",
		"https://example.com:8443/x?y=1#f",
		"", "   ",
	}
	for _, raw := range good {
		if err := validateSourceURL(raw); err != nil {
			t.Errorf("validateSourceURL(%q) = %v, ожидался приём", raw, err)
		}
	}
	bad := []string{
		"javascript:alert(1)",
		"ftp://example.com/x",
		"file:///etc/passwd",
		"data:text/plain,hi",
		"example.com/без-схемы",
		"http://",
		"https://",
		"//example.com/без-схемы",
		strings.Repeat("a", maxSourceURLLen+1),
		"http://" + strings.Repeat("a", maxSourceURLLen),
	}
	for _, raw := range bad {
		err := validateSourceURL(raw)
		appErr, ok := domain.AsAppError(err)
		if !ok || appErr.Code != domain.CodeInvalidMetadata {
			t.Errorf("validateSourceURL(%q) = %v, ожидался INVALID_METADATA", raw, err)
		}
	}
}

// TestReadMetadataCapsSize — поле metadata ограничено 64 КБ до разбора.
func TestReadMetadataCapsSize(t *testing.T) {
	big := strings.Repeat("a", maxMetadataBytes+1)
	r := multipartRequest(t, map[string]string{"metadata": `{"title":"` + big + `"}`})
	if _, err := readMetadata(r); err == nil {
		t.Fatal("metadata больше 64 КБ принят")
	} else if appErr, ok := domain.AsAppError(err); !ok || appErr.Code != domain.CodeInvalidMetadata {
		t.Fatalf("код = %v, ожидался INVALID_METADATA", err)
	}

	r = multipartRequest(t, map[string]string{"metadata": `{"title":"x", "source_url":"javascript:1"}`})
	if _, err := readMetadata(r); err == nil {
		t.Fatal("metadata с плохим source_url принят")
	}

	r = multipartRequest(t, map[string]string{"metadata": `{"title":"Норм", "source_url":"https://example.com/d"}`})
	md, err := readMetadata(r)
	if err != nil {
		t.Fatalf("корректный metadata отвергнут: %v", err)
	}
	if md.Title != "Норм" || md.SourceURL == nil || *md.SourceURL != "https://example.com/d" {
		t.Errorf("metadata разобран неверно: %+v", md)
	}
}

// TestReadUploadRejectsLongTitle — title из metadata режется на 500 рунах.
func TestReadUploadRejectsLongTitle(t *testing.T) {
	cfg := config.Config{MaxUploadBytes: 1 << 20, AllowedUploadExts: []string{".md"}}
	h := &apiHandler{cfg: cfg}
	r := multipartRequestWithFile(t, `{"title":"`+strings.Repeat("я", maxTitleRunes+1)+`"}`, "doc.md", "текст")
	if _, err := h.readUpload(r); err == nil {
		t.Fatal("title длиннее 500 рун принят")
	} else if appErr, ok := domain.AsAppError(err); !ok || appErr.Code != domain.CodeInvalidMetadata {
		t.Fatalf("код = %v, ожидался INVALID_METADATA", err)
	}
}

// TestDispatchRejectsOverlongInput — query/question режутся на 2000 рунах,
// name — на 500 (422 до сценариев).
func TestDispatchRejectsOverlongInput(t *testing.T) {
	kbID := uuid.NewString()
	handler := Handler(config.Config{}, Services{
		Auth:    stubAuth{},
		Search:  stubSearch{},
		Answers: stubAnswers{},
		KB:      stubKB{},
	})

	longQuery := strings.Repeat("я", maxQueryRunes+1)
	for _, tc := range []struct {
		path, body string
	}{
		{"/api/v1/search", `{"kb_id":"` + kbID + `","query":"` + longQuery + `"}`},
		{"/api/v1/answers", `{"kb_id":"` + kbID + `","question":"` + longQuery + `"}`},
		{"/api/v1/kbs", `{"name":"` + strings.Repeat("n", maxNameRunes+1) + `"}`},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("POST %s: код %d, ожидался 422", tc.path, w.Code)
		}
		if !strings.Contains(w.Body.String(), `"code":"INVALID_METADATA"`) {
			t.Errorf("POST %s: нет кода INVALID_METADATA: %s", tc.path, w.Body.String())
		}
	}

	// Граница ровно в лимит — принимается (сценарий-заглушка отвечает 200/201).
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/search", `{"kb_id":"` + kbID + `","query":"` + strings.Repeat("я", maxQueryRunes) + `"}`, http.StatusOK},
		{"/api/v1/answers", `{"kb_id":"` + kbID + `","question":"` + strings.Repeat("я", maxQueryRunes) + `"}`, http.StatusOK},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("POST %s: код %d, ожидался %d (%s)", tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

// TestRateLimiterWindow — квота исчерпывается и обновляется по таймеру.
func TestRateLimiterWindow(t *testing.T) {
	limiter := newRateLimiter(2, time.Minute)
	now := time.Now()
	// Вызовы разнесены по переменным намеренно: в одном условии `||`
	// короткое замыкание скрыло бы, какой именно вызов отвергнут.
	first := limiter.allow("1.2.3.4", now)
	second := limiter.allow("1.2.3.4", now)
	if !first || !second {
		t.Fatalf("первые два запроса в окне отвергнуты: первый=%t второй=%t", first, second)
	}
	if limiter.allow("1.2.3.4", now) {
		t.Fatal("третий запрос в окне принят")
	}
	if !limiter.allow("5.6.7.8", now) {
		t.Fatal("другой IP пострадал от чужой квоты")
	}
	if !limiter.allow("1.2.3.4", now.Add(2*time.Minute)) {
		t.Fatal("квота не обновилась после окна")
	}
}

// TestLimitedRouteScope — считаются только дорогие POST.
func TestLimitedRouteScope(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/v1/search", true},
		{http.MethodPost, "/api/v1/answers", true},
		{http.MethodPost, "/api/v1/kbs/22222222-2222-4222-8222-222222222222/documents", true},
		{http.MethodGet, "/api/v1/search", false},
		{http.MethodGet, "/api/v1/documents", false},
		{http.MethodPost, "/api/v1/kbs", false},
		{http.MethodGet, "/health/live", false},
		{http.MethodPost, "/api/v1/documents/1/retry", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if got := limitedRoute(r); got != tc.want {
			t.Errorf("%s %s: limitedRoute = %v, ожидалось %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestRateLimitMiddlewareReturns429 — сквозная проверка: 61-й POST /search
// за минуту отдаёт 429 TOO_MANY_REQUESTS с единым конвертом ошибки.
func TestRateLimitMiddlewareReturns429(t *testing.T) {
	handler := Handler(config.Config{}, Services{})
	kbID := uuid.NewString()
	body := `{"kb_id":"` + kbID + `","query":"x"}`
	var last *httptest.ResponseRecorder
	for range rateLimitMax + 1 {
		last = httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(body))
		r.RemoteAddr = "10.9.9.9:1234"
		handler.ServeHTTP(last, r)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("61-й запрос: код %d, ожидался 429", last.Code)
	}
	if !strings.Contains(last.Body.String(), `"code":"TOO_MANY_REQUESTS"`) {
		t.Errorf("нет кода TOO_MANY_REQUESTS: %s", last.Body.String())
	}
}

// ── заглушки сценариев ─────────────────────────────────────────────

type stubAuth struct{}

func (stubAuth) Principal(*http.Request) (domain.Principal, error) {
	return domain.LocalPrincipal(), nil
}

type stubSearch struct{}

func (stubSearch) Search(context.Context, domain.Principal, SearchInput) (SearchOutput, error) {
	return SearchOutput{Status: "ok"}, nil
}

type stubAnswers struct{}

func (stubAnswers) Answer(context.Context, domain.Principal, AnswerInput) (AnswerOutput, error) {
	return AnswerOutput{Status: "ok"}, nil
}

type stubKB struct{}

func (stubKB) CreateKB(context.Context, domain.Principal, string) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (stubKB) ListKBs(context.Context, domain.Principal, int, string) ([]KBView, string, error) {
	return nil, "", nil
}

func (stubKB) GetKB(context.Context, domain.Principal, uuid.UUID) (KBView, error) {
	return KBView{}, nil
}

func (stubKB) SetKBACL(context.Context, domain.Principal, uuid.UUID, domain.AllowedList) error {
	return nil
}

// ── конструкторы multipart-запросов ────────────────────────────────

func multipartRequest(t *testing.T, fields map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/kbs/1/documents", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func multipartRequestWithFile(t *testing.T, metadata, filename, content string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("metadata", metadata); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/kbs/1/documents", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}
