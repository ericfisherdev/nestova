// Package httpserver wires the HTTP transport: the router, the core middleware
// chain, and the server lifecycle. It owns the New constructor that feature
// contexts extend by adding fields to Deps.
package httpserver

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
	"github.com/ericfisherdev/nestova/web"
)

const (
	// readinessTimeout bounds the dependency check performed by the readiness
	// probe so a stalled dependency cannot hang the endpoint.
	readinessTimeout = 2 * time.Second
	// requestTimeout bounds in-handler work via the request context. It must be
	// less than the connection-level WriteTimeout below so a handler has time to
	// write a clean 500/503 after its context cancels, rather than racing the
	// connection's write deadline.
	requestTimeout = 13 * time.Second
)

// ReadinessFunc reports whether the server's backing dependencies (e.g. the
// database) are reachable. It returns a non-nil error when the server is not
// ready to serve traffic.
type ReadinessFunc func(ctx context.Context) error

// Deps carries the dependencies the HTTP layer needs. Feature tickets append
// fields here (session manager, repositories, handlers) rather than changing
// the New signature.
type Deps struct {
	// Logger receives the structured per-request log line. Required.
	Logger *slog.Logger
	// Ready backs the /readyz probe; nil means "always ready".
	Ready ReadinessFunc
	// Routes, if non-nil, registers feature routes (pages, HTMX fragments,
	// later contexts' handlers) on the mux after the platform routes. This is
	// the extension point for feature wiring without changing New.
	Routes func(mux *http.ServeMux)
	// Middleware is an optional list of feature middleware inserted between
	// Recoverer and Timeout in the canonical chain (NES-23: session/auth).
	// Middleware is applied in the order given (first entry is outermost).
	Middleware []middleware.Middleware
	// HTTPMetrics, if non-nil, enables per-request Prometheus instrumentation
	// (request count, latency, in-flight gauge) via the Metrics middleware
	// (NES-114). nil disables instrumentation (tests, first-run setup).
	HTTPMetrics *metrics.HTTPMetrics
	// MetricsHandler, if non-nil, is served at GET /metrics (the Prometheus
	// scrape endpoint, typically promhttp.HandlerFor over the registry that
	// HTTPMetrics is registered on). nil leaves the route unregistered.
	MetricsHandler http.Handler
}

// New builds the application's HTTP server from cfg and deps with the core
// middleware (request id, structured logging, panic recovery, per-request
// timeout) applied to every route. Connection timeouts are set to conservative
// defaults to avoid Slowloris-style resource exhaustion on a public listener.
func New(cfg config.Config, deps Deps) *http.Server {
	// Logger is required: the logging and recovery middleware use it on every
	// request. Fail loudly at construction rather than panicking mid-request.
	if deps.Logger == nil {
		panic("httpserver: Deps.Logger is required")
	}
	// Canonical middleware order (outermost first): request id wraps everything so
	// every request is logged with an id even on panic; ForwardedHeaders resolves
	// the effective scheme/client IP from a trusted proxy before anything reads
	// them; SecurityHeaders sets baseline headers (and HSTS, gated on the resolved
	// scheme) on every response; logging records the request; metrics observes it
	// (NES-114) — it sits inside RequestLogger so it reuses the responseWriter the
	// logger creates, and outside Recoverer so a recovered panic's 500 (written
	// through that shared wrapper) is recorded with the real final status;
	// recovery turns panics into 500s; the per-request timeout comes next so its
	// deadline also bounds the session/auth feature middleware (which do database
	// work), not just the final handler; feature middleware (session/auth) runs
	// last before the route handler. CaptureRoutePattern is appended innermost
	// (directly wrapping the mux) to relay the matched route pattern back to the
	// metrics middleware: Timeout and the feature middleware derive request copies
	// via WithContext, so the mux's write to r.Pattern never reaches the request
	// value the metrics middleware holds.
	chain := []middleware.Middleware{
		middleware.RequestID,
		middleware.ForwardedHeaders(cfg.Server.TrustedProxyPrefixes()),
		middleware.SecurityHeaders(hstsHeaderValue(cfg.HSTS)),
		middleware.RequestLogger(deps.Logger),
		middleware.Metrics(deps.HTTPMetrics),
		middleware.Recoverer(deps.Logger),
		middleware.Timeout(requestTimeout),
	}
	chain = append(chain, deps.Middleware...)
	chain = append(chain, middleware.CaptureRoutePattern)

	handler := middleware.Chain(chain...)(routes(deps))

	return &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		MaxHeaderBytes:    1 << 20, // 1 MiB, bounding header memory per connection.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Secure floor for the app-terminated TLS path (NES-54); unused when the
		// server serves plain HTTP behind a TLS-terminating proxy. Go negotiates
		// TLS 1.3 when available and falls back no lower than 1.2.
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// hstsHeaderValue builds the Strict-Transport-Security header value from cfg, or
// "" when HSTS is disabled. max-age is emitted as whole seconds.
func hstsHeaderValue(c config.HSTSConfig) string {
	if !c.Enabled {
		return ""
	}
	// EffectiveMaxAge applies the built-in default only when unset; an explicit
	// max-age=0 is emitted verbatim to clear a previously-sent HSTS policy.
	v := "max-age=" + strconv.FormatInt(int64(c.EffectiveMaxAge().Seconds()), 10)
	if c.IncludeSubdomains {
		v += "; includeSubDomains"
	}
	if c.Preload {
		v += "; preload"
	}
	return v
}

// routes registers the base HTTP routes shared across bounded contexts.
// Feature routes are mounted here as each context's adapter package lands.
func routes(deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(deps.Ready))
	// Prometheus scrape endpoint. Intentionally unauthenticated, like /healthz:
	// it is scraped by Prometheus over the internal docker network and exposes
	// only operational counters/gauges, no user data.
	if deps.MetricsHandler != nil {
		mux.Handle("GET /metrics", deps.MetricsHandler)
	}
	// Embedded front-end assets (built CSS, vendored HTMX/Alpine, fonts).
	mux.Handle("GET /static/", staticAssets())
	// Feature routes (pages, fragments, context handlers) register here.
	if deps.Routes != nil {
		deps.Routes(mux)
	}
	return mux
}

// staticAssets serves the embedded assets under /static/ with a moderate cache
// lifetime. A long/immutable cache is intentionally avoided: the asset URLs are
// not content-hashed, so a deploy reuses the same paths and an over-aggressive
// cache would serve stale CSS/JS.
func staticAssets() http.Handler {
	const cacheControl = "public, max-age=3600"
	assets := http.StripPrefix("/static/", web.StaticHandler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", cacheControl)
		// Prevent MIME sniffing away from the declared content type.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		assets.ServeHTTP(w, r)
	})
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
