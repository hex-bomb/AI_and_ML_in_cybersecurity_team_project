package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Metrics — самописный Prometheus-экспортер HTTP-метрики.
// Кардинальность ограничена: оборачивается только /api/v1/search.
type Metrics struct {
	mu     sync.Mutex
	counts map[reqKey]int64
	sum    map[string]float64
	cnt    map[string]int64
}

type reqKey struct {
	method, path, status string
}

func NewMetrics() *Metrics {
	return &Metrics{
		counts: map[reqKey]int64{},
		sum:    map[string]float64{},
		cnt:    map[string]int64{},
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (m *Metrics) wrap(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		m.record(r.Method, r.URL.Path, rec.status, time.Since(start).Seconds())
	})
}

func (m *Metrics) record(method, path string, status int, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[reqKey{method, path, strconv.Itoa(status)}]++
	m.sum[path] += seconds
	m.cnt[path]++
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	fmt.Fprintln(w, "# HELP http_requests_total Total HTTP requests.")
	fmt.Fprintln(w, "# TYPE http_requests_total counter")
	keys := make([]reqKey, 0, len(m.counts))
	for k := range m.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].path != keys[j].path {
			return keys[i].path < keys[j].path
		}
		return keys[i].status < keys[j].status
	})
	for _, k := range keys {
		fmt.Fprintf(w, "http_requests_total{method=%q,path=%q,status=%q} %d\n", k.method, k.path, k.status, m.counts[k])
	}

	fmt.Fprintln(w, "# HELP http_request_duration_seconds Request duration.")
	fmt.Fprintln(w, "# TYPE http_request_duration_seconds summary")
	for path, s := range m.sum {
		fmt.Fprintf(w, "http_request_duration_seconds_sum{path=%q} %g\n", path, s)
		fmt.Fprintf(w, "http_request_duration_seconds_count{path=%q} %d\n", path, m.cnt[path])
	}
}
