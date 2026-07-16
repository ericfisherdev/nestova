// Package metrics owns the application's Prometheus instrumentation: the
// registry (with the standard process/runtime collectors) and the HTTP request
// metrics observed by the middleware layer. It is the only platform package
// that imports the Prometheus client directly; consumers (middleware, the
// composition root) work through the types defined here so the instrumentation
// backend stays swappable behind one seam.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// NewRegistry returns a fresh Prometheus registry pre-populated with the
// standard collectors: Go runtime metrics (goroutines, GC, memory), process
// metrics (CPU, RSS, fds), and build info (Go version, module version). A
// dedicated registry is used instead of prometheus.DefaultRegisterer so the
// exposed metric set is explicit and tests can build isolated registries
// without cross-test collisions.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)
	return reg
}

// HTTPMetrics bundles the per-request HTTP metrics recorded by the Metrics
// middleware. The fields are exported so the middleware can record values and
// tests can assert on them with prometheus/testutil, but construction always
// goes through NewHTTPMetrics so every instance is registered.
type HTTPMetrics struct {
	// RequestsTotal counts completed requests, labelled by method, matched
	// route pattern, and final status code (numeric string, e.g. "200").
	RequestsTotal *prometheus.CounterVec
	// RequestDuration observes request latency in seconds, labelled by method
	// and matched route pattern. Status is intentionally omitted to bound the
	// histogram's series count (each series carries a full bucket set).
	RequestDuration *prometheus.HistogramVec
	// RequestsInFlight gauges the number of requests currently being served.
	RequestsInFlight prometheus.Gauge
}

// NewHTTPMetrics constructs the HTTP request metrics and registers them on
// reg. It panics when reg is nil (matching the platform convention of failing
// loudly at construction for required dependencies) and when a metric with
// the same name is already registered, so a double-wired registry surfaces at
// boot rather than as silently shared counters.
func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	if reg == nil {
		panic("metrics: NewHTTPMetrics requires a non-nil registerer")
	}
	factory := promauto.With(reg)
	return &HTTPMetrics{
		RequestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "nestova_http_requests_total",
			Help: "Total number of HTTP requests served, by method, route pattern, and status code.",
		}, []string{"method", "route", "status"}),
		RequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nestova_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, by method and route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		RequestsInFlight: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nestova_http_requests_in_flight",
			Help: "Number of HTTP requests currently being served.",
		}),
	}
}
