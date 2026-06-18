package web_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web"
)

// TestStaticFSContainsBuiltCSS verifies the Tailwind build output is embedded and
// carries the A · Hearth design tokens. If this fails, run `make assets`.
func TestStaticFSContainsBuiltCSS(t *testing.T) {
	data, err := fs.ReadFile(web.StaticFS(), "css/app.css")
	if err != nil {
		t.Fatalf("read embedded css/app.css (run `make assets`): %v", err)
	}
	if len(data) == 0 {
		t.Fatal("embedded app.css is empty")
	}
	css := string(data)
	for _, token := range []string{
		"--font-sans",              // font token
		"--color-sage",             // brand color token
		"--color-member-clay-tint", // member color system token
		"--radius-control",         // radius token
		"@font-face",               // self-hosted fonts
	} {
		if !strings.Contains(css, token) {
			t.Errorf("app.css is missing expected token %q", token)
		}
	}
}

// TestStaticHandlerServesAssets verifies the embedded assets are served.
func TestStaticHandlerServesAssets(t *testing.T) {
	srv := httptest.NewServer(http.StripPrefix("/static/", web.StaticHandler()))
	t.Cleanup(srv.Close)

	for _, path := range []string{
		"/static/css/app.css",
		"/static/js/htmx.min.js",
		"/static/js/alpine.min.js",
		"/static/fonts/hanken-grotesk.woff2",
		"/static/fonts/space-mono.woff2",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Errorf("GET %s: %v", path, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, resp.StatusCode, http.StatusOK)
		}
	}
}
