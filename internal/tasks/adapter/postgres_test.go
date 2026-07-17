package adapter_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// newTestPool returns a pool backed by NESTOVA_TEST_DATABASE_URL with the full
// schema applied, or skips when the env var is unset (keeping the default test
// run hermetic). The cleanup handler resets the schema after the test.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the tasks repository tests")
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
	return pool
}

// testCtx returns a per-call context bounded to 10 s so a slow or unresponsive
// database fails the test rather than hanging it.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// refDate is an arbitrary fixed reference date used throughout the tests so
// no test depends on the wall clock.
var refDate = time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC)

// seedHousehold creates a household and two members, returning the household and
// both member IDs. The household adapter is the seeding vehicle so tasks tests
// do not depend on raw SQL.
func seedHousehold(t *testing.T, pool *pgxpool.Pool) (*household.Household, household.MemberID, household.MemberID) {
	t.Helper()
	hhRepo := householdadapter.NewPostgresRepository(pool)

	h := &household.Household{ID: household.NewHouseholdID(), Name: "The Fishers"}
	if err := hhRepo.CreateHousehold(testCtx(t), h); err != nil {
		t.Fatalf("CreateHousehold: %v", err)
	}

	m1 := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: h.ID,
		DisplayName: "Alice",
		Role:        household.RoleAdult,
		Color:       household.ColorSage,
	}
	if err := hhRepo.AddMember(testCtx(t), m1); err != nil {
		t.Fatalf("AddMember(Alice): %v", err)
	}

	m2 := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: h.ID,
		DisplayName: "Bob",
		Role:        household.RoleAdult,
		Color:       household.ColorClay,
	}
	if err := hhRepo.AddMember(testCtx(t), m2); err != nil {
		t.Fatalf("AddMember(Bob): %v", err)
	}

	return h, m1.ID, m2.ID
}

// newWeeklyCadence returns a simple weekly cadence anchored to refDate.
func newWeeklyCadence() household.Cadence {
	return household.Cadence{
		Freq:     household.FreqWeekly,
		Interval: 1,
		Anchor:   refDate,
	}
}

// newAsNeededCadence returns an as-needed (NES-116) cadence anchored to
// refDate. Interval carries no meaning for this frequency; it is set to
// satisfy Cadence.Validate the same way the create-task form does.
func newAsNeededCadence() household.Cadence {
	return household.Cadence{
		Freq:     household.FreqAsNeeded,
		Interval: 1,
		Anchor:   refDate,
	}
}

// seedRecurringTask creates and persists a basic recurring task for the given
// household. The cadence, category, and rotation policy are sensible defaults.
func seedRecurringTask(
	t *testing.T,
	repo *adapter.RecurringTaskRepository,
	householdID household.HouseholdID,
) *domain.RecurringTask {
	t.Helper()
	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    householdID,
		Title:          "Vacuum living room",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationRoundRobin,
		Points:         10,
		LeadTimeDays:   2,
		Active:         true,
	}
	if err := repo.Create(testCtx(t), rt); err != nil {
		t.Fatalf("Create recurring task: %v", err)
	}
	return rt
}

// seedTaskInstance creates and persists a pending task instance for the given
// recurring task, due on the provided date.
func seedTaskInstance(
	t *testing.T,
	repo *adapter.TaskInstanceRepository,
	rt *domain.RecurringTask,
	dueOn time.Time,
) *domain.TaskInstance {
	t.Helper()
	inst := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     rt.HouseholdID,
		DueOn:           domain.DueOnPtr(dueOn),
		Status:          domain.StatusPending,
	}
	if err := repo.Insert(testCtx(t), inst); err != nil {
		t.Fatalf("Insert task instance: %v", err)
	}
	return inst
}

// seedStandingInstance creates and persists a pending standing instance
// (NES-116) for the given as-needed recurring task.
func seedStandingInstance(
	t *testing.T,
	repo *adapter.TaskInstanceRepository,
	rt *domain.RecurringTask,
) *domain.TaskInstance {
	t.Helper()
	inst := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     rt.HouseholdID,
		Status:          domain.StatusPending,
		Kind:            domain.KindStanding,
	}
	if err := repo.Insert(testCtx(t), inst); err != nil {
		t.Fatalf("Insert standing task instance: %v", err)
	}
	return inst
}

// seedOverdueTaskInstance creates a past-due pending instance and runs the
// household-scoped overdue sweep so it is transitioned to the overdue state.
// It returns the now-overdue instance. The instance's due_on is one day before
// refDate, and the sweep uses refDate as asOf so the strict due_on < asOf
// predicate matches.
func seedOverdueTaskInstance(
	t *testing.T,
	repo *adapter.TaskInstanceRepository,
	rt *domain.RecurringTask,
) *domain.TaskInstance {
	t.Helper()
	inst := seedTaskInstance(t, repo, rt, refDate.AddDate(0, 0, -1))
	if _, err := repo.MarkPendingOverdue(testCtx(t), rt.HouseholdID, refDate); err != nil {
		t.Fatalf("MarkPendingOverdue (seed overdue): %v", err)
	}
	got, err := repo.Get(testCtx(t), rt.HouseholdID, inst.ID)
	if err != nil {
		t.Fatalf("Get (seed overdue): %v", err)
	}
	if got.Status != domain.StatusOverdue {
		t.Fatalf("seedOverdueTaskInstance: status = %v, want overdue", got.Status)
	}
	return got
}

// ---------------------------------------------------------------------------
// RecurringTaskRepository tests
// ---------------------------------------------------------------------------

