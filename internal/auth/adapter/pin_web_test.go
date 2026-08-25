package adapter

import (
	"errors"
	"testing"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
)

// TestPINVerificationMessage_CollapsesMismatchAndNotEnrolled proves
// PINVerificationMessage collapses authdomain.ErrPINMismatch and
// ErrPINNotEnrolled into the identical genericPINError message (NES-165
// AC: a submitted PIN's failure must never disclose whether the member is
// enrolled), while reporting authdomain.ErrPINLocked distinctly, since a
// lockout is deliberately visible rather than collapsed.
func TestPINVerificationMessage_CollapsesMismatchAndNotEnrolled(t *testing.T) {
	mismatchMsg, ok := PINVerificationMessage(authdomain.ErrPINMismatch)
	if !ok {
		t.Fatal("ErrPINMismatch: ok = false, want true")
	}
	notEnrolledMsg, ok := PINVerificationMessage(authdomain.ErrPINNotEnrolled)
	if !ok {
		t.Fatal("ErrPINNotEnrolled: ok = false, want true")
	}
	if mismatchMsg != notEnrolledMsg {
		t.Errorf("ErrPINMismatch message %q != ErrPINNotEnrolled message %q, want identical (must not disclose enrollment)", mismatchMsg, notEnrolledMsg)
	}
	if mismatchMsg != genericPINError {
		t.Errorf("message = %q, want genericPINError %q", mismatchMsg, genericPINError)
	}

	lockedMsg, ok := PINVerificationMessage(authdomain.ErrPINLocked)
	if !ok {
		t.Fatal("ErrPINLocked: ok = false, want true")
	}
	if lockedMsg == genericPINError {
		t.Error("ErrPINLocked must not collapse into the same message as a mismatch/not-enrolled failure — the lockout must stay visible")
	}
	if lockedMsg != genericPINLockedError {
		t.Errorf("locked message = %q, want genericPINLockedError %q", lockedMsg, genericPINLockedError)
	}
}

// TestPINVerificationMessage_UnrecognizedErrorReturnsNotOK proves an
// internal (non-sentinel) error is reported as unrecognized, so the caller
// logs it and surfaces a 500 rather than showing it to the member.
func TestPINVerificationMessage_UnrecognizedErrorReturnsNotOK(t *testing.T) {
	if _, ok := PINVerificationMessage(errors.New("boom")); ok {
		t.Error("unrecognized error: ok = true, want false")
	}
}
