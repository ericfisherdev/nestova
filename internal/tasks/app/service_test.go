package app_test

import (
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// TaskService.CompleteInstance hermetic tests
// ---------------------------------------------------------------------------

// TestService_CompleteInstance_CallsCompleteAndAward verifies that the
// TaskService.CompleteInstance use-case delegates to CompleteAndAward on the
// instance repository. The fakeTaskInstanceRepo defined in generator_test.go
// implements CompleteAndAward and transitions the instance to done; we assert
// the instance is marked done after the call.
func TestService_CompleteInstance_CallsCompleteAndAward(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instRepo, nil)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h := household.NewHouseholdID()
	m := household.NewMemberID()

	// Seed a recurring task and a pending instance.
	rt := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: h,
		Points:      10,
		Active:      true,
	}
	if err := taskRepo.Create(t.Context(), rt); err != nil {
		t.Fatalf("Create: %v", err)
	}

	inst := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h,
		DueOn:           domain.DueOnPtr(time.Now()),
		Status:          domain.StatusPending,
	}
	if err := instRepo.Insert(t.Context(), inst); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	at := time.Now()
	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, at); err != nil {
		t.Fatalf("CompleteInstance: %v", err)
	}

	// The fake marks the instance done when CompleteAndAward is called.
	got, err := instRepo.Get(t.Context(), h, inst.ID)
	if err != nil {
		t.Fatalf("Get after CompleteInstance: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}
	if got.CompletedBy == nil || *got.CompletedBy != m {
		t.Errorf("CompletedBy = %v, want %v", got.CompletedBy, m)
	}
}

// TestService_CompleteInstance_OverdueAccepted verifies that an overdue
// instance is still completable: CompleteInstance succeeds and transitions it
// to done (an overdue chore can be completed late).
func TestService_CompleteInstance_OverdueAccepted(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instRepo, nil)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h := household.NewHouseholdID()
	m := household.NewMemberID()

	// Seed a recurring task and an OVERDUE instance.
	rt := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: h,
		Points:      10,
		Active:      true,
	}
	if err := taskRepo.Create(t.Context(), rt); err != nil {
		t.Fatalf("Create: %v", err)
	}

	inst := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h,
		DueOn:           domain.DueOnPtr(time.Now().AddDate(0, 0, -1)),
		Status:          domain.StatusOverdue,
	}
	if err := instRepo.Insert(t.Context(), inst); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	at := time.Now()
	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, at); err != nil {
		t.Fatalf("CompleteInstance(overdue): %v", err)
	}

	got, err := instRepo.Get(t.Context(), h, inst.ID)
	if err != nil {
		t.Fatalf("Get after CompleteInstance(overdue): %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}
	if got.CompletedBy == nil || *got.CompletedBy != m {
		t.Errorf("CompletedBy = %v, want %v", got.CompletedBy, m)
	}
}

// TestService_CompleteInstance_TerminalStatePropagated verifies that when the
// instance is already done (terminal state), CompleteInstance returns
// ErrInstanceInTerminalState without performing a second award.
func TestService_CompleteInstance_TerminalStatePropagated(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instRepo, nil)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h := household.NewHouseholdID()
	m := household.NewMemberID()

	rt := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: h,
		Points:      5,
		Active:      true,
	}
	if err := taskRepo.Create(t.Context(), rt); err != nil {
		t.Fatalf("Create: %v", err)
	}

	inst := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h,
		DueOn:           domain.DueOnPtr(time.Now()),
		Status:          domain.StatusPending,
	}
	if err := instRepo.Insert(t.Context(), inst); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// First completion succeeds.
	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); err != nil {
		t.Fatalf("CompleteInstance (first): %v", err)
	}

	// Second completion must return ErrInstanceInTerminalState.
	err = svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now())
	if !errors.Is(err, domain.ErrInstanceInTerminalState) {
		t.Errorf("CompleteInstance (re-completion) = %v, want ErrInstanceInTerminalState", err)
	}
}

// TestService_CompleteInstance_NotFound verifies that CompleteInstance returns
// ErrInstanceNotFound when the instance id is unknown.
func TestService_CompleteInstance_NotFound(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instRepo, nil)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h := household.NewHouseholdID()
	m := household.NewMemberID()

	err = svc.CompleteInstance(t.Context(), h, domain.NewTaskInstanceID(), m, time.Now())
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("CompleteInstance(unknown) = %v, want ErrInstanceNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// NES-120 photo policy gate
// ---------------------------------------------------------------------------

// seedPolicyTaskAndInstance creates a pending instance of a recurring task
// with the given PhotoPolicy and returns both, for the photo-gate tests
// below.
func seedPolicyTaskAndInstance(
	t *testing.T,
	taskRepo *fakeRecurringTaskRepo,
	instRepo *fakeTaskInstanceRepo,
	h household.HouseholdID,
	policy domain.PhotoPolicy,
) *domain.TaskInstance {
	t.Helper()
	rt := &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: h,
		Points:      10,
		Active:      true,
		PhotoPolicy: policy,
	}
	if err := taskRepo.Create(t.Context(), rt); err != nil {
		t.Fatalf("Create: %v", err)
	}
	inst := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     h,
		DueOn:           domain.DueOnPtr(time.Now()),
		Status:          domain.StatusPending,
	}
	if err := instRepo.Insert(t.Context(), inst); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return inst
}

