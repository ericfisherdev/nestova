package app_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/calendar/app"
)

func mustSigner(t *testing.T, key string) *app.OAuthStateSigner {
	t.Helper()
	s, err := app.NewOAuthStateSigner([]byte(key))
	if err != nil {
		t.Fatalf("NewOAuthStateSigner: %v", err)
	}
	return s
}

func TestStateSignVerifyRoundTrip(t *testing.T) {
	s := mustSigner(t, "the-hmac-key")
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	state := s.Sign("member-123", now)
	got, err := s.Verify(state, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != "member-123" {
		t.Fatalf("Verify = %q, want member-123", got)
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	s := mustSigner(t, "the-hmac-key")
	now := time.Now()
	state := s.Sign("member-123", now)
	tampered := state[:len(state)-1] + "X"
	if _, err := s.Verify(tampered, now); !errors.Is(err, app.ErrInvalidState) {
		t.Fatalf("Verify(tampered) = %v, want ErrInvalidState", err)
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	state := mustSigner(t, "key-a").Sign("member-123", time.Now())
	if _, err := mustSigner(t, "key-b").Verify(state, time.Now()); !errors.Is(err, app.ErrInvalidState) {
		t.Fatalf("Verify(wrong key) = %v, want ErrInvalidState", err)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := mustSigner(t, "the-hmac-key")
	signedAt := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	state := s.Sign("member-123", signedAt)
	// 11 minutes later is past the 10-minute TTL.
	if _, err := s.Verify(state, signedAt.Add(11*time.Minute)); !errors.Is(err, app.ErrInvalidState) {
		t.Fatalf("Verify(expired) = %v, want ErrInvalidState", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	s := mustSigner(t, "the-hmac-key")
	for _, bad := range []string{"", "no-dot", "a.b.c", "!.!!"} {
		if _, err := s.Verify(bad, time.Now()); !errors.Is(err, app.ErrInvalidState) {
			t.Errorf("Verify(%q) = %v, want ErrInvalidState", bad, err)
		}
	}
}

func TestNewOAuthStateSignerRejectsEmptyKey(t *testing.T) {
	if _, err := app.NewOAuthStateSigner(nil); err == nil {
		t.Fatal("NewOAuthStateSigner(nil) error = nil, want non-nil")
	}
}
