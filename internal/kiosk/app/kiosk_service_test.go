package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/app"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
)

// fakeKioskDeviceRepo is an in-memory domain.KioskDeviceRepository for
// exercising KioskService without a database.
type fakeKioskDeviceRepo struct {
	byID map[domain.KioskDeviceID]*domain.KioskDevice
}

func newFakeKioskDeviceRepo() *fakeKioskDeviceRepo {
	return &fakeKioskDeviceRepo{byID: make(map[domain.KioskDeviceID]*domain.KioskDevice)}
}

func (f *fakeKioskDeviceRepo) Create(_ context.Context, device *domain.KioskDevice) error {
	device.CreatedAt = time.Now()
	cp := *device
	f.byID[device.ID] = &cp
	return nil
}

func (f *fakeKioskDeviceRepo) GetByTokenHash(_ context.Context, tokenHash string) (*domain.KioskDevice, error) {
	for _, d := range f.byID {
		if d.TokenHash == tokenHash {
			cp := *d
			return &cp, nil
		}
	}
	return nil, domain.ErrKioskDeviceNotFound
}

func (f *fakeKioskDeviceRepo) Revoke(_ context.Context, householdID household.HouseholdID, id domain.KioskDeviceID, revokedAt time.Time) error {
	d, ok := f.byID[id]
	if !ok || d.HouseholdID != householdID || d.RevokedAt != nil {
		return domain.ErrKioskDeviceNotFound
	}
	d.RevokedAt = &revokedAt
	return nil
}

func (f *fakeKioskDeviceRepo) ListByHousehold(_ context.Context, householdID household.HouseholdID) ([]*domain.KioskDevice, error) {
	var out []*domain.KioskDevice
	for _, d := range f.byID {
		if d.HouseholdID == householdID {
			cp := *d
			out = append(out, &cp)
		}
	}
	return out, nil
}

// fakeActivationCodeRepo is an in-memory domain.ActivationCodeRepository.
// Redeem mirrors the real adapter's atomic contract closely enough for unit
// tests (mark used, revoke the household's active devices, insert the new
// one) against the same in-memory device store; true rollback-on-failure
// atomicity is a Postgres-transaction property covered by the gated adapter
// test, not something a plain in-memory fake can meaningfully assert.
type fakeActivationCodeRepo struct {
	byHash  map[string]*domain.ActivationCode
	devices *fakeKioskDeviceRepo
}

func newFakeActivationCodeRepo(devices *fakeKioskDeviceRepo) *fakeActivationCodeRepo {
	return &fakeActivationCodeRepo{byHash: make(map[string]*domain.ActivationCode), devices: devices}
}

func (f *fakeActivationCodeRepo) Create(_ context.Context, code *domain.ActivationCode) error {
	code.CreatedAt = time.Now()
	cp := *code
	f.byHash[code.CodeHash] = &cp
	return nil
}

func (f *fakeActivationCodeRepo) Redeem(_ context.Context, codeHash string, now time.Time, device *domain.KioskDevice) error {
	code, ok := f.byHash[codeHash]
	if !ok {
		return domain.ErrActivationCodeNotFound
	}
	if code.UsedAt != nil {
		return domain.ErrActivationCodeUsed
	}
	if !now.Before(code.ExpiresAt) {
		return domain.ErrActivationCodeExpired
	}
	usedAt := now
	code.UsedAt = &usedAt

	for _, d := range f.devices.byID {
		if d.HouseholdID == code.HouseholdID && d.RevokedAt == nil {
			revokedAt := now
			d.RevokedAt = &revokedAt
		}
	}
	device.HouseholdID = code.HouseholdID
	device.Name = code.Name
	return f.devices.Create(context.Background(), device)
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// provisionDevice runs the full CreateActivationCode + Redeem round trip a
// real kiosk would perform, for tests that only need an already-provisioned
// device and don't care about the intermediate code.
func provisionDevice(t *testing.T, svc *app.KioskService, householdID household.HouseholdID, name string) (*domain.KioskDevice, string) {
	t.Helper()
	_, rawCode, err := svc.CreateActivationCode(context.Background(), householdID, name)
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}
	device, rawToken, err := svc.Redeem(context.Background(), rawCode)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	return device, rawToken
}

