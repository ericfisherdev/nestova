// Package domain models a household's kiosk device: a wall-mounted touchscreen
// that authenticates with a long-lived bearer token instead of a member
// session, so it can render read-mostly household data without exposing a
// LAN guest to a member login (NES-128).
package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Kiosk device errors.
var (
	// ErrKioskDeviceNotFound is returned when a kiosk device does not exist (or
	// belongs to another household).
	ErrKioskDeviceNotFound = errors.New("kiosk: device not found")
	// ErrInvalidKioskDevice is returned by KioskDevice.Validate for a malformed
	// device.
	ErrInvalidKioskDevice = errors.New("kiosk: invalid device")
	// ErrKioskDeviceRevoked is returned when a token resolves to a device whose
	// access has been revoked.
	ErrKioskDeviceRevoked = errors.New("kiosk: device revoked")
)

// KioskDevice is a household's provisioned kiosk: a device identity (not a
// member) authorized to read the household's low-risk data over the /kiosk/*
// routes. TokenHash is the SHA-256 hex digest of the bearer token the device
// presents via cookie (see HashToken); the raw token is never persisted.
// RevokedAt is nil while the device is active.
type KioskDevice struct {
	ID          KioskDeviceID
	HouseholdID household.HouseholdID
	TokenHash   string
	Name        string
	CreatedAt   time.Time
	RevokedAt   *time.Time
}

// Validate reports whether the device is well-formed, wrapping
// ErrInvalidKioskDevice.
func (d *KioskDevice) Validate() error {
	if d.ID == (KioskDeviceID{}) {
		return fmt.Errorf("%w: id is required", ErrInvalidKioskDevice)
	}
	if d.HouseholdID == (household.HouseholdID{}) {
		return fmt.Errorf("%w: household id is required", ErrInvalidKioskDevice)
	}
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("%w: name must not be blank", ErrInvalidKioskDevice)
	}
	if strings.TrimSpace(d.TokenHash) == "" {
		return fmt.Errorf("%w: token hash must not be blank", ErrInvalidKioskDevice)
	}
	return nil
}

// Active reports whether the device's token is still usable.
func (d *KioskDevice) Active() bool { return d.RevokedAt == nil }

// KioskDeviceRepository persists kiosk devices.
//
// Contracts:
//   - Create inserts a device (the caller sets ID, HouseholdID, TokenHash, and
//     Name); it populates CreatedAt. TokenHash collisions are astronomically
//     unlikely (256 bits of crypto/rand) but are still surfaced as an error
//     rather than silently overwritten.
//   - GetByTokenHash returns the device whose TokenHash matches, regardless of
//     revocation state, so the caller can distinguish "unknown token"
//     (ErrKioskDeviceNotFound) from "known but revoked" (checked via
//     Active()) and log/react to each differently.
//   - Revoke sets RevokedAt to now for the device within householdID, and
//     returns ErrKioskDeviceNotFound when the id is unknown in that household
//     (so a device cannot revoke another household's device) or is already
//     revoked.
//   - ListByHousehold returns the household's devices newest-first, or an
//     empty slice when none exist.
type KioskDeviceRepository interface {
	Create(ctx context.Context, device *KioskDevice) error
	GetByTokenHash(ctx context.Context, tokenHash string) (*KioskDevice, error)
	Revoke(ctx context.Context, householdID household.HouseholdID, id KioskDeviceID, revokedAt time.Time) error
	ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*KioskDevice, error)
}
