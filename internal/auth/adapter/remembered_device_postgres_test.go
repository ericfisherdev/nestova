package adapter_test

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func newTestRememberedDeviceRepo(t *testing.T) (*authadapter.RememberedDeviceRepository, *householdadapter.PostgresRepository, *pgxpool.Pool) {
	t.Helper()
	_, hhRepo, pool := newTestRepos(t)
	return authadapter.NewRememberedDeviceRepository(pool), hhRepo, pool
}

// seedEnrolledMember creates a member with a confirmed MFA enrollment, the
// only kind of member a remembered device can be issued for.
func seedEnrolledMember(t *testing.T, hhRepo *householdadapter.PostgresRepository, pool *pgxpool.Pool) household.MemberID {
	t.Helper()
	memberID := seedMember(t, hhRepo)
	member, err := hhRepo.GetMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	mfaRepo := authadapter.NewMFARepository(pool)
	if err := mfaRepo.BeginEnrollment(testCtx(t), memberID, member.HouseholdID, []byte("ciphertext")); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if err := mfaRepo.ConfirmEnrollmentWithCodes(testCtx(t), memberID, []string{"hash"}); err != nil {
		t.Fatalf("ConfirmEnrollmentWithCodes: %v", err)
	}
	return memberID
}

func newRememberedDevice(token string, owner household.MemberID, created time.Time) *authdomain.RememberedDevice {
	sum := sha256.Sum256([]byte(token))
	return &authdomain.RememberedDevice{
		ID:         authdomain.NewRememberedDeviceID(),
		MemberID:   owner,
		TokenHash:  sum[:],
		UserAgent:  "test-agent",
		CreatedAt:  created,
		ExpiresAt:  created.Add(30 * 24 * time.Hour),
		LastUsedAt: created,
	}
}

func rememberedDeviceNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func createRememberedDevice(t *testing.T, repo *authadapter.RememberedDeviceRepository, d *authdomain.RememberedDevice) {
	t.Helper()
	if err := repo.Create(testCtx(t), d); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestRememberedDevice_CreateThenMarkUsed(t *testing.T) {
	repo, hhRepo, pool := newTestRememberedDeviceRepo(t)
	memberID := seedEnrolledMember(t, hhRepo, pool)
	now := rememberedDeviceNow()
	d := newRememberedDevice("tok-a", memberID, now)
	createRememberedDevice(t, repo, d)

	if err := repo.MarkUsed(testCtx(t), memberID, d.TokenHash, now.Add(time.Hour)); err != nil {
		t.Fatalf("MarkUsed(live token): %v", err)
	}
}

func TestRememberedDevice_MarkUsed_Rejections(t *testing.T) {
	repo, hhRepo, pool := newTestRememberedDeviceRepo(t)
	memberID := seedEnrolledMember(t, hhRepo, pool)
	now := rememberedDeviceNow()
	d := newRememberedDevice("tok-a", memberID, now)
	createRememberedDevice(t, repo, d)

	tests := []struct {
		name   string
		member household.MemberID
		hash   []byte
		at     time.Time
	}{
		{"unknown token", memberID, newRememberedDevice("other", memberID, now).TokenHash, now},
		{"another member", household.NewMemberID(), d.TokenHash, now},
		{"expired", memberID, d.TokenHash, d.ExpiresAt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := repo.MarkUsed(testCtx(t), tc.member, tc.hash, tc.at)
			if !errors.Is(err, authdomain.ErrRememberedDeviceNotFound) {
				t.Errorf("MarkUsed: err = %v, want ErrRememberedDeviceNotFound", err)
			}
		})
	}
}

func TestRememberedDevice_Create_UnknownMember(t *testing.T) {
	repo, _, _ := newTestRememberedDeviceRepo(t)
	err := repo.Create(testCtx(t), newRememberedDevice("tok", household.NewMemberID(), rememberedDeviceNow()))
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("Create for an unknown member: err = %v, want ErrMFANotEnrolled", err)
	}
}

func TestRememberedDevice_Create_RequiresConfirmedEnrollmentAtWriteTime(t *testing.T) {
	repo, hhRepo, pool := newTestRememberedDeviceRepo(t)
	now := rememberedDeviceNow()

	unenrolled := seedMember(t, hhRepo)

	pending := seedMember(t, hhRepo)
	pendingMember, err := hhRepo.GetMember(testCtx(t), pending)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	if err := authadapter.NewMFARepository(pool).BeginEnrollment(testCtx(t), pending, pendingMember.HouseholdID, []byte("ciphertext")); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}

	// The login verified a code against an enrollment that an owner reset then
	// deleted: the revoke found no device, so a later Create must not survive it.
	resetMidLogin := seedEnrolledMember(t, hhRepo, pool)
	resetMember, err := hhRepo.GetMember(testCtx(t), resetMidLogin)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	if err := repo.RevokeAllForMember(testCtx(t), resetMidLogin); err != nil {
		t.Fatalf("RevokeAllForMember: %v", err)
	}
	if err := authadapter.NewMFARepository(pool).DeleteEnrollment(testCtx(t), resetMember.HouseholdID, resetMidLogin); err != nil {
		t.Fatalf("DeleteEnrollment: %v", err)
	}

	// The enrollment was replaced and confirmed after this request's clock.
	reenrolled := seedEnrolledMember(t, hhRepo, pool)

	tests := []struct {
		name    string
		member  household.MemberID
		created time.Time
	}{
		{"no enrollment", unenrolled, now},
		{"unconfirmed enrollment", pending, now},
		{"enrollment deleted after the code was verified", resetMidLogin, now},
		{"enrollment confirmed after the request began", reenrolled, now.Add(-time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := repo.Create(testCtx(t), newRememberedDevice("tok-"+tc.name, tc.member, tc.created))
			if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
				t.Errorf("Create: err = %v, want ErrMFANotEnrolled", err)
			}
			if n := countRememberedDevices(t, pool, tc.member); n != 0 {
				t.Errorf("%d rows survived a rejected Create, want 0", n)
			}
		})
	}
}