func TestNewKioskServiceRequiresRepos(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	if _, err := app.NewKioskService(nil, codes, nil); err == nil {
		t.Error("NewKioskService(nil devices, ...) should error")
	}
	if _, err := app.NewKioskService(devices, nil, nil); err == nil {
		t.Error("NewKioskService(..., nil codes, ...) should error")
	}
}

func TestKioskService_CreateActivationCodeThenRedeem(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	svc, err := app.NewKioskService(devices, codes, fixedClock(time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	householdID := household.NewHouseholdID()

	code, rawCode, err := svc.CreateActivationCode(context.Background(), householdID, "Kitchen wall display")
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}
	if rawCode == "" {
		t.Fatal("CreateActivationCode returned an empty raw code")
	}
	if code.CodeHash == rawCode {
		t.Fatal("the persisted code must store the code's hash, never the raw value")
	}

	device, rawToken, err := svc.Redeem(context.Background(), rawCode)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if rawToken == "" {
		t.Fatal("Redeem returned an empty raw device token")
	}
	if device.TokenHash == rawToken {
		t.Fatal("the persisted device must store the token's hash, never the raw value")
	}
	if device.HouseholdID != householdID {
		t.Errorf("device household = %v, want %v", device.HouseholdID, householdID)
	}
	if device.Name != "Kitchen wall display" {
		t.Errorf("device name = %q, want the activation code's own name", device.Name)
	}

	authenticated, err := svc.Authenticate(context.Background(), rawToken)
	if err != nil {
		t.Fatalf("Authenticate(rawToken): %v", err)
	}
	if authenticated.ID != device.ID {
		t.Errorf("Authenticate resolved a different device: got %s, want %s", authenticated.ID, device.ID)
	}
}

func TestKioskService_RedeemIsSingleUse(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	svc, err := app.NewKioskService(devices, codes, fixedClock(time.Now()))
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	householdID := household.NewHouseholdID()

	_, rawCode, err := svc.CreateActivationCode(context.Background(), householdID, "Kitchen")
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}
	if _, _, err := svc.Redeem(context.Background(), rawCode); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if _, _, err := svc.Redeem(context.Background(), rawCode); !errors.Is(err, domain.ErrActivationCodeUsed) {
		t.Errorf("second Redeem of the same code error = %v, want ErrActivationCodeUsed", err)
	}
}

func TestKioskService_RedeemExpiredCode(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	start := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	clock := start
	svc, err := app.NewKioskService(devices, codes, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	householdID := household.NewHouseholdID()

	_, rawCode, err := svc.CreateActivationCode(context.Background(), householdID, "Kitchen")
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}

	// Advance the clock past the code's TTL before redeeming.
	clock = start.Add(domain.ActivationCodeTTL + time.Minute)
	if _, _, err := svc.Redeem(context.Background(), rawCode); !errors.Is(err, domain.ErrActivationCodeExpired) {
		t.Errorf("Redeem after expiry error = %v, want ErrActivationCodeExpired", err)
	}
}

func TestKioskService_RedeemUnknownCode(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	svc, err := app.NewKioskService(devices, codes, nil)
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	if _, _, err := svc.Redeem(context.Background(), "NEVER-ISSUED"); !errors.Is(err, domain.ErrActivationCodeNotFound) {
		t.Errorf("Redeem(unknown) error = %v, want ErrActivationCodeNotFound", err)
	}
	if _, _, err := svc.Redeem(context.Background(), ""); !errors.Is(err, domain.ErrActivationCodeNotFound) {
		t.Errorf("Redeem(\"\") error = %v, want ErrActivationCodeNotFound", err)
	}
}

