package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	contracts "github.com/kbsearch/kb/api"
	domain "github.com/kbsearch/kb/internal/envelope"
)

// TestStrictJSONRejectsUnknownFields — ТЗ требует отклонять неизвестные поля
// в теле запроса. Сгенерированный oapi-codegen strict-server этого не делает:
func TestStrictJSONRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"лишнее поле", `{"kb_id":"22222222-2222-4222-8222-222222222222","query":"x","выдумка":1}`},
		{"опечатка в имени", `{"kb_id":"22222222-2222-4222-8222-222222222222","queri":"x"}`},
		{"лишнее поле с null", `{"kb_id":"22222222-2222-4222-8222-222222222222","query":"x","лишнее":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/search", strings.NewReader(tc.body))
			var target contracts.SearchBody
			err := strictJSON(r, &target)
			if err == nil {
				t.Fatalf("тело с неизвестным полем принято: %+v", target)
			}
			appErr, ok := domain.AsAppError(err)
			if !ok || appErr.Code != domain.CodeInvalidMetadata {
				t.Fatalf("код ошибки: %v, ожидался INVALID_METADATA", err)
			}
		})
	}
}

// TestStrictJSONAcceptsValidBody — обратная сторона: валидное тело проходит.
func TestStrictJSONAcceptsValidBody(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/v1/search",
		strings.NewReader(`{"kb_id":"22222222-2222-4222-8222-222222222222","query":"момент затяжки","top_k":5}`))
	var target contracts.SearchBody
	if err := strictJSON(r, &target); err != nil {
		t.Fatalf("валидное тело отвергнуто: %v", err)
	}
	if target.Query != "момент затяжки" {
		t.Errorf("query = %q", target.Query)
	}
	if target.TopK == nil || *target.TopK != 5 {
		t.Errorf("top_k не разобран: %v", target.TopK)
	}
}

// TestStrictJSONRejectsTrailingValue — два JSON-значения подряд принимает
// только ленивый декодер, и лишнее молча терялось бы.
func TestStrictJSONRejectsTrailingValue(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/v1/search",
		strings.NewReader(`{"query":"a"}{"query":"b"}`))
	var target contracts.SearchBody
	if err := strictJSON(r, &target); err == nil {
		t.Fatal("два значения в теле приняты")
	}
}

// TestParseLimitCapsValue — без верхней границы клиент запросил бы миллион
// строк и положил бы процесс.
func TestParseLimitCapsValue(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 50},
		{"10", 10},
		{"99999", 200},
		{"-5", 50},
		{"мусор", 50},
		{"0", 50},
	}
	for _, tc := range cases {
		if got := parseLimit(tc.raw, 50, 200); got != tc.want {
			t.Errorf("parseLimit(%q) = %d, ожидалось %d", tc.raw, got, tc.want)
		}
	}
}

// TestWriteErrorUsesDomainStatus — код ответа берётся из доменной ошибки,
// иначе ТЗ 2.3 «единый формат» соблюдался бы только на словах.
func TestWriteErrorUsesDomainStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{domain.NotFound("нет"), 404},
		{domain.InvalidMetadata("плохо"), 422},
		{domain.FileTooLarge("велик"), 413},
		{domain.UnsupportedFormat("не тот"), 415},
		{domain.ServiceUnavailable("нет связи"), 503},
		{domain.LLMUnavailable("нет модели"), 503},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		WriteError(w, r, tc.err)
		if w.Code != tc.want {
			t.Errorf("код для %v = %d, ожидалось %d", tc.err, w.Code, tc.want)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
	}
}

// TestErrorEnvelopeHasRequestID — ТЗ 2.3 требует request_id в каждом
// ответе об ошибке; без него ошибку нельзя связать с записью в логе.
func TestErrorEnvelopeHasRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(contextWithRequestID(r.Context(), "req-123"))
	WriteError(w, r, domain.NotFound("нет"))

	var body ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("разбор тела: %v", err)
	}
	if body.RequestID != "req-123" {
		t.Errorf("request_id = %q, ожидался req-123", body.RequestID)
	}
	if body.Error.Code != domain.CodeNotFound {
		t.Errorf("code = %q", body.Error.Code)
	}
}
