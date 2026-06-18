// Package middleware provides composable net/http middleware for the request
// backbone: request IDs, structured logging, panic recovery, and per-request
// timeouts. Each middleware is a func(http.Handler) http.Handler so they can be
// composed with Chain.
package middleware

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"time"
)

// Middleware wraps an http.Handler with additional behavior.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware into a single Middleware. The first argument is the
// outermost wrapper: Chain(a, b)(h) yields a(b(h)), so a runs first on the way
// in and last on the way out.
func Chain(mws ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			next = mws[i](next)
		}
		return next
	}
}

// contextKey is an unexported type for context keys defined in this package, so
// they never collide with keys from other packages.
type contextKey int

const requestIDKey contextKey = iota

const (
	// requestIDHeader is the canonical request-id header echoed to clients and
	// read from upstream proxies.
	requestIDHeader = "X-Request-Id"
	// maxRequestIDLen bounds an accepted inbound id.
	maxRequestIDLen = 128
)

// RequestID assigns each request a correlation id: it reuses a *valid* incoming
// X-Request-Id, otherwise it generates one. The id is stored in the request
// context and echoed on the response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			// Reject empty, over-long, or unexpected-character ids before
			// reflecting them into the response header and logs, preventing
			// header/log injection or amplification via attacker-controlled
			// values.
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validRequestID reports whether an inbound id is safe to reflect: non-empty,
// within the length bound, and restricted to an unambiguous token charset.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// RequestIDFromContext returns the request id stored by RequestID, or "" when
// none is present.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// newRequestID returns a random 128-bit hex id. crypto/rand.Read never returns
// an error for a 16-byte buffer, but if it ever did we fall back to a fixed
// marker rather than panicking in the request path.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// Timeout bounds each request's context to d. Handlers observe cancellation via
// the request context. A context deadline (rather than http.TimeoutHandler) is
// used so streaming responses (HTMX SSE, hijacking) keep working.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// responseWriter wraps http.ResponseWriter to capture the status code and bytes
// written for logging, while preserving optional interfaces (Flusher, Hijacker)
// so streaming and connection hijacking keep working through the middleware.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (w *responseWriter) WriteHeader(code int) {
	// Suppress duplicate WriteHeader calls (only the first status takes effect),
	// avoiding net/http's "superfluous response.WriteHeader call" warning.
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController (Go 1.20+).
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush forwards to the underlying writer when it supports flushing. It marks
// the header as written first (matching net/http, which sends a 200 on the
// first flush) so a later panic is not mistaken for "no response started".
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wroteHeader {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Hijack forwards to the underlying writer when it supports hijacking.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		conn, rw, err := h.Hijack()
		if err == nil {
			// The caller owns the connection now; mark the response as started
			// so Recoverer does not try to write a 500 over a hijacked conn.
			w.wroteHeader = true
		}
		return conn, rw, err
	}
	// Use the stdlib sentinel so callers can errors.Is against
	// http.ErrNotSupported / errors.ErrUnsupported.
	return nil, nil, http.ErrNotSupported
}
