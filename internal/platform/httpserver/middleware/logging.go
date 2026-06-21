package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

// RequestLogger logs one structured line per request (method, path, status,
// bytes, duration, request id) using the provided logger. It wraps the
// ResponseWriter to capture the status and size.
func RequestLogger(logger *slog.Logger) Middleware {
	if logger == nil {
		panic("middleware: RequestLogger requires a non-nil logger")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w}

			next.ServeHTTP(rw, r)

			// Default to 200 when the handler wrote a body without an explicit
			// status (matching net/http's implicit WriteHeader).
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			// Prefer the resolved client IP (XFF-aware behind a trusted proxy) over
			// the raw peer; fall back to RemoteAddr when ForwardedHeaders did not run
			// (e.g. logging used outside the canonical chain). ClientIP is host-only,
			// so strip the port from the fallback for a consistent log field.
			clientIP := ClientIP(r.Context())
			if clientIP == "" {
				if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
					clientIP = host
				} else {
					clientIP = r.RemoteAddr // already host-only or malformed
				}
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", rw.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("client_ip", clientIP),
				slog.String("request_id", RequestIDFromContext(r.Context())),
			)
		})
	}
}

// Recoverer converts a panic in a downstream handler into a 500 response and a
// logged error (with stack trace and request id) instead of crashing the
// server or leaking the panic value to the client.
func Recoverer(logger *slog.Logger) Middleware {
	if logger == nil {
		panic("middleware: Recoverer requires a non-nil logger")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Ensure we hold a wrapper that tracks whether a response started,
			// so Recoverer works correctly regardless of composition order. In
			// the canonical chain RequestLogger already wrapped w, so reuse it
			// rather than double-wrapping.
			rw, ok := w.(*responseWriter)
			if !ok {
				rw = &responseWriter{ResponseWriter: w}
			}

			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler is the documented way to abort a
					// handler; propagate it instead of treating it as a 500.
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
						slog.Any("panic", rec),
						slog.String("request_id", RequestIDFromContext(r.Context())),
						slog.String("stack", string(debug.Stack())),
					)
					// Write a 500 only if no response has started; otherwise the
					// status/headers are already sent (or the connection was
					// hijacked) and writing would corrupt the response.
					if !rw.wroteHeader {
						rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
						rw.WriteHeader(http.StatusInternalServerError)
						_, _ = rw.Write([]byte("internal server error"))
					}
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}
