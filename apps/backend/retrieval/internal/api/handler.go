// Package api реализует HTTP-слой сервиса: маршруты, авторизацию и метрики.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"vektor/retrieval/internal/jwks"
	"vektor/retrieval/internal/search"
)

// requestTimeout ограничивает весь поиск: embedding и запросы к ES.
// Серверный WriteTimeout должен быть больше этого дедлайна.
const requestTimeout = 15 * time.Second

// SearchRequest — тело POST /api/v1/search. База знаний одна, поэтому scope
// берётся из сессии, а не из тела.
type SearchRequest struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
	// FocusDocumentRef — документ, к которому привязана страница виджета.
	// Повышает его фрагменты, но не ограничивает выдачу и не меняет доступ.
	FocusDocumentRef string `json:"focus_document_ref"`
}

// SearchResponse — ответ POST /api/v1/search; Status принимает "ok" или "no_results".
type SearchResponse struct {
	Status    string       `json:"status"`
	Hits      []search.Hit `json:"hits"`
	RequestID string       `json:"request_id"`
}

// SearchService — то, что нужно HTTP-слою от ядра поиска.
type SearchService interface {
	Search(ctx context.Context, scope search.Scope, query string, topK int, focus string) ([]search.Hit, error)
}

// Handler держит зависимости HTTP-слоя; маршруты регистрирует Routes.
type Handler struct {
	search   SearchService
	verifier *jwks.Verifier
	metrics  *Metrics
}

func NewHandler(searchSvc SearchService, verifier *jwks.Verifier, metrics *Metrics) *Handler {
	return &Handler{search: searchSvc, verifier: verifier, metrics: metrics}
}

// Routes регистрирует все эндпоинты.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", h.metrics)
	mux.Handle("POST /api/v1/search", h.metrics.wrap(h.authenticated(h.searchHandler)))
	return mux
}

type authedHandler func(http.ResponseWriter, *http.Request, jwks.Claims)

func (h *Handler) authenticated(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := requestID(r)
		w.Header().Set("X-Request-ID", rid)

		token := tokenFrom(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Требуется авторизация", rid)
			return
		}
		claims, err := h.verifier.Verify(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Некорректная сессия", rid)
			return
		}
		next(w, r, claims)
	}
}

func (h *Handler) searchHandler(w http.ResponseWriter, r *http.Request, claims jwks.Claims) {
	rid := w.Header().Get("X-Request-ID")

	var req SearchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Некорректный запрос", rid)
		return
	}
	if len(req.Query) > 1000 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Некорректный запрос", rid)
		return
	}
	if req.TopK == 0 {
		req.TopK = 5
	}
	if req.TopK < 1 || req.TopK > 10 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "top_k вне диапазона 1..10", rid)
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeJSON(w, http.StatusOK, SearchResponse{Status: "no_results", Hits: []search.Hit{}, RequestID: rid})
		return
	}
	// Пустой scp — ни одного разрешённого канала, значит доступа нет.
	if len(claims.Scp) == 0 {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Доступ запрещён", rid)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	scope := search.Scope{Channels: claims.Channels(), Groups: claims.Grp}
	if len(req.FocusDocumentRef) > 64 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "focus_document_ref слишком длинный", rid)
		return
	}
	hits, err := h.search.Search(ctx, scope, req.Query, req.TopK, req.FocusDocumentRef)
	if err != nil {
		slog.Error("search failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, "SEARCH_UNAVAILABLE", "Поиск недоступен", rid)
		return
	}

	status := "ok"
	if len(hits) == 0 {
		status = "no_results"
	}
	writeJSON(w, http.StatusOK, SearchResponse{Status: status, Hits: hits, RequestID: rid})
}

func tokenFrom(r *http.Request) string {
	if c, err := r.Cookie("vektor_session"); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func requestID(r *http.Request) string {
	// Клиентский X-Request-ID уходит обратно в ответ и в тело ошибки,
	// поэтому длину ограничиваем.
	const maxLen = 128
	if v := r.Header.Get("X-Request-ID"); v != "" && len(v) <= maxLen {
		return v
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg, rid string) {
	writeJSON(w, status, map[string]any{
		"error":      map[string]string{"code": code, "message": msg},
		"request_id": rid,
	})
}
