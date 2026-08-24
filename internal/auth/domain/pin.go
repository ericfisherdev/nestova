package domain

import (
	"context"
	"errors"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// PIN domain errors (NES-165). AuthorizeTaskAction, the single
// authorization gate these back, lands unused here — wiring it into the
// complete/skip surfaces is the follow-up ticket "require a member PIN to
// complete or skip a chore" (NES-166).
var (
	// ErrPINNotEnrolled is returned by GetPINHash and ClearPIN when the
	// member has no PIN row on file. Consumers must collapse it with
	// ErrPINMismatch into one user-facing message so a submitted PIN's
	// failure never discloses whether the member is enrolled.
	ErrPINNotEnrolled = errors.New("auth: pin not enrolled")
	// ErrPINMismatch is returned when a submitted PIN does not match the
	// member's stored hash.
	ErrPINMismatch = errors.New("auth: pin mismatch")
	// ErrPINLocked is returned when the member is currently in the
	// strike-limiter's backoff window. Unlike ErrPINMismatch/
	// ErrPINNotEnrolled, this state is deliberately visible (not collapsed)
	// so an owner or adult can see and reset an active lockout.
	ErrPINLocked = errors.New("auth: pin verification locked")
	// ErrInvalidPINFormat is returned by PINService when a submitted PIN is
	// not 4-8 digits.
	ErrInvalidPINFormat = errors.New("auth: pin must be 4-8 digits")
)

// PINRepository is the outbound port for persisting a member's PIN hash.
// Implementations live in the adapter package.
//
// Error contracts:
//   - SetPIN upserts (member_id, household_id, pin_hash), replacing any
//     existing hash for memberID. Returns household.ErrMemberNotFound when
//     memberID does not belong to householdID (the composite tenant FK on
//     identity.member_pin).
//   - GetPINHash returns ErrPINNotEnrolled when the member has no PIN row.
//   - ClearPIN is scoped to householdID exactly as SetPIN is, so a member id
//     from another household can never match a row. It returns
//     ErrPINNotEnrolled when no row exists to delete WITHIN that household —
//     which is deliberately indistinguishable from "that member is not in
//     this household", so a caller cannot probe for foreign member ids.
//   - EnrolledMembers returns every member id with a PIN row scoped to
//     householdID, for the settings page's admin member list.
type PINRepository interface {
	SetPIN(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, pinHash string) error
	GetPINHash(ctx context.Context, memberID household.MemberID) (string, error)
	ClearPIN(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID) error
	EnrolledMembers(ctx context.Context, householdID household.HouseholdID) ([]household.MemberID, error)
}
