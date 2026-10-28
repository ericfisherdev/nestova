package httpserver

import "github.com/ericfisherdev/nestcore/httpserver/middleware"

// contentSecurityPolicy returns the strict Content-Security-Policy enforced on
// every Nestova response (NES-212). It is a fixed compile-time policy: no
// environment toggle, no Report-Only phase, no nonces and no hashes. A LAN
// appliance has no report-uri collector, so violations surface where the
// e2e csp.spec.js asserts on them; every page loads only self-hosted assets
// and carries no inline script or style (NES-211).
//
// The directive order is part of the contract: tests assert the rendered
// header byte for byte.
func contentSecurityPolicy() middleware.ContentSecurityPolicy {
	self := []string{"'self'"}
	return middleware.ContentSecurityPolicy{
		// Fallback for every fetch directive not listed below.
		{Name: "default-src", Values: self},
		// Self-hosted HTMX, Alpine (CSP build) and the page scripts under
		// /static/js. No 'unsafe-inline' or 'unsafe-eval'.
		{Name: "script-src", Values: self},
		// The built Tailwind sheet under /static/css. Inline style attributes
		// and <style> blocks are not used.
		{Name: "style-src", Values: self},
		// 'self' for photos and icons; data: for the MFA enrolment and kiosk
		// QR codes, which render as data:image/png;base64 URIs.
		{Name: "img-src", Values: []string{"'self'", "data:"}},
		// Self-hosted Hanken Grotesk and Space Mono under /static/fonts.
		{Name: "font-src", Values: self},
		// HTMX requests, fetch() uploads and the WebAuthn ceremonies all go
		// back to the same origin.
		{Name: "connect-src", Values: self},
		// Explicit so /sw.js stays covered by 'self' however script-src changes
		// later; without it worker-src falls back to script-src.
		{Name: "worker-src", Values: self},
		// /static/manifest.webmanifest for the installable PWA.
		{Name: "manifest-src", Values: self},
		// No plugins.
		{Name: "object-src", Values: []string{"'none'"}},
		// Stops an injected <base> from redirecting relative URLs.
		{Name: "base-uri", Values: self},
		// Forms post only to Nestova itself.
		{Name: "form-action", Values: self},
		// Framing stays limited to the page's own origin, as before.
		{Name: "frame-ancestors", Values: self},
		// upgrade-insecure-requests is deliberately absent: the kiosk and
		// plain-HTTP LAN access must keep working.
	}
}
