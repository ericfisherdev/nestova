package middleware

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// unmatchedRoute is the route label recorded when no ServeMux pattern matched
// (404s, or middleware that short-circuited before routing). Raw URL paths are
// never used as label values: attacker-chosen paths would create unbounded
// label cardinality and blow up the metrics store.
const unmatchedRoute = "unmatched"

// otherMethod is the method label recorded for any HTTP method outside the
// RFC 9110 set. Like the route label, the method is client-controlled: an
// arbitrary method string must never become a label value or attacker-chosen
// methods would create unbounded label cardinality.
const otherMethod = "OTHER"

// normalizeMethod collapses non-standard HTTP methods to otherMethod so the
// method label stays bounded to the RFC 9110 set plus PATCH.
func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return otherMethod
	}
}

// routePatternHolder carries the matched ServeMux pattern from the innermost
// point of the chain (CaptureRoutePattern) back out to the Metrics middleware.
// A mutable holder in the context is required because middleware between
// Metrics and the mux (Timeout, session, auth) derive new *http.Request copies
// via WithContext, so the mux's write to r.Pattern never reaches the request
// value Metrics holds.
type routePatternHolder struct {
	pattern string
}

// CaptureRoutePattern records the ServeMux pattern that matched the request
// into the holder placed in the context by Metrics. It must sit innermost in
// the chain (directly wrapping the mux) so it reads r.Pattern from the exact
// request value the mux mutated. The capture is deferred so a matched pattern
// is still recorded when the handler panics (Recoverer, further out, turns
// that into a 500 which Metrics then attributes to the right route). It is a
// no-op when no holder is present (metrics disabled).
func CaptureRoutePattern(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		holder, ok := r.Context().Value(routePatternKey).(*routePatternHolder)
		if ok {
			defer func() { holder.pattern = r.Pattern }()
		}
		next.ServeHTTP(w, r)
	})
}

// Metrics records the request count, latency, and in-flight gauge defined in
// the metrics package for every request. A nil m returns a passthrough
// middleware so the canonical chain can include Metrics unconditionally while
// tests (and the first-run setup server) run without a registry.
//
// The route label is the matched ServeMux pattern, delivered by
// CaptureRoutePattern via a context holder (see routePatternHolder for why
// reading r.Pattern here is not enough). When used standalone without
// CaptureRoutePattern, r.Pattern is used as a fallback (it propagates only if
// no intervening middleware copies the request); failing both, the label is
// "unmatched". The status defaults to 200 when the handler wrote a body
// without an explicit WriteHeader, matching RequestLogger's convention.
func Metrics(m *metrics.HTTPMetrics) Middleware {
	if m == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Reuse the responseWriter created by RequestLogger (the canonical
			// chain lists Metrics after it) so the status observed here is the
			// final one — including the 500 the inner Recoverer writes through
			// the shared wrapper on a recovered panic. Wrap only when used
			// standalone.
			rw, ok := w.(*responseWriter)
			if !ok {
				rw = &responseWriter{ResponseWriter: w}
			}

			holder := &routePatternHolder{}
			r = r.WithContext(context.WithValue(r.Context(), routePatternKey, holder))

			start := time.Now()
			m.RequestsInFlight.Inc()
			defer m.RequestsInFlight.Dec()

			next.ServeHTTP(rw, r)

			// Default to 200 when the handler wrote a body without an explicit
			// status (same convention as RequestLogger).
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			route := holder.pattern
			if route == "" {
				// Standalone fallback: without intervening request copies the
				// mux mutated the request value we still hold.
				route = r.Pattern
			}
			if route == "" {
				route = unmatchedRoute
			}
			method := normalizeMethod(r.Method)
			m.RequestsTotal.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
			m.RequestDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		})
	}
}
