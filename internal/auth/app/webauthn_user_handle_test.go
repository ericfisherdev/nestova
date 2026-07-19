package app_test

import (
	"bytes"
	"testing"

	"github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// TestWebAuthnUserHandleDeriver_Derive_IsDeterministic covers determinism
// both within one deriver instance (two calls) AND across two SEPARATE
// instances constructed from the same key — the latter is what actually
// proves the derivation is a pure function of (key, memberID) with no
// hidden per-instance state, since a stateful implementation could still
// pass a same-instance-only check while producing a different handle after
// a process restart (a fresh instance).
func TestWebAuthnUserHandleDeriver_Derive_IsDeterministic(t *testing.T) {
	t.Parallel()
	key := []byte("a-test-webauthn-user-handle-key")
	d, err := app.NewWebAuthnUserHandleDeriver(key)
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver: %v", err)
	}
	memberID := household.NewMemberID()

	handle1 := d.Derive(memberID)
	handle2 := d.Derive(memberID)

	d2, err := app.NewWebAuthnUserHandleDeriver(key)
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver (second instance): %v", err)
	}
	handle3 := d2.Derive(memberID)

	if !bytes.Equal(handle1, handle2) || !bytes.Equal(handle1, handle3) {
		t.Error("Derive returned different handles for the SAME member across two calls or across two instances built from the same key")
	}
}

func TestWebAuthnUserHandleDeriver_Derive_DistinctPerMember(t *testing.T) {
	t.Parallel()
	d, err := app.NewWebAuthnUserHandleDeriver([]byte("a-test-webauthn-user-handle-key"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver: %v", err)
	}

	handleA := d.Derive(household.NewMemberID())
	handleB := d.Derive(household.NewMemberID())
	if bytes.Equal(handleA, handleB) {
		t.Error("two DIFFERENT members derived the SAME handle")
	}
}

func TestWebAuthnUserHandleDeriver_Derive_WithinSpecLength(t *testing.T) {
	// WebAuthnID's own doc: "an opaque byte sequence with a maximum size of
	// 64 bytes".
	t.Parallel()
	d, err := app.NewWebAuthnUserHandleDeriver([]byte("a-test-webauthn-user-handle-key"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver: %v", err)
	}
	handle := d.Derive(household.NewMemberID())
	if len(handle) == 0 || len(handle) > 64 {
		t.Errorf("Derive returned a %d-byte handle, want 1-64 bytes", len(handle))
	}
}

func TestWebAuthnUserHandleDeriver_Derive_DistinctAcrossKeys(t *testing.T) {
	t.Parallel()
	d1, err := app.NewWebAuthnUserHandleDeriver([]byte("key-one-key-one-key-one"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver (1): %v", err)
	}
	d2, err := app.NewWebAuthnUserHandleDeriver([]byte("key-two-key-two-key-two"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver (2): %v", err)
	}
	memberID := household.NewMemberID()

	if bytes.Equal(d1.Derive(memberID), d2.Derive(memberID)) {
		t.Error("the same member derived the same handle under two DIFFERENT keys")
	}
}

func TestNewWebAuthnUserHandleDeriver_RejectsEmptyKey(t *testing.T) {
	t.Parallel()
	if _, err := app.NewWebAuthnUserHandleDeriver(nil); err == nil {
		t.Error("NewWebAuthnUserHandleDeriver(nil key) must return an error")
	}
	if _, err := app.NewWebAuthnUserHandleDeriver([]byte{}); err == nil {
		t.Error("NewWebAuthnUserHandleDeriver(empty, non-nil key) must return an error")
	}
}

func TestNewWebAuthnUserHandleDeriverFromSecret_DerivesDistinctKeysPerPurpose(t *testing.T) {
	t.Parallel()
	secret := []byte("shared-session-secret-shared-session-secret")

	d1, err := app.NewWebAuthnUserHandleDeriverFromSecret(secret, "nestova:auth:webauthn-user-handle:v1")
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriverFromSecret (1): %v", err)
	}
	d2, err := app.NewWebAuthnUserHandleDeriverFromSecret(secret, "a-different-purpose")
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriverFromSecret (2): %v", err)
	}
	memberID := household.NewMemberID()

	if bytes.Equal(d1.Derive(memberID), d2.Derive(memberID)) {
		t.Error("a handle derived under one purpose matched the SAME derivation under a different purpose")
	}
}

func TestNewWebAuthnUserHandleDeriverFromSecret_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	if _, err := app.NewWebAuthnUserHandleDeriverFromSecret(nil, "purpose"); err == nil {
		t.Error("NewWebAuthnUserHandleDeriverFromSecret(nil secret) must return an error")
	}
	if _, err := app.NewWebAuthnUserHandleDeriverFromSecret([]byte{}, "purpose"); err == nil {
		t.Error("NewWebAuthnUserHandleDeriverFromSecret(empty, non-nil secret) must return an error")
	}
	if _, err := app.NewWebAuthnUserHandleDeriverFromSecret([]byte("secret"), ""); err == nil {
		t.Error("NewWebAuthnUserHandleDeriverFromSecret(empty purpose) must return an error")
	}
}