// TestRecurringTask_CreateAndGet verifies that a recurring task round-trips
// through Create and Get with all fields intact, including the cadence jsonb.
func TestRecurringTask_CreateAndGet(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, repo, h.ID)

	got, err := repo.Get(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != rt.ID {
		t.Errorf("ID = %v, want %v", got.ID, rt.ID)
	}
	if got.Title != rt.Title {
		t.Errorf("Title = %q, want %q", got.Title, rt.Title)
	}
	if got.Category != rt.Category {
		t.Errorf("Category = %v, want %v", got.Category, rt.Category)
	}
	if got.RotationPolicy != rt.RotationPolicy {
		t.Errorf("RotationPolicy = %v, want %v", got.RotationPolicy, rt.RotationPolicy)
	}
	if got.Points != rt.Points {
		t.Errorf("Points = %d, want %d", got.Points, rt.Points)
	}
	if got.LeadTimeDays != rt.LeadTimeDays {
		t.Errorf("LeadTimeDays = %d, want %d", got.LeadTimeDays, rt.LeadTimeDays)
	}
	if !got.Active {
		t.Error("Active = false, want true")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	// Cadence must round-trip through jsonb.
	if got.Cadence.Freq != rt.Cadence.Freq {
		t.Errorf("Cadence.Freq = %v, want %v", got.Cadence.Freq, rt.Cadence.Freq)
	}
	if got.Cadence.Interval != rt.Cadence.Interval {
		t.Errorf("Cadence.Interval = %d, want %d", got.Cadence.Interval, rt.Cadence.Interval)
	}
	if !got.Cadence.Anchor.Equal(rt.Cadence.Anchor) {
		t.Errorf("Cadence.Anchor = %v, want %v", got.Cadence.Anchor, rt.Cadence.Anchor)
	}
}

// TestRecurringTask_PhotoPolicy_DefaultsToNone verifies NES-120's zero-value
// defaulting contract against the real database: a RecurringTask created
// without ever setting PhotoPolicy (every pre-NES-120 caller, including
// seedRecurringTask itself) persists and reads back as PhotoPolicyNone —
// proving the adapter's insertRecurringTask default and the 00030
// migration's column DEFAULT agree, not just the in-memory fakes.
func TestRecurringTask_PhotoPolicy_DefaultsToNone(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, repo, h.ID)
	if rt.PhotoPolicy != domain.PhotoPolicyNone {
		t.Errorf("PhotoPolicy on the inserted struct = %v, want PhotoPolicyNone (Insert must reflect the defaulted value back)", rt.PhotoPolicy)
	}

	got, err := repo.Get(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PhotoPolicy != domain.PhotoPolicyNone {
		t.Errorf("PhotoPolicy = %v, want PhotoPolicyNone", got.PhotoPolicy)
	}
}

// TestRecurringTask_PhotoPolicy_RoundTrips verifies that an explicitly set
// PhotoPolicy (NES-120) persists through Create and is read back unchanged
// by Get, ListActive, and ListAllActive alike.
func TestRecurringTask_PhotoPolicy_RoundTrips(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Clean garage",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Active:         true,
		PhotoPolicy:    domain.PhotoPolicyBeforeAfter,
	}
	if err := repo.Create(testCtx(t), rt); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PhotoPolicy != domain.PhotoPolicyBeforeAfter {
		t.Errorf("Get: PhotoPolicy = %v, want PhotoPolicyBeforeAfter", got.PhotoPolicy)
	}

	active, err := repo.ListActive(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 1 || active[0].PhotoPolicy != domain.PhotoPolicyBeforeAfter {
		t.Errorf("ListActive PhotoPolicy did not round-trip: %+v", active)
	}

	all, err := repo.ListAllActive(testCtx(t))
	if err != nil {
		t.Fatalf("ListAllActive: %v", err)
	}
	found := false
	for _, task := range all {
		if task.ID == rt.ID {
			found = true
			if task.PhotoPolicy != domain.PhotoPolicyBeforeAfter {
				t.Errorf("ListAllActive: PhotoPolicy = %v, want PhotoPolicyBeforeAfter", task.PhotoPolicy)
			}
		}
	}
	if !found {
		t.Fatal("ListAllActive did not return the seeded task")
	}
}

// TestRecurringTask_GetCrossHousehold verifies that a task belonging to one
// household is invisible to a query scoped to a different household.
func TestRecurringTask_GetCrossHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, repo, h.ID)

	// Query with a different, unknown household ID.
	otherHouseholdID := household.NewHouseholdID()
	_, err := repo.Get(testCtx(t), otherHouseholdID, rt.ID)
	if !errors.Is(err, domain.ErrTaskNotFound) {
		t.Errorf("Get(cross-household) = %v, want ErrTaskNotFound", err)
	}
}

// TestRecurringTask_ListActive verifies that ListActive returns only active
// tasks for the household and omits inactive ones.
func TestRecurringTask_ListActive(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	active1 := seedRecurringTask(t, repo, h.ID)

	// Create an inactive task.
	inactive := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Old task",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Points:         0,
		LeadTimeDays:   0,
		Active:         false,
	}
	if err := repo.Create(testCtx(t), inactive); err != nil {
		t.Fatalf("Create inactive task: %v", err)
	}

	active2 := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Mop floors",
		Category:       domain.MaintenanceCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationFixed,
		Points:         5,
		LeadTimeDays:   1,
		Active:         true,
	}
	if err := repo.Create(testCtx(t), active2); err != nil {
		t.Fatalf("Create active2 task: %v", err)
	}

	got, err := repo.ListActive(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListActive returned %d tasks, want 2", len(got))
	}

	// Both active tasks must appear; inactive must not.
	ids := map[domain.RecurringTaskID]bool{got[0].ID: true, got[1].ID: true}
	if !ids[active1.ID] {
		t.Errorf("ListActive missing active1 (%v)", active1.ID)
	}
	if !ids[active2.ID] {
		t.Errorf("ListActive missing active2 (%v)", active2.ID)
	}
	if ids[inactive.ID] {
		t.Errorf("ListActive returned inactive task (%v)", inactive.ID)
	}
}

// TestRecurringTask_ListActiveUnknownHousehold verifies the documented
// contract: an unknown household returns an empty slice, not an error.
func TestRecurringTask_ListActiveUnknownHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)

	got, err := repo.ListActive(testCtx(t), household.NewHouseholdID())
	if err != nil {
		t.Fatalf("ListActive(unknown) error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("ListActive(unknown) returned %d tasks, want 0", len(got))
	}
}

// TestRecurringTask_SetAndGetRotationMembers verifies that SetRotationMembers
// persists members in position order and RotationMembers returns them in that
// order. It also verifies that replacing the pool with a smaller set works.
func TestRecurringTask_SetAndGetRotationMembers(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	rt := seedRecurringTask(t, repo, h.ID)

	// Set an initial pool: [m1, m2].
	if err := repo.SetRotationMembers(testCtx(t), h.ID, rt.ID, []household.MemberID{m1, m2}); err != nil {
		t.Fatalf("SetRotationMembers([m1,m2]): %v", err)
	}

	got, err := repo.RotationMembers(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("RotationMembers: %v", err)
	}
	if len(got) != 2 || got[0] != m1 || got[1] != m2 {
		t.Errorf("RotationMembers = %v, want [%v %v]", got, m1, m2)
	}

	// Replace the pool with just [m2]; m1 must no longer appear.
	if err := repo.SetRotationMembers(testCtx(t), h.ID, rt.ID, []household.MemberID{m2}); err != nil {
		t.Fatalf("SetRotationMembers([m2]): %v", err)
	}

	got, err = repo.RotationMembers(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("RotationMembers after replace: %v", err)
	}
	if len(got) != 1 || got[0] != m2 {
		t.Errorf("RotationMembers after replace = %v, want [%v]", got, m2)
	}
}

