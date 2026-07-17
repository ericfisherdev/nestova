package adapter_test

import (
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Claim — claim_warned_at lifecycle (NES-118)
// ---------------------------------------------------------------------------

// TestTaskInstance_Claim_NewClaimStartsUnwarned verifies that a brand new
// claim on a previously-unassigned instance starts with no warning sent.
func TestTaskInstance_Claim_NewClaimStartsUnwarned(t *testing.T) {
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
	if got.ClaimWarnedAt != nil {
		t.Errorf("ClaimWarnedAt = %v, want nil for a fresh claim", *got.ClaimWarnedAt)
	}
}

// TestTaskInstance_Claim_SelfClaimNeverWarned verifies that a no-risk
// self-claim (already-assigned instance) leaves claim_warned_at nil, matching
// its nil claim_expires_at.
func TestTaskInstance_Claim_SelfClaimNeverWarned(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedAssignedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7), m1)

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim(self): %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after self-claim: %v", err)
	}
	if got.ClaimWarnedAt != nil {
		t.Errorf("ClaimWarnedAt = %v, want nil (self-claim carries no risk, so nothing to warn about)", *got.ClaimWarnedAt)
	}
}

// TestTaskInstance_Claim_ReassertPreservesWarnedAt verifies that re-claiming
// an already-held claim leaves claim_warned_at UNCHANGED, mirroring
// claim_expires_at's re-assert protection: a member must not be able to
// silence or reset the warning by repeatedly calling Claim.
func TestTaskInstance_Claim_ReassertPreservesWarnedAt(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Bring the claim into its warning window and warn it.
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	warnAsOf := claimed.ClaimExpiresAt.Add(-90 * time.Minute)
	warnings, err := instRepo.ClaimWarnings(testCtx(t), warnAsOf)
	if err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("ClaimWarnings returned %d warnings, want 1", len(warnings))
	}

	warned, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after ClaimWarnings: %v", err)
	}
	if warned.ClaimWarnedAt == nil {
		t.Fatal("ClaimWarnedAt is nil after ClaimWarnings, want set")
	}

	// Re-claim the same instance as the same member.
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim (re-claim): %v", err)
	}
	reasserted, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after re-claim: %v", err)
	}
	if reasserted.ClaimWarnedAt == nil {
		t.Fatal("ClaimWarnedAt is nil after re-claim, want preserved")
	}
	if !reasserted.ClaimWarnedAt.Equal(*warned.ClaimWarnedAt) {
		t.Errorf("ClaimWarnedAt changed on re-claim: %v -> %v, want exactly preserved",
			warned.ClaimWarnedAt, reasserted.ClaimWarnedAt)
	}
}

// ---------------------------------------------------------------------------
// ClaimWarnings (NES-118 AC2)
// ---------------------------------------------------------------------------

// TestClaimWarnings_WarnsClaimEnteringWindow is the core case: a claim whose
// expiry falls inside domain.ClaimWarningWindow of asOf is returned and
// claim_warned_at is stamped.
func TestClaimWarnings_WarnsClaimEnteringWindow(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}

	// 90 minutes before expiry is comfortably inside the 2h warning window.
	asOf := claimed.ClaimExpiresAt.Add(-90 * time.Minute)
	warnings, err := instRepo.ClaimWarnings(testCtx(t), asOf)
	if err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("ClaimWarnings returned %d warnings, want 1", len(warnings))
	}
	w := warnings[0]
	if w.InstanceID != inst.ID {
		t.Errorf("InstanceID = %v, want %v", w.InstanceID, inst.ID)
	}
	if w.HouseholdID != h.ID {
		t.Errorf("HouseholdID = %v, want %v", w.HouseholdID, h.ID)
	}
	if w.ClaimedBy != m1 {
		t.Errorf("ClaimedBy = %v, want %v", w.ClaimedBy, m1)
	}
	if w.Title != rt.Title {
		t.Errorf("Title = %q, want %q", w.Title, rt.Title)
	}
	if !w.ExpiresAt.Equal(*claimed.ClaimExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", w.ExpiresAt, *claimed.ClaimExpiresAt)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after ClaimWarnings: %v", err)
	}
	if got.ClaimWarnedAt == nil {
		t.Error("ClaimWarnedAt is nil after ClaimWarnings, want stamped")
	}
}

