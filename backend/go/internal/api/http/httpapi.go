// Package httpapi — транспортный слой: маршруты, middleware, формат ответа.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	contracts "github.com/kbsearch/kb/api"
	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/platform/config"
)

// Services — сценарии транспорта. Интерфейсы здесь ради заглушек в тестах.
type Services struct {
	Search   SearchScenario
	Answers  AnswerScenario
	KB       KBScenario
	Document DocumentScenario
	Settings SettingsScenario
	Job      JobScenario
	Health   HealthScenario
	Auth     AuthScenario
}

// Handler собирает HTTP-обработчик.
func Handler(cfg config.Config, svc Services) http.Handler {
	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(Recoverer)
	r.Use(Logging)
	if len(cfg.CORSAllowedOrigins) > 0 {
		r.Use(CORS(cfg.CORSAllowedOrigins))
	}
	// Per-IP fixed window на дорогие POST (внутри Logging: отказ 429 тоже в логе).
	r.Use(newRateLimiter(rateLimitMax, rateLimitWindow).limit)

	// Маршруты сгенерированы из OpenAPI. HandlerWithOptions пишет прямо на
	// BaseRouter; r.Mount сюда же дал бы рекурсию и зависшие 404.
	api := &apiHandler{cfg: cfg, svc: svc}
	// Спека отдаётся из бинаря: в distroless нет файлов репозитория.
	r.Get("/api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(contracts.Spec())
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, domain.NotFound("Маршрут не найден: "+r.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, domain.MethodNotAllowed(
			"Метод "+r.Method+" не поддерживается для "+r.URL.Path))
	})

	return contracts.HandlerWithOptions(api, contracts.ChiServerOptions{
		BaseRouter: r,
	})
}

// RequestID проставляет X-Request-ID. Чужой невалидный UUID заменяется.
func RequestID(next http.Handler) http.Handler {
	const header = "X-Request-ID"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(header)
		if _, err := uuid.Parse(id); err != nil {
			id = uuid.NewString()
		}
		w.Header().Set(header, id)
		ctx := contextWithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Recoverer превращает панику в 500, не роняя процесс.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				id := requestIDFrom(r.Context())
				logPrintf("panic %v request_id=%s method=%s path=%s", rec, id, r.Method, r.URL.Path)
				WriteError(w, r, domain.ServiceUnavailable("Внутренняя ошибка"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Logging пишет одну строку на запрос: метод, путь, код, длительность.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logPrintf("%s %s -> %d за %s request_id=%s",
			r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond), requestIDFrom(r.Context()))
	})
}

// CORS ограничивает источники списком из конфигурации (без wildcard).
func CORS(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if _, ok := set[origin]; ok {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
					w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
				}
			}
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, Idempotency-Key")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ErrorBody — единый формат ошибки ТЗ 2.3.
type ErrorBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// WriteError отвечает доменной ошибкой. Не-AppError трактуется как 500.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	appErr, ok := domain.AsAppError(err)
	if !ok {
		logPrintf("не-AppError: %v request_id=%s", err, requestIDFrom(r.Context()))
		appErr = domain.ServiceUnavailable("Внутренняя ошибка")
	}

	var body ErrorBody
	body.Error.Code = appErr.Code
	body.Error.Message = appErr.Message
	body.Error.Details = appErr.Details
	body.RequestID = requestIDFrom(r.Context())

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteJSON отвечает успешным результатом.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Код ответа уже отправлен, исправить нельзя — остаётся лог.
		logPrintf("кодирование ответа: %v", err)
	}
}

// strictJSON декодирует тело и отклоняет неизвестные поля (генератор этого не делает).
func strictJSON(r *http.Request, target any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return domain.InvalidMetadata(fmt.Sprintf("Некорректное тело запроса: %s", cleanJSONError(err)))
	}
	if dec.More() {
		return domain.InvalidMetadata("В теле запроса больше одного JSON-значения")
	}
	return nil
}

// maxBodyBytes — лимит JSON-тела (multipart загрузки проверяется отдельно).
const maxBodyBytes = 1 << 20

// cleanJSONError прячет внутренние пути типов из ошибок JSON.
func cleanJSONError(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("некорректный JSON, позиция %d", syntax.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Sprintf("поле %q ожидает %s", typeErr.Field, typeErr.Type)
	}
	msg := err.Error()
	if i := strings.Index(msg, "json: unknown field "); i >= 0 {
		return "неизвестное поле: " + strings.TrimPrefix(msg[i+len("json: unknown field "):], "")
	}
	return msg
}

// statusRecorder запоминает код ответа для логирования.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
