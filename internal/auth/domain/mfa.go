package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// MFA domain errors (NES-134). Login enforcement is a follow-up ticket
// (NES-135) — none of these are returned by the login flow today.
var (
	// ErrMFAAlreadyEnrolled is returned by BeginEnrollment and
	// ConfirmEnrollment when the member already has a CONFIRMED TOTP
	// enrollment: it must be disabled (owner reset) or disenrolled
	// (self-service) before a new one can be confirmed. An UNCONFIRMED
	// enrollment does NOT trigger this — re-enrolling before confirming
	// simply replaces it.
	ErrMFAAlreadyEnrolled = errors.New("auth: mfa already enrolled")
	// ErrMFANotEnrolled is returned by ConfirmEnrollment, Disenroll,
	// RegenerateRecoveryCodes, and ResetMemberMFA when the member has no
	// enrollment (confirmed or not) on file.
	ErrMFANotEnrolled = errors.New("auth: mfa not enrolled")
	// ErrInvalidTOTPCode is returned when a submitted TOTP code does not
	// validate against the member's stored secret.
	ErrInvalidTOTPCode = errors.New("auth: invalid totp code")
	// ErrRecoveryCodeInvalid is returned when a submitted recovery code does
	// not match any unused code on file for the member.
	ErrRecoveryCodeInvalid = errors.New("auth: invalid recovery code")
	// ErrMFAVerificationRequired is returned by Disenroll when neither a TOTP
	// code nor a recovery code was submitted.
	ErrMFAVerificationRequired = errors.New("auth: a current totp code or recovery code is required")
	// ErrOwnerReauthRequired is returned by ResetMemberMFA when the acting
	// owner's supplied password does not match their stored credential.
	ErrOwnerReauthRequired = errors.New("auth: owner re-authentication required")
	// ErrNotHouseholdOwner is returned by ResetMemberMFA when the acting
	// member is not the household owner (adults may be parents but cannot
	// reset another member's MFA — only the owner specifically can).
	ErrNotHouseholdOwner = errors.New("auth: action requires the household owner")
	// ErrInvalidMFAEnrollment is returned by MFAEnrollment.Validate for a
	// malformed enrollment.
	ErrInvalidMFAEnrollment = errors.New("auth: invalid mfa enrollment")
)

