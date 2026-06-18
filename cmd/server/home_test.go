package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func webMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerWebRoutes(mux, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return mux
}

func TestDashboardRendersShell(t *testing.T) {
	rec := httptest.NewRecorder()
	webMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		`href="/static/css/app.css"`, // styled
		"Nestova",                    // wordmark
		`aria-label="Primary"`,       // sidebar nav
		"Calendar", "Chores",         // nav pills
		"Create",    // sidebar Create CTA
		"Dashboard", // page heading
		// full placeholder card set (templ escapes "&")
		"Meals &amp; Recipes", "Groceries", "Photos", "Subscriptions",
		"Maya",         // family list
		`id="sidebar"`, // shell sidebar
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard page missing %q", want)
		}
	}
}

// TestPrimaryNavActive verifies only the matching nav item is marked active.
func TestPrimaryNavActive(t *testing.T) {
	nav := primaryNav("/chores")
	var activeCount int
	for _, item := range nav {
		if item.Active {
			activeCount++
			if item.Href != "/chores" {
				t.Errorf("active item = %q, want /chores", item.Href)
			}
		}
	}
	if activeCount != 1 {
		t.Errorf("active nav items = %d, want 1", activeCount)
	}
	for _, item := range primaryNav("") {
		if item.Active {
			t.Errorf("no item should be active for empty selection, got %q", item.Href)
		}
	}
}
