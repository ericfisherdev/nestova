package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Activation code errors.
var (
	// ErrActivationCodeNotFound is returned when a code does not resolve to any
	// row (unknown or never issued).
	ErrActivationCodeNotFound = errors.New("kiosk: activation code not found")
	// ErrActivationCodeUsed is returned when a code has already been redeemed.
	ErrActivationCodeUsed = errors.New("kiosk: activation code already used")
	// ErrActivationCodeExpired is returned when a code's expiry has passed.
	ErrActivationCodeExpired = errors.New("kiosk: activation code expired")
	// ErrInvalidActivationCode is returned by ActivationCode.Validate for a
	// malformed code.
	ErrInvalidActivationCode = errors.New("kiosk: invalid activation code")
)

// ActivationCodeTTL is how long a generated activation code remains
// redeemable before it expires — long enough for a parent to walk to the
// kiosk device and type or scan it, short enough that a code that leaks via
// browser history, access logs, or a Referer header is worthless soon after.
const ActivationCodeTTL = 15 * time.Minute

// ActivationCode is a short-lived, single-use credential a parent generates
// from the settings page and redeems on the kiosk device itself to provision
// it. It exists so the settings page never has to display the long-lived
// kiosk_device bearer token: that token is generated only inside Redeem,
// after the code is validated, and is returned to the caller exactly once.
type ActivationCode struct {
	ID          ActivationCodeID
	HouseholdID household.HouseholdID
	// CodeHash is the SHA-256 hex digest of the normalized raw code (see
	// NormalizeActivationCode); the raw code is never persisted.
	CodeHash  string
	Name      string
	CreatedAt time.Time
	ExpiresAt time.Time
	// UsedAt is nil until Redeem consumes the code.
	UsedAt *time.Time
}

// Validate reports whether the code is well-formed, wrapping
// ErrInvalidActivationCode.
func (c *ActivationCode) Validate() error {
	if c.ID == (ActivationCodeID{}) {
		return fmt.Errorf("%w: id is required", ErrInvalidActivationCode)
	}
	if c.HouseholdID == (household.HouseholdID{}) {
		return fmt.Errorf("%w: household id is required", ErrInvalidActivationCode)
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: name must not be blank", ErrInvalidActivationCode)
	}
	if strings.TrimSpace(c.CodeHash) == "" {
		return fmt.Errorf("%w: code hash must not be blank", ErrInvalidActivationCode)
	}
	if c.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: expires at is required", ErrInvalidActivationCode)
	}
	return nil
}

// Usable reports whether the code can still be redeemed as of now.
func (c *ActivationCode) Usable(now time.Time) bool {
	return c.UsedAt == nil && now.Before(c.ExpiresAt)
}

// ActivationCodeRepository persists activation codes and atomically redeems
// one into a newly provisioned kiosk device.
//
// Contracts:
//   - Create inserts a code (the caller sets ID, HouseholdID, CodeHash, Name,
//     and ExpiresAt); it populates CreatedAt.
//   - Redeem atomically validates codeHash against now (the code must be
//     unused and unexpired), marks it used, revokes any currently active
//     device for the code's household, and inserts device — all within one
//     transaction, so a failure at ANY step (including the device insert)
//     rolls back the whole operation: the code stays unused and the
//     previous device (if any) stays active. The caller supplies only
//     device.ID and device.TokenHash; Redeem populates device.HouseholdID
//     (from the resolved code) and device.Name (from the code's own Name,
//     the label the parent chose at generation time) and overwrites
//     anything the caller set for those two fields, plus device.CreatedAt
//     on success.
//     Returns ErrActivationCodeNotFound for an unknown hash,
//     ErrActivationCodeUsed for an already-redeemed code, and
//     ErrActivationCodeExpired for one past its expiry.
type ActivationCodeRepository interface {
	Create(ctx context.Context, code *ActivationCode) error
	Redeem(ctx context.Context, codeHash string, now time.Time, device *KioskDevice) error
}