// TestClaimWarnings_NotYetInWindowReturnsNone verifies that a claim whose
// expiry is further away than domain.ClaimWarningWindow is not yet warned.
func TestClaimWarnings_NotYetInWindowReturnsNone(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}

	// 3 hours before expiry is outside the 2h warning window.
	asOf := claimed.ClaimExpiresAt.Add(-3 * time.Hour)
	warnings, err := instRepo.ClaimWarnings(testCtx(t), asOf)
	if err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("ClaimWarnings returned %d warnings, want 0 (not yet in window)", len(warnings))
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ClaimWarnedAt != nil {
		t.Error("ClaimWarnedAt set for a claim outside its warning window")
	}
}

// TestClaimWarnings_AlreadyExpiredExcluded verifies that a claim whose
// expiry has already passed as of asOf is excluded — that claim belongs to
// SweepExpiredClaims instead.
func TestClaimWarnings_AlreadyExpiredExcluded(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	warnings, err := instRepo.ClaimWarnings(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("ClaimWarnings returned %d warnings, want 0 (claim already expired)", len(warnings))
	}
}

// TestClaimWarnings_AlreadyWarnedNotWarnedAgain verifies the idempotency
// guarantee: a second ClaimWarnings call at the same or a later in-window
// asOf never returns an already-warned claim again.
func TestClaimWarnings_AlreadyWarnedNotWarnedAgain(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	asOf := claimed.ClaimExpiresAt.Add(-90 * time.Minute)

	first, err := instRepo.ClaimWarnings(testCtx(t), asOf)
	if err != nil {
		t.Fatalf("ClaimWarnings (first): %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("ClaimWarnings (first) returned %d warnings, want 1", len(first))
	}

	second, err := instRepo.ClaimWarnings(testCtx(t), asOf)
	if err != nil {
		t.Fatalf("ClaimWarnings (second): %v", err)
	}
	if len(second) != 0 {
		t.Errorf("ClaimWarnings (second) returned %d warnings, want 0 (already warned)", len(second))
	}
}

// TestClaimWarnings_OrphanedClaimExcluded verifies that a claim whose
// claimant's member row was deleted (claimed_by nulled, claim_expires_at
// surviving) is never warned — there is no one to notify.
func TestClaimWarnings_OrphanedClaimExcluded(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	asOf := claimed.ClaimExpiresAt.Add(-90 * time.Minute)

	if _, err := pool.Exec(testCtx(t), "DELETE FROM member WHERE id = $1", m1.String()); err != nil {
		t.Fatalf("delete member: %v", err)
	}

	warnings, err := instRepo.ClaimWarnings(testCtx(t), asOf)
	if err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("ClaimWarnings returned %d warnings, want 0 (orphaned claim has no claimant to warn)", len(warnings))
	}
}

// ---------------------------------------------------------------------------
// ClearClaimWarning — recovery, scoped to the current claim window (NES-118)
// ---------------------------------------------------------------------------

// TestClearClaimWarning_ClearsMatchingWindow verifies the recovery path: a
// warned claim's claim_warned_at is reset to nil when the caller supplies the
// instance's actual, still-current claim_expires_at.
func TestClearClaimWarning_ClearsMatchingWindow(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	if _, err := instRepo.ClaimWarnings(testCtx(t), claimed.ClaimExpiresAt.Add(-90*time.Minute)); err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}
	warned, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after ClaimWarnings: %v", err)
	}
	if warned.ClaimWarnedAt == nil {
		t.Fatal("ClaimWarnedAt is nil after ClaimWarnings, want set")
	}

	if err := instRepo.ClearClaimWarning(testCtx(t), inst.ID, *warned.ClaimExpiresAt); err != nil {
		t.Fatalf("ClearClaimWarning: %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after ClearClaimWarning: %v", err)
	}
	if got.ClaimWarnedAt != nil {
		t.Errorf("ClaimWarnedAt = %v, want nil (reset by ClearClaimWarning)", *got.ClaimWarnedAt)
	}
}

