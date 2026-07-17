package adapter_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/adapter"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

// newTestPool returns a pool against NESTOVA_TEST_DATABASE_URL with a freshly
// reset+migrated schema. It refuses to run unless the DSN's database name is
// "test" or ends with "_test" so migrate.Reset can never wipe a real database.
// Mirrors internal/media/adapter/postgres_test.go's helper of the same name.
//
// Isolation note: every test using this helper calls migrate.Reset (drop
// everything) then migrate.Up (recreate the full schema) against the SAME
// shared DSN, both on setup and again in t.Cleanup. This is safe for tests
// WITHIN this package running sequentially (Go's default `go test` behavior
// for a single package), but two DIFFERENT gated packages must never be run
// concurrently against the same database: one package's Reset can drop the
// schema out from under another package's in-flight test. Always invoke a
// gated package's tests as a single, separate `go test ./path/to/one/pkg/...`
// command — never combined with another gated package (including
// internal/platform/db/migrate's own tests, which exercise Reset/Up
// directly) in the same run, and never run two gated packages' test binaries
// at the same time against one shared NESTOVA_TEST_DATABASE_URL.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the kiosk adapter tests")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	name := strings.ToLower(cfg.ConnConfig.Database)
	if name != "test" && !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to reset database %q; name must be \"test\" or end with \"_test\"", name)
	}

	setupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := migrate.Reset(setupCtx, dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := migrate.Up(setupCtx, dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		if err := migrate.Reset(cleanupCtx, dsn); err != nil {
			t.Logf("cleanup reset failed: %v", err)
		}
	})

	poolCtx, cancelPool := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPool()
	pool, err := db.New(poolCtx, config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func seedHousehold(t *testing.T, pool *pgxpool.Pool) household.HouseholdID {
	t.Helper()
	id := household.NewHouseholdID()
	if _, err := pool.Exec(testCtx(t), `INSERT INTO household (id, name) VALUES ($1, $2)`, id.String(), "The Fishers"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	return id
}

func newDevice(hh household.HouseholdID, name, rawToken string) *domain.KioskDevice {
	return &domain.KioskDevice{
		ID: domain.NewKioskDeviceID(), HouseholdID: hh,
		TokenHash: domain.HashToken(rawToken), Name: name,
	}
}

func TestKioskDeviceRepositoryCreateAndGetByTokenHash(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewKioskDeviceRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	device := newDevice(hh, "Kitchen wall display", "raw-token-1")
	if err := repo.Create(ctx, device); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if device.CreatedAt.IsZero() {
		t.Fatal("Create did not populate CreatedAt")
	}

	got, err := repo.GetByTokenHash(ctx, domain.HashToken("raw-token-1"))
	if err != nil {
		t.Fatalf("GetByTokenHash: %v", err)
	}
	if got.ID != device.ID || got.Name != "Kitchen wall display" || !got.Active() {
		t.Fatalf("GetByTokenHash = %+v", got)
	}

	if _, err := repo.GetByTokenHash(ctx, domain.HashToken("never-issued")); !errors.Is(err, domain.ErrKioskDeviceNotFound) {
		t.Errorf("GetByTokenHash(unknown) error = %v, want ErrKioskDeviceNotFound", err)
	}
}

func TestKioskDeviceRepositoryCreateUnknownHouseholdFails(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewKioskDeviceRepository(pool)
	ctx := testCtx(t)

	device := newDevice(household.NewHouseholdID(), "Orphan device", "raw-token-2")
	if err := repo.Create(ctx, device); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Errorf("Create with unknown household error = %v, want ErrHouseholdNotFound", err)
	}
}

func TestKioskDeviceRepositoryRevoke(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewKioskDeviceRepository(pool)
	hh := seedHousehold(t, pool)
	other := seedHousehold(t, pool)
	ctx := testCtx(t)

	device := newDevice(hh, "Wall display", "raw-token-3")
	if err := repo.Create(ctx, device); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A different household cannot revoke this device.
	if err := repo.Revoke(ctx, other, device.ID, time.Now()); !errors.Is(err, domain.ErrKioskDeviceNotFound) {
		t.Errorf("cross-household Revoke error = %v, want ErrKioskDeviceNotFound", err)
	}

	revokedAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.Revoke(ctx, hh, device.ID, revokedAt); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	got, err := repo.GetByTokenHash(ctx, domain.HashToken("raw-token-3"))
	if err != nil {
		t.Fatalf("GetByTokenHash after revoke: %v", err)
	}
	if got.Active() {
		t.Fatal("device should not be active after Revoke")
	}
	if got.RevokedAt == nil || !got.RevokedAt.Equal(revokedAt) {
		t.Errorf("RevokedAt = %v, want %v", got.RevokedAt, revokedAt)
	}

	// Revoking an already-revoked (or unknown) id reports not-found.
	if err := repo.Revoke(ctx, hh, device.ID, time.Now()); !errors.Is(err, domain.ErrKioskDeviceNotFound) {
		t.Errorf("re-revoke error = %v, want ErrKioskDeviceNotFound", err)
	}
}

func TestKioskDeviceRepositoryListByHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewKioskDeviceRepository(pool)
	hh := seedHousehold(t, pool)
	other := seedHousehold(t, pool)
	ctx := testCtx(t)

	if devices, err := repo.ListByHousehold(ctx, hh); err != nil || len(devices) != 0 {
		t.Fatalf("ListByHousehold on an empty household = (%v, %v), want empty slice", devices, err)
	}

	first := newDevice(hh, "Old tablet", "raw-token-4")
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // ensure a distinct created_at ordering
	second := newDevice(hh, "New wall display", "raw-token-5")
	if err := repo.Create(ctx, second); err != nil {
		t.Fatalf("Create second: %v", err)
	}
	inOther := newDevice(other, "Someone else's kiosk", "raw-token-6")
	if err := repo.Create(ctx, inOther); err != nil {
		t.Fatalf("Create in other household: %v", err)
	}

	devices, err := repo.ListByHousehold(ctx, hh)
	if err != nil {
		t.Fatalf("ListByHousehold: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("ListByHousehold returned %d devices, want 2", len(devices))
	}
	// Newest first.
	if devices[0].ID != second.ID || devices[1].ID != first.ID {
		t.Errorf("ListByHousehold order = [%s, %s], want [%s, %s] (newest first)",
			devices[0].ID, devices[1].ID, second.ID, first.ID)
	}
}

// ---------------------------------------------------------------------------
// ActivationCodeRepository
// ---------------------------------------------------------------------------

func newActivationCode(hh household.HouseholdID, name, rawCode string, expiresAt time.Time) *domain.ActivationCode {
	return &domain.ActivationCode{
		ID: domain.NewActivationCodeID(), HouseholdID: hh,
		CodeHash: domain.HashToken(domain.NormalizeActivationCode(rawCode)), Name: name, ExpiresAt: expiresAt,
	}
}

func TestActivationCodeRepositoryRedeemHappyPath(t *testing.T) {
	pool := newTestPool(t)
	devices := adapter.NewKioskDeviceRepository(pool)
	codes := adapter.NewActivationCodeRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	code := newActivationCode(hh, "Kitchen wall display", "raw-code-1", time.Now().Add(15*time.Minute))
	if err := codes.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if code.CreatedAt.IsZero() {
		t.Fatal("Create did not populate CreatedAt")
	}

	device := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), TokenHash: domain.HashToken("raw-device-token-1")}
	if err := codes.Redeem(ctx, code.CodeHash, time.Now(), device); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if device.HouseholdID != hh {
		t.Errorf("Redeem did not populate device.HouseholdID: got %v, want %v", device.HouseholdID, hh)
	}
	if device.Name != "Kitchen wall display" {
		t.Errorf("Redeem did not populate device.Name from the code: got %q", device.Name)
	}
	if device.CreatedAt.IsZero() {
		t.Error("Redeem did not populate device.CreatedAt")
	}

	got, err := devices.GetByTokenHash(ctx, domain.HashToken("raw-device-token-1"))
	if err != nil {
		t.Fatalf("GetByTokenHash after redeem: %v", err)
	}
	if !got.Active() {
		t.Error("redeemed device should be active")
	}
}

