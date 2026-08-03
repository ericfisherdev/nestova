package adapter_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/household/adapter"
	"github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db/dbtest"
)

// newTestRepo returns a repository over this package's own derived
// database (NES-149), freshly reset and migrated. dbtest.NewIsolatedPool
// owns the safety rail, the on-demand CREATE DATABASE, and the
// reset/migrate lifecycle.
func newTestRepo(t *testing.T) *adapter.PostgresRepository {
	t.Helper()
	return adapter.NewPostgresRepository(dbtest.NewIsolatedPool(t, "household"))
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
		if err := repo.AddMember(testCtx(t), m); err != nil {
			t.Fatalf("AddMember(%s): %v", name, err)
		}
		used = append(used, m.Color)
		ids = append(ids, m.ID)
	}

	members, err := repo.ListMembers(testCtx(t), h.ID)
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

	got, err := repo.GetMember(testCtx(t), ids[0])
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	if got.DisplayName != "Maya" {
		t.Errorf("GetMember name = %q, want Maya", got.DisplayName)
	}
}

func TestAddMemberDuplicateName(t *testing.T) {
	repo := newTestRepo(t)
	h := seedHousehold(t, repo)

	first := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "Maya", Role: domain.RoleAdult, Color: domain.ColorSage}
	if err := repo.AddMember(testCtx(t), first); err != nil {
		t.Fatalf("AddMember(first): %v", err)
	}
	// Case-insensitive duplicate must be rejected.
	dup := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "maya", Role: domain.RoleChild, Color: domain.ColorClay}
	if err := repo.AddMember(testCtx(t), dup); !errors.Is(err, domain.ErrDuplicateMember) {
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

	if _, err := repo.GetHousehold(testCtx(t), domain.NewHouseholdID()); !errors.Is(err, domain.ErrHouseholdNotFound) {
		t.Errorf("GetHousehold(unknown) error = %v, want ErrHouseholdNotFound", err)
	}
	if _, err := repo.GetMember(testCtx(t), domain.NewMemberID()); !errors.Is(err, domain.ErrMemberNotFound) {
		t.Errorf("GetMember(unknown) error = %v, want ErrMemberNotFound", err)
	}
}

// TestHasAnyHousehold verifies the first-run guard: false on an empty schema,
// true after CreateHousehold.
func TestHasAnyHousehold(t *testing.T) {
	repo := newTestRepo(t)

	// After migrate.Reset + migrate.Up the schema is clean; no households exist.
	got, err := repo.HasAnyHousehold(testCtx(t))
	if err != nil {
		t.Fatalf("HasAnyHousehold (empty): %v", err)
	}
	if got {
		t.Error("HasAnyHousehold (empty) = true, want false")
	}

	// Insert one household; the guard must now report true.
	seedHousehold(t, repo)

	got, err = repo.HasAnyHousehold(testCtx(t))
	if err != nil {
		t.Fatalf("HasAnyHousehold (after create): %v", err)
	}
	if !got {
		t.Error("HasAnyHousehold (after create) = false, want true")
	}
}

// TestGetMember_NoProfileRow_DefaultsColor verifies the fallback documented
// on PostgresRepository (NSTR-115): a member visible through identity.member
// with no nestova.member_profile row still renders with a valid palette
// color rather than an empty one. AddMember always writes both rows
// atomically, so the profile row is deleted out of band here to exercise
// the LEFT JOIN's COALESCE against the profile-less case directly.
func TestGetMember_NoProfileRow_DefaultsColor(t *testing.T) {
	pool := dbtest.NewIsolatedPool(t, "household")
	repo := adapter.NewPostgresRepository(pool)
	h := seedHousehold(t, repo)

	m := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "Profileless", Role: domain.RoleAdult, Color: domain.ColorPlum}
	if err := repo.AddMember(testCtx(t), m); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if _, err := pool.Exec(testCtx(t), "DELETE FROM member_profile WHERE member_id = $1", m.ID.String()); err != nil {
		t.Fatalf("delete profile row: %v", err)
	}

	got, err := repo.GetMember(testCtx(t), m.ID)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	if got.Color != domain.ColorSage {
		t.Errorf("GetMember color = %v, want the default %v (no profile row)", got.Color, domain.ColorSage)
	}
}