// TestClearClaimWarning_NoopWhenExpiryDoesNotMatch verifies the window-scope
// guard: a ClearClaimWarning call carrying an expiresAt that does NOT match
// the instance's current claim_expires_at is a no-op — it must never clear a
// different (e.g. later, independent) claim window's warned status.
func TestClearClaimWarning_NoopWhenExpiryDoesNotMatch(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	if _, err := instRepo.ClaimWarnings(testCtx(t), claimed.ClaimExpiresAt.Add(-90*time.Minute)); err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}
	warned, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after ClaimWarnings: %v", err)
	}
	if warned.ClaimWarnedAt == nil {
		t.Fatal("ClaimWarnedAt is nil after ClaimWarnings, want set")
	}

	// A stale expiresAt (an hour off the actual claim_expires_at) simulates a
	// recovery call racing against the instance having moved to a different
	// claim window in the meantime.
	staleExpiresAt := warned.ClaimExpiresAt.Add(-time.Hour)
	if err := instRepo.ClearClaimWarning(testCtx(t), inst.ID, staleExpiresAt); err != nil {
		t.Fatalf("ClearClaimWarning: %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after ClearClaimWarning: %v", err)
	}
	if got.ClaimWarnedAt == nil {
		t.Error("ClaimWarnedAt is nil after a stale-window ClearClaimWarning, want preserved (guard should have no-op'd)")
	}
}

// TestClearClaimWarning_NoopForUnknownInstance verifies that ClearClaimWarning
// is a no-op (nil error) for an id that does not exist — recovery must be
// idempotent and tolerant of a row deleted between claim and clear.
func TestClearClaimWarning_NoopForUnknownInstance(t *testing.T) {
	pool := newTestPool(t)
	instRepo := adapter.NewTaskInstanceRepository(pool)

	if err := instRepo.ClearClaimWarning(testCtx(t), domain.NewTaskInstanceID(), time.Now()); err != nil {
		t.Errorf("ClearClaimWarning(unknown id): %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// Terminal transitions and claim revert clear claim_warned_at (NES-118)
// ---------------------------------------------------------------------------

// TestTaskInstance_CompleteAndAward_ClearsClaimWarnedAt verifies that
// completing a warned claim clears claim_warned_at along with the other
// "current claim" fields.
func TestTaskInstance_CompleteAndAward_ClearsClaimWarnedAt(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 5)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 1))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	if _, err := instRepo.ClaimWarnings(testCtx(t), claimed.ClaimExpiresAt.Add(-90*time.Minute)); err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}

	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward: %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after CompleteAndAward: %v", err)
	}
	if got.ClaimWarnedAt != nil {
		t.Errorf("ClaimWarnedAt = %v, want nil (cleared on completion)", *got.ClaimWarnedAt)
	}
}

// TestTaskInstance_Skip_ClearsClaimWarnedAt verifies that skipping a warned
// claim clears claim_warned_at along with the other "current claim" fields.
func TestTaskInstance_Skip_ClearsClaimWarnedAt(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 1))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	if _, err := instRepo.ClaimWarnings(testCtx(t), claimed.ClaimExpiresAt.Add(-90*time.Minute)); err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}

	if err := instRepo.Skip(testCtx(t), h.ID, inst.ID); err != nil {
		t.Fatalf("Skip: %v", err)
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Skip: %v", err)
	}
	if got.ClaimWarnedAt != nil {
		t.Errorf("ClaimWarnedAt = %v, want nil (cleared on skip)", *got.ClaimWarnedAt)
	}
}

// TestSweepExpiredClaims_ClearsClaimWarnedAt verifies that a warned claim
// which subsequently expires has its claim_warned_at cleared by the revert,
// alongside claimed_by/claimed_at/claim_expires_at.
func TestSweepExpiredClaims_ClearsClaimWarnedAt(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after Claim: %v", err)
	}
	if _, err := instRepo.ClaimWarnings(testCtx(t), claimed.ClaimExpiresAt.Add(-90*time.Minute)); err != nil {
		t.Fatalf("ClaimWarnings: %v", err)
	}

	claims, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("SweepExpiredClaims returned %d claims, want 1", len(claims))
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after sweep: %v", err)
	}
	if got.ClaimWarnedAt != nil {
		t.Errorf("ClaimWarnedAt = %v, want nil (cleared on revert)", *got.ClaimWarnedAt)
	}
}

// ---------------------------------------------------------------------------
// PointLedgerRepository.History (NES-118 AC4)
// ---------------------------------------------------------------------------

// TestPointLedger_History_TaskCompletionAward verifies that a task-completion
// award entry is enriched with the parent recurring task's title.
func TestPointLedger_History_TaskCompletionAward(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 5)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 1))
	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward: %v", err)
	}

	entries, err := ledgerRepo.History(testCtx(t), h.ID, m1, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("History = %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.SourceType != "task_instance" {
		t.Errorf("SourceType = %q, want task_instance", e.SourceType)
	}
	if e.Points != 5 {
		t.Errorf("Points = %d, want 5", e.Points)
	}
	if e.TaskTitle != rt.Title {
		t.Errorf("TaskTitle = %q, want %q", e.TaskTitle, rt.Title)
	}
	if e.RewardName != "" {
		t.Errorf("RewardName = %q, want empty for a task completion", e.RewardName)
	}
}

