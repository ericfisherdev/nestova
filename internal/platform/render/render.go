// Package render is the HTTP rendering seam for templ components. It writes
// components as HTML responses and supports the HTMX full-page vs. fragment
// pattern. It deliberately ships no layout component: the layout is supplied by
// the caller (the app shell is owned by NES-21), so this package stays a thin,
// reusable seam.
package render

import (
	"bytes"
	"context"
	"net/http"

	"github.com/a-h/templ"
)

// IsHTMX reports whether the request was issued by HTMX (a partial/AJAX request)
// rather than a full browser navigation.
func IsHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// Render writes c as an HTML response with the given status code. The component
// is rendered into a buffer first: if rendering fails, nothing is written to w
// (the status is not committed), so the caller can send an error response
// instead. On success the status, headers, and body are written together. Pages
// are small, so buffering is preferable to streaming a half-written response;
// genuinely streamed responses (e.g. SSE) should not use this seam.
func Render(ctx context.Context, w http.ResponseWriter, status int, c templ.Component) error {
	var buf bytes.Buffer
	if err := c.Render(ctx, &buf); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := buf.WriteTo(w)
	return err
}

// Page renders content for a full navigation or an HTMX request: HTMX requests
// receive the bare content fragment, while normal navigations receive content
// wrapped by layout. The Vary: HX-Request header keeps the two variants cached
// separately. layout takes the content component and returns the full-page
// component (e.g. the app shell from NES-21).
func Page(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	layout func(templ.Component) templ.Component,
	content templ.Component,
) error {
	w.Header().Set("Vary", "HX-Request")
	if IsHTMX(r) {
		return Render(ctx, w, http.StatusOK, content)
	}
	return Render(ctx, w, http.StatusOK, layout(content))
}
