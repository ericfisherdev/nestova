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

	// Flip one character in the payload segment (before the "."), leaving
	// the signature segment untouched — the signature must no longer match.
	dot := strings.IndexByte(token, '.')
	if dot <= 0 {
		t.Fatalf("token %q has no payload/signature separator", token)
	}
	mutated := []byte(token)
	mutated[dot-1] = flipBase64URLChar(mutated[dot-1])
	tampered := string(mutated)

	if _, err := signer.Verify(tampered, now); !errors.Is(err, app.ErrInvalidRememberToken) {
		t.Errorf("Verify(tampered token): err = %v, want ErrInvalidRememberToken", err)
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
