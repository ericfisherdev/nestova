// Package web embeds and serves the front-end static assets (the built Tailwind
// CSS, vendored HTMX/Alpine, and self-hosted fonts) so they ship inside the
// server binary.
package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Go's mime package has no built-in mapping for .webmanifest, so
// http.FileServerFS would sniff the manifest and serve it as text/plain.
// Chrome requires a JSON-ish type (application/manifest+json) before it
// will accept a manifest, so registering the extension here is what makes
// the app installable from the Go binary alone, with no reverse proxy
// involved (NES-151).
func init() {
	if err := mime.AddExtensionType(".webmanifest", "application/manifest+json"); err != nil {
		panic("web: registering .webmanifest MIME type: " + err.Error())
	}
}

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