func TestActivationCodeRepositoryRedeemUnknownCode(t *testing.T) {
	pool := newTestPool(t)
	codes := adapter.NewActivationCodeRepository(pool)
	ctx := testCtx(t)

	device := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), TokenHash: domain.HashToken("raw-device-token-2")}
	if err := codes.Redeem(ctx, domain.HashToken("never-issued"), time.Now(), device); !errors.Is(err, domain.ErrActivationCodeNotFound) {
		t.Errorf("Redeem(unknown) error = %v, want ErrActivationCodeNotFound", err)
	}
}

// TestActivationCodeRepositoryRedeemIsSingleUse is the gated regression test
// for a second redemption of the same code: it must be rejected, never
// silently minting a second device.
func TestActivationCodeRepositoryRedeemIsSingleUse(t *testing.T) {
	pool := newTestPool(t)
	codes := adapter.NewActivationCodeRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	code := newActivationCode(hh, "Kitchen", "raw-code-3", time.Now().Add(15*time.Minute))
	if err := codes.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), TokenHash: domain.HashToken("raw-device-token-3a")}
	if err := codes.Redeem(ctx, code.CodeHash, time.Now(), first); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}

	second := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), TokenHash: domain.HashToken("raw-device-token-3b")}
	if err := codes.Redeem(ctx, code.CodeHash, time.Now(), second); !errors.Is(err, domain.ErrActivationCodeUsed) {
		t.Errorf("second Redeem error = %v, want ErrActivationCodeUsed", err)
	}
}

