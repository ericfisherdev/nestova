package httpserver_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
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
