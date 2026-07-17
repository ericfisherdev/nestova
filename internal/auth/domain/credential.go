// Package domain contains the auth bounded context's domain model: credentials
// and the repository port for persisting them. It depends on the household
// domain for MemberID (the member is the identity anchor), but does not own
// the Member entity itself.
package domain

import (
	"context"
	"errors"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Credential pairs a MemberID with the stored argon2id password hash. It is
// the auth context's sole aggregate.
type Credential struct {
	MemberID     household.MemberID
	PasswordHash string
}

// ErrInvalidCredentials is returned by CredentialRepository when no matching
// credential is found. It is intentionally generic — callers must not
// distinguish "user not found" from "wrong password" to prevent user
// enumeration.
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

// ErrEmailAlreadyInUse is returned by SetPassword when the email is already
// assigned to a different member (the email column is unique).
var ErrEmailAlreadyInUse = errors.New("auth: email already in use")

// CredentialRepository is the outbound port for persisting and retrieving
// credentials. Implementations live in the adapter package.
//
// Error contracts:
//   - FindByEmail returns ErrInvalidCredentials when no member with that email
//     and a password_hash exists (no user enumeration).
//   - FindByMemberID returns ErrInvalidCredentials when the member has no
//     password_hash set.
//   - SetPassword returns household.ErrMemberNotFound when the member id does
//     not exist, and ErrEmailAlreadyInUse when the email belongs to another
//     member.
type CredentialRepository interface {
	// FindByEmail looks up the credential for the given email address. It
	// returns ErrInvalidCredentials when no active credential is found, so
	// callers cannot distinguish "no account" from "wrong password".
	FindByEmail(ctx context.Context, email string) (*Credential, error)

	// FindByMemberID looks up the credential for a known member id, used by
	// password re-authentication flows where the caller already has an
	// authenticated member (e.g. the household owner re-entering their own
	// password to authorize an admin action, NES-134) rather than an email.
	// Unlike FindByEmail, there is no user-enumeration concern here — the
	// caller already knows memberID exists — but the error contract mirrors
	// it for consistency: ErrInvalidCredentials when the member has no
	// password_hash set.
	FindByMemberID(ctx context.Context, memberID household.MemberID) (*Credential, error)

	// SetPassword stores (or replaces) the email and password hash on the
	// member row identified by memberID. Returns household.ErrMemberNotFound
	// when the member does not exist.
	SetPassword(ctx context.Context, memberID household.MemberID, email, passwordHash string) error
}