// TestRecurringTask_RotationMembersEmpty verifies that RotationMembers returns
// an empty slice (not an error) when no members have been added to the pool.
func TestRecurringTask_RotationMembersEmpty(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, repo, h.ID)

	got, err := repo.RotationMembers(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("RotationMembers(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("RotationMembers(empty) = %v, want []", got)
	}
}

// TestRecurringTask_SetRotationMembersUnknownTask verifies that
// SetRotationMembers returns ErrTaskNotFound for an unknown task id.
func TestRecurringTask_SetRotationMembersUnknownTask(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	err := repo.SetRotationMembers(testCtx(t), h.ID, domain.NewRecurringTaskID(), []household.MemberID{m1})
	if !errors.Is(err, domain.ErrTaskNotFound) {
		t.Errorf("SetRotationMembers(unknown task) = %v, want ErrTaskNotFound", err)
	}
}

// TestRecurringTask_ClearRotationMembers verifies that passing an empty slice
// to SetRotationMembers clears the pool and RotationMembers returns [].
func TestRecurringTask_ClearRotationMembers(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, repo, h.ID)

	if err := repo.SetRotationMembers(testCtx(t), h.ID, rt.ID, []household.MemberID{m1}); err != nil {
		t.Fatalf("SetRotationMembers: %v", err)
	}

	// Clear the pool.
	if err := repo.SetRotationMembers(testCtx(t), h.ID, rt.ID, []household.MemberID{}); err != nil {
		t.Fatalf("SetRotationMembers(clear): %v", err)
	}

	got, err := repo.RotationMembers(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("RotationMembers after clear: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("RotationMembers after clear = %v, want []", got)
	}
}

// ---------------------------------------------------------------------------
// TaskInstanceRepository tests
// ---------------------------------------------------------------------------

// TestTaskInstance_InsertAndGet verifies that an instance round-trips through
// Insert and Get with DueOn preserved as a calendar date (midnight UTC).
func TestTaskInstance_InsertAndGet(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	dueOn := refDate.AddDate(0, 0, 7)
	inst := seedTaskInstance(t, instRepo, rt, dueOn)

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != inst.ID {
		t.Errorf("ID = %v, want %v", got.ID, inst.ID)
	}
	if got.RecurringTaskID != rt.ID {
		t.Errorf("RecurringTaskID = %v, want %v", got.RecurringTaskID, rt.ID)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("Status = %v, want pending", got.Status)
	}
	// DueOn must be midnight UTC regardless of what was passed in.
	wantDueOn := domain.DateOf(dueOn)
	if !got.DueOn.Equal(wantDueOn) {
		t.Errorf("DueOn = %v, want %v", got.DueOn, wantDueOn)
	}
	if got.AssigneeID != nil {
		t.Errorf("AssigneeID = %v, want nil", got.AssigneeID)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil", got.CompletedAt)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// TestTaskInstance_InsertDuplicateReturnsErrDuplicateInstance verifies that
// inserting a second instance for the same (recurring_task_id, due_on) pair
// returns domain.ErrDuplicateInstance.
func TestTaskInstance_InsertDuplicateReturnsErrDuplicateInstance(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	dueOn := refDate.AddDate(0, 0, 7)

	seedTaskInstance(t, instRepo, rt, dueOn)

	// A second insert for the same task+due_on must be rejected.
	dup := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h.ID,
		DueOn:           domain.DueOnPtr(dueOn),
		Status:          domain.StatusPending,
	}
	err := instRepo.Insert(testCtx(t), dup)
	if !errors.Is(err, domain.ErrDuplicateInstance) {
		t.Errorf("Insert(duplicate) = %v, want ErrDuplicateInstance", err)
	}
}

// TestTaskInstance_GetCrossHousehold verifies that an instance belonging to one
// household is invisible to a query scoped to a different household.
func TestTaskInstance_GetCrossHousehold(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	_, err := instRepo.Get(testCtx(t), household.NewHouseholdID(), inst.ID)
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("Get(cross-household) = %v, want ErrInstanceNotFound", err)
	}
}

// TestTaskInstance_Claim_Success verifies that claiming a pending, unassigned
// instance succeeds and the assignee is persisted.
func TestTaskInstance_Claim_Success(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	if got.AssigneeID == nil || *got.AssigneeID != m1 {
		t.Errorf("AssigneeID = %v, want %v", got.AssigneeID, m1)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("Status = %v after Claim, want pending", got.Status)
	}
}

// TestTaskInstance_Claim_AlreadyClaimed verifies that claiming an already-
// assigned pending instance returns ErrInstanceAlreadyClaimed.
func TestTaskInstance_Claim_AlreadyClaimed(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	// m1 claims first.
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim(m1): %v", err)
	}

	// m2 tries to claim the same instance.
	err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m2)
	if !errors.Is(err, domain.ErrInstanceAlreadyClaimed) {
		t.Errorf("Claim(already claimed) = %v, want ErrInstanceAlreadyClaimed", err)
	}
}

// TestTaskInstance_Claim_TerminalState verifies that claiming an instance in a
// terminal (done or skipped) state returns ErrInstanceInTerminalState.
func TestTaskInstance_Claim_TerminalState(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	// Skip the instance to put it in a terminal state.
	if err := instRepo.Skip(testCtx(t), h.ID, inst.ID); err != nil {
		t.Fatalf("Skip: %v", err)
	}

	err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1)
	if !errors.Is(err, domain.ErrInstanceInTerminalState) {
		t.Errorf("Claim(terminal) = %v, want ErrInstanceInTerminalState", err)
	}
}

