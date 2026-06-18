package httpserver_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
)

// doRequest builds the server's handler and serves a single GET request to path.
func doRequest(t *testing.T, ready httpserver.ReadinessFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	srv := httpserver.New(":0", ready)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	srv.Handler.ServeHTTP(rec, req)
	return rec
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
