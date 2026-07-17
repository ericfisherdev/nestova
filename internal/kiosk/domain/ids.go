package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// KioskDeviceID uniquely identifies a kiosk device.
type KioskDeviceID uuid.UUID

// NewKioskDeviceID returns a new time-ordered (UUIDv7) kiosk device id, which
// gives better B-tree index locality than random v4 ids. uuid.NewV7 only errors
// if the crypto random source is unavailable, the same failure under which
// uuid.New panics, so Must is appropriate here.
func NewKioskDeviceID() KioskDeviceID { return KioskDeviceID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id KioskDeviceID) String() string { return uuid.UUID(id).String() }

// ParseKioskDeviceID parses a canonical UUID string into a KioskDeviceID.
func ParseKioskDeviceID(s string) (KioskDeviceID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return KioskDeviceID{}, fmt.Errorf("parse kiosk device id: %w", err)
	}
	return KioskDeviceID(u), nil
}

// ActivationCodeID uniquely identifies a kiosk activation code.
type ActivationCodeID uuid.UUID

// NewActivationCodeID returns a new time-ordered (UUIDv7) activation code id.
// See NewKioskDeviceID for why uuid.Must is appropriate here.
func NewActivationCodeID() ActivationCodeID { return ActivationCodeID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id ActivationCodeID) String() string { return uuid.UUID(id).String() }

// ParseActivationCodeID parses a canonical UUID string into an ActivationCodeID.
func ParseActivationCodeID(s string) (ActivationCodeID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ActivationCodeID{}, fmt.Errorf("parse activation code id: %w", err)
	}
	return ActivationCodeID(u), nil
}