// TestTaskInstance_Claim_OverdueSuccess verifies that an overdue, unassigned
// instance is still claimable (NES-32): claiming it assigns the member while
// the status stays overdue.
func TestTaskInstance_Claim_OverdueSuccess(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedOverdueTaskInstance(t, instRepo, rt)

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim(overdue): %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim(overdue): %v", err)
	}
	if got.AssigneeID == nil || *got.AssigneeID != m1 {
		t.Errorf("AssigneeID = %v, want %v", got.AssigneeID, m1)
	}
	if got.Status != domain.StatusOverdue {
		t.Errorf("Status = %v after Claim(overdue), want overdue", got.Status)
	}
}

// TestTaskInstance_Claim_NotFound verifies that claiming an unknown instance
// returns ErrInstanceNotFound.
func TestTaskInstance_Claim_NotFound(t *testing.T) {
	pool := newTestPool(t)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	err := instRepo.Claim(testCtx(t), h.ID, domain.NewTaskInstanceID(), m1)
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("Claim(unknown) = %v, want ErrInstanceNotFound", err)
	}
}

// TestTaskInstance_Claim_CrossHouseholdAssigneeRejected verifies the composite
// tenant FK prevents assigning an instance to a member of another household.
func TestTaskInstance_Claim_CrossHouseholdAssigneeRejected(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	hA, _, _ := seedHousehold(t, pool)
	_, otherMember, _ := seedHousehold(t, pool) // member in a different household

	rt := seedRecurringTask(t, taskRepo, hA.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 1))

	if err := instRepo.Claim(testCtx(t), hA.ID, inst.ID, otherMember); err == nil {
		t.Error("Claim with a cross-household assignee succeeded, want rejection by the tenant FK")
	}
}

// TestTaskInstance_Complete_NotFound verifies that completing an unknown
// instance returns ErrInstanceNotFound.
func TestTaskInstance_Complete_NotFound(t *testing.T) {
	pool := newTestPool(t)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	err := instRepo.Complete(testCtx(t), h.ID, domain.NewTaskInstanceID(), m1, refDate)
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("Complete(unknown) = %v, want ErrInstanceNotFound", err)
	}
}

// TestTaskInstance_Complete_Success verifies that completing a pending instance
// transitions it to done and persists completed_at and completed_by.
func TestTaskInstance_Complete_Success(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	// Claim before completing so the NES-117 claim-clearing assertion below is
	// meaningful — an instance that was never claimed trivially has nil claim
	// fields regardless of whether Complete clears them.
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	completedAt := refDate.AddDate(0, 0, 8)
	if err := instRepo.Complete(testCtx(t), h.ID, inst.ID, m1, completedAt); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Complete: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}
	if got.CompletedBy == nil || *got.CompletedBy != m1 {
		t.Errorf("CompletedBy = %v, want %v", got.CompletedBy, m1)
	}
	if got.CompletedAt == nil {
		t.Fatal("CompletedAt is nil, want a time")
	}
	if !got.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt = %s, want %s", got.CompletedAt.Format(time.RFC3339), completedAt.Format(time.RFC3339))
	}
	// NES-117: a done instance has no CURRENT claim.
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil (cleared on completion)", got.ClaimedBy)
	}
	if got.ClaimedAt != nil {
		t.Errorf("ClaimedAt = %v, want nil (cleared on completion)", got.ClaimedAt)
	}
	if got.ClaimExpiresAt != nil {
		t.Errorf("ClaimExpiresAt = %v, want nil (cleared on completion)", got.ClaimExpiresAt)
	}
}

// TestTaskInstance_Complete_AlreadyTerminal verifies that completing an
// already-completed instance returns ErrInstanceInTerminalState.
func TestTaskInstance_Complete_AlreadyTerminal(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	completedAt := refDate.AddDate(0, 0, 8)
	if err := instRepo.Complete(testCtx(t), h.ID, inst.ID, m1, completedAt); err != nil {
		t.Fatalf("Complete(first): %v", err)
	}

	// Completing again must be rejected.
	err := instRepo.Complete(testCtx(t), h.ID, inst.ID, m1, completedAt)
	if !errors.Is(err, domain.ErrInstanceInTerminalState) {
		t.Errorf("Complete(already done) = %v, want ErrInstanceInTerminalState", err)
	}
}

// TestTaskInstance_Skip_Success verifies that skipping a pending instance
// transitions it to skipped.
func TestTaskInstance_Skip_Success(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	// Claim before skipping so the NES-117 claim-clearing assertion below is
	// meaningful — an instance that was never claimed trivially has nil claim
	// fields regardless of whether Skip clears them.
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := instRepo.Skip(testCtx(t), h.ID, inst.ID); err != nil {
		t.Fatalf("Skip: %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Skip: %v", err)
	}
	if got.Status != domain.StatusSkipped {
		t.Errorf("Status = %v, want skipped", got.Status)
	}
	// NES-117: a skipped instance has no CURRENT claim.
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil (cleared on skip)", got.ClaimedBy)
	}
	if got.ClaimedAt != nil {
		t.Errorf("ClaimedAt = %v, want nil (cleared on skip)", got.ClaimedAt)
	}
	if got.ClaimExpiresAt != nil {
		t.Errorf("ClaimExpiresAt = %v, want nil (cleared on skip)", got.ClaimExpiresAt)
	}
}

// TestTaskInstance_Skip_AlreadyTerminal verifies that skipping a skipped
// instance returns ErrInstanceInTerminalState.
func TestTaskInstance_Skip_AlreadyTerminal(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Skip(testCtx(t), h.ID, inst.ID); err != nil {
		t.Fatalf("Skip(first): %v", err)
	}

	err := instRepo.Skip(testCtx(t), h.ID, inst.ID)
	if !errors.Is(err, domain.ErrInstanceInTerminalState) {
		t.Errorf("Skip(already skipped) = %v, want ErrInstanceInTerminalState", err)
	}
}

// TestTaskInstance_Skip_NotFound verifies that skipping an unknown instance
// returns ErrInstanceNotFound.
func TestTaskInstance_Skip_NotFound(t *testing.T) {
	pool := newTestPool(t)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	err := instRepo.Skip(testCtx(t), h.ID, domain.NewTaskInstanceID())
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("Skip(unknown) = %v, want ErrInstanceNotFound", err)
	}
}

