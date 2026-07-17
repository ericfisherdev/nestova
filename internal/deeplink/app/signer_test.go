package app_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/deeplink/app"
	"github.com/ericfisherdev/nestova/internal/deeplink/domain"
)

func TestNewSigner_RejectsEmptyKey(t *testing.T) {
	if _, err := app.NewSigner(nil); err == nil {
		t.Fatal("NewSigner(nil) error = nil, want non-nil")
	}
	if _, err := app.NewSigner([]byte{}); err == nil {
		t.Fatal("NewSigner([]byte{}) error = nil, want non-nil")
	}
}

func TestNewSignerFromSecret_DerivesDistinctKeyPerPurpose(t *testing.T) {
	secret := []byte("a-32-byte-or-longer-test-secret!")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	deepLinkSigner, err := app.NewSignerFromSecret(secret, "nestova:deeplink:v1")
	if err != nil {
		t.Fatalf("NewSignerFromSecret: %v", err)
	}
	otherPurposeSigner, err := app.NewSignerFromSecret(secret, "nestova:other:v1")
	if err != nil {
		t.Fatalf("NewSignerFromSecret (other purpose): %v", err)
	}
	rawSigner, err := app.NewSigner(secret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	path := "/go/claim-task/abc-123"
	exp, sig := deepLinkSigner.Sign(path, now)

	// A signature minted by the deeplink-purpose signer must not verify under
	// a signer derived for a different purpose, nor under the raw (undreived)
	// secret directly — proving the derivation actually changes the key
	// rather than being a no-op, and that the raw secret is never usable
	// interchangeably with the derived key (NES-129's "never reuse a raw
	// secret across purposes" requirement).
	if err := otherPurposeSigner.Verify(path, exp, sig, now); err == nil {
		t.Error("signature verified under a differently-purposed derived key, want rejection")
	}
	if err := rawSigner.Verify(path, exp, sig, now); err == nil {
		t.Error("signature verified under the raw (un-derived) secret, want rejection")
	}
	if err := deepLinkSigner.Verify(path, exp, sig, now); err != nil {
		t.Errorf("signature failed to verify under its own signer: %v", err)
	}
}

func TestSigner_SignVerify_RoundTrip(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := "/go/redeem-reward/reward-1"

	exp, sig := signer.Sign(path, now)

	if err := signer.Verify(path, exp, sig, now); err != nil {
		t.Fatalf("Verify() immediately after Sign() = %v, want nil", err)
	}
	// Still valid just before expiry.
	if err := signer.Verify(path, exp, sig, now.Add(app.LinkTTL-time.Second)); err != nil {
		t.Errorf("Verify() just before expiry = %v, want nil", err)
	}
}

// TestSigner_Verify_ExpiresAtExactBoundary asserts expiry is treated as an
// exclusive upper bound: verifying at the EXACT expiry instant (not just
// after it) is already expired, closing the one-second window a strict ">"
// comparison would otherwise leave open.
func TestSigner_Verify_ExpiresAtExactBoundary(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := "/go/claim-task/abc-123"
	exp, sig := signer.Sign(path, now)

	err = signer.Verify(path, exp, sig, time.Unix(exp, 0))
	if !errors.Is(err, domain.ErrLinkExpired) {
		t.Fatalf("Verify() at the exact expiry instant = %v, want %v", err, domain.ErrLinkExpired)
	}
}

func TestSigner_Verify_RejectsTamperedPath(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	exp, sig := signer.Sign("/go/claim-task/abc-123", now)

	err = signer.Verify("/go/claim-task/DIFFERENT-ID", exp, sig, now)
	if !errors.Is(err, domain.ErrLinkInvalidSignature) {
		t.Fatalf("Verify(tampered path) = %v, want %v", err, domain.ErrLinkInvalidSignature)
	}
}

func TestSigner_Verify_RejectsTamperedExpiry(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := "/go/claim-task/abc-123"
	exp, sig := signer.Sign(path, now)

	// Extending the expiry without re-signing must fail: the signature covers
	// the expiry value itself, not just the path.
	err = signer.Verify(path, exp+3600, sig, now)
	if !errors.Is(err, domain.ErrLinkInvalidSignature) {
		t.Fatalf("Verify(tampered expiry) = %v, want %v", err, domain.ErrLinkInvalidSignature)
	}
}

func TestSigner_Verify_RejectsMalformedSignature(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	exp, _ := signer.Sign("/go/claim-task/abc-123", now)

	err = signer.Verify("/go/claim-task/abc-123", exp, "not-valid-base64url!!!", now)
	if !errors.Is(err, domain.ErrLinkInvalidSignature) {
		t.Fatalf("Verify(malformed sig) = %v, want %v", err, domain.ErrLinkInvalidSignature)
	}
}

func TestSigner_Verify_RejectsExpiredLink(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := "/go/claim-task/abc-123"
	exp, sig := signer.Sign(path, now)

	err = signer.Verify(path, exp, sig, now.Add(app.LinkTTL+time.Second))
	if !errors.Is(err, domain.ErrLinkExpired) {
		t.Fatalf("Verify(expired) = %v, want %v", err, domain.ErrLinkExpired)
	}
}

func TestSigner_Verify_SignatureCheckedBeforeExpiry(t *testing.T) {
	// A tampered AND expired link must report ErrLinkInvalidSignature, not
	// ErrLinkExpired — the signature check runs first, so a forged expiry can
	// never be distinguished from a forged path by an attacker probing which
	// error comes back (both are already merged into the same friendly HTTP
	// response by the adapter, but the app layer's own error precedence must
	// still be deterministic and tested independently).
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	exp, sig := signer.Sign("/go/claim-task/abc-123", now.Add(-time.Hour))

	err = signer.Verify("/go/claim-task/DIFFERENT-ID", exp, sig, now)
	if !errors.Is(err, domain.ErrLinkInvalidSignature) {
		t.Fatalf("Verify(tampered + expired) = %v, want %v", err, domain.ErrLinkInvalidSignature)
	}
}
