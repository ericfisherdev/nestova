package adapter

import "testing"

// TestSanitizeNext covers the post-login redirect sanitizer's same-origin
// guard directly (white-box: sanitizeNext is unexported). Prior to NES-129
// this had no dedicated coverage of its own — only indirect exercise through
// higher-level login-flow tests — despite guarding an open-redirect surface;
// this fills that gap and extends it with the /go/ deep-link shape NES-129
// adds (a same-origin path carrying its own query string, which must survive
// the sanitizer unchanged for the QR login-continuation flow to work).
func TestSanitizeNext(t *testing.T) {
	tests := []struct {
		name string
		next string
		want string
	}{
		{"empty defaults to root", "", "/"},
		{"simple path", "/tasks", "/tasks"},
		{"path with query string", "/tasks?foo=bar", "/tasks?foo=bar"},
		{
			name: "deep-link path with exp/sig query string is preserved (NES-129)",
			next: "/go/claim-task/abc-123?exp=1234567890&sig=abcDEF123_-",
			want: "/go/claim-task/abc-123?exp=1234567890&sig=abcDEF123_-",
		},
		{"add-chore deep link has no id segment", "/go/add-chore?exp=1&sig=x", "/go/add-chore?exp=1&sig=x"},
		{"absolute URL is rejected", "https://evil.example/steal", "/"},
		{"protocol-relative URL is rejected", "//evil.example/steal", "/"},
		{"missing leading slash is rejected", "evil.example", "/"},
		{"ordinary traversal is cleaned, not rejected", "/foo/../bar", "/bar"},
		{"traversal past root collapses to a same-origin path, not rejected", "/foo/..//evil.com", "/evil.com"},
		{"malformed percent-encoding falls back to root", "/%zz", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeNext(tt.next); got != tt.want {
				t.Errorf("sanitizeNext(%q) = %q, want %q", tt.next, got, tt.want)
			}
		})
	}
}
