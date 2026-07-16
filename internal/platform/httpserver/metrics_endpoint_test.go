package httpserver_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// newMetricsServer builds a server with the full NES-114 metrics wiring: a
// fresh registry, the HTTP request metrics, and the promhttp scrape handler,
// mirroring the composition root.
func newMetricsServer(t *testing.T) *http.Server {
	t.Helper()
	reg := metrics.NewRegistry()
	return httpserver.New(testConfig(), httpserver.Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPMetrics:    metrics.NewHTTPMetrics(reg),
		MetricsHandler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}),
	})
}

// TestMetricsEndpoint verifies GET /metrics serves the Prometheus exposition:
// the standard runtime collectors are present, and a previously served request
// shows up in the middleware-recorded counter with its method, matched route
// pattern, and status labels.
func TestMetricsEndpoint(t *testing.T) {
	srv := newMetricsServer(t)

	// Serve one instrumented request first so the request counter has a sample.
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", rec.Code, http.StatusOK)
	}

	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "go_goroutines") {
		t.Error("exposition is missing the go_goroutines runtime metric")
	}
	const wantSample = `nestova_http_requests_total{method="GET",route="GET /healthz",status="200"} 1`
	if !strings.Contains(body, wantSample) {
		t.Errorf("exposition is missing the instrumented request sample %q", wantSample)
	}
}

// TestMetricsRouteAbsentWithoutHandler verifies /metrics is not registered when
// no MetricsHandler is configured (tests, first-run setup server).
func TestMetricsRouteAbsentWithoutHandler(t *testing.T) {
	rec := doRequest(t, nil, "/metrics")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /metrics without a handler: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
