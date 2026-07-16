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

	svc, err := app.NewTaskService(taskRepo, instRepo)
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

	svc, err := app.NewTaskService(taskRepo, instRepo)
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

	svc, err := app.NewTaskService(taskRepo, instRepo)
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

	svc, err := app.NewTaskService(taskRepo, instRepo)
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
