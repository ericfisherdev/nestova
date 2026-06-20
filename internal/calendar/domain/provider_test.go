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
