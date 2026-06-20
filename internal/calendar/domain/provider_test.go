package domain_test

import (
	"testing"

	calendar "github.com/ericfisherdev/nestova/internal/calendar/domain"
)

func TestProviderValidAndParse(t *testing.T) {
	for _, p := range calendar.Providers() {
		if !p.Valid() {
			t.Errorf("Providers() returned invalid provider %q", p)
		}
		parsed, err := calendar.ParseProvider(p.String())
		if err != nil {
			t.Errorf("ParseProvider(%q) error = %v", p, err)
		}
		if parsed != p {
			t.Errorf("ParseProvider(%q) = %q, want %q", p, parsed, p)
		}
	}
}

func TestParseProviderInvalid(t *testing.T) {
	if _, err := calendar.ParseProvider("outlook"); err == nil {
		t.Fatal("ParseProvider(\"outlook\") error = nil, want non-nil")
	}
}

// TestProviderWireValues pins the stored string for each provider. These literal
// values are mirrored by the provider CHECK constraint in the 00016 migration, so
// changing one here without updating the migration would silently break inserts.
func TestProviderWireValues(t *testing.T) {
	if got := calendar.ProviderGoogle.String(); got != "google" {
		t.Fatalf("ProviderGoogle.String() = %q, want \"google\"", got)
	}
	seen := make(map[calendar.Provider]bool)
	for _, p := range calendar.Providers() {
		if seen[p] {
			t.Fatalf("Providers() contains a duplicate: %q", p)
		}
		seen[p] = true
	}
}
