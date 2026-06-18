// Package web embeds and serves the front-end static assets (the built Tailwind
// CSS, vendored HTMX/Alpine, and self-hosted fonts) so they ship inside the
// server binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// StaticFS returns the embedded static assets rooted so that, e.g.,
// "css/app.css" resolves to web/static/css/app.css.
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// staticFS always contains the "static" directory at build time, so a
		// failure here is a programming/build error.
		panic("web: embedded static assets missing: " + err.Error())
	}
	return sub
}

// StaticHandler serves the embedded static assets. Mount it under "/static/".
func StaticHandler() http.Handler {
	return http.FileServerFS(StaticFS())
}