// TestPointLedger_History_ClaimExpiryPenalty verifies that a claim-expiry
// penalty entry is enriched with the parent recurring task's title so the
// caller can render "Claim expired: <title>" (NES-118 AC4).
func TestPointLedger_History_ClaimExpiryPenalty(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10) // penalty = 5
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf()); err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}

	entries, err := ledgerRepo.History(testCtx(t), h.ID, m1, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("History = %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.SourceType != domain.SourceTypeClaimExpiry {
		t.Errorf("SourceType = %q, want %q", e.SourceType, domain.SourceTypeClaimExpiry)
	}
	if e.Points != -5 {
		t.Errorf("Points = %d, want -5", e.Points)
	}
	if e.TaskTitle != rt.Title {
		t.Errorf("TaskTitle = %q, want %q", e.TaskTitle, rt.Title)
	}
}

// TestPointLedger_History_RedemptionDebit verifies that a redemption debit
// entry is enriched with the reward's name.
func TestPointLedger_History_RedemptionDebit(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	appendEntry(t, ledgerRepo, h.ID, m1, 100, time.Now().UTC().Add(-time.Hour))
	reward := seedReward(t, rewardRepo, h.ID, "Movie night pick", 20)

	redemption := &domain.RewardRedemption{
		ID:          domain.NewRewardRedemptionID(),
		HouseholdID: h.ID,
		RewardID:    reward.ID,
		MemberID:    m1,
		Status:      domain.RedemptionPending,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), redemption); err != nil {
		t.Fatalf("RedeemWithDebit: %v", err)
	}

	entries, err := ledgerRepo.History(testCtx(t), h.ID, m1, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("History = %d entries, want 2 (award + redemption)", len(entries))
	}
	// Newest first: the redemption debit was appended after the manual award.
	e := entries[0]
	if e.SourceType != "redemption" {
		t.Errorf("SourceType = %q, want redemption", e.SourceType)
	}
	if e.Points != -20 {
		t.Errorf("Points = %d, want -20", e.Points)
	}
	if e.RewardName != reward.Name {
		t.Errorf("RewardName = %q, want %q", e.RewardName, reward.Name)
	}
	if e.TaskTitle != "" {
		t.Errorf("TaskTitle = %q, want empty for a redemption", e.TaskTitle)
	}
}

// TestPointLedger_History_OrderedNewestFirstAndLimited verifies that History
// orders entries by created_at descending and truncates to limit.
func TestPointLedger_History_OrderedNewestFirstAndLimited(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		appendEntry(t, ledgerRepo, h.ID, m1, i+1, base.AddDate(0, 0, i))
	}

	entries, err := ledgerRepo.History(testCtx(t), h.ID, m1, 3)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("History = %d entries, want 3 (limit applied)", len(entries))
	}
	// Newest first: points 5, 4, 3 (days 4, 3, 2 after base).
	wantPoints := []int{5, 4, 3}
	for i, e := range entries {
		if e.Points != wantPoints[i] {
			t.Errorf("entries[%d].Points = %d, want %d (newest-first order)", i, e.Points, wantPoints[i])
		}
	}
}

// TestPointLedger_History_EmptyReturnsSlice verifies that History returns an
// empty slice (not an error) for a member with no ledger entries.
func TestPointLedger_History_EmptyReturnsSlice(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	entries, err := ledgerRepo.History(testCtx(t), h.ID, m1, 10)
	if err != nil {
		t.Fatalf("History(empty): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("History(empty) = %d entries, want 0", len(entries))
	}
}

// TestPointLedger_History_CrossHouseholdIsolation verifies that History never
// leaks another household's entries for the same member id (which cannot
// legitimately happen given tenant-scoped member ids, but the query's WHERE
// clause is exercised directly here for defence-in-depth).
func TestPointLedger_History_CrossHouseholdIsolation(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	hA, mA, _ := seedHousehold(t, pool)
	hB, _, _ := seedHousehold(t, pool)

	appendEntry(t, ledgerRepo, hA.ID, mA, 10, time.Now().UTC())

	entries, err := ledgerRepo.History(testCtx(t), hB.ID, mA, 10)
	if err != nil {
		t.Fatalf("History(hB, mA): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("History(hB, mA) = %d entries, want 0 (cross-household isolation)", len(entries))
	}
}