// TestEnsureMemberProfile_CreatesRowViaNextColor is the NSTR-117
// create-if-missing proof: a member visible through identity.member (as it
// would be for a member provisioned by Nestorage, arriving here for the
// first time over a shared session) but with no nestova.member_profile row
// gets one created via NextColor against colors already used in the
// household, not merely a display-time default.
func TestEnsureMemberProfile_CreatesRowViaNextColor(t *testing.T) {
	pool := dbtest.NewIsolatedPool(t, "household")
	repo := adapter.NewPostgresRepository(pool)
	h := seedHousehold(t, repo)

	// Two members already have profile rows using sage and clay.
	first := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "Maya", Role: domain.RoleAdult, Color: domain.ColorSage}
	if err := repo.AddMember(testCtx(t), first); err != nil {
		t.Fatalf("AddMember(first): %v", err)
	}
	second := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "Daniel", Role: domain.RoleAdult, Color: domain.ColorClay}
	if err := repo.AddMember(testCtx(t), second); err != nil {
		t.Fatalf("AddMember(second): %v", err)
	}

	// A third member exists in identity.member (as if provisioned by
	// Nestorage) but has no nestova.member_profile row yet.
	crossApp := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "Ivy", Role: domain.RoleAdult, Color: domain.ColorOchre}
	if err := repo.AddMember(testCtx(t), crossApp); err != nil {
		t.Fatalf("AddMember(crossApp): %v", err)
	}
	if _, err := pool.Exec(testCtx(t), "DELETE FROM member_profile WHERE member_id = $1", crossApp.ID.String()); err != nil {
		t.Fatalf("delete profile row: %v", err)
	}

	got, err := repo.EnsureMemberProfile(testCtx(t), crossApp.ID)
	if err != nil {
		t.Fatalf("EnsureMemberProfile: %v", err)
	}
	if got.Color != domain.ColorOchre {
		t.Errorf("EnsureMemberProfile color = %v, want %v (first unused color in the household)", got.Color, domain.ColorOchre)
	}

	var persisted string
	if err := pool.QueryRow(testCtx(t), "SELECT color_key FROM member_profile WHERE member_id = $1", crossApp.ID.String()).Scan(&persisted); err != nil {
		t.Fatalf("query persisted profile row: %v", err)
	}
	if persisted != domain.ColorOchre.String() {
		t.Errorf("persisted color_key = %q, want %q", persisted, domain.ColorOchre.String())
	}
}

// TestEnsureMemberProfile_ExistingRowIsUnchanged verifies idempotence: a
// member who already has a profile row is returned exactly as stored,
// without EnsureMemberProfile reassigning a new color.
func TestEnsureMemberProfile_ExistingRowIsUnchanged(t *testing.T) {
	repo := newTestRepo(t)
	h := seedHousehold(t, repo)

	m := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "Maya", Role: domain.RoleAdult, Color: domain.ColorPlum}
	if err := repo.AddMember(testCtx(t), m); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	got, err := repo.EnsureMemberProfile(testCtx(t), m.ID)
	if err != nil {
		t.Fatalf("EnsureMemberProfile: %v", err)
	}
	if got.Color != domain.ColorPlum {
		t.Errorf("EnsureMemberProfile color = %v, want unchanged %v", got.Color, domain.ColorPlum)
	}
}

// TestEnsureMemberProfile_UnknownMember verifies the documented error
// contract: an unknown member id reports ErrMemberNotFound, exactly like
// GetMember.
func TestEnsureMemberProfile_UnknownMember(t *testing.T) {
	repo := newTestRepo(t)
	if _, err := repo.EnsureMemberProfile(testCtx(t), domain.NewMemberID()); !errors.Is(err, domain.ErrMemberNotFound) {
		t.Errorf("EnsureMemberProfile(unknown) error = %v, want ErrMemberNotFound", err)
	}
}

// TestEnsureMemberProfile_ConcurrentDifferentMembers_NoColorCollision proves
// the household advisory lock actually closes the race a naive
// read-then-insert would have: two DIFFERENT profile-less members of the
// SAME household, provisioned concurrently, must not both land on the same
// "first unused" color — member_profile has no per-household color
// uniqueness constraint to catch that at the database level, so this is the
// only thing that would.
func TestEnsureMemberProfile_ConcurrentDifferentMembers_NoColorCollision(t *testing.T) {
	pool := dbtest.NewIsolatedPool(t, "household")
	repo := adapter.NewPostgresRepository(pool)
	h := seedHousehold(t, repo)

	a := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "A", Role: domain.RoleAdult, Color: domain.ColorSage}
	if err := repo.AddMember(testCtx(t), a); err != nil {
		t.Fatalf("AddMember(a): %v", err)
	}
	b := &domain.Member{ID: domain.NewMemberID(), HouseholdID: h.ID, DisplayName: "B", Role: domain.RoleAdult, Color: domain.ColorClay}
	if err := repo.AddMember(testCtx(t), b); err != nil {
		t.Fatalf("AddMember(b): %v", err)
	}
	for _, m := range []*domain.Member{a, b} {
		if _, err := pool.Exec(testCtx(t), "DELETE FROM member_profile WHERE member_id = $1", m.ID.String()); err != nil {
			t.Fatalf("delete profile row(%s): %v", m.DisplayName, err)
		}
	}

	var wg sync.WaitGroup
	results := make([]*domain.Member, 2)
	errs := make([]error, 2)
	for i, m := range []*domain.Member{a, b} {
		wg.Add(1)
		go func(i int, id domain.MemberID) {
			defer wg.Done()
			results[i], errs[i] = repo.EnsureMemberProfile(testCtx(t), id)
		}(i, m.ID)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureMemberProfile(%d): %v", i, err)
		}
	}
	if results[0].Color == results[1].Color {
		t.Errorf("both concurrently provisioned members got color %v, want two distinct colors", results[0].Color)
	}
}
