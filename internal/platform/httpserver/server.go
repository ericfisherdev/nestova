// Package httpserver wires the HTTP transport: routing and server lifecycle.
package httpserver

import (
	"context"
	"net/http"
	"time"
)

// readinessTimeout bounds the dependency check performed by the readiness probe
// so a stalled dependency cannot hang the endpoint.
const readinessTimeout = 2 * time.Second

// ReadinessFunc reports whether the server's backing dependencies (e.g. the
// database) are reachable. It returns a non-nil error when the server is not
// ready to serve traffic.
type ReadinessFunc func(ctx context.Context) error

// New builds the application's HTTP server bound to addr with the base routes
// registered. ready is invoked by the /readyz probe to verify backing
// dependencies; pass nil when there are none. Timeouts are set to conservative
// defaults to avoid Slowloris-style resource exhaustion on a public listener.
func New(addr string, ready ReadinessFunc) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           routes(ready),
		MaxHeaderBytes:    1 << 20, // 1 MiB, bounding header memory per connection.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// routes registers the base HTTP routes shared across bounded contexts.
// Feature routes are mounted here as each context's adapter package lands.
func routes(ready ReadinessFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(ready))
	return mux
}

// handleHealthz reports process liveness for load balancers and uptime checks.
// It does not touch backing dependencies; use /readyz for that.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz reports whether the server is ready to serve traffic by checking
// its backing dependencies via ready. It returns 200 when ready (or when no
// readiness check is configured) and 503 when a dependency is unreachable.
func handleReadyz(ready ReadinessFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
			defer cancel()
			if err := ready(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("unavailable"))
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	}
}