func TestKioskService_RedeemAcceptsCaseAndHyphenVariants(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	svc, err := app.NewKioskService(devices, codes, fixedClock(time.Now()))
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	householdID := household.NewHouseholdID()

	_, rawCode, err := svc.CreateActivationCode(context.Background(), householdID, "Kitchen")
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}
	typedByHand := domain.NormalizeActivationCode(rawCode) // uppercase, no hyphens, as if hand-typed
	if _, _, err := svc.Redeem(context.Background(), typedByHand); err != nil {
		t.Errorf("Redeem with a normalized (hand-typed) variant of the code: %v", err)
	}
}

func TestKioskService_RedeemRevokesPreviousActiveDevice(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	svc, err := app.NewKioskService(devices, codes, fixedClock(time.Now()))
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	householdID := household.NewHouseholdID()

	first, firstToken := provisionDevice(t, svc, householdID, "Old tablet")
	_, secondToken := provisionDevice(t, svc, householdID, "New wall display")

	if _, err := svc.Authenticate(context.Background(), firstToken); !errors.Is(err, domain.ErrKioskDeviceRevoked) {
		t.Errorf("first device token Authenticate error = %v, want ErrKioskDeviceRevoked", err)
	}
	if _, err := svc.Authenticate(context.Background(), secondToken); err != nil {
		t.Errorf("second (current) device token should still authenticate: %v", err)
	}

	stored := devices.byID[first.ID]
	if stored.RevokedAt == nil {
		t.Error("redeeming a replacement code should revoke the previous device")
	}
}

func TestKioskService_AuthenticateUnknownToken(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	svc, err := app.NewKioskService(devices, codes, nil)
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), "never-issued"); !errors.Is(err, domain.ErrKioskDeviceNotFound) {
		t.Errorf("Authenticate(unknown) error = %v, want ErrKioskDeviceNotFound", err)
	}
	if _, err := svc.Authenticate(context.Background(), ""); !errors.Is(err, domain.ErrKioskDeviceNotFound) {
		t.Errorf("Authenticate(\"\") error = %v, want ErrKioskDeviceNotFound", err)
	}
}

func TestKioskService_RevokeUnknownDeviceInHousehold(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	svc, err := app.NewKioskService(devices, codes, fixedClock(time.Now()))
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	householdA := household.NewHouseholdID()
	householdB := household.NewHouseholdID()

	device, _ := provisionDevice(t, svc, householdA, "Device A")
	// A different household must not be able to revoke another household's device.
	if err := svc.Revoke(context.Background(), householdB, device.ID); !errors.Is(err, domain.ErrKioskDeviceNotFound) {
		t.Errorf("cross-household Revoke error = %v, want ErrKioskDeviceNotFound", err)
	}
	if err := svc.Revoke(context.Background(), householdA, device.ID); err != nil {
		t.Errorf("same-household Revoke: %v", err)
	}
}

func TestKioskService_ActiveDevice(t *testing.T) {
	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	svc, err := app.NewKioskService(devices, codes, fixedClock(time.Now()))
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	householdID := household.NewHouseholdID()

	if _, ok, err := svc.ActiveDevice(context.Background(), householdID); err != nil || ok {
		t.Fatalf("ActiveDevice before provisioning: ok=%v err=%v, want ok=false", ok, err)
	}

	device, _ := provisionDevice(t, svc, householdID, "Wall display")
	active, ok, err := svc.ActiveDevice(context.Background(), householdID)
	if err != nil || !ok {
		t.Fatalf("ActiveDevice after provisioning: ok=%v err=%v, want ok=true", ok, err)
	}
	if active.ID != device.ID {
		t.Errorf("ActiveDevice returned %s, want %s", active.ID, device.ID)
	}

	if err := svc.Revoke(context.Background(), householdID, device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok, err := svc.ActiveDevice(context.Background(), householdID); err != nil || ok {
		t.Fatalf("ActiveDevice after revoke: ok=%v err=%v, want ok=false", ok, err)
	}
}
