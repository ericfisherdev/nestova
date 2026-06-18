package middleware_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
)

// hijackRecorder is an httptest.ResponseRecorder that also satisfies
// http.Hijacker, for exercising the post-hijack code paths.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestChainOrder(t *testing.T) {
	var order []string
	mw := func(name string) middleware.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := middleware.Chain(mw("a"), mw("b"), mw("c"))(okHandler())
	serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := strings.Join(order, ","); got != "a,b,c" {
		t.Errorf("execution order = %q, want %q (first arg outermost)", got, "a,b,c")
	}
}

func TestRequestIDGeneratedAndEchoed(t *testing.T) {
	var seen string
	h := middleware.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = middleware.RequestIDFromContext(r.Context())
	}))
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Error("RequestIDFromContext is empty; id not stored in context")
	}
	if got := rec.Header().Get("X-Request-Id"); got != seen {
		t.Errorf("echoed header = %q, want context id %q", got, seen)
	}
}

func TestRequestIDReusesIncoming(t *testing.T) {
	const incoming = "client-correlation-123"
	var seen string
	h := middleware.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = middleware.RequestIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", incoming)
	rec := serve(h, req)

	if seen != incoming {
		t.Errorf("context id = %q, want incoming %q", seen, incoming)
	}
	if got := rec.Header().Get("X-Request-Id"); got != incoming {
		t.Errorf("echoed header = %q, want %q", got, incoming)
	}
}

func TestRequestIDRejectsInvalidIncoming(t *testing.T) {
	// Each of these must be rejected and replaced with a freshly generated id
	// (32 hex chars), never reflected back.
	bad := map[string]string{
		"too long":          strings.Repeat("a", 200),
		"space":             "has space",
		"header injection":  "abc\r\nX-Evil: 1",
		"control character": "abc\x00def",
	}
	for name, value := range bad {
		t.Run(name, func(t *testing.T) {
			var seen string
			h := middleware.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = middleware.RequestIDFromContext(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Request-Id", value)
			rec := serve(h, req)

			if seen == value {
				t.Errorf("reflected invalid incoming id %q", value)
			}
			if len(seen) != 32 { // 16 random bytes hex-encoded
				t.Errorf("generated id %q is not the expected 32-hex format", seen)
			}
			if got := rec.Header().Get("X-Request-Id"); got != seen {
				t.Errorf("echoed header %q != context id %q", got, seen)
			}
		})
	}
}

func TestRecovererReturns500AndDoesNotLeak(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	const secret = "super-secret-panic-value"

	// Recoverer sits behind RequestLogger in the real chain, so the writer is
	// the wrapper that lets Recoverer confirm no response started before it
	// writes the 500.
	h := middleware.Chain(middleware.RequestLogger(logger), middleware.Recoverer(logger))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(secret)
		}))
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("response body leaked the panic value: %q", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "panic recovered") {
		t.Error("panic was not logged")
	}
}

// TestRecovererStandaloneReturns500 verifies Recoverer produces a 500 even when
// used without RequestLogger (it wraps the writer itself to track response state).
func TestRecovererStandaloneReturns500(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(bytes.NewBuffer(nil), nil))
	h := middleware.Recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("standalone Recoverer status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// TestRecovererSkips500AfterHijack verifies that a panic after a successful
// hijack does not cause Recoverer to write a 500 over the hijacked connection.
func TestRecovererSkips500AfterHijack(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(bytes.NewBuffer(nil), nil))
	rr := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	h := middleware.Recoverer(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		panic("after hijack")
	}))
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if !rr.hijacked {
		t.Fatal("handler did not hijack the connection")
	}
	if rr.Code == http.StatusInternalServerError {
		t.Error("Recoverer wrote a 500 after the connection was hijacked")
	}
}

// TestRecovererRepanicsErrAbortHandler verifies the documented http.ErrAbortHandler
// sentinel is propagated (not swallowed into a 500), so net/http can abort the
// connection as intended.
func TestRecovererRepanicsErrAbortHandler(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(bytes.NewBuffer(nil), nil))
	h := middleware.Recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler to be re-panicked", r)
		}
	}()
	serve(h, httptest.NewRequest(http.MethodGet, "/", nil))
	t.Fatal("expected a re-panic of http.ErrAbortHandler")
}

// TestRecovererKeepsPartialResponse verifies that when a handler panics after
// the response has started, Recoverer does not append a corrupt 500 body or try
// to overwrite the already-sent status.
func TestRecovererKeepsPartialResponse(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(bytes.NewBuffer(nil), nil))
	// RequestLogger (outermost) wraps the writer so Recoverer sees a
	// *responseWriter that tracks whether headers were sent.
	h := middleware.Chain(middleware.RequestLogger(logger), middleware.Recoverer(logger))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("partial"))
			panic("boom after write")
		}))
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d (must not overwrite a sent status)", rec.Code, http.StatusTeapot)
	}
	if strings.Contains(rec.Body.String(), "internal server error") {
		t.Errorf("Recoverer appended to a partial response: %q", rec.Body.String())
	}
}

func TestRequestLoggerNilLoggerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("RequestLogger(nil) did not panic")
		}
	}()
	middleware.RequestLogger(nil)
}

func TestRecovererNilLoggerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Recoverer(nil) did not panic")
		}
	}()
	middleware.Recoverer(nil)
}

func TestTimeoutSetsDeadline(t *testing.T) {
	var hasDeadline bool
	h := middleware.Timeout(50 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, hasDeadline = r.Context().Deadline()
	}))
	serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if !hasDeadline {
		t.Error("request context has no deadline; Timeout middleware not applied")
	}
}

// TestTimeoutCancelsSlowHandler verifies the request context is cancelled once
// the timeout elapses. The middleware signals via context cancellation (not a
// 504) so streaming responses keep working; handlers observe r.Context().Done().
func TestTimeoutCancelsSlowHandler(t *testing.T) {
	var cancelled bool
	h := middleware.Timeout(10 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second): // slow path; should not be taken
		case <-r.Context().Done():
			cancelled = true
		}
	}))
	serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if !cancelled {
		t.Error("handler context was not cancelled after the timeout elapsed")
	}
}

func TestRequestLoggerCapturesStatusAndSize(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	const body = "created!"
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	})
	// RequestID before the logger so the logged line carries a request id.
	h := middleware.Chain(middleware.RequestID, middleware.RequestLogger(logger))(handler)
	serve(h, httptest.NewRequest(http.MethodPost, "/widgets", nil))

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v (%q)", err, buf.String())
	}
	if rec["status"] != float64(http.StatusCreated) {
		t.Errorf("logged status = %v, want %d", rec["status"], http.StatusCreated)
	}
	if rec["bytes"] != float64(len(body)) {
		t.Errorf("logged bytes = %v, want %d", rec["bytes"], len(body))
	}
	if rec["method"] != http.MethodPost {
		t.Errorf("logged method = %v, want %q", rec["method"], http.MethodPost)
	}
	if id, _ := rec["request_id"].(string); id == "" {
		t.Error("logged request_id is empty")
	}
}

// TestResponseWriterPreservesFlusher verifies the logger's ResponseWriter
// wrapper keeps the http.Flusher interface available to handlers (needed for
// HTMX/SSE streaming).
func TestResponseWriterPreservesFlusher(t *testing.T) {
	var flushable bool
	logger := slog.New(slog.NewJSONHandler(bytes.NewBuffer(nil), nil))
	h := middleware.RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flushable = w.(http.Flusher)
	}))
	serve(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if !flushable {
		t.Error("wrapped ResponseWriter does not expose http.Flusher")
	}
}
