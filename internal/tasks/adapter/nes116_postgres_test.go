package adapter_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

// seedAsNeededTaskWithPoints creates and persists a claimable, as-needed
// recurring task via CreateWithRotation — the atomic, production creation
// path that also materialises the task's initial standing instance
// (NES-116). RotationClaimable is the only policy an as-needed task accepts.
func seedAsNeededTaskWithPoints(
	t *testing.T,
	repo *adapter.RecurringTaskRepository,
	householdID household.HouseholdID,
	points int,
) *domain.RecurringTask {
	t.Helper()
	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    householdID,
		Title:          "Refill the soap dispenser",
		Category:       domain.ChoreCategory,
		Cadence:        newAsNeededCadence(),
		RotationPolicy: domain.RotationClaimable,
		Points:         points,
		Active:         true,
	}
	if err := repo.CreateWithRotation(testCtx(t), rt, nil); err != nil {
		t.Fatalf("seedAsNeededTaskWithPoints: CreateWithRotation: %v", err)
	}
	return rt
}

// ---------------------------------------------------------------------------
// Insert / Get round trip
// ---------------------------------------------------------------------------

// TestTaskInstance_InsertStandingAndGet verifies that a standing instance
// round-trips through Insert and Get with a nil DueOn and kind=standing
// preserved, independent of the CreateWithRotation seam that materialises one
// automatically on task creation.
func TestTaskInstance_InsertStandingAndGet(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Refill the soap dispenser",
		Category:       domain.ChoreCategory,
		Cadence:        newAsNeededCadence(),
		RotationPolicy: domain.RotationClaimable,
		Active:         true,
	}
	if err := taskRepo.Create(testCtx(t), rt); err != nil {
		t.Fatalf("Create: %v", err)
	}

	inst := seedStandingInstance(t, instRepo, rt)

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != domain.KindStanding {
		t.Errorf("Kind = %v, want standing", got.Kind)
	}
	if got.DueOn != nil {
		t.Errorf("DueOn = %v, want nil", got.DueOn)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("Status = %v, want pending", got.Status)
	}
}

// ---------------------------------------------------------------------------
// CreateWithRotation — initial standing instance (NES-116)
// ---------------------------------------------------------------------------

// TestRecurringTask_CreateWithRotation_AsNeededSeedsStandingInstance verifies
// the AC2 "before" half: creating an as-needed task materialises exactly one
// open (pending, kind=standing, due_on=NULL) instance in the same transaction
// as the task itself.
func TestRecurringTask_CreateWithRotation_AsNeededSeedsStandingInstance(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	rt := seedAsNeededTaskWithPoints(t, taskRepo, h.ID, 3)

	standing, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStanding: %v", err)
	}
	if len(standing) != 1 {
		t.Fatalf("ListStanding = %d instances, want 1", len(standing))
	}
	got := standing[0]
	if got.RecurringTaskID != rt.ID {
		t.Errorf("standing instance RecurringTaskID = %v, want %v", got.RecurringTaskID, rt.ID)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("standing instance Status = %v, want pending", got.Status)
	}
	if got.Kind != domain.KindStanding {
		t.Errorf("standing instance Kind = %v, want standing", got.Kind)
	}
	if got.DueOn != nil {
		t.Errorf("standing instance DueOn = %v, want nil", got.DueOn)
	}
	if got.AssigneeID != nil {
		t.Errorf("standing instance AssigneeID = %v, want nil (unclaimed)", got.AssigneeID)
	}
}

// TestRecurringTask_CreateWithRotation_ScheduledCadenceNoStandingInstance is
// the control case: a normal (non-as-needed) task creates no standing
// instance at all.
func TestRecurringTask_CreateWithRotation_ScheduledCadenceNoStandingInstance(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	if err := taskRepo.CreateWithRotation(testCtx(t), &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Vacuum living room",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Active:         true,
	}, nil); err != nil {
		t.Fatalf("CreateWithRotation: %v", err)
	}

	standing, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStanding: %v", err)
	}
	if len(standing) != 0 {
		t.Errorf("ListStanding = %d instances, want 0 for a scheduled-cadence task", len(standing))
	}
}

// ---------------------------------------------------------------------------
// CompleteAndAward — standing instance respawn (NES-116)
// ---------------------------------------------------------------------------

// TestCompleteAndAward_StandingInstanceRespawns is the AC3 regression test:
// completing the standing instance awards points and materialises a fresh
// standing instance for the same task, in the same transaction as the
// completion.
func TestCompleteAndAward_StandingInstanceRespawns(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedAsNeededTaskWithPoints(t, taskRepo, h.ID, 4)
	before, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil || len(before) != 1 {
		t.Fatalf("ListStanding (before) = %v, %v, want 1 instance", before, err)
	}
	original := before[0]

	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, original.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward: %v", err)
	}

	// The original instance is now done.
	got, err := instRepo.Get(testCtx(t), h.ID, original.ID)
	if err != nil {
		t.Fatalf("Get after CompleteAndAward: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("original instance Status = %v, want done", got.Status)
	}

	// Points were awarded.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 4 {
		t.Errorf("Balance = %d, want 4", balance)
	}

	// Exactly one open standing instance exists again, and it is a fresh row.
	after, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStanding (after): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("ListStanding (after) = %d instances, want 1", len(after))
	}
	if after[0].ID == original.ID {
		t.Error("respawned standing instance has the same id as the completed one, want a fresh row")
	}
	if after[0].RecurringTaskID != rt.ID {
		t.Errorf("respawned instance RecurringTaskID = %v, want %v", after[0].RecurringTaskID, rt.ID)
	}
	if after[0].DueOn != nil {
		t.Errorf("respawned instance DueOn = %v, want nil", after[0].DueOn)
	}
}

