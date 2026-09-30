package httpserver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRefuseNULBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     string
		body       string
		wantStatus int
	}{
		{"clean form passes", "/x", "name=Milk", http.StatusNoContent},
		{"NUL in a form value is refused", "/x", "name=" + url.QueryEscape("a\x00b"), http.StatusUnprocessableEntity},
		{"NUL in a form key is refused", "/x", url.QueryEscape("a\x00b") + "=1", http.StatusUnprocessableEntity},
		{"NUL in the query string is refused", "/x?q=" + url.QueryEscape("a\x00b"), "", http.StatusUnprocessableEntity},
		{"NUL beside a malformed pair is refused, not forwarded", "/x", "name=" + url.QueryEscape("a\x00b") + "&junk=%zz", http.StatusBadRequest},
		{"a body over the form cap is refused", "/x", "name=" + strings.Repeat("a", maxFormBodyBytes), http.StatusBadRequest},
		{"other control characters pass", "/x", "name=" + url.QueryEscape("tab\there"), http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reached := false
			handler := refuseNULBytes(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if reached != (tc.wantStatus == http.StatusNoContent) {
				t.Fatalf("handler reached = %v with status %d", reached, rec.Code)
			}
		})
	}
}

func TestRefuseNULBytes_HandlerStillReadsParsedForm(t *testing.T) {
	t.Parallel()

	var got string
	handler := refuseNULBytes(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.FormValue("name")
	}))
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("name=Milk"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got != "Milk" {
		t.Fatalf("FormValue = %q, want Milk", got)
	}
}
