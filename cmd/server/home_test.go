package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func exampleMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerExampleRoutes(mux, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return mux
}

func TestExampleHomeRendersFullPage(t *testing.T) {
	rec := httptest.NewRecorder()
	exampleMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", `href="/static/css/app.css"`, "Create", "Nestova"} {
		if !strings.Contains(body, want) {
			t.Errorf("full page missing %q", want)
		}
	}
}

func TestExampleHomeHTMXReturnsFragment(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	exampleMux().ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") || strings.Contains(body, "<html") {
		t.Errorf("HTMX home should return a fragment without a full document: %q", body[:min(120, len(body))])
	}
	if !strings.Contains(body, "Create") {
		t.Errorf("HTMX home fragment missing content: %q", body)
	}
}

func TestExampleFragmentRoute(t *testing.T) {
	rec := httptest.NewRecorder()
	exampleMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fragment", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Errorf("fragment route should not return a full document: %q", body)
	}
	if !strings.Contains(body, "Swapped!") {
		t.Errorf("fragment missing expected content: %q", body)
	}
}
