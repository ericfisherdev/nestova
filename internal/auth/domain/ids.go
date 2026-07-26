package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// RecoveryCodeID uniquely identifies one MFA recovery code.
type RecoveryCodeID uuid.UUID

// NewRecoveryCodeID returns a new time-ordered (UUIDv7) recovery code id,
// which gives better B-tree index locality than random v4 ids. uuid.NewV7
// only errors if the crypto random source is unavailable — the same failure
// under which uuid.New itself panics — so Must is appropriate here.
func NewRecoveryCodeID() RecoveryCodeID { return RecoveryCodeID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id RecoveryCodeID) String() string { return uuid.UUID(id).String() }

// ParseRecoveryCodeID parses a canonical UUID string into a RecoveryCodeID.
func ParseRecoveryCodeID(s string) (RecoveryCodeID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return RecoveryCodeID{}, fmt.Errorf("parse recovery code id: %w", err)
	}
	return RecoveryCodeID(u), nil
}

// AuthorizationCodeID uniquely identifies a federation authorization code
// (NSTR-105/NSTR-110).
type AuthorizationCodeID uuid.UUID

// NewAuthorizationCodeID returns a new time-ordered (UUIDv7) authorization
// code id, which gives better B-tree index locality than random v4 ids.
// uuid.NewV7 only errors if the crypto random source is unavailable — the
// same failure under which uuid.New itself panics — so Must is appropriate
// here.
func NewAuthorizationCodeID() AuthorizationCodeID {
	return AuthorizationCodeID(uuid.Must(uuid.NewV7()))
}

// String returns the canonical UUID string.
func (id AuthorizationCodeID) String() string { return uuid.UUID(id).String() }

// ParseAuthorizationCodeID parses a canonical UUID string into an
// AuthorizationCodeID.
func ParseAuthorizationCodeID(s string) (AuthorizationCodeID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return AuthorizationCodeID{}, fmt.Errorf("parse authorization code id: %w", err)
	}
	return AuthorizationCodeID(u), nil
}
