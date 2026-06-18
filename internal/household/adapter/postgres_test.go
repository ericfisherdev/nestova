package adapter_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/household/adapter"
	"github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

// newTestRepo returns a repository backed by NESTOVA_TEST_DATABASE_URL with the
// baseline schema applied, or skips when the env var is unset (keeping the
// default test run hermetic).
func newTestRepo(t *testing.T) *adapter.PostgresRepository {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the household repository tests")
	}
	// Bound migration setup so a slow/unresponsive database fails the test
	// rather than hanging it.
	setupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := migrate.Reset(setupCtx, dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := migrate.Up(setupCtx, dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := migrate.Reset(cleanupCtx, dsn); err != nil {
			t.Logf("cleanup reset failed: %v", err)
		}
	})

	pool, err := db.New(setupCtx, config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return adapter.NewPostgresRepository(pool)
}

// testCtx returns a per-call context bounded so a slow/unresponsive database
// fails the test rather than hanging it.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func seedHousehold(t *testing.T, repo *adapter.PostgresRepository) *domain.Household {
	t.Helper()
	h := &domain.Household{ID: domain.NewHouseholdID(), Name: "The Fishers"}
	if err := repo.CreateHousehold(testCtx(t), h); err != nil {
		t.Fatalf("CreateHousehold: %v", err)
	}
	return h
}

func TestCreateAndGetHousehold(t *testing.T) {
	repo := newTestRepo(t)
	h := seedHousehold(t, repo)

	got, err := repo.GetHousehold(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("GetHousehold: %v", err)
	}
	if got.ID != h.ID || got.Name != "The Fishers" {
		t.Errorf("GetHousehold = %+v, want id %v name %q", got, h.ID, "The Fishers")
	}
	if got.CreatedAt.IsZero() {
		t.Error("GetHousehold returned zero CreatedAt")
	}
}

func TestAddListAndGetMembers(t *testing.T) {
	repo := newTestRepo(t)
	ctx := testCtx(t)
	h := seedHousehold(t, repo)

	names := []string{"Maya", "Daniel", "Ivy"}
	var used []domain.MemberColor
	var ids []domain.MemberID
	for _, name := range names {
		m := &domain.Member{
			ID:          domain.NewMemberID(),
			HouseholdID: h.ID,
			DisplayName: name,
			Role:        domain.RoleAdult,
			Color:       domain.NextColor(used),
		}
		if err := repo.AddMember(ctx, m); err != nil {
			t.Fatalf("AddMember(%s): %v", name, err)
		}
		used = append(used, m.Color)
		ids = append(ids, m.ID)
	}

	members, err := repo.ListMembers(ctx, h.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != len(names) {
		t.Fatalf("ListMembers returned %d, want %d", len(members), len(names))
	}
	// Insertion order is preserved and colors were assigned in canonical order.
	if members[0].DisplayName != "Maya" || members[0].Color != domain.ColorSage {
		t.Errorf("members[0] = (%s, %s), want (Maya, sage)", members[0].DisplayName, members[0].Color)
	}
	if members[1].DisplayName != "Daniel" || members[1].Color != domain.ColorClay {
		t.Errorf("members[1] = (%s, %s), want (Daniel, clay)", members[1].DisplayName, members[1].Color)
	}

	got, err := repo.GetMember(ctx, ids[0])
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	if got.DisplayName != "Maya" {
		t.Errorf("GetMember name = %q, want Maya", got.DisplayName)
	}
}

func TestAddMemberDuplicateName(t *testing.T) {
	repo := newTestRepo(t)
	ctx := testCtx(t)
	h := seedHousehold(t, repo)

	first := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "Maya", Role: domain.RoleAdult, Color: domain.ColorSage}
	if err := repo.AddMember(ctx, first); err != nil {
		t.Fatalf("AddMember(first): %v", err)
	}
	// Case-insensitive duplicate must be rejected.
	dup := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "maya", Role: domain.RoleChild, Color: domain.ColorClay}
	if err := repo.AddMember(ctx, dup); !errors.Is(err, domain.ErrDuplicateMember) {
		t.Errorf("AddMember(duplicate) error = %v, want ErrDuplicateMember", err)
	}
}

func TestAddMemberUnknownHousehold(t *testing.T) {
	repo := newTestRepo(t)
	m := &domain.Member{
		ID:          domain.NewMemberID(),
		HouseholdID: domain.NewHouseholdID(), // not persisted
		DisplayName: "Orphan",
		Role:        domain.RoleAdult,
		Color:       domain.ColorSage,
	}
	if err := repo.AddMember(testCtx(t), m); !errors.Is(err, domain.ErrHouseholdNotFound) {
		t.Errorf("AddMember(unknown household) error = %v, want ErrHouseholdNotFound", err)
	}
}

func TestListMembersUnknownHousehold(t *testing.T) {
	repo := newTestRepo(t)
	// ListMembers fails open: an unknown household yields an empty slice, not an
	// error (documented contract).
	got, err := repo.ListMembers(testCtx(t), domain.NewHouseholdID())
	if err != nil {
		t.Fatalf("ListMembers(unknown) error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("ListMembers(unknown) returned %d members, want 0", len(got))
	}
}

func TestNotFoundErrors(t *testing.T) {
	repo := newTestRepo(t)
	ctx := testCtx(t)

	if _, err := repo.GetHousehold(ctx, domain.NewHouseholdID()); !errors.Is(err, domain.ErrHouseholdNotFound) {
		t.Errorf("GetHousehold(unknown) error = %v, want ErrHouseholdNotFound", err)
	}
	if _, err := repo.GetMember(ctx, domain.NewMemberID()); !errors.Is(err, domain.ErrMemberNotFound) {
		t.Errorf("GetMember(unknown) error = %v, want ErrMemberNotFound", err)
	}
}