// TestTaskInstance_Complete_OverdueSuccess verifies that an overdue instance is
// still completable (NES-32): completing it transitions it to done and records
// completed_at and completed_by. The done⟺completed_at CHECK is satisfied.
func TestTaskInstance_Complete_OverdueSuccess(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedOverdueTaskInstance(t, instRepo, rt)

	completedAt := refDate.AddDate(0, 0, 1)
	if err := instRepo.Complete(testCtx(t), h.ID, inst.ID, m1, completedAt); err != nil {
		t.Fatalf("Complete(overdue): %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Complete(overdue): %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}
	if got.CompletedBy == nil || *got.CompletedBy != m1 {
		t.Errorf("CompletedBy = %v, want %v", got.CompletedBy, m1)
	}
	if got.CompletedAt == nil {
		t.Fatal("CompletedAt is nil, want a time (done⟺completed_at)")
	}
	if !got.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt = %s, want %s", got.CompletedAt.Format(time.RFC3339), completedAt.Format(time.RFC3339))
	}
}

// TestTaskInstance_Skip_OverdueSuccess verifies that an overdue instance is
// still skippable (NES-32): skipping it transitions it to skipped and leaves
// completed_at NULL (skipped⟺completed_at IS NULL).
func TestTaskInstance_Skip_OverdueSuccess(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedOverdueTaskInstance(t, instRepo, rt)

	if err := instRepo.Skip(testCtx(t), h.ID, inst.ID); err != nil {
		t.Fatalf("Skip(overdue): %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Skip(overdue): %v", err)
	}
	if got.Status != domain.StatusSkipped {
		t.Errorf("Status = %v, want skipped", got.Status)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil for a skipped instance", got.CompletedAt)
	}
}

// TestTaskInstance_MarkPendingOverdue verifies that MarkPendingOverdue
// transitions only pending instances whose due_on < asOf and returns the count.
// Instances on or after asOf must remain pending.
func TestTaskInstance_MarkPendingOverdue(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)

	// Three instances: two past due, one future.
	pastDue1 := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, -3))
	pastDue2 := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h.ID,
		DueOn:           domain.DueOnPtr(refDate.AddDate(0, 0, -1)),
		Status:          domain.StatusPending,
	}
	if err := instRepo.Insert(testCtx(t), pastDue2); err != nil {
		t.Fatalf("Insert pastDue2: %v", err)
	}
	future := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h.ID,
		DueOn:           domain.DueOnPtr(refDate.AddDate(0, 0, 7)),
		Status:          domain.StatusPending,
	}
	if err := instRepo.Insert(testCtx(t), future); err != nil {
		t.Fatalf("Insert future: %v", err)
	}

	// Use refDate as asOf: due_on < refDate matches pastDue1 and pastDue2.
	count, err := instRepo.MarkPendingOverdue(testCtx(t), h.ID, refDate)
	if err != nil {
		t.Fatalf("MarkPendingOverdue: %v", err)
	}
	if count != 2 {
		t.Errorf("MarkPendingOverdue count = %d, want 2", count)
	}

	// Past-due instances must now be overdue.
	got1, err := instRepo.Get(testCtx(t), h.ID, pastDue1.ID)
	if err != nil {
		t.Fatalf("Get pastDue1: %v", err)
	}
	if got1.Status != domain.StatusOverdue {
		t.Errorf("pastDue1 Status = %v, want overdue", got1.Status)
	}
	got2, err := instRepo.Get(testCtx(t), h.ID, pastDue2.ID)
	if err != nil {
		t.Fatalf("Get pastDue2: %v", err)
	}
	if got2.Status != domain.StatusOverdue {
		t.Errorf("pastDue2 Status = %v, want overdue", got2.Status)
	}

	// Future instance must remain pending.
	gotFuture, err := instRepo.Get(testCtx(t), h.ID, future.ID)
	if err != nil {
		t.Fatalf("Get future: %v", err)
	}
	if gotFuture.Status != domain.StatusPending {
		t.Errorf("future Status = %v, want pending", gotFuture.Status)
	}
}

// TestTaskInstance_ListByHousehold verifies that ListByHousehold filters
// correctly by status and date window, and returns an empty slice when none
// match.
func TestTaskInstance_ListByHousehold(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)

	// Three pending instances on different dates.
	inst1 := seedTaskInstance(t, instRepo, rt, refDate)
	inst2 := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h.ID,
		DueOn:           domain.DueOnPtr(refDate.AddDate(0, 0, 7)),
		Status:          domain.StatusPending,
	}
	if err := instRepo.Insert(testCtx(t), inst2); err != nil {
		t.Fatalf("Insert inst2: %v", err)
	}
	inst3 := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h.ID,
		DueOn:           domain.DueOnPtr(refDate.AddDate(0, 0, 14)),
		Status:          domain.StatusPending,
	}
	if err := instRepo.Insert(testCtx(t), inst3); err != nil {
		t.Fatalf("Insert inst3: %v", err)
	}

	// Complete inst1 so it is no longer pending.
	if err := instRepo.Complete(testCtx(t), h.ID, inst1.ID, m1, refDate.Add(time.Hour)); err != nil {
		t.Fatalf("Complete inst1: %v", err)
	}

	// Query pending instances within [refDate, refDate+7d]. Only inst2 qualifies.
	from := refDate
	to := refDate.AddDate(0, 0, 7)
	got, err := instRepo.ListByHousehold(testCtx(t), h.ID, domain.StatusPending, from, to)
	if err != nil {
		t.Fatalf("ListByHousehold: %v", err)
	}
	if len(got) != 1 || got[0].ID != inst2.ID {
		t.Errorf("ListByHousehold = %v IDs, want [%v]", idsOf(got), inst2.ID)
	}

	// Query with a status that matches nothing in the window.
	got, err = instRepo.ListByHousehold(testCtx(t), h.ID, domain.StatusSkipped, from, to)
	if err != nil {
		t.Fatalf("ListByHousehold(no match): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListByHousehold(no match) = %d rows, want 0", len(got))
	}
}

