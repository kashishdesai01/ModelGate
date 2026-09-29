package api

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type Metrics struct {
	requests          atomic.Uint64
	errors            atomic.Uint64
	cacheHits         atomic.Uint64
	rateLimited       atomic.Uint64
	totalLatencyNanos atomic.Uint64
}

func NewMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) Observe(status int, cacheHit bool, latency time.Duration) {
	m.requests.Add(1)
	if status >= 500 {
		m.errors.Add(1)
	}
	if status == http.StatusTooManyRequests {
		m.rateLimited.Add(1)
	}
	if cacheHit {
		m.cacheHits.Add(1)
	}
	m.totalLatencyNanos.Add(uint64(max(latency.Nanoseconds(), 0)))
}

func (m *Metrics) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
	requests := m.requests.Load()
	average := 0.0
	if requests > 0 {
		average = float64(m.totalLatencyNanos.Load()) / float64(requests) / float64(time.Second)
	}
	_, _ = fmt.Fprintf(writer, "# TYPE modelgate_requests_total counter\nmodelgate_requests_total %d\n", requests)
	_, _ = fmt.Fprintf(writer, "# TYPE modelgate_errors_total counter\nmodelgate_errors_total %d\n", m.errors.Load())
	_, _ = fmt.Fprintf(writer, "# TYPE modelgate_cache_hits_total counter\nmodelgate_cache_hits_total %d\n", m.cacheHits.Load())
	_, _ = fmt.Fprintf(writer, "# TYPE modelgate_rate_limited_total counter\nmodelgate_rate_limited_total %d\n", m.rateLimited.Load())
	_, _ = fmt.Fprintf(writer, "# TYPE modelgate_request_duration_seconds gauge\nmodelgate_request_duration_seconds %.9f\n", average)
}
