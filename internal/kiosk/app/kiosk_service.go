// Package app is the kiosk bounded context's use-case boundary: issuing and
// redeeming activation codes, revoking a household's kiosk device, and
// authenticating a bearer token presented by the kiosk's cookie back to its
// device.
package app

import (
	"context"
	"errors"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
)

// KioskService issues and redeems activation codes, revokes and
// authenticates kiosk devices.
type KioskService struct {
	devices domain.KioskDeviceRepository
	codes   domain.ActivationCodeRepository
	now     func() time.Time
}

// NewKioskService constructs the service with injected repositories. now
// defaults to time.Now; a caller may inject a fixed clock for deterministic
// tests.
func NewKioskService(devices domain.KioskDeviceRepository, codes domain.ActivationCodeRepository, now func() time.Time) (*KioskService, error) {
	if devices == nil {
		return nil, errors.New("kiosk/app: NewKioskService requires a non-nil KioskDeviceRepository")
	}
	if codes == nil {
		return nil, errors.New("kiosk/app: NewKioskService requires a non-nil ActivationCodeRepository")
	}
	if now == nil {
		now = time.Now
	}
	return &KioskService{devices: devices, codes: codes, now: now}, nil
}

// CreateActivationCode issues a new short-lived, single-use activation code
// for householdID, returning the created code and the raw code (shown to the
// parent exactly once — the caller must never log or persist it; only its
// hash is stored). name labels the device this code will provision once
// redeemed (e.g. "Kitchen wall display"). The settings page never displays a
// long-lived device token: that is generated only inside Redeem.
func (s *KioskService) CreateActivationCode(ctx context.Context, householdID household.HouseholdID, name string) (*domain.ActivationCode, string, error) {
	raw, err := domain.GenerateActivationCode()
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	code := &domain.ActivationCode{
		ID:          domain.NewActivationCodeID(),
		HouseholdID: householdID,
		CodeHash:    domain.HashToken(domain.NormalizeActivationCode(raw)),
		Name:        strings.TrimSpace(name),
		ExpiresAt:   now.Add(domain.ActivationCodeTTL),
	}
	if err := code.Validate(); err != nil {
		return nil, "", err
	}
	if err := s.codes.Create(ctx, code); err != nil {
		return nil, "", err
	}
	return code, raw, nil
}

// Redeem consumes rawCode (as typed manually or carried by the one-click
// activation link) and, if it is unused and unexpired, atomically mints a new
// kiosk device: the code is marked used, the household's previously active
// device (if any) is revoked, and the new device is persisted — all in one
// transaction (domain.ActivationCodeRepository.Redeem's contract), so a
// failure at any step leaves the code unused and the previous device (if any)
// still active rather than a half-provisioned household. It returns the new
// device and its raw bearer token (to be set as the kiosk cookie — shown/used
// exactly once; only its hash is persisted).
//
// Returns domain.ErrActivationCodeNotFound, ErrActivationCodeUsed, or
// ErrActivationCodeExpired for a code that cannot be redeemed.
func (s *KioskService) Redeem(ctx context.Context, rawCode string) (*domain.KioskDevice, string, error) {
	normalized := domain.NormalizeActivationCode(rawCode)
	if normalized == "" {
		return nil, "", domain.ErrActivationCodeNotFound
	}
	rawToken, err := domain.GenerateToken()
	if err != nil {
		return nil, "", err
	}
	device := &domain.KioskDevice{
		ID:        domain.NewKioskDeviceID(),
		TokenHash: domain.HashToken(rawToken),
	}
	if err := s.codes.Redeem(ctx, domain.HashToken(normalized), s.now(), device); err != nil {
		return nil, "", err
	}
	return device, rawToken, nil
}

// Revoke invalidates householdID's device with the given id. Returns
// domain.ErrKioskDeviceNotFound when the id is unknown in that household.
func (s *KioskService) Revoke(ctx context.Context, householdID household.HouseholdID, id domain.KioskDeviceID) error {
	return s.devices.Revoke(ctx, householdID, id, s.now())
}

// Authenticate resolves a raw bearer token (as presented by the kiosk cookie)
// to its device. It returns domain.ErrKioskDeviceNotFound for an unknown
// token and domain.ErrKioskDeviceRevoked for a known-but-revoked one, so the
// caller (the auth middleware) can react identically to both by denying
// access, while still being able to log which case occurred.
func (s *KioskService) Authenticate(ctx context.Context, rawToken string) (*domain.KioskDevice, error) {
	if strings.TrimSpace(rawToken) == "" {
		return nil, domain.ErrKioskDeviceNotFound
	}
	device, err := s.devices.GetByTokenHash(ctx, domain.HashToken(rawToken))
	if err != nil {
		return nil, err
	}
	if !device.Active() {
		return nil, domain.ErrKioskDeviceRevoked
	}
	return device, nil
}

// ActiveDevice returns the household's currently active kiosk device, if any,
// for the settings page's status display.
func (s *KioskService) ActiveDevice(ctx context.Context, householdID household.HouseholdID) (*domain.KioskDevice, bool, error) {
	devices, err := s.devices.ListByHousehold(ctx, householdID)
	if err != nil {
		return nil, false, err
	}
	for _, d := range devices {
		if d.Active() {
			return d, true, nil
		}
	}
	return nil, false, nil
}

// ListByHousehold returns every device (active or revoked) the household has
// ever provisioned, newest first, for the settings page's history/audit view.
func (s *KioskService) ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.KioskDevice, error) {
	return s.devices.ListByHousehold(ctx, householdID)
}
