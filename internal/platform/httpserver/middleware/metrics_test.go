package middleware_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// newHTTPMetrics returns an HTTPMetrics bundle on a fresh isolated registry so
// each test asserts against its own counters.
func newHTTPMetrics(t *testing.T) *metrics.HTTPMetrics {
	t.Helper()
	return metrics.NewHTTPMetrics(metrics.NewRegistry())
}

// widgetsMux returns a mux with a single "GET /widgets" route whose handler
// responds with 200.
func widgetsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /widgets", okHandler())
	return mux
}

// TestMetricsRecordsRequestLabels verifies a served request increments the
// counter with the method, matched route pattern, and status labels, and
// observes one latency sample for the same method/route.
func TestMetricsRecordsRequestLabels(t *testing.T) {
	m := newHTTPMetrics(t)
	h := middleware.Chain(middleware.Metrics(m), middleware.CaptureRoutePattern)(widgetsMux())
	serve(h, httptest.NewRequest(http.MethodGet, "/widgets", nil))

	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("GET", "GET /widgets", "200")); got != 1 {
		t.Errorf(`requests_total{GET,"GET /widgets",200} = %v, want 1`, got)
	}
	if got := testutil.CollectAndCount(m.RequestDuration); got != 1 {
		t.Errorf("request_duration series count = %d, want 1", got)
	}
}

// TestMetricsRoutePatternSurvivesRequestCopies pins the reason the route label
// travels through a context holder rather than r.Pattern: middleware between
// Metrics and the mux (Timeout here, session/auth in production) derive new
// request copies via WithContext, so the mux's write to r.Pattern never reaches
// the request value Metrics holds. The holder, being a pointer stored as a
// context value, survives those copies.
func TestMetricsRoutePatternSurvivesRequestCopies(t *testing.T) {
	m := newHTTPMetrics(t)
	h := middleware.Chain(
		middleware.Metrics(m),
		middleware.Timeout(time.Second), // copies the request via WithContext
		middleware.CaptureRoutePattern,
	)(widgetsMux())
	serve(h, httptest.NewRequest(http.MethodGet, "/widgets", nil))

	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("GET", "GET /widgets", "200")); got != 1 {
		t.Errorf(`requests_total route label lost through request copies: {GET,"GET /widgets",200} = %v, want 1`, got)
	}
}

// TestMetricsStandaloneFallsBackToRequestPattern verifies the documented
// standalone fallback: without CaptureRoutePattern (and without intervening
// request copies) the mux mutates the request value Metrics passed down, so
// r.Pattern still yields the route label.
func TestMetricsStandaloneFallsBackToRequestPattern(t *testing.T) {
	m := newHTTPMetrics(t)
	h := middleware.Metrics(m)(widgetsMux())
	serve(h, httptest.NewRequest(http.MethodGet, "/widgets", nil))

	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("GET", "GET /widgets", "200")); got != 1 {
		t.Errorf(`standalone requests_total{GET,"GET /widgets",200} = %v, want 1`, got)
	}
}

// TestMetricsUnmatchedRouteLabel verifies a request no route matches is
// recorded under the fixed "unmatched" label — never the raw URL path, which
// would create unbounded label cardinality from attacker-chosen paths.
func TestMetricsUnmatchedRouteLabel(t *testing.T) {
	m := newHTTPMetrics(t)
	h := middleware.Chain(middleware.Metrics(m), middleware.CaptureRoutePattern)(widgetsMux())
	serve(h, httptest.NewRequest(http.MethodGet, "/no/such/route", nil))

	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("GET", "unmatched", "404")); got != 1 {
		t.Errorf(`requests_total{GET,unmatched,404} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("GET", "/no/such/route", "404")); got != 0 {
		t.Errorf("raw URL path used as route label value (%v observations); must stay bounded", got)
	}
}

// TestMetricsNonStandardMethodLabel verifies an arbitrary request method is
// recorded under the fixed "OTHER" label — never the raw method string, which
// (like the route label) would create unbounded label cardinality from
// attacker-chosen methods.
func TestMetricsNonStandardMethodLabel(t *testing.T) {
	m := newHTTPMetrics(t)
	h := middleware.Chain(middleware.Metrics(m), middleware.CaptureRoutePattern)(widgetsMux())
	serve(h, httptest.NewRequest("EVILMETHOD", "/widgets", nil))

	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("OTHER", "unmatched", "405")); got != 1 {
		t.Errorf(`requests_total{OTHER,unmatched,405} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("EVILMETHOD", "unmatched", "405")); got != 0 {
		t.Errorf("raw method used as label value (%v observations); must stay bounded", got)
	}
}

// TestMetricsRecordsRecoveredPanicAs500 verifies the canonical placement
// (inside RequestLogger, outside Recoverer): when the handler panics, the inner
// Recoverer writes the 500 through the responseWriter created by RequestLogger
// and shared by Metrics, so the counter records the real final status against
// the matched route.
func TestMetricsRecordsRecoveredPanicAs500(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(bytes.NewBuffer(nil), nil))
	m := newHTTPMetrics(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	h := middleware.Chain(
		middleware.RequestLogger(logger),
		middleware.Metrics(m),
		middleware.Recoverer(logger),
		middleware.CaptureRoutePattern,
	)(mux)
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("GET", "GET /boom", "500")); got != 1 {
		t.Errorf(`requests_total{GET,"GET /boom",500} = %v, want 1`, got)
	}
}

// TestMetricsInFlightGauge verifies the gauge reads 1 while a request is being
// served and returns to 0 once it completes.
func TestMetricsInFlightGauge(t *testing.T) {
	m := newHTTPMetrics(t)
	var during float64
	h := middleware.Metrics(m)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		during = testutil.ToFloat64(m.RequestsInFlight)
		w.WriteHeader(http.StatusOK)
	}))
	serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if during != 1 {
		t.Errorf("in-flight gauge during request = %v, want 1", during)
	}
	if got := testutil.ToFloat64(m.RequestsInFlight); got != 0 {
		t.Errorf("in-flight gauge after request = %v, want 0", got)
	}
}

// TestMetricsNilIsPassthrough verifies Metrics(nil) wraps nothing: the request
// reaches the handler and the response is unchanged, so the canonical chain can
// include Metrics unconditionally while tests and the first-run setup server
// run without a registry.
func TestMetricsNilIsPassthrough(t *testing.T) {
	const body = "passed through"
	h := middleware.Metrics(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(body))
	}))
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}