func TestRememberedDevice_DeleteEnrollmentCascadesDevices(t *testing.T) {
	repo, hhRepo, pool := newTestRememberedDeviceRepo(t)
	alice, bob := seedEnrolledMember(t, hhRepo, pool), seedEnrolledMember(t, hhRepo, pool)
	now := rememberedDeviceNow()
	createRememberedDevice(t, repo, newRememberedDevice("alice", alice, now))
	createRememberedDevice(t, repo, newRememberedDevice("bob", bob, now))
	member, err := hhRepo.GetMember(testCtx(t), alice)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}

	if err := authadapter.NewMFARepository(pool).DeleteEnrollment(testCtx(t), member.HouseholdID, alice); err != nil {
		t.Fatalf("DeleteEnrollment: %v", err)
	}
	if n := countRememberedDevices(t, pool, alice); n != 0 {
		t.Errorf("alice has %d devices after her enrollment was deleted, want 0", n)
	}
	if n := countRememberedDevices(t, pool, bob); n != 1 {
		t.Errorf("bob has %d devices, want 1 (another member's enrollment is untouched)", n)
	}
}

func TestRememberedDevice_Create_SweepsOnlyThatMembersExpiredRows(t *testing.T) {
	repo, hhRepo, pool := newTestRememberedDeviceRepo(t)
	alice, bob := seedEnrolledMember(t, hhRepo, pool), seedEnrolledMember(t, hhRepo, pool)
	now := rememberedDeviceNow()
	longAgo := now.Add(-60 * 24 * time.Hour)
	aliceExpired := newRememberedDevice("alice-old", alice, longAgo)
	bobExpired := newRememberedDevice("bob-old", bob, longAgo)
	// Create refuses a device dated before the enrollment was confirmed, so
	// plant the long-expired rows directly.
	insertRememberedDeviceRow(t, pool, aliceExpired)
	insertRememberedDeviceRow(t, pool, bobExpired)

	createRememberedDevice(t, repo, newRememberedDevice("alice-new", alice, now))

	// The sweep is observable only through the table: count rows per member.
	// MarkUsed rejects expired rows either way, so read the table directly.
	if n := countRememberedDevices(t, pool, alice); n != 1 {
		t.Errorf("alice has %d rows after Create, want 1 (her expired row swept)", n)
	}
	if n := countRememberedDevices(t, pool, bob); n != 1 {
		t.Errorf("bob has %d rows, want 1 (another member's Create must not sweep his)", n)
	}
}

func TestRememberedDevice_RevokeAllForMember_OnlyThatMember(t *testing.T) {
	repo, hhRepo, pool := newTestRememberedDeviceRepo(t)
	alice, bob := seedEnrolledMember(t, hhRepo, pool), seedEnrolledMember(t, hhRepo, pool)
	now := rememberedDeviceNow()
	a1, a2, b1 := newRememberedDevice("a1", alice, now), newRememberedDevice("a2", alice, now), newRememberedDevice("b1", bob, now)
	for _, d := range []*authdomain.RememberedDevice{a1, a2, b1} {
		createRememberedDevice(t, repo, d)
	}

	if err := repo.RevokeAllForMember(testCtx(t), alice); err != nil {
		t.Fatalf("RevokeAllForMember: %v", err)
	}
	for _, d := range []*authdomain.RememberedDevice{a1, a2} {
		if err := repo.MarkUsed(testCtx(t), alice, d.TokenHash, now); !errors.Is(err, authdomain.ErrRememberedDeviceNotFound) {
			t.Errorf("MarkUsed after revoke: err = %v, want ErrRememberedDeviceNotFound", err)
		}
	}
	if n := countRememberedDevices(t, pool, alice); n != 0 {
		t.Errorf("alice has %d rows after revoke, want 0", n)
	}
	if err := repo.MarkUsed(testCtx(t), bob, b1.TokenHash, now); err != nil {
		t.Errorf("bob's device must survive alice's revoke: %v", err)
	}
	if err := repo.RevokeAllForMember(testCtx(t), alice); err != nil {
		t.Errorf("RevokeAllForMember with nothing to revoke: %v", err)
	}
}

func insertRememberedDeviceRow(t *testing.T, pool *pgxpool.Pool, d *authdomain.RememberedDevice) {
	t.Helper()
	const q = `
		INSERT INTO remembered_device (id, member_id, token_hash, user_agent, created_at, expires_at, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	if _, err := pool.Exec(testCtx(t), q, d.ID.String(), d.MemberID.String(), d.TokenHash, d.UserAgent, d.CreatedAt, d.ExpiresAt, d.LastUsedAt); err != nil {
		t.Fatalf("insert remembered device row: %v", err)
	}
}

func countRememberedDevices(t *testing.T, pool *pgxpool.Pool, memberID household.MemberID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(testCtx(t), `SELECT count(*) FROM remembered_device WHERE member_id = $1`, memberID.String()).Scan(&n); err != nil {
		t.Fatalf("count remembered devices: %v", err)
	}
	return n
}
