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
	repo, hhRepo, _ := newTestRememberedDeviceRepo(t)
	memberID := seedMember(t, hhRepo)
	now := rememberedDeviceNow()
	d := newRememberedDevice("tok-a", memberID, now)
	createRememberedDevice(t, repo, d)

	if err := repo.MarkUsed(testCtx(t), memberID, d.TokenHash, now.Add(time.Hour)); err != nil {
		t.Fatalf("MarkUsed(live token): %v", err)
	}
}

func TestRememberedDevice_MarkUsed_Rejections(t *testing.T) {
	repo, hhRepo, _ := newTestRememberedDeviceRepo(t)
	memberID := seedMember(t, hhRepo)
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
	if !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("Create for an unknown member: err = %v, want ErrMemberNotFound", err)
	}
}

func TestRememberedDevice_Create_SweepsOnlyThatMembersExpiredRows(t *testing.T) {
	repo, hhRepo, pool := newTestRememberedDeviceRepo(t)
	alice, bob := seedMember(t, hhRepo), seedMember(t, hhRepo)
	now := rememberedDeviceNow()
	longAgo := now.Add(-60 * 24 * time.Hour)
	aliceExpired := newRememberedDevice("alice-old", alice, longAgo)
	bobExpired := newRememberedDevice("bob-old", bob, longAgo)
	createRememberedDevice(t, repo, aliceExpired)
	createRememberedDevice(t, repo, bobExpired)

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
	alice, bob := seedMember(t, hhRepo), seedMember(t, hhRepo)
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

func countRememberedDevices(t *testing.T, pool *pgxpool.Pool, memberID household.MemberID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(testCtx(t), `SELECT count(*) FROM remembered_device WHERE member_id = $1`, memberID.String()).Scan(&n); err != nil {
		t.Fatalf("count remembered devices: %v", err)
	}
	return n
}