// TestService_CompleteInstance_BeforeAfterPolicy_BlocksUntilBothPhotos
// verifies AC1: a before_after task cannot be completed with no photos, nor
// with only a before photo, and succeeds only once both exist.
func TestService_CompleteInstance_BeforeAfterPolicy_BlocksUntilBothPhotos(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newFakeTaskInstanceRepo()
	photoChecker := newFakeProofPhotoChecker()

	svc, err := app.NewTaskService(taskRepo, instRepo, photoChecker)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h := household.NewHouseholdID()
	m := household.NewMemberID()
	inst := seedPolicyTaskAndInstance(t, taskRepo, instRepo, h, domain.PhotoPolicyBeforeAfter)

	// No photos yet.
	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); !errors.Is(err, domain.ErrBeforePhotoRequired) {
		t.Errorf("CompleteInstance(no photos) = %v, want ErrBeforePhotoRequired", err)
	}

	// Before only.
	photoChecker.seed(inst.ID, "before-id", "")
	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); !errors.Is(err, domain.ErrAfterPhotoRequired) {
		t.Errorf("CompleteInstance(before only) = %v, want ErrAfterPhotoRequired", err)
	}

	// Both photos.
	photoChecker.seed(inst.ID, "before-id", "after-id")
	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); err != nil {
		t.Errorf("CompleteInstance(both photos) = %v, want nil", err)
	}
	got, err := instRepo.Get(t.Context(), h, inst.ID)
	if err != nil {
		t.Fatalf("Get after CompleteInstance: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}
}

// TestService_CompleteInstance_AfterOnlyPolicy_RequiresOnlyAfter verifies
// AC1: an after_only task is blocked with no after photo (even with a
// before photo present) and succeeds once the after photo exists — the
// before photo is never required.
func TestService_CompleteInstance_AfterOnlyPolicy_RequiresOnlyAfter(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newFakeTaskInstanceRepo()
	photoChecker := newFakeProofPhotoChecker()

	svc, err := app.NewTaskService(taskRepo, instRepo, photoChecker)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h := household.NewHouseholdID()
	m := household.NewMemberID()
	inst := seedPolicyTaskAndInstance(t, taskRepo, instRepo, h, domain.PhotoPolicyAfterOnly)

	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); !errors.Is(err, domain.ErrAfterPhotoRequired) {
		t.Errorf("CompleteInstance(no after photo) = %v, want ErrAfterPhotoRequired", err)
	}

	// A before photo alone (never required for after_only) still leaves the
	// gate unsatisfied.
	photoChecker.seed(inst.ID, "before-id", "")
	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); !errors.Is(err, domain.ErrAfterPhotoRequired) {
		t.Errorf("CompleteInstance(before only, after_only policy) = %v, want ErrAfterPhotoRequired", err)
	}

	// After photo alone is sufficient — before is never required.
	photoChecker.seed(inst.ID, "", "after-id")
	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); err != nil {
		t.Errorf("CompleteInstance(after only) = %v, want nil", err)
	}
}

// TestService_CompleteInstance_NonePolicy_BehavesAsToday verifies AC1: a
// task with PhotoPolicy none (the default for every task predating NES-120)
// completes exactly as before NES-120 — including with a nil photoChecker,
// proving a household with no photo-policy tasks is entirely unaffected by
// the feature.
func TestService_CompleteInstance_NonePolicy_BehavesAsToday(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instRepo, nil)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h := household.NewHouseholdID()
	m := household.NewMemberID()
	inst := seedPolicyTaskAndInstance(t, taskRepo, instRepo, h, domain.PhotoPolicyNone)

	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); err != nil {
		t.Errorf("CompleteInstance(none policy, nil checker) = %v, want nil", err)
	}
}

// TestService_CompleteInstance_PhotoPolicy_NilCheckerFailsClosed verifies
// that a task requiring photos with no photoChecker configured never
// silently allows completion — the instance must remain pending/incomplete,
// not be marked done despite the misconfiguration.
func TestService_CompleteInstance_PhotoPolicy_NilCheckerFailsClosed(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newFakeTaskInstanceRepo()

	svc, err := app.NewTaskService(taskRepo, instRepo, nil)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h := household.NewHouseholdID()
	m := household.NewMemberID()
	inst := seedPolicyTaskAndInstance(t, taskRepo, instRepo, h, domain.PhotoPolicyAfterOnly)

	if err := svc.CompleteInstance(t.Context(), h, inst.ID, m, time.Now()); err == nil {
		t.Fatal("CompleteInstance(photo policy, nil checker) = nil error, want a fail-closed error")
	}
	got, err := instRepo.Get(t.Context(), h, inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("Status = %v, want pending (completion must not silently succeed)", got.Status)
	}
}