// TestActivationCodeRepositoryRedeemExpiredCode is the gated regression test
// for a code redeemed after its expiry.
func TestActivationCodeRepositoryRedeemExpiredCode(t *testing.T) {
	pool := newTestPool(t)
	codes := adapter.NewActivationCodeRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	code := newActivationCode(hh, "Kitchen", "raw-code-4", time.Now().Add(-time.Minute)) // already expired
	if err := codes.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}

	device := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), TokenHash: domain.HashToken("raw-device-token-4")}
	if err := codes.Redeem(ctx, code.CodeHash, time.Now(), device); !errors.Is(err, domain.ErrActivationCodeExpired) {
		t.Errorf("Redeem(expired) error = %v, want ErrActivationCodeExpired", err)
	}
}

func TestActivationCodeRepositoryRedeemRevokesPreviousActiveDevice(t *testing.T) {
	pool := newTestPool(t)
	deviceRepo := adapter.NewKioskDeviceRepository(pool)
	codes := adapter.NewActivationCodeRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	previous := newDevice(hh, "Old tablet", "raw-device-token-5a")
	if err := deviceRepo.Create(ctx, previous); err != nil {
		t.Fatalf("seed previous device: %v", err)
	}

	code := newActivationCode(hh, "New wall display", "raw-code-5", time.Now().Add(15*time.Minute))
	if err := codes.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}
	newDeviceRow := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), TokenHash: domain.HashToken("raw-device-token-5b")}
	if err := codes.Redeem(ctx, code.CodeHash, time.Now(), newDeviceRow); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	got, err := deviceRepo.GetByTokenHash(ctx, domain.HashToken("raw-device-token-5a"))
	if err != nil {
		t.Fatalf("GetByTokenHash previous device: %v", err)
	}
	if got.Active() {
		t.Error("the previous device should have been revoked by Redeem")
	}
}

// TestActivationCodeRepositoryRedeemInsertFailureLeavesPreviousStateIntact is
// the atomicity regression test: when the device insert step fails (forced
// here by a primary-key collision with an already-existing device), the
// WHOLE transaction must roll back — the code must remain unused and the
// previously active device must remain active, never a half-provisioned
// household.
func TestActivationCodeRepositoryRedeemInsertFailureLeavesPreviousStateIntact(t *testing.T) {
	pool := newTestPool(t)
	deviceRepo := adapter.NewKioskDeviceRepository(pool)
	codes := adapter.NewActivationCodeRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	previous := newDevice(hh, "Old tablet", "raw-device-token-6a")
	if err := deviceRepo.Create(ctx, previous); err != nil {
		t.Fatalf("seed previous device: %v", err)
	}

	code := newActivationCode(hh, "New wall display", "raw-code-6", time.Now().Add(15*time.Minute))
	if err := codes.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Force the insert step to fail: reusing 'previous's own ID collides with
	// kiosk_device's primary key.
	colliding := &domain.KioskDevice{ID: previous.ID, TokenHash: domain.HashToken("raw-device-token-6b")}
	if err := codes.Redeem(ctx, code.CodeHash, time.Now(), colliding); err == nil {
		t.Fatal("Redeem with a colliding device id should fail")
	}

	// The previous device must still be active — the revoke was rolled back.
	got, err := deviceRepo.GetByTokenHash(ctx, domain.HashToken("raw-device-token-6a"))
	if err != nil {
		t.Fatalf("GetByTokenHash previous device: %v", err)
	}
	if !got.Active() {
		t.Error("a failed Redeem must not leave the previous device revoked")
	}

	// The code must still be unused — a fresh device can redeem it.
	retry := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), TokenHash: domain.HashToken("raw-device-token-6c")}
	if err := codes.Redeem(ctx, code.CodeHash, time.Now(), retry); err != nil {
		t.Errorf("a failed Redeem must not consume the code; retry failed: %v", err)
	}
}