// TestTaskInstance_LatestDueOn verifies that LatestDueOn returns the most
// recent due_on and ok=true, or (zero, false, nil) when no instances exist.
func TestTaskInstance_LatestDueOn(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)

	// No instances yet: must return (zero, false, nil).
	ts, ok, err := instRepo.LatestDueOn(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("LatestDueOn (empty): %v", err)
	}
	if ok {
		t.Errorf("LatestDueOn (empty) ok = true, want false")
	}
	if !ts.IsZero() {
		t.Errorf("LatestDueOn (empty) ts = %v, want zero", ts)
	}

	// Add two instances; the later one must be returned.
	seedTaskInstance(t, instRepo, rt, refDate)
	inst2 := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h.ID,
		DueOn:           domain.DueOnPtr(refDate.AddDate(0, 0, 14)),
		Status:          domain.StatusPending,
	}
	if err := instRepo.Insert(testCtx(t), inst2); err != nil {
		t.Fatalf("Insert inst2: %v", err)
	}

	ts, ok, err = instRepo.LatestDueOn(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("LatestDueOn (with instances): %v", err)
	}
	if !ok {
		t.Fatal("LatestDueOn (with instances) ok = false, want true")
	}
	wantLatest := domain.DateOf(refDate.AddDate(0, 0, 14))
	if !ts.Equal(wantLatest) {
		t.Errorf("LatestDueOn = %v, want %v", ts, wantLatest)
	}
}

// TestRecurringTask_ListAllActive verifies that ListAllActive returns all active
// tasks across multiple households and omits inactive ones.
func TestRecurringTask_ListAllActive(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)

	// Two separate households.
	hA, _, _ := seedHousehold(t, pool)
	hB, _, _ := seedHousehold(t, pool)

	// One active task per household.
	activeA := seedRecurringTask(t, repo, hA.ID)
	activeB := seedRecurringTask(t, repo, hB.ID)

	// One inactive task in household A.
	inactive := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    hA.ID,
		Title:          "Inactive task",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Active:         false,
	}
	if err := repo.Create(testCtx(t), inactive); err != nil {
		t.Fatalf("Create inactive task: %v", err)
	}

	got, err := repo.ListAllActive(testCtx(t))
	if err != nil {
		t.Fatalf("ListAllActive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListAllActive returned %d tasks, want 2", len(got))
	}

	ids := map[domain.RecurringTaskID]bool{got[0].ID: true, got[1].ID: true}
	if !ids[activeA.ID] {
		t.Errorf("ListAllActive missing activeA (%v)", activeA.ID)
	}
	if !ids[activeB.ID] {
		t.Errorf("ListAllActive missing activeB (%v)", activeB.ID)
	}
	if ids[inactive.ID] {
		t.Errorf("ListAllActive returned inactive task (%v)", inactive.ID)
	}
}

// TestGenerator_EndToEnd is a gated integration test that exercises the full
// materialisation pipeline against a real database: it seeds a household with
// two members, creates a round-robin recurring task via TaskService, runs
// GenerateDue, verifies instances and assignee cycling, then verifies that a
// second GenerateDue call inserts nothing new.
func TestGenerator_EndToEnd(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instanceRepo := adapter.NewTaskInstanceRepository(pool)

	h, m1, m2 := seedHousehold(t, pool)

	// Build and create a round-robin weekly task via TaskService.
	svc, err := tasksapp.NewTaskService(taskRepo, instanceRepo, nil)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	rt := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: h.ID,
		Title:       "Vacuum",
		Category:    domain.ChoreCategory,
		Cadence: household.Cadence{
			Freq:     household.FreqWeekly,
			Interval: 1,
			Anchor:   refDate, // 2025-03-10
		},
		RotationPolicy: domain.RotationRoundRobin,
		Points:         10,
		Active:         true,
	}

	memberPool := []household.MemberID{m1, m2}
	if err := svc.CreateRecurringTask(testCtx(t), rt, memberPool); err != nil {
		t.Fatalf("CreateRecurringTask: %v", err)
	}

	// Run the generator with a 14-day horizon, asOf = refDate.
	// Window: (refDate-1ns, refDate+14d] → occurrences at refDate, +7d, +14d = 3.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gen, err := tasksapp.NewGenerator(taskRepo, instanceRepo, logger, 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}

	count, err := gen.GenerateDue(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("GenerateDue (run 1): %v", err)
	}
	if count != 3 {
		t.Errorf("GenerateDue (run 1) inserted %d instances, want 3", count)
	}

	// Verify instances via ListByHousehold.
	from := refDate
	to := refDate.AddDate(0, 0, 14)
	instances, err := instanceRepo.ListByHousehold(testCtx(t), h.ID, domain.StatusPending, from, to)
	if err != nil {
		t.Fatalf("ListByHousehold: %v", err)
	}
	if len(instances) != 3 {
		t.Fatalf("ListByHousehold returned %d instances, want 3", len(instances))
	}

	// Verify round-robin cycling: ordinals 0,1,2 → m1, m2, m1.
	wantAssignees := []household.MemberID{m1, m2, m1}
	for i, inst := range instances {
		if inst.AssigneeID == nil {
			t.Errorf("instance %d (due %s): assignee is nil", i, inst.DueOn.Format(time.DateOnly))
			continue
		}
		if *inst.AssigneeID != wantAssignees[i] {
			t.Errorf("instance %d (due %s): assignee = %v, want %v",
				i, inst.DueOn.Format(time.DateOnly), *inst.AssigneeID, wantAssignees[i])
		}
	}

	// Second run must insert nothing.
	count2, err := gen.GenerateDue(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("GenerateDue (run 2): %v", err)
	}
	if count2 != 0 {
		t.Errorf("GenerateDue (run 2) inserted %d instances, want 0", count2)
	}

	// Row count must be unchanged.
	instances2, err := instanceRepo.ListByHousehold(testCtx(t), h.ID, domain.StatusPending, from, to)
	if err != nil {
		t.Fatalf("ListByHousehold (after run 2): %v", err)
	}
	if len(instances2) != 3 {
		t.Errorf("ListByHousehold after run 2 returned %d instances, want 3", len(instances2))
	}
}

// TestRecurringTask_CreateWithRotation_Atomic verifies that CreateWithRotation
// is atomic: when a rotation_member insert fails (here, a member id from a
// DIFFERENT household, rejected by the composite tenant FK), the whole
// transaction rolls back and NO recurring_task row is persisted.
func TestRecurringTask_CreateWithRotation_Atomic(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)

	hA, _, _ := seedHousehold(t, pool)
	_, otherMember, _ := seedHousehold(t, pool) // member belongs to a different household

	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    hA.ID,
		Title:          "Atomic vacuum",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationRoundRobin,
		Points:         10,
		Active:         true,
	}

	// The cross-household member violates the rotation_member composite tenant FK,
	// so the rotation_member insert (and thus the whole transaction) must fail.
	err := repo.CreateWithRotation(testCtx(t), rt, []household.MemberID{otherMember})
	if err == nil {
		t.Fatal("CreateWithRotation with a cross-household member succeeded, want failure")
	}

	// The task must NOT have been persisted — the failed rotation insert rolls
	// back the recurring_task insert too.
	if _, err := repo.Get(testCtx(t), hA.ID, rt.ID); !errors.Is(err, domain.ErrTaskNotFound) {
		t.Errorf("Get after rolled-back CreateWithRotation = %v, want ErrTaskNotFound", err)
	}
}

