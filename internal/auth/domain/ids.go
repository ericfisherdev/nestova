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
