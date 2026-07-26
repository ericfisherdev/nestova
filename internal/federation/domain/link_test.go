package domain_test

import (
	"errors"
	"testing"

	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "https with trailing slash", raw: "https://Nestorage.Example.ts.net/", want: "https://nestorage.example.ts.net"},
		{name: "http scheme", raw: "http://nestorage.local:8080", want: "http://nestorage.local:8080"},
		{name: "mixed-case scheme", raw: "HTTPS://nestorage.example.ts.net", want: "https://nestorage.example.ts.net"},
		{name: "strips query and fragment", raw: "https://nestorage.example.ts.net/?foo=bar#frag", want: "https://nestorage.example.ts.net"},
		{name: "leading/trailing whitespace", raw: "  https://nestorage.example.ts.net  ", want: "https://nestorage.example.ts.net"},
		{name: "blank", raw: "", wantErr: true},
		{name: "whitespace only", raw: "   ", wantErr: true},
		{name: "no scheme", raw: "nestorage.example.ts.net", wantErr: true},
		{name: "unsupported scheme", raw: "ftp://nestorage.example.ts.net", wantErr: true},
		{name: "no host", raw: "https:///path", wantErr: true},
		{name: "malformed", raw: "https://%zz", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domain.NormalizeBaseURL(tt.raw)
			if tt.wantErr {
				if !errors.Is(err, domain.ErrInvalidBaseURL) {
					t.Fatalf("NormalizeBaseURL(%q) error = %v, want ErrInvalidBaseURL", tt.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeBaseURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeBaseURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNormalizeBaseURLIsIdempotent(t *testing.T) {
	// The unique index on lower(base_url) and this function's own equality
	// pre-checks (LinkService.Attach) both depend on normalizing an
	// already-normalized url back to the identical string.
	const normalized = "https://nestorage.example.ts.net"
	got, err := domain.NormalizeBaseURL(normalized)
	if err != nil {
		t.Fatalf("NormalizeBaseURL: %v", err)
	}
	if got != normalized {
		t.Errorf("NormalizeBaseURL(%q) = %q, want unchanged", normalized, got)
	}
}

func TestInstanceLinkValidate(t *testing.T) {
	base := func() *domain.InstanceLink {
		return &domain.InstanceLink{
			ID:          domain.NewInstanceLinkID(),
			HouseholdID: household.NewHouseholdID(),
			BaseURL:     "https://nestorage.example.ts.net",
			APIKeyEnc:   []byte("encrypted"),
		}
	}

	t.Run("valid link", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Errorf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("invalid base url", func(t *testing.T) {
		link := base()
		link.BaseURL = "not-a-url"
		if err := link.Validate(); !errors.Is(err, domain.ErrInvalidBaseURL) {
			t.Errorf("Validate() error = %v, want ErrInvalidBaseURL", err)
		}
	})

	t.Run("empty encrypted key", func(t *testing.T) {
		link := base()
		link.APIKeyEnc = nil
		if err := link.Validate(); err == nil {
			t.Error("Validate() error = nil, want an error for empty api key")
		}
	})
}
