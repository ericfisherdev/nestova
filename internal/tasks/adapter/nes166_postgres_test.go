package adapter_test

import (
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// The assignee-guarded transitions (NES-166). The PIN gate verifies a
// submitted PIN against the instance's CURRENT assignee, and accepting a chore
// trade swaps assignee_id on a pending instance — so the check has to live in
// the mutation's own predicate. These tests exercise that predicate against a
// real database, which is the only place it exists.

// seedAssignedInstance creates a pending instance held by assignee.
func seedAssignedInstance(
	t *testing.T,
	instRepo *adapter.TaskInstanceRepository,
	rt *domain.RecurringTask,
	assignee household.MemberID,
	dueOn time.Time,
) *domain.TaskInstance {
	t.Helper()
	// (recurring_task_id, due_on) is unique, so a test seeding two instances
	// for one task must space their due dates.
	inst := seedTaskInstance(t, instRepo, rt, dueOn)
	if err := instRepo.Claim(testCtx(t), rt.HouseholdID, inst.ID, assignee); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return inst
}

func TestCompleteAndAwardAsAssignee_CurrentAssignee(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)

	h, m1, _ := seedHousehold(t, pool)
	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedAssignedInstance(t, instRepo, rt, m1, time.Now())

	if err := instRepo.CompleteAndAwardAsAssignee(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAwardAsAssignee(current assignee) = %v, want nil", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("status = %s, want done", got.Status)
	}
	if got.CompletedBy == nil || *got.CompletedBy != m1 {
		t.Errorf("completed_by = %v, want the assignee %v", got.CompletedBy, m1)
	}
}

// TestCompleteAndAwardAsAssignee_Reassigned is the race the guard closes: the
// instance moved to the other member (as an accepted trade moves it) after the
// PIN was verified against the first.
func TestCompleteAndAwardAsAssignee_Reassigned(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)

	h, m1, m2 := seedHousehold(t, pool)
	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedAssignedInstance(t, instRepo, rt, m2, time.Now())

	err := instRepo.CompleteAndAwardAsAssignee(testCtx(t), h.ID, inst.ID, m1, time.Now())
	if !errors.Is(err, domain.ErrAssigneeChanged) {
		t.Fatalf("CompleteAndAwardAsAssignee(former assignee) = %v, want ErrAssigneeChanged", err)
	}

	got, getErr := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("status = %s, want pending: a refused completion must not transition the row", got.Status)
	}
	if got.CompletedBy != nil {
		t.Errorf("completed_by = %v, want nil", *got.CompletedBy)
	}
}

// TestCompleteAndAwardAsAssignee_Unassigned proves an instance nobody holds is
// refused too, rather than the predicate quietly matching a NULL assignee_id.
func TestCompleteAndAwardAsAssignee_Unassigned(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)

	h, m1, _ := seedHousehold(t, pool)
	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, time.Now())

	err := instRepo.CompleteAndAwardAsAssignee(testCtx(t), h.ID, inst.ID, m1, time.Now())
	if !errors.Is(err, domain.ErrAssigneeChanged) {
		t.Fatalf("CompleteAndAwardAsAssignee(unassigned) = %v, want ErrAssigneeChanged", err)
	}
}

// TestCompleteAndAwardAsAssignee_TerminalStateWins proves the guard does not
// mask the older sentinel: an already-finished chore still reports that, not a
// reassignment.
func TestCompleteAndAwardAsAssignee_TerminalStateWins(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)

	h, m1, _ := seedHousehold(t, pool)
	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedAssignedInstance(t, instRepo, rt, m1, time.Now())

	if err := instRepo.CompleteAndAwardAsAssignee(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("first completion: %v", err)
	}
	err := instRepo.CompleteAndAwardAsAssignee(testCtx(t), h.ID, inst.ID, m1, time.Now())
	if !errors.Is(err, domain.ErrInstanceInTerminalState) {
		t.Fatalf("second completion = %v, want ErrInstanceInTerminalState", err)
	}
}

func TestSkipAsAssignee_CurrentAssigneeAndReassigned(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)

	h, m1, m2 := seedHousehold(t, pool)
	rt := seedRecurringTask(t, taskRepo, h.ID)

	held := seedAssignedInstance(t, instRepo, rt, m1, time.Now())
	if err := instRepo.SkipAsAssignee(testCtx(t), h.ID, held.ID, m1); err != nil {
		t.Fatalf("SkipAsAssignee(current assignee) = %v, want nil", err)
	}
	got, err := instRepo.Get(testCtx(t), h.ID, held.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.StatusSkipped {
		t.Errorf("status = %s, want skipped", got.Status)
	}

	traded := seedAssignedInstance(t, instRepo, rt, m2, time.Now().AddDate(0, 0, 1))
	if err := instRepo.SkipAsAssignee(testCtx(t), h.ID, traded.ID, m1); !errors.Is(err, domain.ErrAssigneeChanged) {
		t.Fatalf("SkipAsAssignee(former assignee) = %v, want ErrAssigneeChanged", err)
	}
	stillOpen, err := instRepo.Get(testCtx(t), h.ID, traded.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stillOpen.Status != domain.StatusPending {
		t.Errorf("status = %s, want pending", stillOpen.Status)
	}
}
