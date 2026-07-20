package httpserver_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
	"github.com/ericfisherdev/nestova/web"
)

// testConfig returns a minimal config for building the server in tests.
// RequestTimeout is set explicitly (rather than left at its zero value) since
// New derives both the connection-level ReadTimeout/WriteTimeout and the
// per-request middleware.Timeout deadline from it; a zero RequestTimeout would
// produce a negative per-request deadline (already-expired) once
// requestTimeoutMargin is subtracted.
func testConfig() config.Config {
	return config.Config{
		Server: config.ServerConfig{Addr: ":0", RequestTimeout: 30 * time.Second},
		Env:    config.EnvTest,
	}
}

// doRequest builds the server's handler and serves a single GET request to path.
func doRequest(t *testing.T, ready httpserver.ReadinessFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	deps := httpserver.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Ready: ready}
	srv := httpserver.New(testConfig(), deps)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

// TestRequestIDHeader verifies the core middleware is applied to all routes: the
// server echoes a request id even on the simple health route.
func TestRequestIDHeader(t *testing.T) {
	rec := doRequest(t, nil, "/healthz")
	if got := rec.Header().Get("X-Request-Id"); got == "" {
		t.Error("X-Request-Id header missing; request-id middleware not applied")
	}
}

// TestStaticAssetHeaders verifies embedded assets are served with the expected
// caching, security, and content-type headers.
func TestStaticAssetHeaders(t *testing.T) {
	rec := doRequest(t, nil, "/static/css/app.css")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want %q", got, "public, max-age=3600")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
}

// TestServerStartsAndShutsDown exercises a server built by New over a real
// connection and verifies it shuts down gracefully.
func TestServerStartsAndShutsDown(t *testing.T) {
	srv := httpserver.New(testConfig(), httpserver.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Ensure the server and listener are released even if an assertion fails
	// before the explicit Shutdown below. Bound it so a hung server cannot
	// stall the test run.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	// Bound the wait so a Serve goroutine that never returns cannot hang the run.
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Serve did not return after Shutdown")
	}
}

func TestHealthz(t *testing.T) {
	rec := doRequest(t, nil, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name     string
		ready    httpserver.ReadinessFunc
		wantCode int
		wantBody string
	}{
		{
			name:     "no readiness check configured",
			ready:    nil,
			wantCode: http.StatusOK,
			wantBody: "ready",
		},
		{
			name:     "dependency healthy",
			ready:    func(context.Context) error { return nil },
			wantCode: http.StatusOK,
			wantBody: "ready",
		},
		{
			name:     "dependency unreachable",
			ready:    func(context.Context) error { return errors.New("db down") },
			wantCode: http.StatusServiceUnavailable,
			wantBody: "unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, tt.ready, "/readyz")
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

// TestServiceWorkerRoute covers the two properties that make the service
// worker actually work, both of which fail silently in a browser (NES-152).
func TestServiceWorkerRoute(t *testing.T) {
	rec := doRequest(t, nil, "/sw.js")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	// Served from the ROOT, not /static/: a worker's scope is capped at the
	// directory it is served from, so a /static/sw.js could never control a
	// navigation. This route's existence is the scope guarantee.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q, want text/javascript", ct)
	}
	// no-cache, deliberately unlike the 1-hour /static/ lifetime: a cached
	// worker script strands clients on superseded code after a deploy.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
	if body := rec.Body.String(); !strings.Contains(body, "CACHE_NAME") {
		t.Errorf("body does not look like the service worker script: %q", body)
	}
}

// TestOfflineRoute confirms the navigation fallback renders without auth —
// it has to work when the network, not the session, is what failed.
func TestOfflineRoute(t *testing.T) {
	rec := doRequest(t, nil, "/offline")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"You're offline", `data-testid="offline-page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("offline page missing %q", want)
		}
	}
	// The page must reference only pre-cached assets: anything else would
	// fail to load in the exact situation the page exists for.
	for _, forbidden := range []string{"/static/js/", "hx-get", "hx-post"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("offline page references %q, which is not pre-cached", forbidden)
		}
	}
	// Positive contract, not just the deny-list above: every asset the page
	// actually references must appear in the worker's pre-cache list, so
	// dropping one from either side fails here rather than producing an
	// unstyled offline page that only shows up when the network is down.
	sw, err := fs.ReadFile(web.StaticFS(), "sw.js")
	if err != nil {
		t.Fatalf("read sw.js: %v", err)
	}
	for _, asset := range []string{
		"/static/favicon.svg",
		"/static/css/app.css",
		"/static/icons/icon-192.png",
	} {
		if !strings.Contains(body, asset) {
			t.Errorf("offline page no longer references %s; update this test and the pre-cache list", asset)
		}
		if !strings.Contains(string(sw), asset) {
			t.Errorf("sw.js does not pre-cache %s, which the offline page needs", asset)
		}
	}
	if !strings.Contains(string(sw), `'/offline'`) {
		t.Error("sw.js does not pre-cache /offline, so the fallback could not render")
	}
}

// TestServiceWorkerRouteRevalidates confirms the script carries a validator:
// Cache-Control: no-cache asks clients to revalidate before reuse, which is
// only cheap if there is something to revalidate against — otherwise every
// check refetches the whole script instead of getting a 304.
func TestServiceWorkerRouteRevalidates(t *testing.T) {
	first := doRequest(t, nil, "/sw.js")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on /sw.js; no-cache cannot produce a 304 without a validator")
	}

	// Same content must yield the same ETag (it is a content hash, not a
	// per-request value), and a matching conditional request must 304.
	second := doRequest(t, nil, "/sw.js")
	if got := second.Header().Get("ETag"); got != etag {
		t.Errorf("ETag changed between identical requests: %q then %q", etag, got)
	}

	deps := httpserver.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httpserver.New(testConfig(), deps)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	req.Header.Set("If-None-Match", etag)
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional GET status = %d, want %d", rec.Code, http.StatusNotModified)
	}
}
