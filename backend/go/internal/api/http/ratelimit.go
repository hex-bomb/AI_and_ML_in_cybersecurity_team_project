package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	domain "github.com/kbsearch/kb/internal/envelope"
)

// RateLimit — минимальный per-IP fixed window для дорогих POST
// (search/answers/upload): грубая защита от абуза без внешних зависимостей.
const (
	rateLimitWindow = time.Minute
	rateLimitMax    = 60
)

// rateLimiter считает запросы по IP в фиксированном окне (потокобезопасно).
type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*rateWindow
}

// rateWindow — счётчик одного IP внутри текущего окна.
type rateWindow struct {
	count int
	reset time.Time
}

// newRateLimiter собирает лимитер (max запросов за window с одного IP).
func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{max: max, window: window, hits: map[string]*rateWindow{}}
}

// allow списывает единицу квоты (исчерпана — false; окно сдвигается по таймеру).
func (l *rateLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.hits[ip]
	if !ok || !now.Before(w.reset) {
		l.hits[ip] = &rateWindow{count: 1, reset: now.Add(l.window)}
		// Залежавшиеся IP не копятся: чистка при переполнении карты.
		if len(l.hits) > 10000 {
			for key, old := range l.hits {
				if !now.Before(old.reset) {
					delete(l.hits, key)
				}
			}
		}
		return true
	}
	w.count++
	return w.count <= l.max
}

// limit пропускает мимо дешёвые маршруты, дорогие считает (исчерпание — 429).
func (l *rateLimiter) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limitedRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
		if !l.allow(ip, time.Now()) {
			WriteError(w, r, domain.TooManyRequests("Слишком много запросов"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitedRoute — только дорогие POST: поиск, ответы, загрузка.
func limitedRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	path := r.URL.Path
	return path == "/api/v1/search" ||
		path == "/api/v1/answers" ||
		(strings.HasPrefix(path, "/api/v1/kbs/") && strings.HasSuffix(path, "/documents"))
}
