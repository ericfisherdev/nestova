package render_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/ericfisherdev/nestova/internal/platform/render"
)

// comp builds a templ.Component that writes a fixed string, for testing the seam
// without real templ files.
func comp(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}

// wrap is a test layout: it surrounds the content with <layout>…</layout>.
func wrap(content templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, "<layout>"); err != nil {
			return err
		}
		if err := content.Render(ctx, w); err != nil {
			return err
		}
		_, err := io.WriteString(w, "</layout>")
		return err
	})
}

func TestIsHTMX(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if render.IsHTMX(req) {
		t.Error("IsHTMX = true for a request without HX-Request")
	}
	req.Header.Set("HX-Request", "true")
	if !render.IsHTMX(req) {
		t.Error("IsHTMX = false for a request with HX-Request: true")
	}
}

func TestRenderWritesStatusAndContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := render.Render(context.Background(), rec, http.StatusCreated, comp("hi")); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if rec.Body.String() != "hi" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "hi")
	}
}

func TestRenderBuffersAndDoesNotCommitOnError(t *testing.T) {
	rec := httptest.NewRecorder()
	failing := templ.ComponentFunc(func(context.Context, io.Writer) error {
		return io.ErrClosedPipe
	})
	if err := render.Render(context.Background(), rec, http.StatusOK, failing); err == nil {
		t.Fatal("Render returned nil error for a failing component")
	}
	// On a render failure nothing should be written: the status must not be
	// committed (httptest defaults Code to 200 but writes none), and the body
	// must be empty, so the caller can send its own error response.
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty on render error, got %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Errorf("Content-Type should not be set on render error, got %q", rec.Header().Get("Content-Type"))
	}
}

func TestPageFullNavigationWrapsInLayout(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil) // not HTMX
	if err := render.Page(req.Context(), rec, req, wrap, comp("CONTENT")); err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := rec.Body.String(); got != "<layout>CONTENT</layout>" {
		t.Errorf("full-navigation body = %q, want the layout-wrapped content", got)
	}
	if got := rec.Header().Get("Vary"); got != "HX-Request" {
		t.Errorf("Vary = %q, want %q", got, "HX-Request")
	}
}

func TestPageHTMXReturnsFragmentOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	if err := render.Page(req.Context(), rec, req, wrap, comp("CONTENT")); err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := rec.Body.String(); got != "CONTENT" {
		t.Errorf("HTMX body = %q, want the bare content (no layout)", got)
	}
	if strings.Contains(rec.Body.String(), "<layout>") {
		t.Error("HTMX response was wrapped in the layout")
	}
}