// TestRecurringTask_CreateWithRotation_Success verifies the happy path:
// CreateWithRotation persists the task and its pool in position order.
func TestRecurringTask_CreateWithRotation_Success(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecurringTaskRepository(pool)

	h, m1, m2 := seedHousehold(t, pool)

	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Atomic dishes",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationRoundRobin,
		Points:         5,
		Active:         true,
	}

	if err := repo.CreateWithRotation(testCtx(t), rt, []household.MemberID{m1, m2}); err != nil {
		t.Fatalf("CreateWithRotation: %v", err)
	}

	// The task must round-trip through Get with its timestamps populated.
	got, err := repo.Get(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("Get after CreateWithRotation: %v", err)
	}
	if got.ID != rt.ID {
		t.Errorf("ID = %v, want %v", got.ID, rt.ID)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want a populated timestamp")
	}

	// The rotation pool must be persisted in position order.
	members, err := repo.RotationMembers(testCtx(t), h.ID, rt.ID)
	if err != nil {
		t.Fatalf("RotationMembers: %v", err)
	}
	if len(members) != 2 || members[0] != m1 || members[1] != m2 {
		t.Errorf("RotationMembers = %v, want [%v %v]", members, m1, m2)
	}
}

// TestTaskInstance_MarkPendingOverdueAll verifies that MarkPendingOverdueAll
// transitions only pending instances whose due_on < asOf across ALL households
// and returns the correct count. Instances on or after asOf and instances in
// non-pending states must be untouched.
//
// This is a system-process method intentionally NOT household-scoped — the
// same precedent as [RecurringTaskRepository.ListAllActive] (NES-30) in that
// it operates across the full database rather than a single tenant.
func TestTaskInstance_MarkPendingOverdueAll(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)

	// Two separate households, each with one recurring task.
	hA, m1A, _ := seedHousehold(t, pool)
	hB, _, _ := seedHousehold(t, pool)

	rtA := seedRecurringTask(t, taskRepo, hA.ID)
	rtB := seedRecurringTask(t, taskRepo, hB.ID)

	// Seed a second recurring task for hA so we can have an additional past-due
	// instance without hitting the (recurring_task_id, due_on) unique constraint.
	rtA2 := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    hA.ID,
		Title:          "Second task A",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Points:         5,
		Active:         true,
	}
	if err := taskRepo.Create(testCtx(t), rtA2); err != nil {
		t.Fatalf("Create rtA2: %v", err)
	}

	// Past-due pending instances in both households (3 total).
	pastDueA1 := seedTaskInstance(t, instRepo, rtA, refDate.AddDate(0, 0, -3))
	pastDueA2 := seedTaskInstance(t, instRepo, rtA2, refDate.AddDate(0, 0, -1))
	pastDueB := seedTaskInstance(t, instRepo, rtB, refDate.AddDate(0, 0, -2))

	// Future pending instances — must NOT be swept.
	futureA := seedTaskInstance(t, instRepo, rtA, refDate.AddDate(0, 0, 7))
	futureB := seedTaskInstance(t, instRepo, rtB, refDate.AddDate(0, 0, 7))

	// Boundary: due_on == asOf must NOT be swept (the predicate is strict <).
	boundary := seedTaskInstance(t, instRepo, rtB, refDate)

	// A past-due done instance — must NOT flip to overdue (only pending → overdue).
	// Seed it as pending then complete it to transition it to done.
	rtA3 := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    hA.ID,
		Title:          "Third task A",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Points:         5,
		Active:         true,
	}
	if err := taskRepo.Create(testCtx(t), rtA3); err != nil {
		t.Fatalf("Create rtA3: %v", err)
	}
	doneInst := seedTaskInstance(t, instRepo, rtA3, refDate.AddDate(0, 0, -5))
	if err := instRepo.Complete(testCtx(t), hA.ID, doneInst.ID, m1A, refDate); err != nil {
		t.Fatalf("Complete doneInst: %v", err)
	}

	// Run the system-wide overdue sweep with asOf = refDate.
	// Qualifies: due_on < refDate AND status = 'pending' → pastDueA1, pastDueA2, pastDueB = 3.
	targets, err := instRepo.MarkPendingOverdueAll(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("MarkPendingOverdueAll: %v", err)
	}
	if len(targets) != 3 {
		t.Errorf("MarkPendingOverdueAll returned %d targets, want 3", len(targets))
	}
	// All returned targets must have Kind=overdue.
	for i, tgt := range targets {
		if tgt.Kind != domain.ReminderOverdue {
			t.Errorf("targets[%d].Kind = %v, want ReminderOverdue", i, tgt.Kind)
		}
		if tgt.Title == "" {
			t.Errorf("targets[%d].Title is empty, want recurring task title", i)
		}
	}

	// Past-due pending instances across both households must now be overdue.
	gotA1, err := instRepo.Get(testCtx(t), hA.ID, pastDueA1.ID)
	if err != nil {
		t.Fatalf("Get pastDueA1: %v", err)
	}
	if gotA1.Status != domain.StatusOverdue {
		t.Errorf("pastDueA1 Status = %v, want overdue", gotA1.Status)
	}

	gotA2, err := instRepo.Get(testCtx(t), hA.ID, pastDueA2.ID)
	if err != nil {
		t.Fatalf("Get pastDueA2: %v", err)
	}
	if gotA2.Status != domain.StatusOverdue {
		t.Errorf("pastDueA2 Status = %v, want overdue", gotA2.Status)
	}

	gotB, err := instRepo.Get(testCtx(t), hB.ID, pastDueB.ID)
	if err != nil {
		t.Fatalf("Get pastDueB: %v", err)
	}
	if gotB.Status != domain.StatusOverdue {
		t.Errorf("pastDueB Status = %v, want overdue", gotB.Status)
	}

	// Future instances must remain pending.
	gotFutureA, err := instRepo.Get(testCtx(t), hA.ID, futureA.ID)
	if err != nil {
		t.Fatalf("Get futureA: %v", err)
	}
	if gotFutureA.Status != domain.StatusPending {
		t.Errorf("futureA Status = %v, want pending", gotFutureA.Status)
	}

	gotFutureB, err := instRepo.Get(testCtx(t), hB.ID, futureB.ID)
	if err != nil {
		t.Fatalf("Get futureB: %v", err)
	}
	if gotFutureB.Status != domain.StatusPending {
		t.Errorf("futureB Status = %v, want pending", gotFutureB.Status)
	}

	// The due_on == asOf boundary instance must remain pending (strict <).
	gotBoundary, err := instRepo.Get(testCtx(t), hB.ID, boundary.ID)
	if err != nil {
		t.Fatalf("Get boundary: %v", err)
	}
	if gotBoundary.Status != domain.StatusPending {
		t.Errorf("boundary (due_on == asOf) Status = %v, want pending", gotBoundary.Status)
	}

	// The done instance must remain done (overdue sweep skips non-pending rows).
	gotDone, err := instRepo.Get(testCtx(t), hA.ID, doneInst.ID)
	if err != nil {
		t.Fatalf("Get doneInst: %v", err)
	}
	if gotDone.Status != domain.StatusDone {
		t.Errorf("doneInst Status = %v, want done", gotDone.Status)
	}
}

