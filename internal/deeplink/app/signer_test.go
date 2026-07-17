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
	if _, err := otherPurposeSigner.Verify(path, exp, sig, now); err == nil {
		t.Error("signature verified under a differently-purposed derived key, want rejection")
	}
	if _, err := rawSigner.Verify(path, exp, sig, now); err == nil {
		t.Error("signature verified under the raw (un-derived) secret, want rejection")
	}
	if _, err := deepLinkSigner.Verify(path, exp, sig, now); err != nil {
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

	if _, err := signer.Verify(path, exp, sig, now); err != nil {
		t.Fatalf("Verify() immediately after Sign() = %v, want nil", err)
	}
	// Still valid just before expiry.
	if _, err := signer.Verify(path, exp, sig, now.Add(app.LinkTTL-time.Second)); err != nil {
		t.Errorf("Verify() just before expiry = %v, want nil", err)
	}
}

// TestSigner_Verify_ReturnsDecodedSignatureBytes asserts Verify's second
// return value is the signature's raw decoded bytes (used as the
// deeplink/adapter redemption-replay guard's idempotency key — see that
// package and decode's own doc comment for why the canonical DECODED bytes,
// not the presented string, are what must key it).
func TestSigner_Verify_ReturnsDecodedSignatureBytes(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := "/go/claim-task/abc-123"
	exp, sig := signer.Sign(path, now)

	decoded, err := signer.Verify(path, exp, sig, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(decoded) == 0 {
		t.Fatal("Verify() returned no decoded signature bytes")
	}
	// HMAC-SHA256 output is always 32 bytes.
	if len(decoded) != 32 {
		t.Errorf("len(decoded) = %d, want 32 (HMAC-SHA256 output size)", len(decoded))
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

	_, err = signer.Verify(path, exp, sig, time.Unix(exp, 0))
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

	_, err = signer.Verify("/go/claim-task/DIFFERENT-ID", exp, sig, now)
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
	_, err = signer.Verify(path, exp+3600, sig, now)
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

	_, err = signer.Verify("/go/claim-task/abc-123", exp, "not-valid-base64url!!!", now)
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

	_, err = signer.Verify(path, exp, sig, now.Add(app.LinkTTL+time.Second))
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

	_, err = signer.Verify("/go/claim-task/DIFFERENT-ID", exp, sig, now)
	if !errors.Is(err, domain.ErrLinkInvalidSignature) {
		t.Fatalf("Verify(tampered + expired) = %v, want %v", err, domain.ErrLinkInvalidSignature)
	}
}

// ---------------------------------------------------------------------------
// Canonical signature encoding (CodeRabbit finding): a base64url-encoded
// HMAC-SHA256 output has 2 unconstrained "don't care" bits in its final
// character, so an ordinary (non-strict) decoder accepts several distinct
// strings that all decode to the identical signature bytes. If Verify
// tolerated any of them, an idempotency guard keyed off the presented
// signature STRING (rather than its canonical decoded bytes) could be
// bypassed by presenting a cosmetically different but byte-equivalent
// signature. Every variant below must be rejected outright; only the exact
// string Sign produces may verify.
// ---------------------------------------------------------------------------

func TestSigner_Verify_RejectsNonCanonicalSignatureEncoding(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := "/go/redeem-reward/reward-1"
	exp, sig := signer.Sign(path, now)

	if len(sig) == 0 {
		t.Fatal("Sign produced an empty signature")
	}

	// Canonical form must still verify.
	if _, err := signer.Verify(path, exp, sig, now); err != nil {
		t.Fatalf("Verify(canonical) = %v, want nil", err)
	}

	// Flip the last character to every other symbol in the base64url
	// alphabet; any that still base64-decode must nonetheless be REJECTED
	// unless they happen to reproduce the exact canonical string.
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	lastIdx := len(sig) - 1
	tested := 0
	for _, c := range alphabet {
		variant := sig[:lastIdx] + string(c)
		if variant == sig {
			continue
		}
		tested++
		if _, err := signer.Verify(path, exp, variant, now); err == nil {
			t.Errorf("Verify(non-canonical variant %q) = nil, want an error (canonical is %q)", variant, sig)
		}
	}
	if tested == 0 {
		t.Fatal("test setup produced no non-canonical variants to check")
	}
}

func TestSigner_Verify_RejectsWhitespaceAndPaddingVariants(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := "/go/redeem-reward/reward-1"
	exp, sig := signer.Sign(path, now)

	tests := []struct {
		name    string
		variant string
	}{
		{"trailing space", sig + " "},
		{"leading space", " " + sig},
		{"trailing padding", sig + "="},
		{"percent-encoded trailing space (literal %20, not decoded)", sig + "%20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := signer.Verify(path, exp, tt.variant, now); err == nil {
				t.Errorf("Verify(%q) = nil, want an error", tt.variant)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// HashSignature (NES-129 redemption-replay guard's idempotency key)
// ---------------------------------------------------------------------------

func TestHashSignature_DeterministicAndFixedLength(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	path := "/go/redeem-reward/reward-1"
	exp, sig := signer.Sign(path, now)

	decoded, err := signer.Verify(path, exp, sig, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	a := app.HashSignature(decoded)
	b := app.HashSignature(decoded)
	if a != b {
		t.Errorf("HashSignature is not deterministic: %q != %q", a, b)
	}
	// SHA-256, hex-encoded, is always 64 characters.
	if len(a) != 64 {
		t.Errorf("len(HashSignature(...)) = %d, want 64 (hex-encoded SHA-256)", len(a))
	}
}

func TestHashSignature_DistinctSignaturesHashDifferently(t *testing.T) {
	signer, err := app.NewSigner([]byte("test-key-does-not-need-32-bytes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	expA, sigA := signer.Sign("/go/redeem-reward/reward-1", now)
	decodedA, err := signer.Verify("/go/redeem-reward/reward-1", expA, sigA, now)
	if err != nil {
		t.Fatalf("Verify(A): %v", err)
	}

	expB, sigB := signer.Sign("/go/redeem-reward/reward-2", now)
	decodedB, err := signer.Verify("/go/redeem-reward/reward-2", expB, sigB, now)
	if err != nil {
		t.Fatalf("Verify(B): %v", err)
	}

	if app.HashSignature(decodedA) == app.HashSignature(decodedB) {
		t.Error("HashSignature produced the same hash for two distinct signatures")
	}
}
