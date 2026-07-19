package app_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestRememberDeviceSigner_SignVerify_RoundTrips(t *testing.T) {
	t.Parallel()
	signer, err := app.NewRememberDeviceSigner([]byte("a-test-remember-device-signing-key"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner: %v", err)
	}
	memberID := household.NewMemberID()
	now := time.Now()

	token := signer.Sign(memberID, now)
	got, err := signer.Verify(token, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != memberID {
		t.Errorf("Verify returned %v, want %v", got, memberID)
	}
}

func TestRememberDeviceSigner_Verify_ExpiredRejected(t *testing.T) {
	t.Parallel()
	signer, err := app.NewRememberDeviceSigner([]byte("a-test-remember-device-signing-key"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner: %v", err)
	}
	memberID := household.NewMemberID()
	now := time.Now()
	token := signer.Sign(memberID, now)

	afterExpiry := now.Add(app.RememberDeviceTTL + time.Minute)
	if _, err := signer.Verify(token, afterExpiry); !errors.Is(err, app.ErrInvalidRememberToken) {
		t.Errorf("Verify(expired token): err = %v, want ErrInvalidRememberToken", err)
	}
}

func TestRememberDeviceSigner_Verify_TamperedRejected(t *testing.T) {
	t.Parallel()
	signer, err := app.NewRememberDeviceSigner([]byte("a-test-remember-device-signing-key"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner: %v", err)
	}
	now := time.Now()
	token := signer.Sign(household.NewMemberID(), now)

	// Flip the FIRST character of the payload segment (before the "."),
	// leaving the signature segment untouched — the signature must no
	// longer match.
	//
	// The first character is used deliberately, not the last: base64
	// encodes 6 bits per character, and when the encoded length is not a
	// multiple of 4 characters, the FINAL character's low bits are padding
	// that a non-strict decoder (base64.RawURLEncoding.DecodeString, what
	// rememberDecode uses) ignores entirely. This payload (a 36-character
	// UUID + "|" + a Unix timestamp) can land on exactly such a boundary,
	// so flipping the last character sometimes decodes to IDENTICAL
	// payload bytes — a value-preserving mutation the MAC then correctly
	// still verifies, making the test flaky rather than the signer wrong
	// (see internal/deeplink/app/signer.go's decode() for the same
	// base64-trailing-bit malleability, documented in more detail there).
	// The first character's 6 bits are always part of the first FULL byte,
	// so any flip there is guaranteed to change the decoded payload.
	dot := strings.IndexByte(token, '.')
	if dot <= 0 {
		t.Fatalf("token %q has no payload/signature separator", token)
	}
	mutated := []byte(token)
	mutated[0] = flipBase64URLChar(mutated[0])
	tampered := string(mutated)

	if _, err := signer.Verify(tampered, now); !errors.Is(err, app.ErrInvalidRememberToken) {
		t.Errorf("Verify(tampered token): err = %v, want ErrInvalidRememberToken", err)
	}
}

// TestRememberDeviceSigner_Verify_TamperedSignatureRejected covers the
// other tamperable segment: flipping a character in the SIGNATURE (after
// the ".") rather than the payload. It flips the segment's FIRST character
// for the same reason TestRememberDeviceSigner_Verify_TamperedRejected
// does for the payload's first character: those bits are always part of
// the first full byte, so the flip is guaranteed to change the decoded
// value regardless of the segment's total length — the signature's OWN
// last character has the identical base64 trailing-bit malleability the
// payload's last character does (32 bytes does not encode to a multiple of
// 4 base64url characters either), so that position would be just as
// unsafe to flip here.
func TestRememberDeviceSigner_Verify_TamperedSignatureRejected(t *testing.T) {
	t.Parallel()
	signer, err := app.NewRememberDeviceSigner([]byte("a-test-remember-device-signing-key"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner: %v", err)
	}
	now := time.Now()
	token := signer.Sign(household.NewMemberID(), now)

	dot := strings.IndexByte(token, '.')
	if dot < 0 || dot+1 >= len(token) {
		t.Fatalf("token %q has no non-empty signature segment", token)
	}
	mutated := []byte(token)
	mutated[dot+1] = flipBase64URLChar(mutated[dot+1])
	tampered := string(mutated)

	if _, err := signer.Verify(tampered, now); !errors.Is(err, app.ErrInvalidRememberToken) {
		t.Errorf("Verify(tampered signature): err = %v, want ErrInvalidRememberToken", err)
	}
}

// flipBase64URLChar returns a base64url alphabet character guaranteed to
// differ from c, for constructing a tampered-but-still-decodable token.
func flipBase64URLChar(c byte) byte {
	if c == 'A' {
		return 'B'
	}
	return 'A'
}

func TestRememberDeviceSigner_Verify_MalformedRejected(t *testing.T) {
	t.Parallel()
	signer, err := app.NewRememberDeviceSigner([]byte("a-test-remember-device-signing-key"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner: %v", err)
	}
	for _, bad := range []string{"", "not-a-token", "abc.def", "abc.def.ghi"} {
		if _, err := signer.Verify(bad, time.Now()); !errors.Is(err, app.ErrInvalidRememberToken) {
			t.Errorf("Verify(%q): err = %v, want ErrInvalidRememberToken", bad, err)
		}
	}
}

func TestRememberDeviceSigner_Verify_WrongKeyRejected(t *testing.T) {
	t.Parallel()
	signerA, err := app.NewRememberDeviceSigner([]byte("key-a-key-a-key-a-key-a"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner (A): %v", err)
	}
	signerB, err := app.NewRememberDeviceSigner([]byte("key-b-key-b-key-b-key-b"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner (B): %v", err)
	}
	now := time.Now()
	token := signerA.Sign(household.NewMemberID(), now)

	if _, err := signerB.Verify(token, now); !errors.Is(err, app.ErrInvalidRememberToken) {
		t.Errorf("Verify with a different signer's key: err = %v, want ErrInvalidRememberToken", err)
	}
}

func TestNewRememberDeviceSignerFromSecret_DerivesDistinctKeysPerPurpose(t *testing.T) {
	t.Parallel()
	secret := []byte("shared-session-secret-shared-session-secret")

	signerA, err := app.NewRememberDeviceSignerFromSecret(secret, "nestova:auth:remember-device:v1")
	if err != nil {
		t.Fatalf("NewRememberDeviceSignerFromSecret (A): %v", err)
	}
	signerB, err := app.NewRememberDeviceSignerFromSecret(secret, "a-different-purpose")
	if err != nil {
		t.Fatalf("NewRememberDeviceSignerFromSecret (B): %v", err)
	}

	now := time.Now()
	token := signerA.Sign(household.NewMemberID(), now)
	if _, err := signerB.Verify(token, now); !errors.Is(err, app.ErrInvalidRememberToken) {
		t.Error("a token signed under one purpose verified under a different purpose's derived key")
	}
}

func TestNewRememberDeviceSigner_RejectsEmptyKey(t *testing.T) {
	t.Parallel()
	if _, err := app.NewRememberDeviceSigner(nil); err == nil {
		t.Error("NewRememberDeviceSigner(nil key) must return an error")
	}
}

func TestNewRememberDeviceSignerFromSecret_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	if _, err := app.NewRememberDeviceSignerFromSecret(nil, "purpose"); err == nil {
		t.Error("NewRememberDeviceSignerFromSecret(nil secret) must return an error")
	}
	if _, err := app.NewRememberDeviceSignerFromSecret([]byte("secret"), ""); err == nil {
		t.Error("NewRememberDeviceSignerFromSecret(empty purpose) must return an error")
	}
}
