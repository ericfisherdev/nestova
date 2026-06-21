package middleware

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// forwardedCtxKey keys the resolved scheme/client IP in the request context. A
// distinct struct type avoids any collision with other context keys.
type forwardedCtxKey struct{}

// forwarded holds the request's effective scheme and client IP after accounting
// for a trusted reverse proxy's X-Forwarded-* headers.
type forwarded struct {
	scheme   string
	clientIP string
}

// ForwardedHeaders resolves the effective request scheme and client IP from the
// X-Forwarded-Proto / X-Forwarded-For headers, but only when the immediate peer
// (r.RemoteAddr) falls inside one of the trusted proxy networks. From an
// untrusted peer the forwarded headers are ignored, so a direct client cannot
// spoof an https (secure) context by sending X-Forwarded-Proto: https. The
// resolved values are stored once on the request context; read them with
// RequestScheme, ClientIP, and IsHTTPS so every downstream consumer agrees.
func ForwardedHeaders(trusted []netip.Prefix) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fwd := resolveForwarded(r, trusted)
			ctx := context.WithValue(r.Context(), forwardedCtxKey{}, fwd)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveForwarded computes the effective scheme and client IP. It starts from
// the on-wire values (TLS state and the direct peer) and overrides them from the
// forwarded headers only when the peer is a trusted proxy.
func resolveForwarded(r *http.Request, trusted []netip.Prefix) forwarded {
	scheme := onWireScheme(r)
	peer := remoteIP(r.RemoteAddr)

	fwd := forwarded{scheme: scheme}
	if peer.IsValid() {
		fwd.clientIP = peer.String()
	} else {
		fwd.clientIP = r.RemoteAddr
	}

	if !peer.IsValid() || !ipInPrefixes(peer, trusted) {
		return fwd // untrusted (or unparseable) peer: ignore forwarded headers
	}

	if proto := normalizeScheme(firstToken(r.Header.Get("X-Forwarded-Proto"))); proto != "" {
		fwd.scheme = proto
	}
	if client := clientFromXFF(r.Header.Values("X-Forwarded-For"), trusted); client.IsValid() {
		fwd.clientIP = client.String()
	}
	return fwd
}

// onWireScheme reports the scheme of the connection to this server: https when
// TLS terminated here (NES-54), otherwise http.
func onWireScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// remoteIP extracts the IP from a host:port RemoteAddr, returning the zero Addr
// when it cannot be parsed. IPv4-mapped IPv6 addresses are unmapped so they match
// IPv4 prefixes.
func remoteIP(remoteAddr string) netip.Addr {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

// ipInPrefixes reports whether addr is contained by any of the prefixes.
func ipInPrefixes(addr netip.Addr, prefixes []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// normalizeScheme returns a lowercased "http" or "https", or "" for anything
// else. Restricting X-Forwarded-Proto to the two known schemes keeps a
// misconfigured (or hostile) trusted proxy from injecting an arbitrary scheme
// or header-smuggling payload into the effective scheme.
func normalizeScheme(s string) string {
	switch strings.ToLower(s) {
	case "http":
		return "http"
	case "https":
		return "https"
	default:
		return ""
	}
}

// firstToken returns the first comma-separated token of s, trimmed. For
// X-Forwarded-Proto with multiple proxies (e.g. "https, http") the first token
// is the scheme the original client used.
func firstToken(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// clientFromXFF returns the real client address from X-Forwarded-For: the
// rightmost address that is not itself one of our trusted proxies. It flattens
// possibly-multiple header lines and skips non-IP tokens (e.g. "unknown").
// Returns the zero Addr when no client address can be determined (caller keeps
// the direct peer).
func clientFromXFF(values []string, trusted []netip.Prefix) netip.Addr {
	var all []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				all = append(all, part)
			}
		}
	}
	for i := len(all) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(all[i])
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if !ipInPrefixes(addr, trusted) {
			return addr
		}
	}
	return netip.Addr{}
}

// fromContext returns the forwarded values stored by ForwardedHeaders.
func fromContext(ctx context.Context) forwarded {
	fwd, _ := ctx.Value(forwardedCtxKey{}).(forwarded)
	return fwd
}

// RequestScheme returns the effective request scheme ("http" or "https") as
// resolved by ForwardedHeaders, or "" when the middleware did not run.
func RequestScheme(ctx context.Context) string {
	return fromContext(ctx).scheme
}

// ClientIP returns the resolved client IP — XFF-aware behind a trusted proxy,
// otherwise the direct peer — or "" when the middleware did not run.
func ClientIP(ctx context.Context) string {
	return fromContext(ctx).clientIP
}

// IsHTTPS reports whether the effective request scheme is https. It is the
// signal used to gate Secure cookies (NES-51) and HSTS (NES-52).
func IsHTTPS(ctx context.Context) bool {
	return RequestScheme(ctx) == "https"
}
