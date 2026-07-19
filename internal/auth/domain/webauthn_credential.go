package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// WebAuthnCredentialID uniquely identifies one registered WebAuthn
// credential row. Distinct from the WebAuthn credential's own ID
// (WebAuthnCredential.CredentialID, an opaque handle the authenticator
// itself generates) — this is Nestova's own row identifier, used for
// rename/revoke.
type WebAuthnCredentialID uuid.UUID

// NewWebAuthnCredentialID returns a new time-ordered (UUIDv7) credential row
// id, mirroring NewRecoveryCodeID's reasoning (better B-tree index locality
// than random v4 ids).
func NewWebAuthnCredentialID() WebAuthnCredentialID {
	return WebAuthnCredentialID(uuid.Must(uuid.NewV7()))
}

// String returns the canonical UUID string.
func (id WebAuthnCredentialID) String() string { return uuid.UUID(id).String() }

// ParseWebAuthnCredentialID parses a canonical UUID string into a
// WebAuthnCredentialID.
func ParseWebAuthnCredentialID(s string) (WebAuthnCredentialID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return WebAuthnCredentialID{}, fmt.Errorf("parse webauthn credential id: %w", err)
	}
	return WebAuthnCredentialID(u), nil
}

// WebAuthn domain errors (NES-136). Login enforcement is a follow-up ticket
// (NES-137) — none of these are returned by the login flow today.
var (
	// ErrWebAuthnCredentialNotFound is returned by Rename and Delete when no
	// credential matches the given id, member, and household — including
	// when the id is valid but belongs to a DIFFERENT member or household
	// (reported identically, so neither action leaks which one occurred).
	ErrWebAuthnCredentialNotFound = errors.New("auth: webauthn credential not found")
	// ErrWebAuthnVerificationFailed is returned by
	// app.WebAuthnService.FinishRegistration when the browser's attestation
	// response fails verification against the pending challenge — a wrong
	// RP ID, a challenge mismatch, an expired challenge, or a replayed
	// (already-consumed) challenge are all reported identically, so a
	// caller cannot distinguish which one occurred (mirroring
	// ErrInvalidTOTPCode's no-oracle convention).
	ErrWebAuthnVerificationFailed = errors.New("auth: webauthn verification failed")
)

// WebAuthnCredential is one member's registered platform passkey. A member
// may register several (phone, laptop, security key); each row is
// independent — revoking one never affects the others.
type WebAuthnCredential struct {
	ID          WebAuthnCredentialID
	MemberID    household.MemberID
	HouseholdID household.HouseholdID
	// CredentialID is the WebAuthn credential id the authenticator itself
	// generated — an opaque byte handle, globally unique by construction.
	CredentialID []byte
	// PublicKey is the CBOR-encoded credential public key. Not encrypted at
	// rest: a public key is not a secret (see the member_credential
	// migration's own doc comment).
	PublicKey []byte
	// SignCount is the authenticator's signature counter as of the last
	// successful ceremony (registration sets it once; NES-137's login
	// ceremony updates it thereafter for clone detection).
	SignCount uint32
	// Transports are the authenticator's advertised transport hints (e.g.
	// "internal", "hybrid") — advisory only, never a security boundary.
	Transports []string
	// AAGUID identifies the authenticator model, when the authenticator
	// reports one; nil for a model that reports none.
	AAGUID *uuid.UUID
	// Nickname is the member-chosen label shown in the "Your devices" list.
	Nickname string
	// UserHandle is this member's stable, HMAC-derived WebAuthn user handle
	// (authapp.WebAuthnUserHandleDeriver), stored redundantly on every one
	// of the member's credential rows — see the migration's doc comment for
	// why (NES-137's usernameless login lookup needs it here, not on
	// member).
	UserHandle []byte
	CreatedAt  time.Time
	// LastUsedAt is nil until NES-137's login ceremony first uses this
	// credential.
	LastUsedAt *time.Time
}

// WebAuthnCredentialRepository is the outbound port for persisting and
// retrieving a member's registered WebAuthn credentials. Implementations
// live in the adapter package.
//
// Error contracts:
//   - ListByMember never returns ErrWebAuthnCredentialNotFound for a member
//     with no credentials — it returns an empty slice.
//   - Create returns household.ErrMemberNotFound when memberID does not
//     belong to householdID (FK violation), mirroring
//     MFARepository.BeginEnrollment's tenant guard.
//   - Rename and Delete return ErrWebAuthnCredentialNotFound when no row
//     matches id scoped to BOTH memberID and householdID — a
//     defense-in-depth tenant check identical in shape to
//     MFARepository.DeleteEnrollment's.
type WebAuthnCredentialRepository interface {
	// ListByMember returns every credential registered by memberID, oldest
	// first.
	ListByMember(ctx context.Context, memberID household.MemberID) ([]WebAuthnCredential, error)

	// Create persists a newly registered credential. The caller supplies a
	// fully populated WebAuthnCredential (ID already assigned via
	// NewWebAuthnCredentialID).
	Create(ctx context.Context, householdID household.HouseholdID, cred *WebAuthnCredential) error

	// Rename updates the nickname on the credential identified by id,
	// scoped to memberID and householdID.
	Rename(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, id WebAuthnCredentialID, nickname string) error

	// Delete removes the credential identified by id, scoped to memberID
	// and householdID — revoking it immediately (NES-136 AC).
	Delete(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, id WebAuthnCredentialID) error
}
