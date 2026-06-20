package domain

import "fmt"

// Provider is a connected calendar provider. Stored as text, validated here (not
// iota), so a matching Postgres CHECK can mirror the allowed set. Google is the
// only provider today; the type leaves room for more.
type Provider string

// Calendar providers.
const (
	// ProviderGoogle is Google Calendar (the only provider currently supported).
	ProviderGoogle Provider = "google"
)

// Providers returns the supported providers in canonical order. Callers (e.g. a
// CHECK-constraint generator or a UI list) can range over this rather than
// hard-coding the set.
func Providers() []Provider {
	return []Provider{ProviderGoogle}
}

// Valid reports whether p is a known provider.
func (p Provider) Valid() bool {
	switch p {
	case ProviderGoogle:
		return true
	default:
		return false
	}
}

// String returns the provider's stored value.
func (p Provider) String() string { return string(p) }

// ParseProvider validates and returns a Provider, or an error for an unknown value.
func ParseProvider(s string) (Provider, error) {
	p := Provider(s)
	if !p.Valid() {
		return "", fmt.Errorf("invalid provider %q", s)
	}
	return p, nil
}