// MFAEnrollment is a member's TOTP enrollment — at most one per member. The
// secret is stored encrypted at rest (TOTPSecretEnc); it is never persisted
// or logged in plaintext. ConfirmedAt is nil until the member proves control
// of their authenticator app by submitting one valid code back
// (MFARepository.ConfirmEnrollment); an unconfirmed enrollment is inert
// (ignored by every check in this package that requires an active
// enrollment, and — per NES-135 — will be ignored by login too).
type MFAEnrollment struct {
	MemberID      household.MemberID
	HouseholdID   household.HouseholdID
	TOTPSecretEnc []byte
	ConfirmedAt   *time.Time
	// LastTOTPStep is the RFC 6238 step of the most recently accepted LOGIN
	// TOTP code (NES-135), or nil when the member has never completed login
	// MFA verification. It is the durable replay guard MFARepository's
	// RecordLoginStep maintains — see that method's doc.
	LastTOTPStep *int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Confirmed reports whether e represents an active enrollment — i.e. the
// member has proven control of their authenticator app.
func (e *MFAEnrollment) Confirmed() bool {
	return e != nil && e.ConfirmedAt != nil
}

// Validate reports whether the enrollment is well-formed, wrapping
// ErrInvalidMFAEnrollment.
func (e *MFAEnrollment) Validate() error {
	if e.MemberID == (household.MemberID{}) {
		return fmt.Errorf("%w: member id is required", ErrInvalidMFAEnrollment)
	}
	if e.HouseholdID == (household.HouseholdID{}) {
		return fmt.Errorf("%w: household id is required", ErrInvalidMFAEnrollment)
	}
	if len(e.TOTPSecretEnc) == 0 {
		return fmt.Errorf("%w: totp secret is required", ErrInvalidMFAEnrollment)
	}
	return nil
}

// RecoveryCode is one single-use recovery code. Only CodeHash (an argon2id
// PHC string, the same format and KDF as member passwords) is ever
// persisted; the raw code is shown to the member exactly once, at generation
// time, and never stored.
type RecoveryCode struct {
	ID        RecoveryCodeID
	MemberID  household.MemberID
	CodeHash  string
	UsedAt    *time.Time
	CreatedAt time.Time
}

// Used reports whether the code has already been consumed.
func (c RecoveryCode) Used() bool {
	return c.UsedAt != nil
}

// MFARepository is the outbound port for persisting a member's TOTP
// enrollment and recovery codes. Implementations live in the adapter
// package.
//
// Error contracts:
//   - GetEnrollment returns ErrMFANotEnrolled when the member has no
//     enrollment row (confirmed or not).
//   - BeginEnrollment upserts an UNCONFIRMED enrollment for memberID,
//     replacing any existing unconfirmed row in place. It returns
//     ErrMFAAlreadyEnrolled when the existing row is already CONFIRMED (the
//     caller must disable/disenroll first), and household.ErrMemberNotFound
//     both when memberID does not belong to householdID (no such row exists
//     yet) AND when an existing row belongs to a DIFFERENT household than
//     householdID (a tenant-consistency guard: implementations must never
//     touch another household's row, and must report both cases identically
//     so neither leaks which one occurred).
//   - ConfirmEnrollmentWithCodes atomically, in a single transaction, sets
//     confirmed_at = now on the member's existing unconfirmed row AND
//     replaces their recovery codes with one fresh row per hash — the two
//     writes MUST be atomic (not two separate calls) so that two concurrent
//     callers racing to confirm the SAME still-unconfirmed enrollment can
//     never both "win": the loser's hashes are never persisted, and it
//     receives ErrMFAAlreadyEnrolled rather than silently returning raw
//     codes to its caller that were never actually stored. Returns
//     ErrMFANotEnrolled when no row exists, and ErrMFAAlreadyEnrolled when
//     the row is already confirmed (including by a racing, now-committed,
//     concurrent call to this same method).
//   - DeleteEnrollment removes the member's enrollment (confirmed or not),
//     cascading its recovery codes, scoped to householdID as a
//     defense-in-depth tenant check (used by both self-service disenroll,
//     where householdID is always the caller's own, and the owner admin
//     reset, where it is the target member's household). Returns
//     ErrMFANotEnrolled when no row exists in that household.
//   - ReplaceRecoveryCodes atomically deletes every existing recovery code
//     for memberID and inserts one fresh row per hash, in a single
//     transaction (delete-then-insert), so a failure leaves the previous
//     set intact rather than a partially-regenerated one. Used only for
//     regenerating an ALREADY-confirmed enrollment's codes (see
//     ConfirmEnrollmentWithCodes for the first-confirmation case, which has
//     a race ReplaceRecoveryCodes alone does not close).
//   - ListUnusedRecoveryCodes returns every not-yet-used recovery code for
//     memberID (never used ones), for verifying a submitted code against.
//   - MarkRecoveryCodeUsed sets used_at = now on the given code id. It is the
//     caller's responsibility to have already matched the code's hash.
//   - RecordLoginStep (NES-135) durably persists step as memberID's
//     last-accepted login TOTP step, IF AND ONLY IF the stored value is
//     still nil or strictly less than step — an atomic, race-safe replay
//     guard (implementations must apply this as a single conditional
//     UPDATE, not a read-then-write). Returns ErrInvalidTOTPCode when the
//     guard fails: either because step has already been used or superseded
//     (a replay, including one that lost a race to a concurrent call for a
//     later step) or because memberID has no member_mfa row — both reported
//     identically, since the caller has always already confirmed enrollment
//     via GetEnrollment before calling this.
type MFARepository interface {
	GetEnrollment(ctx context.Context, memberID household.MemberID) (*MFAEnrollment, error)
	BeginEnrollment(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, secretEnc []byte) error
	ConfirmEnrollmentWithCodes(ctx context.Context, memberID household.MemberID, recoveryCodeHashes []string) error
	DeleteEnrollment(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID) error
	ReplaceRecoveryCodes(ctx context.Context, memberID household.MemberID, hashes []string) error
	ListUnusedRecoveryCodes(ctx context.Context, memberID household.MemberID) ([]RecoveryCode, error)
	MarkRecoveryCodeUsed(ctx context.Context, codeID RecoveryCodeID) error
	RecordLoginStep(ctx context.Context, memberID household.MemberID, step int64) error
}
