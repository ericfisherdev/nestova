package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

func TestParseE164Phone_Valid(t *testing.T) {
	cases := []string{
		"+15551234567",
		"+447911123456",
		"+8613800138000",
		"+1234567890123",
	}
	for _, s := range cases {
		got, err := domain.ParseE164Phone(s)
		if err != nil {
			t.Errorf("ParseE164Phone(%q) error = %v, want nil", s, err)
			continue
		}
		if got.String() != s {
			t.Errorf("ParseE164Phone(%q).String() = %q, want %q", s, got.String(), s)
		}
	}
}

func TestParseE164Phone_Invalid(t *testing.T) {
	cases := []string{
		"",
		"5551234567",        // missing leading '+'
		"+0123456789",       // leading zero after '+' (ambiguous country code)
		"+1 555 123 4567",   // spaces
		"+1-555-123-4567",   // hyphens
		"+",                 // bare plus
		"++15551234567",     // double plus
		"+1234567890123456", // 16 digits, over E.164's 15-digit max
		"not-a-phone-number",
	}
	for _, s := range cases {
		if _, err := domain.ParseE164Phone(s); err == nil {
			t.Errorf("ParseE164Phone(%q) error = nil, want an error", s)
		}
	}
}

// TestParseE164Phone_RejectsBeforeAnyAPICall documents the value object's
// entire reason for existing (NES-138): a malformed number is rejected
// HERE, in-process, before any AWS API call — AWS End User Messaging
// bills a SendTextMessage attempt against a malformed destination the same
// as a successful send.
func TestParseE164Phone_RejectsBeforeAnyAPICall(t *testing.T) {
	_, err := domain.ParseE164Phone("this is not a phone number at all")
	if err == nil {
		t.Fatal("ParseE164Phone(malformed) must reject before construction succeeds")
	}
}
