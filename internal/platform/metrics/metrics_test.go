package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// TestNewRegistryIncludesStandardCollectors verifies the registry gathers the
// runtime, process, and build-info collector families out of the box.
func TestNewRegistryIncludesStandardCollectors(t *testing.T) {
	reg := metrics.NewRegistry()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{"go_goroutines", "go_build_info"} {
		if !names[want] {
			t.Errorf("registry is missing standard collector family %q", want)
		}
	}
}

// TestNewHTTPMetricsRegistersOnRegistry verifies the constructor registers all
// three metrics on the given registerer. The vectors are exercised with a
// zero-value observation first: vector families are lazy and appear in a
// gather only once at least one child series exists.
func TestNewHTTPMetricsRegistersOnRegistry(t *testing.T) {
	reg := metrics.NewRegistry()
	m := metrics.NewHTTPMetrics(reg)
	m.RequestsTotal.WithLabelValues("GET", "GET /widgets", "200").Add(0)
	m.RequestDuration.WithLabelValues("GET", "GET /widgets").Observe(0)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{
		"nestova_http_requests_in_flight",
		"nestova_http_requests_total",
		"nestova_http_request_duration_seconds",
	} {
		if !names[want] {
			t.Errorf("%s not registered on the provided registry", want)
		}
	}
}

// TestNewHTTPMetricsNilRegistererPanics pins the platform convention of failing
// loudly at construction when a required dependency is missing.
func TestNewHTTPMetricsNilRegistererPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewHTTPMetrics(nil) did not panic")
		}
	}()
	metrics.NewHTTPMetrics(nil)
}

// TestHandlerServesRegistryFamilies verifies the scrape handler exposes the
// families registered on the provided registry, so the composition root can
// mount it without touching promhttp directly.
func TestHandlerServesRegistryFamilies(t *testing.T) {
	reg := metrics.NewRegistry()
	metrics.NewHTTPMetrics(reg).RequestsInFlight.Set(0)

	rr := httptest.NewRecorder()
	metrics.Handler(reg).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{"go_goroutines", "nestova_http_requests_in_flight"} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape body is missing family %q", want)
		}
	}
}

// TestHandlerNilRegistryPanics pins the platform convention of failing loudly
// at construction when a required dependency is missing.
func TestHandlerNilRegistryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Handler(nil) did not panic")
		}
	}()
	metrics.Handler(nil)
}
