// Package metrics owns the application's Prometheus instrumentation: the
// registry (with the standard process/runtime collectors), the HTTP request
// metrics observed by the middleware layer, and the background scheduler and
// calendar sync metrics recorded through the TickRecorder and SyncRecorder
// ports. It is the only platform package that imports the Prometheus client
// directly; consumers (middleware, schedulers, the composition root) work
// through the types defined here so the instrumentation backend stays
// swappable behind one seam.
//
// The generic registry/HTTP-metrics/scheduler-tick machinery now lives in
// github.com/ericfisherdev/nestcore/metrics, generalized to take a
// caller-supplied namespace (and, for scheduler ticks, a known-name allowlist)
// so more than one application can share a scrape target without their metric
// names colliding. This package binds that generic machinery to the "nestova"
// namespace so every existing consumer and every existing metric name
// (nestova_http_requests_total, etc.) is unchanged; Nestova's own domain
// metrics (calendar_sync.go, email.go, sms.go) stay defined directly in this
// package, since they have no nestcore equivalent.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	ncmetrics "github.com/ericfisherdev/nestcore/metrics"
)

// namespace is the fixed Prometheus namespace every metric this package
// registers is prefixed with, preserving the nestova_... series names in
// place before nestcore's metrics package generalized namespacing.
const namespace = "nestova"

// NewRegistry returns a fresh Prometheus registry pre-populated with the
// standard collectors: Go runtime metrics (goroutines, GC, memory), process
// metrics (CPU, RSS, fds), and build info (Go version, module version). A
// dedicated registry is used instead of prometheus.DefaultRegisterer so the
// exposed metric set is explicit and tests can build isolated registries
// without cross-test collisions.
func NewRegistry() *prometheus.Registry {
	return ncmetrics.NewRegistry()
}

// Handler returns the HTTP scrape handler for reg, keeping the promhttp
// dependency inside this package so the composition root never imports the
// Prometheus client directly. It panics when reg is nil (matching the
// platform convention of failing loudly at construction for required
// dependencies).
func Handler(reg *prometheus.Registry) http.Handler {
	return ncmetrics.Handler(reg)
}

// HTTPMetrics bundles the per-request HTTP metrics recorded by the Metrics
// middleware. The fields are exported so the middleware can record values and
// tests can assert on them with prometheus/testutil, but construction always
// goes through NewHTTPMetrics so every instance is registered.
type HTTPMetrics = ncmetrics.HTTPMetrics

// NewHTTPMetrics constructs the HTTP request metrics and registers them on
// reg. It panics when reg is nil (matching the platform convention of failing
// loudly at construction for required dependencies) and when a metric with
// the same name is already registered, so a double-wired registry surfaces at
// boot rather than as silently shared counters.
func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	return ncmetrics.NewHTTPMetrics(reg, namespace)
}
