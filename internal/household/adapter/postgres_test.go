package adapter_test

import (
	"context"
	"errors"
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

// TestSetAndGetQuietHours verifies the NES-139 quiet-hours round trip
// through the real pgtype.Time codec: a freshly created household has
// quiet hours disabled, SetQuietHours persists both bounds, and passing
// nil for both disables them again.
func TestSetAndGetQuietHours(t *testing.T) {
	repo := newTestRepo(t)
	h := seedHousehold(t, repo)

	got, err := repo.GetHousehold(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("GetHousehold (fresh): %v", err)
	}
	if got.QuietHoursStart != nil || got.QuietHoursEnd != nil {
		t.Errorf("fresh household quiet hours = (%v, %v), want (nil, nil)", got.QuietHoursStart, got.QuietHoursEnd)
	}

	start, end := 22*time.Hour, 7*time.Hour
	if err := repo.SetQuietHours(testCtx(t), h.ID, &start, &end); err != nil {
		t.Fatalf("SetQuietHours: %v", err)
	}
	got, err = repo.GetHousehold(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("GetHousehold (after set): %v", err)
	}
	if got.QuietHoursStart == nil || *got.QuietHoursStart != start {
		t.Errorf("QuietHoursStart = %v, want %v", got.QuietHoursStart, start)
	}
	if got.QuietHoursEnd == nil || *got.QuietHoursEnd != end {
		t.Errorf("QuietHoursEnd = %v, want %v", got.QuietHoursEnd, end)
	}

	if err := repo.SetQuietHours(testCtx(t), h.ID, nil, nil); err != nil {
		t.Fatalf("SetQuietHours (disable): %v", err)
	}
	got, err = repo.GetHousehold(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("GetHousehold (after disable): %v", err)
	}
	if got.QuietHoursStart != nil || got.QuietHoursEnd != nil {
		t.Errorf("disabled quiet hours = (%v, %v), want (nil, nil)", got.QuietHoursStart, got.QuietHoursEnd)
	}
}

func TestSetQuietHours_UnknownHousehold_ReturnsNotFound(t *testing.T) {
	repo := newTestRepo(t)
	start, end := 22*time.Hour, 7*time.Hour
	err := repo.SetQuietHours(testCtx(t), domain.NewHouseholdID(), &start, &end)
	if !errors.Is(err, domain.ErrHouseholdNotFound) {
		t.Errorf("SetQuietHours(unknown household) error = %v, want ErrHouseholdNotFound", err)
	}
}

// TestSetQuietHours_OnlyOneBoundSet_Rejected is the regression test for
// CodeRabbit round 2 (minor finding #3): the repository is the last line
// of defense against a half-set quiet-hours pair — domain.Household's own
// doc states both nil means disabled, so exactly one bound set has no
// defined meaning. Also confirms a rejected call never reaches the
// database: the household's quiet hours stay exactly as they were before.
func TestSetQuietHours_OnlyOneBoundSet_Rejected(t *testing.T) {
	repo := newTestRepo(t)
	h := seedHousehold(t, repo)

	start := 22 * time.Hour
	end := 7 * time.Hour

	tests := []struct {
		name       string
		start, end *time.Duration
	}{
		{"start set, end nil", &start, nil},
		{"start nil, end set", nil, &end},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := repo.SetQuietHours(testCtx(t), h.ID, tt.start, tt.end); err == nil {
				t.Fatal("SetQuietHours(exactly one bound set) error = nil, want non-nil")
			}
			got, err := repo.GetHousehold(testCtx(t), h.ID)
			if err != nil {
				t.Fatalf("GetHousehold: %v", err)
			}
			if got.QuietHoursStart != nil || got.QuietHoursEnd != nil {
				t.Errorf("a rejected SetQuietHours call must not persist anything, got (%v, %v)", got.QuietHoursStart, got.QuietHoursEnd)
			}
		})
	}
}
