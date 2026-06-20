package adapter_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericfisherdev/nestova/internal/calendar/adapter"
	"github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

// newTestPool resets+migrates the NESTOVA_TEST_DATABASE_URL database, refusing to
// run unless its name is "test" or ends with "_test" so migrate.Reset cannot wipe
// a real database.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the calendar adapter tests")
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

func seedMember(t *testing.T, pool *pgxpool.Pool, hh household.HouseholdID, name string) household.MemberID {
	t.Helper()
	id := household.NewMemberID()
	if _, err := pool.Exec(testCtx(t),
		`INSERT INTO member (id, household_id, display_name, role, color_key) VALUES ($1, $2, $3, 'owner', 'sage')`,
		id.String(), hh.String(), name); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func newAccount(id domain.CalendarAccountID, member household.MemberID, hh household.HouseholdID) *domain.CalendarAccount {
	return &domain.CalendarAccount{
		ID:              id,
		MemberID:        member,
		HouseholdID:     hh,
		Provider:        domain.ProviderGoogle,
		AccessTokenEnc:  []byte{0x01, 0x02, 0x03},
		RefreshTokenEnc: []byte{0x04, 0x05, 0x06},
		TokenExpiry:     time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		CalendarIDs:     []string{"primary", "work"},
	}
}

func TestCalendarAccountRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewCalendarAccountRepository(pool)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")

	acc := newAccount(domain.NewCalendarAccountID(), member, hh)
	if err := repo.Create(testCtx(t), acc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if acc.CreatedAt.IsZero() || acc.UpdatedAt.IsZero() {
		t.Fatal("Create did not populate timestamps")
	}

	got, err := repo.Get(testCtx(t), acc.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.MemberID != member || got.HouseholdID != hh || got.Provider != domain.ProviderGoogle {
		t.Fatalf("Get returned %+v", got)
	}
	if string(got.AccessTokenEnc) != string(acc.AccessTokenEnc) || string(got.RefreshTokenEnc) != string(acc.RefreshTokenEnc) {
		t.Fatal("encrypted token bytes did not round-trip")
	}
	if got.SyncToken != nil {
		t.Fatalf("SyncToken = %v, want nil", got.SyncToken)
	}
	if len(got.CalendarIDs) != 2 || got.CalendarIDs[0] != "primary" || got.CalendarIDs[1] != "work" {
		t.Fatalf("CalendarIDs = %v, want [primary work]", got.CalendarIDs)
	}
}

func TestGetUnknown(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewCalendarAccountRepository(pool)
	if _, err := repo.Get(testCtx(t), domain.NewCalendarAccountID()); !errors.Is(err, domain.ErrCalendarAccountNotFound) {
		t.Fatalf("Get(unknown) = %v, want ErrCalendarAccountNotFound", err)
	}
}

func TestCreateUnknownHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewCalendarAccountRepository(pool)
	acc := newAccount(domain.NewCalendarAccountID(), household.NewMemberID(), household.NewHouseholdID())
	if err := repo.Create(testCtx(t), acc); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Fatalf("Create(unknown household) = %v, want ErrHouseholdNotFound", err)
	}
}

func TestCreateUnknownMember(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewCalendarAccountRepository(pool)
	hh := seedHousehold(t, pool)
	acc := newAccount(domain.NewCalendarAccountID(), household.NewMemberID(), hh) // member not seeded
	if err := repo.Create(testCtx(t), acc); !errors.Is(err, household.ErrMemberNotFound) {
		t.Fatalf("Create(unknown member) = %v, want ErrMemberNotFound", err)
	}
}

