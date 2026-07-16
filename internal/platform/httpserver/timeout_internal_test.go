package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// TestNewDerivesConnectionTimeoutsFromRequestTimeout verifies the fix for the
// slow-large-upload timeout bug: ReadTimeout and WriteTimeout both track
// cfg.Server.RequestTimeout (rather than a hardcoded 15s), since WriteTimeout's
// deadline is armed once headers finish reading and does not reset while the
// body is still being read — a slow-but-successful upload would otherwise fail
// when the handler tries to write its response.
func TestNewDerivesConnectionTimeoutsFromRequestTimeout(t *testing.T) {
	cfg := config.Config{
		Server: config.ServerConfig{Addr: ":0", RequestTimeout: 45 * time.Second},
		Env:    config.EnvTest,
	}
	srv := New(cfg, Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	if srv.ReadTimeout != cfg.Server.RequestTimeout {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, cfg.Server.RequestTimeout)
	}
	if srv.WriteTimeout != cfg.Server.RequestTimeout {
		t.Errorf("WriteTimeout = %v, want %v", srv.WriteTimeout, cfg.Server.RequestTimeout)
	}
}

// TestPerRequestContextDeadlineLeavesMargin verifies the per-request context
// deadline (applied by the Timeout middleware) sits requestTimeoutMargin short
// of RequestTimeout — the headroom a handler needs to write a clean error
// after its own context expires, before the connection's WriteTimeout (set to
// the full RequestTimeout) would cut the response off mid-write.
func TestPerRequestContextDeadlineLeavesMargin(t *testing.T) {
	cfg := config.Config{
		Server: config.ServerConfig{Addr: ":0", RequestTimeout: 45 * time.Second},
		Env:    config.EnvTest,
	}
	var remaining time.Duration
	deps := Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Routes: func(mux *http.ServeMux) {
			mux.HandleFunc("GET /deadline-probe", func(w http.ResponseWriter, r *http.Request) {
				deadline, ok := r.Context().Deadline()
				if !ok {
					t.Error("request context has no deadline")
					return
				}
				remaining = time.Until(deadline)
				w.WriteHeader(http.StatusOK)
			})
		},
	}
	srv := New(cfg, deps)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/deadline-probe", nil)
	srv.Handler.ServeHTTP(rec, req)

	want := cfg.Server.RequestTimeout - requestTimeoutMargin
	// Generous slack for the time elapsed between New() arming the deadline
	// and the handler reading time.Until(deadline) within the same test.
	const tolerance = 2 * time.Second
	if diff := want - remaining; diff < -tolerance || diff > tolerance {
		t.Errorf("per-request deadline remaining = %v, want ~%v (RequestTimeout - margin)", remaining, want)
	}
}