// TestCompleteAndAward_StandingInstance_ConcurrentCompletion is the AC4
// regression test: two concurrent completions of the same standing instance
// resolve to exactly one success and one ErrInstanceInTerminalState, with
// points awarded exactly once.
func TestCompleteAndAward_StandingInstance_ConcurrentCompletion(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	seedAsNeededTaskWithPoints(t, taskRepo, h.ID, 7)
	standing, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil || len(standing) != 1 {
		t.Fatalf("ListStanding (seed) = %v, %v, want 1 instance", standing, err)
	}
	instanceID := standing[0].ID

	var (
		wg         sync.WaitGroup
		errs       = make([]error, 2)
		completers = [2]household.MemberID{m1, m2}
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = instRepo.CompleteAndAward(context.Background(), h.ID, instanceID, completers[i], time.Now())
		}(i)
	}
	wg.Wait()

	successes, terminalErrs := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrInstanceInTerminalState):
			terminalErrs++
		default:
			t.Errorf("unexpected error from concurrent CompleteAndAward: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if terminalErrs != 1 {
		t.Errorf("ErrInstanceInTerminalState count = %d, want 1", terminalErrs)
	}

	// Points awarded exactly once, regardless of which member won the race.
	total := 0
	for _, m := range completers {
		balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m)
		if err != nil {
			t.Fatalf("Balance(%v): %v", m, err)
		}
		total += balance
	}
	if total != 7 {
		t.Errorf("total balance across both members = %d, want 7 (points awarded exactly once)", total)
	}

	// Exactly one open standing instance remains — the respawn happened once,
	// not zero or twice.
	after, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStanding (after race): %v", err)
	}
	if len(after) != 1 {
		t.Errorf("ListStanding (after race) = %d instances, want 1", len(after))
	}
}

// ---------------------------------------------------------------------------
// Standing instances never overdue, never remind (NES-116 AC5)
// ---------------------------------------------------------------------------

// TestTaskInstance_MarkPendingOverdueAll_ExcludesStanding verifies that a
// standing instance is never transitioned to overdue, even when asOf is far
// in the future.
func TestTaskInstance_MarkPendingOverdueAll_ExcludesStanding(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	seedAsNeededTaskWithPoints(t, taskRepo, h.ID, 1)
	standing, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil || len(standing) != 1 {
		t.Fatalf("ListStanding (seed) = %v, %v, want 1 instance", standing, err)
	}

	// A year in the future — would sweep any scheduled instance with a past due_on.
	farFuture := refDate.AddDate(1, 0, 0)
	if _, err := instRepo.MarkPendingOverdueAll(testCtx(t), farFuture); err != nil {
		t.Fatalf("MarkPendingOverdueAll: %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, standing[0].ID)
	if err != nil {
		t.Fatalf("Get after MarkPendingOverdueAll: %v", err)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("standing instance Status = %v, want pending (never overdue)", got.Status)
	}
}

// TestTaskInstance_ClaimDueSoonReminders_ExcludesStanding verifies that a
// standing instance never enters the due-soon reminder stream.
func TestTaskInstance_ClaimDueSoonReminders_ExcludesStanding(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	seedAsNeededTaskWithPoints(t, taskRepo, h.ID, 1)

	targets, err := instRepo.ClaimDueSoonReminders(testCtx(t), refDate)
	if err != nil {
		t.Fatalf("ClaimDueSoonReminders: %v", err)
	}
	for _, tgt := range targets {
		if tgt.HouseholdID == h.ID {
			t.Errorf("ClaimDueSoonReminders returned a target for the as-needed household's standing instance: %+v", tgt)
		}
	}
}

// ---------------------------------------------------------------------------
// ListStanding scoping
// ---------------------------------------------------------------------------

// TestTaskInstance_ListStanding_HouseholdScoped verifies that ListStanding
// only returns pending standing instances for the requested household, and
// excludes a standing instance that has already been completed.
func TestTaskInstance_ListStanding_HouseholdScoped(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	hA, mA, _ := seedHousehold(t, pool)
	hB, _, _ := seedHousehold(t, pool)

	seedAsNeededTaskWithPoints(t, taskRepo, hA.ID, 1)
	seedAsNeededTaskWithPoints(t, taskRepo, hB.ID, 1)

	// Complete household A's standing instance; its replacement should still
	// be the only row ListStanding(hA) returns.
	beforeA, err := instRepo.ListStanding(testCtx(t), hA.ID)
	if err != nil || len(beforeA) != 1 {
		t.Fatalf("ListStanding(hA, seed) = %v, %v, want 1 instance", beforeA, err)
	}
	if err := instRepo.CompleteAndAward(testCtx(t), hA.ID, beforeA[0].ID, mA, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward: %v", err)
	}

	afterA, err := instRepo.ListStanding(testCtx(t), hA.ID)
	if err != nil {
		t.Fatalf("ListStanding(hA, after): %v", err)
	}
	if len(afterA) != 1 {
		t.Errorf("ListStanding(hA, after) = %d instances, want 1 (the respawned one)", len(afterA))
	}
	if len(afterA) == 1 && afterA[0].ID == beforeA[0].ID {
		t.Error("ListStanding(hA, after) still returns the completed instance")
	}

	gotB, err := instRepo.ListStanding(testCtx(t), hB.ID)
	if err != nil {
		t.Fatalf("ListStanding(hB): %v", err)
	}
	if len(gotB) != 1 {
		t.Errorf("ListStanding(hB) = %d instances, want 1 (household A's completion must not affect household B)", len(gotB))
	}
}
