package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestValidateEmail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		email string
		want  error
	}{
		{"ordinary address", "alex@example.com", nil},
		{"dotless domain", "a@b", nil},
		{"internationalized domain", "idn@bücher.example", nil},
		{"local part at the limit", strings.Repeat("l", domain.MaxEmailLocalPartLength) + "@example.com", nil},
		{"double at", "a@@b.com", domain.ErrEmailMalformed},
		{"quoted local part holding an at", `"a@b"@c.com`, domain.ErrEmailMalformed},
		{"no at", "alex.example.com", domain.ErrEmailMalformed},
		{"empty local part", "@example.com", domain.ErrEmailMalformed},
		{"display name form", "Alex <alex@example.com>", domain.ErrEmailMalformed},
		{"trailing comment", "alex@example.com (Alex)", domain.ErrEmailMalformed},
		{"embedded space", "al ex@example.com", domain.ErrEmailMalformed},
		{"local part one over", strings.Repeat("l", domain.MaxEmailLocalPartLength+1) + "@example.com", domain.ErrEmailTooLong},
		{"300 character local part", strings.Repeat("l", 300) + "@test.local", domain.ErrEmailTooLong},
		{"address one over", "a@" + strings.Repeat("d", domain.MaxEmailLength-1), domain.ErrEmailTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := domain.ValidateEmail(tc.email); !errors.Is(err, tc.want) {
				t.Errorf("ValidateEmail(%q) = %v, want %v", tc.email, err, tc.want)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		password string
		want     error
	}{
		{"at the limit", strings.Repeat("p", domain.MaxPasswordLength), nil},
		{"multibyte at the limit", strings.Repeat("家", domain.MaxPasswordLength), nil},
		{"one over", strings.Repeat("p", domain.MaxPasswordLength+1), domain.ErrPasswordTooLong},
		{"one million characters", strings.Repeat("p", 1_000_000), domain.ErrPasswordTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := domain.ValidatePassword(tc.password); !errors.Is(err, tc.want) {
				t.Errorf("ValidatePassword() = %v, want %v", err, tc.want)
			}
		})
	}
}