// TestTaskInstance_ClaimDueSoonReminders verifies the idempotent due-soon claim:
//   - A pending instance inside its lead-time window is returned once.
//   - A second call returns nothing for the same instance (reminded_at guards it).
//   - An instance outside the lead-time window is not returned.
func TestTaskInstance_ClaimDueSoonReminders(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	// recurring task with a 2-day lead window (same as seedRecurringTask default).
	rt := seedRecurringTask(t, taskRepo, h.ID) // LeadTimeDays = 2

	// asOf = refDate. A due-soon instance is one whose due_on is within
	// lead_time_days days of asOf, i.e. due_on <= asOf + 2 days.
	// Seed an instance due 1 day after asOf → inside the 2-day window.
	insideWindow := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 1))

	// Seed a second recurring task with its own instance due 10 days out — outside
	// the 2-day window. We need a second recurring_task to avoid the unique
	// constraint on (recurring_task_id, due_on).
	rt2 := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Mop floors",
		Category:       domain.MaintenanceCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Points:         5,
		LeadTimeDays:   2,
		Active:         true,
	}
	if err := taskRepo.Create(testCtx(t), rt2); err != nil {
		t.Fatalf("Create rt2: %v", err)
	}
	outsideWindow := seedTaskInstance(t, instRepo, rt2, refDate.AddDate(0, 0, 10))

	// First claim: must return only the inside-window instance.
	targets, err := instRepo.ClaimDueSoonReminders(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("ClaimDueSoonReminders (first): %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("ClaimDueSoonReminders (first) returned %d targets, want 1", len(targets))
	}
	if targets[0].Kind != domain.ReminderDueSoon {
		t.Errorf("targets[0].Kind = %v, want ReminderDueSoon", targets[0].Kind)
	}
	if targets[0].InstanceID != insideWindow.ID {
		t.Errorf("targets[0].InstanceID = %v, want %v (inside-window instance)", targets[0].InstanceID, insideWindow.ID)
	}
	if targets[0].Title == "" {
		t.Error("targets[0].Title is empty, want recurring task title")
	}
	if targets[0].HouseholdID != h.ID {
		t.Errorf("targets[0].HouseholdID = %v, want %v", targets[0].HouseholdID, h.ID)
	}

	// Second claim: reminded_at is now set → must return nothing (idempotency).
	targets2, err := instRepo.ClaimDueSoonReminders(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("ClaimDueSoonReminders (second): %v", err)
	}
	if len(targets2) != 0 {
		t.Errorf("ClaimDueSoonReminders (second) returned %d targets, want 0 (idempotent)", len(targets2))
	}

	// The outside-window instance must never have been claimed.
	_ = outsideWindow // present in DB but must not be returned
}

// TestTaskInstance_ClaimDueSoonReminders_OutsideWindow confirms that an
// instance whose due_on is strictly beyond the lead window is excluded.
func TestTaskInstance_ClaimDueSoonReminders_OutsideWindow(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID) // LeadTimeDays = 2

	// due_on = asOf + 3 days > lead_time_days of 2 → outside window.
	seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 3))

	targets, err := instRepo.ClaimDueSoonReminders(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("ClaimDueSoonReminders: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("ClaimDueSoonReminders returned %d targets for outside-window instance, want 0", len(targets))
	}
}

// TestTaskInstance_MarkPendingOverdueAll_ReturnsTargetsWithTitle is an
// additional assertion on top of TestTaskInstance_MarkPendingOverdueAll:
// the returned []ReminderTarget includes the recurring_task title and category
// so the caller can build notifications without an extra query.
func TestTaskInstance_MarkPendingOverdueAll_ReturnsTargetsWithTitle(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	// One past-due pending instance.
	seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, -1))

	targets, err := instRepo.MarkPendingOverdueAll(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("MarkPendingOverdueAll: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("MarkPendingOverdueAll returned %d targets, want 1", len(targets))
	}

	tgt := targets[0]
	if tgt.Kind != domain.ReminderOverdue {
		t.Errorf("Kind = %v, want ReminderOverdue", tgt.Kind)
	}
	if tgt.Title != rt.Title {
		t.Errorf("Title = %q, want %q", tgt.Title, rt.Title)
	}
	if tgt.Category != rt.Category {
		t.Errorf("Category = %v, want %v", tgt.Category, rt.Category)
	}
	if tgt.HouseholdID != h.ID {
		t.Errorf("HouseholdID = %v, want %v", tgt.HouseholdID, h.ID)
	}

	// A second call must return nothing (each row transitions only once).
	targets2, err := instRepo.MarkPendingOverdueAll(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("MarkPendingOverdueAll (second): %v", err)
	}
	if len(targets2) != 0 {
		t.Errorf("MarkPendingOverdueAll (second) returned %d targets, want 0 (idempotent)", len(targets2))
	}
}

// idsOf extracts task instance IDs for readable test error messages.
func idsOf(instances []*domain.TaskInstance) []domain.TaskInstanceID {
	ids := make([]domain.TaskInstanceID, len(instances))
	for i, inst := range instances {
		ids[i] = inst.ID
	}
	return ids
}
