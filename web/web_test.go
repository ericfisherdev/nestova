package web_test

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
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

// TestStaticHandler_ServesManifestAsManifestJSON guards the .webmanifest MIME
// registration in this package's init (NES-151). Go has no built-in mapping
// for the extension, so without it the manifest is sniffed as text/plain and
// Chrome silently refuses to install the app — a failure that shows up only
// on a real device, never in a normal test run.
func TestStaticHandler_ServesManifestAsManifestJSON(t *testing.T) {
	srv := httptest.NewServer(http.StripPrefix("/static/", web.StaticHandler()))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/static/manifest.webmanifest")
	if err != nil {
		t.Fatalf("GET manifest: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET manifest status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("manifest Content-Type = %q, want application/manifest+json", ct)
	}
}

// TestStaticHandler_ServesEveryManifestIcon walks the manifest's own icon
// list rather than a hardcoded copy of it, so adding an icon entry without
// shipping the file fails here instead of in Chrome's install prompt.
func TestStaticHandler_ServesEveryManifestIcon(t *testing.T) {
	raw, err := fs.ReadFile(web.StaticFS(), "manifest.webmanifest")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Icons []struct {
			Src     string `json:"src"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if len(manifest.Icons) == 0 {
		t.Fatal("manifest declares no icons")
	}

	srv := httptest.NewServer(http.StripPrefix("/static/", web.StaticHandler()))
	t.Cleanup(srv.Close)

	var sawMaskable bool
	for _, icon := range manifest.Icons {
		// purpose is a space-separated token list per the spec: "maskable"
		// and "any maskable" both declare a maskable icon, so match the
		// token rather than the whole field.
		if slices.Contains(strings.Fields(icon.Purpose), "maskable") {
			sawMaskable = true
		}
		resp, err := http.Get(srv.URL + icon.Src)
		if err != nil {
			t.Errorf("GET %s: %v", icon.Src, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", icon.Src, resp.StatusCode)
		}
	}
	// Android adaptive icons clip anything without a maskable entry.
	if !sawMaskable {
		t.Error("manifest declares no maskable icon")
	}
}
