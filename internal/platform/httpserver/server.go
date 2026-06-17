// Package httpserver wires the HTTP transport: routing and server lifecycle.
package httpserver

import (
	"net/http"
	"time"
)

// New builds the application's HTTP server bound to addr with the base routes
// registered. Timeouts are set to conservative defaults to avoid
// Slowloris-style resource exhaustion on a public listener.
func New(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           routes(),
		MaxHeaderBytes:    1 << 20, // 1 MiB, bounding header memory per connection.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// routes registers the base HTTP routes shared across bounded contexts.
// Feature routes are mounted here as each context's adapter package lands.
func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

// handleHealthz reports process liveness for load balancers and uptime checks.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
