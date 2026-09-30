package main

import (
	"context"
	"sync"
	"time"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// fakeRememberedDeviceRepo is an in-memory authdomain.RememberedDeviceRepository
// for the login-MFA test harnesses, mirroring the real repository's contract:
// MarkUsed only succeeds for the owning member's unexpired row.
type fakeRememberedDeviceRepo struct {
	mu      sync.Mutex
	devices []*authdomain.RememberedDevice
}

func newFakeRememberedDeviceRepo() *fakeRememberedDeviceRepo { return &fakeRememberedDeviceRepo{} }

func (f *fakeRememberedDeviceRepo) Create(_ context.Context, device *authdomain.RememberedDevice) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *device
	f.devices = append(f.devices, &cp)
	return nil
}

func (f *fakeRememberedDeviceRepo) MarkUsed(_ context.Context, memberID household.MemberID, tokenHash []byte, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.devices {
		if d.MemberID == memberID && string(d.TokenHash) == string(tokenHash) && d.ExpiresAt.After(now) {
			d.LastUsedAt = now
			return nil
		}
	}
	return authdomain.ErrRememberedDeviceNotFound
}

func (f *fakeRememberedDeviceRepo) RevokeAllForMember(_ context.Context, memberID household.MemberID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.devices[:0]
	for _, d := range f.devices {
		if d.MemberID != memberID {
			kept = append(kept, d)
		}
	}
	f.devices = kept
	return nil
}