func TestGetByMemberProvider(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewCalendarAccountRepository(pool)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	acc := newAccount(domain.NewCalendarAccountID(), member, hh)
	if err := repo.Create(testCtx(t), acc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByMemberProvider(testCtx(t), member, domain.ProviderGoogle)
	if err != nil {
		t.Fatalf("GetByMemberProvider: %v", err)
	}
	if got.ID != acc.ID {
		t.Fatalf("GetByMemberProvider id = %s, want %s", got.ID, acc.ID)
	}

	if _, err := repo.GetByMemberProvider(testCtx(t), household.NewMemberID(), domain.ProviderGoogle); !errors.Is(err, domain.ErrCalendarAccountNotFound) {
		t.Fatalf("GetByMemberProvider(unknown) = %v, want ErrCalendarAccountNotFound", err)
	}
}

func TestUpdateTokensResetsSyncToken(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewCalendarAccountRepository(pool)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	acc := newAccount(domain.NewCalendarAccountID(), member, hh)
	if err := repo.Create(testCtx(t), acc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Give it a sync token first.
	tok := "sync-123"
	if err := repo.UpdateSyncState(testCtx(t), acc.ID, []byte{0x09}, time.Now(), &tok); err != nil {
		t.Fatalf("UpdateSyncState: %v", err)
	}

	newExpiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := repo.UpdateTokens(testCtx(t), acc.ID, []byte{0xAA}, []byte{0xBB}, newExpiry, []string{"primary"}); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	got, err := repo.Get(testCtx(t), acc.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.AccessTokenEnc) != string([]byte{0xAA}) || string(got.RefreshTokenEnc) != string([]byte{0xBB}) {
		t.Fatal("UpdateTokens did not rewrite the tokens")
	}
	if got.SyncToken != nil {
		t.Fatalf("UpdateTokens did not reset sync_token, got %v", got.SyncToken)
	}
	if !got.TokenExpiry.Equal(newExpiry) {
		t.Fatalf("TokenExpiry = %s, want %s", got.TokenExpiry, newExpiry)
	}

	if err := repo.UpdateTokens(testCtx(t), domain.NewCalendarAccountID(), []byte{0x1}, []byte{0x2}, newExpiry, nil); !errors.Is(err, domain.ErrCalendarAccountNotFound) {
		t.Fatalf("UpdateTokens(unknown) = %v, want ErrCalendarAccountNotFound", err)
	}
}

func TestUpdateSyncState(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewCalendarAccountRepository(pool)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Alex")
	acc := newAccount(domain.NewCalendarAccountID(), member, hh)
	if err := repo.Create(testCtx(t), acc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok := "next-sync"
	if err := repo.UpdateSyncState(testCtx(t), acc.ID, []byte{0xCC}, time.Now().Add(time.Hour), &tok); err != nil {
		t.Fatalf("UpdateSyncState: %v", err)
	}
	got, err := repo.Get(testCtx(t), acc.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.AccessTokenEnc) != string([]byte{0xCC}) {
		t.Fatal("UpdateSyncState did not rewrite the access token")
	}
	if got.SyncToken == nil || *got.SyncToken != "next-sync" {
		t.Fatalf("SyncToken = %v, want next-sync", got.SyncToken)
	}
	// The refresh token must be untouched.
	if string(got.RefreshTokenEnc) != string(acc.RefreshTokenEnc) {
		t.Fatal("UpdateSyncState must not change the refresh token")
	}

	if err := repo.UpdateSyncState(testCtx(t), domain.NewCalendarAccountID(), []byte{0x1}, time.Now(), nil); !errors.Is(err, domain.ErrCalendarAccountNotFound) {
		t.Fatalf("UpdateSyncState(unknown) = %v, want ErrCalendarAccountNotFound", err)
	}
}

func TestListByHouseholdAndAll(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewCalendarAccountRepository(pool)
	hh1 := seedHousehold(t, pool)
	hh2 := seedHousehold(t, pool)
	m1 := seedMember(t, pool, hh1, "Alex")
	m2 := seedMember(t, pool, hh2, "Sam")
	if err := repo.Create(testCtx(t), newAccount(domain.NewCalendarAccountID(), m1, hh1)); err != nil {
		t.Fatalf("Create hh1: %v", err)
	}
	if err := repo.Create(testCtx(t), newAccount(domain.NewCalendarAccountID(), m2, hh2)); err != nil {
		t.Fatalf("Create hh2: %v", err)
	}

	h1, err := repo.ListByHousehold(testCtx(t), hh1)
	if err != nil {
		t.Fatalf("ListByHousehold: %v", err)
	}
	if len(h1) != 1 || h1[0].HouseholdID != hh1 {
		t.Fatalf("ListByHousehold(hh1) = %d rows, want 1 in hh1", len(h1))
	}

	all, err := repo.ListAll(testCtx(t))
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListAll = %d rows, want 2", len(all))
	}
}
