package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
)

// runSecurity chains ForwardedHeaders (so IsHTTPS is resolved exactly as in the
// real server) ahead of SecurityHeaders and returns the response headers.
func runSecurity(t *testing.T, hsts string, setup func(*http.Request)) http.Header {
	t.Helper()
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	h := middleware.ForwardedHeaders(trusted)(
		middleware.SecurityHeaders(hsts)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	if setup != nil {
		setup(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result().Header
}

func TestSecurityHeaders(t *testing.T) {
	t.Run("baseline headers on every response", func(t *testing.T) {
		hdr := runSecurity(t, "", nil)
		want := map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "strict-origin-when-cross-origin",
			"Content-Security-Policy": "frame-ancestors 'self'",
			"X-Frame-Options":         "SAMEORIGIN",
		}
		for k, v := range want {
			if got := hdr.Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
	})

	t.Run("HSTS emitted over https when enabled", func(t *testing.T) {
		const value = "max-age=15552000; includeSubDomains"
		hdr := runSecurity(t, value, func(r *http.Request) {
			r.Header.Set("X-Forwarded-Proto", "https")
		})
		if got := hdr.Get("Strict-Transport-Security"); got != value {
			t.Errorf("Strict-Transport-Security = %q, want %q", got, value)
		}
	})

	t.Run("HSTS absent over http", func(t *testing.T) {
		hdr := runSecurity(t, "max-age=15552000", nil) // no XFP -> effective http
		if got := hdr.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("Strict-Transport-Security = %q, want empty over http", got)
		}
	})

	t.Run("HSTS absent when disabled even over https", func(t *testing.T) {
		hdr := runSecurity(t, "", func(r *http.Request) {
			r.Header.Set("X-Forwarded-Proto", "https")
		})
		if got := hdr.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("Strict-Transport-Security = %q, want empty when disabled", got)
		}
	})

	t.Run("no duplicate X-Content-Type-Options", func(t *testing.T) {
		hdr := runSecurity(t, "", nil)
		if vals := hdr.Values("X-Content-Type-Options"); len(vals) != 1 {
			t.Errorf("X-Content-Type-Options has %d values, want 1", len(vals))
		}
	})
}
