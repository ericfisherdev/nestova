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

// seedAssignedTaskInstance creates and persists a pending task instance that
// is already assigned to assignee at insert time, mirroring how the
// generator materialises a fixed/round-robin instance (NES-117: this is the
// shape whose Claim call must be a no-expiry self-claim, and must reject any
// other member).
func seedAssignedTaskInstance(
	t *testing.T,
	repo *adapter.TaskInstanceRepository,
	rt *domain.RecurringTask,
	dueOn time.Time,
	assignee household.MemberID,
) *domain.TaskInstance {
	t.Helper()
	inst := &domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rt.ID,
		HouseholdID:     rt.HouseholdID,
		AssigneeID:      &assignee,
		DueOn:           domain.DueOnPtr(dueOn),
		Status:          domain.StatusPending,
	}
	if err := repo.Insert(testCtx(t), inst); err != nil {
		t.Fatalf("seedAssignedTaskInstance: Insert: %v", err)
	}
	return inst
}

// farFutureAsOf is a real-wall-clock instant far enough ahead that any claim
// made "now" during a test (claim_expires_at = now() + 12h, using Postgres's
// real now()) is guaranteed to have already expired. refDate cannot be used
// here — SweepExpiredClaims compares against claim_expires_at, which is
// always derived from the database's actual clock, not the tests' fixed
// refDate.
func farFutureAsOf() time.Time {
	return time.Now().AddDate(1, 0, 0)
}

// ---------------------------------------------------------------------------
// Claim — NES-117 expiry semantics
// ---------------------------------------------------------------------------

// TestTaskInstance_Claim_UnassignedSetsExpiry verifies AC1's first half:
// claiming a chore that was not originally assigned to anyone (assignee_id
// was NULL) records the claim and sets claim_expires_at exactly
// domain.ClaimWindow after claimed_at.
func TestTaskInstance_Claim_UnassignedSetsExpiry(t *testing.T) {
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
	if got.ClaimedBy == nil || *got.ClaimedBy != m1 {
		t.Errorf("ClaimedBy = %v, want %v", got.ClaimedBy, m1)
	}
	if got.ClaimedAt == nil {
		t.Fatal("ClaimedAt is nil, want set")
	}
	if got.ClaimExpiresAt == nil {
		t.Fatal("ClaimExpiresAt is nil, want set (claim on a previously-unassigned instance)")
	}
	// claimed_at and claim_expires_at are both derived from the same
	// statement's now(), which Postgres holds constant for the whole
	// transaction, so the gap is exactly domain.ClaimWindow.
	if gap := got.ClaimExpiresAt.Sub(*got.ClaimedAt); gap != domain.ClaimWindow {
		t.Errorf("ClaimExpiresAt - ClaimedAt = %v, want %v", gap, domain.ClaimWindow)
	}
}

// TestTaskInstance_Claim_SelfClaimSetsNoExpiry verifies AC1's second half:
// claiming a chore already assigned to the claiming member (a
// fixed/round-robin instance's own assignee) records the claim but sets no
// expiry, and leaves assignee_id unchanged.
func TestTaskInstance_Claim_SelfClaimSetsNoExpiry(t *testing.T) {
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
	if got.AssigneeID == nil || *got.AssigneeID != m1 {
		t.Errorf("AssigneeID = %v, want unchanged %v", got.AssigneeID, m1)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != m1 {
		t.Errorf("ClaimedBy = %v, want %v", got.ClaimedBy, m1)
	}
	if got.ClaimedAt == nil {
		t.Error("ClaimedAt is nil, want set (self-claim still records the claim)")
	}
	if got.ClaimExpiresAt != nil {
		t.Errorf("ClaimExpiresAt = %v, want nil (self-claim carries no risk)", *got.ClaimExpiresAt)
	}
}

// TestTaskInstance_Claim_AssignedToDifferentMemberRejected verifies that a
// pre-assigned instance (e.g. a fixed/round-robin instance's own assignee
// slot) still cannot be taken over by a different member — the NES-117
// widened WHERE clause only admits NULL or a matching assignee_id.
func TestTaskInstance_Claim_AssignedToDifferentMemberRejected(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedAssignedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7), m1)

	err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m2)
	if err == nil {
		t.Fatal("Claim(different member on pre-assigned instance) = nil error, want ErrInstanceAlreadyClaimed")
	}
	if !errors.Is(err, domain.ErrInstanceAlreadyClaimed) {
		t.Errorf("Claim(different member) = %v, want ErrInstanceAlreadyClaimed", err)
	}

	// The instance must be untouched by the rejected attempt.
	got, getErr := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if getErr != nil {
		t.Fatalf("Get after rejected claim: %v", getErr)
	}
	if got.AssigneeID == nil || *got.AssigneeID != m1 {
		t.Errorf("AssigneeID = %v, want unchanged %v", got.AssigneeID, m1)
	}
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil (rejected claim must not record anything)", got.ClaimedBy)
	}
}

// TestTaskInstance_Claim_ReclaimPreservesExpiry is the CodeRabbit-flagged
// CRITICAL regression: claiming an already-actively-claimed instance again
// (the same member calling Claim a second time) must NOT reset or clear
// claim_expires_at. Before the fix, a re-claim matched the self-claim branch
// (claimed_by already equals the caller) and wiped claim_expires_at to NULL,
// letting a member evade the penalty indefinitely by re-claiming their own
// claim right before it expired. The fix preserves claimed_at/
// claim_expires_at exactly when an active claim by the same member already
// exists, and the sweep must still catch and penalize the (unaltered)
// original expiry.
func TestTaskInstance_Claim_ReclaimPreservesExpiry(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10) // penalty = 5
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim (first): %v", err)
	}
	first, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after first claim: %v", err)
	}
	if first.ClaimedAt == nil || first.ClaimExpiresAt == nil {
		t.Fatal("first claim did not record ClaimedAt/ClaimExpiresAt")
	}

	// Re-claim: same member, same instance, still within the active window.
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim (re-claim): %v", err)
	}
	second, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after re-claim: %v", err)
	}
	if second.ClaimedAt == nil {
		t.Fatal("ClaimedAt is nil after re-claim, want preserved")
	}
	if second.ClaimExpiresAt == nil {
		t.Fatal("ClaimExpiresAt is nil after re-claim, want preserved (evasion bug: timer was reset/cleared)")
	}
	if !second.ClaimedAt.Equal(*first.ClaimedAt) {
		t.Errorf("ClaimedAt changed on re-claim: %v -> %v, want exactly preserved", first.ClaimedAt, second.ClaimedAt)
	}
	if !second.ClaimExpiresAt.Equal(*first.ClaimExpiresAt) {
		t.Errorf("ClaimExpiresAt changed on re-claim: %v -> %v, want exactly preserved (evasion bug)",
			first.ClaimExpiresAt, second.ClaimExpiresAt)
	}

	// The original expiry must still be enforceable: the sweep still catches
	// and penalizes it, proving the re-claim did not silently extend the
	// member's risk-free window.
	claims, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("SweepExpiredClaims returned %d claims, want 1 (re-claim must not evade the penalty)", len(claims))
	}
	if claims[0].PenaltyPoints != 5 {
		t.Errorf("PenaltyPoints = %d, want 5", claims[0].PenaltyPoints)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != -5 {
		t.Errorf("Balance = %d, want -5 (re-claim did not evade the penalty)", balance)
	}
}

// ---------------------------------------------------------------------------
// SweepExpiredClaims — revert, penalty, idempotency (NES-117 AC2, AC3, AC5)
// ---------------------------------------------------------------------------

// TestSweepExpiredClaims_RevertsAndPenalizesHalfPoints is the AC2 core case:
// an expired claim is released within the swept interval, the ledger shows a
// negative entry of exactly half the task's points, and the leaderboard
// reflects it.
func TestSweepExpiredClaims_RevertsAndPenalizesHalfPoints(t *testing.T) {
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

	claims, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("SweepExpiredClaims returned %d claims, want 1", len(claims))
	}
	claim := claims[0]
	if claim.InstanceID != inst.ID {
		t.Errorf("claim.InstanceID = %v, want %v", claim.InstanceID, inst.ID)
	}
	if claim.ClaimedBy != m1 {
		t.Errorf("claim.ClaimedBy = %v, want %v", claim.ClaimedBy, m1)
	}
	if claim.PenaltyPoints != 5 {
		t.Errorf("claim.PenaltyPoints = %d, want 5 (half of 10)", claim.PenaltyPoints)
	}
	if claim.Title != rt.Title {
		t.Errorf("claim.Title = %q, want %q", claim.Title, rt.Title)
	}

	// The instance reverted to the pool: unassigned, unclaimed, still pending.
	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after sweep: %v", err)
	}
	if got.AssigneeID != nil {
		t.Errorf("AssigneeID = %v, want nil (reverted to pool)", got.AssigneeID)
	}
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil", got.ClaimedBy)
	}
	if got.ClaimedAt != nil {
		t.Errorf("ClaimedAt = %v, want nil", got.ClaimedAt)
	}
	if got.ClaimExpiresAt != nil {
		t.Errorf("ClaimExpiresAt = %v, want nil", got.ClaimExpiresAt)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("Status = %v, want pending (expiry does not change status)", got.Status)
	}

	// The ledger and leaderboard both reflect the penalty.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != -5 {
		t.Errorf("Balance = %d, want -5", balance)
	}

	board, err := ledgerRepo.Leaderboard(testCtx(t), h.ID, refDate.AddDate(-1, 0, 0))
	if err != nil {
		t.Fatalf("Leaderboard: %v", err)
	}
	found := false
	for _, mp := range board {
		if mp.MemberID == m1 {
			found = true
			if mp.Points != -5 {
				t.Errorf("Leaderboard points for m1 = %d, want -5", mp.Points)
			}
		}
	}
	if !found {
		t.Error("Leaderboard did not include m1 despite a penalty entry")
	}
}

// TestSweepExpiredClaims_MinimumOnePointFloor verifies the AC2/penalty-floor
// interaction: a zero-point task still incurs the 1-point minimum penalty.
func TestSweepExpiredClaims_MinimumOnePointFloor(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 0)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))

	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	claims, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}
	if len(claims) != 1 || claims[0].PenaltyPoints != 1 {
		t.Fatalf("SweepExpiredClaims claims = %+v, want 1 claim with PenaltyPoints=1", claims)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != -1 {
		t.Errorf("Balance = %d, want -1 (1-point floor on a zero-point task)", balance)
	}
}

// TestSweepExpiredClaims_NegativeBalanceGoesFurtherNegative is the AC3 case:
// a member who is already at a negative balance still receives the full
// penalty, unclamped, and the balance goes further negative.
func TestSweepExpiredClaims_NegativeBalanceGoesFurtherNegative(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	// Put m1 at a negative balance via a manual ledger adjustment (source_id
	// nil, matching the "manual adjustment" contract in PointEntry's doc).
	if err := ledgerRepo.Append(testCtx(t), &domain.PointEntry{
		ID:          domain.NewPointEntryID(),
		HouseholdID: h.ID,
		MemberID:    m1,
		SourceType:  "manual_adjustment",
		Points:      -3,
	}); err != nil {
		t.Fatalf("seed negative balance: Append: %v", err)
	}
	preBalance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance (pre): %v", err)
	}
	if preBalance != -3 {
		t.Fatalf("Balance (pre) = %d, want -3", preBalance)
	}

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10) // penalty = 5
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	claims, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("SweepExpiredClaims returned %d claims, want 1", len(claims))
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance (post): %v", err)
	}
	if balance != -8 {
		t.Errorf("Balance (post) = %d, want -8 (already -3, penalty -5, never floored at 0)", balance)
	}
}

// TestSweepExpiredClaims_CompletingBeforeExpiryAwardsNoPenalty is the AC4
// case: completing the instance before expiry awards points normally and the
// (now-done, no-longer pending/overdue) instance is excluded from any later
// sweep, even though its stale claim fields were never cleared by Complete.
func TestSweepExpiredClaims_CompletingBeforeExpiryAwardsNoPenalty(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward: %v", err)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance (after completion): %v", err)
	}
	if balance != 10 {
		t.Fatalf("Balance (after completion) = %d, want 10 (full award, no penalty)", balance)
	}

	// A later sweep, even far enough in the future to catch anything still
	// claimed, must not touch the now-done instance.
	claims, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}
	if len(claims) != 0 {
		t.Errorf("SweepExpiredClaims after completion returned %d claims, want 0", len(claims))
	}

	balance, err = ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance (after sweep): %v", err)
	}
	if balance != 10 {
		t.Errorf("Balance (after sweep) = %d, want unchanged 10 (no penalty on a completed instance)", balance)
	}
}

// TestSweepExpiredClaims_NotYetExpiredIsUntouched verifies that a claim well
// within its window is not swept: SweepExpiredClaims called with asOf close
// to "now" (well before the 12h window elapses) must not revert it.
func TestSweepExpiredClaims_NotYetExpiredIsUntouched(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	claims, err := instRepo.SweepExpiredClaims(testCtx(t), time.Now())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("SweepExpiredClaims(asOf=now) returned %d claims, want 0 (claim is not yet expired)", len(claims))
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AssigneeID == nil || *got.AssigneeID != m1 {
		t.Errorf("AssigneeID = %v, want still %v (untouched)", got.AssigneeID, m1)
	}
	if got.ClaimExpiresAt == nil {
		t.Error("ClaimExpiresAt = nil, want still set (untouched)")
	}
}

// TestSweepExpiredClaims_SequentialDoubleSweep_ExactlyOnePenalty is the AC5
// case verified sequentially: sweeping the same expired claim twice produces
// exactly one penalty entry (the second sweep finds nothing, because the
// first already reverted and cleared the claim fields that make a row a
// sweep candidate).
func TestSweepExpiredClaims_SequentialDoubleSweep_ExactlyOnePenalty(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	asOf := farFutureAsOf()
	first, err := instRepo.SweepExpiredClaims(testCtx(t), asOf)
	if err != nil {
		t.Fatalf("SweepExpiredClaims (first): %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first sweep returned %d claims, want 1", len(first))
	}

	second, err := instRepo.SweepExpiredClaims(testCtx(t), asOf)
	if err != nil {
		t.Fatalf("SweepExpiredClaims (second): %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second sweep returned %d claims, want 0 (nothing left to revert)", len(second))
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != -5 {
		t.Errorf("Balance after double sweep = %d, want -5 (exactly one penalty)", balance)
	}
}

// TestSweepExpiredClaims_ConcurrentSweepsPenalizeOnce exercises the FOR
// UPDATE SKIP LOCKED guard directly: two goroutines call SweepExpiredClaims
// concurrently over the same expired claim. Exactly one must revert and
// penalize it; the ledger must show exactly one penalty entry regardless of
// which goroutine won the race. This mirrors the concurrency pattern already
// established for the standing-instance respawn guard (NES-116).
func TestSweepExpiredClaims_ConcurrentSweepsPenalizeOnce(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	asOf := farFutureAsOf()
	var (
		wg      sync.WaitGroup
		results [2][]domain.ExpiredClaim
		errs    [2]error
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = instRepo.SweepExpiredClaims(context.Background(), asOf)
		}(i)
	}
	wg.Wait()

	totalClaims := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("SweepExpiredClaims (goroutine %d): %v", i, err)
		}
		totalClaims += len(results[i])
	}
	if totalClaims != 1 {
		t.Errorf("total claims across both concurrent sweeps = %d, want 1", totalClaims)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != -5 {
		t.Errorf("Balance after concurrent sweeps = %d, want -5 (exactly one penalty)", balance)
	}
}

// ---------------------------------------------------------------------------
// SweepExpiredClaims — standing instance interaction (NES-116)
// ---------------------------------------------------------------------------

// TestSweepExpiredClaims_StandingInstance_RevertsWithoutRespawnOrTermination
// verifies the ticket's explicit NES-116 invariant: an expired claim on a
// standing instance reverts to the pool (assignee NULL) in place — it is
// NEVER terminated or respawned, so exactly one open standing instance (the
// same row) exists both before and after the sweep.
func TestSweepExpiredClaims_StandingInstance_RevertsWithoutRespawnOrTermination(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	seedAsNeededTaskWithPoints(t, taskRepo, h.ID, 4) // penalty = 2
	standing, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil || len(standing) != 1 {
		t.Fatalf("ListStanding (seed) = %v, %v, want 1 instance", standing, err)
	}
	original := standing[0]

	if err := instRepo.Claim(testCtx(t), h.ID, original.ID, m1); err != nil {
		t.Fatalf("Claim(standing): %v", err)
	}

	claims, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("SweepExpiredClaims returned %d claims, want 1", len(claims))
	}
	if claims[0].PenaltyPoints != 2 {
		t.Errorf("PenaltyPoints = %d, want 2 (half of 4)", claims[0].PenaltyPoints)
	}

	// Exactly one open standing instance — the SAME row, reverted in place,
	// not terminated and not respawned.
	after, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStanding (after sweep): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("ListStanding (after sweep) = %d instances, want 1", len(after))
	}
	if after[0].ID != original.ID {
		t.Errorf("ListStanding (after sweep) returned a different instance id (%v), want the same row %v (no respawn)",
			after[0].ID, original.ID)
	}
	if after[0].AssigneeID != nil {
		t.Errorf("AssigneeID = %v, want nil (reverted to pool)", after[0].AssigneeID)
	}
	if after[0].Status != domain.StatusPending {
		t.Errorf("Status = %v, want pending", after[0].Status)
	}
}

// TestCompleteAndAward_StandingInstance_ClearsClaimAndStillRespawns verifies
// that the NES-117 claim-clearing added to CompleteAndAward's UPDATE does not
// disturb the NES-116 standing-instance respawn: the completed row's claim
// fields are cleared, points are still awarded, and a fresh, unclaimed
// standing instance still replaces it in the same transaction.
func TestCompleteAndAward_StandingInstance_ClearsClaimAndStillRespawns(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	seedAsNeededTaskWithPoints(t, taskRepo, h.ID, 6)
	standing, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil || len(standing) != 1 {
		t.Fatalf("ListStanding (seed) = %v, %v, want 1 instance", standing, err)
	}
	original := standing[0]

	if err := instRepo.Claim(testCtx(t), h.ID, original.ID, m1); err != nil {
		t.Fatalf("Claim(standing): %v", err)
	}
	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, original.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward: %v", err)
	}

	// The completed row's claim fields are cleared.
	got, err := instRepo.Get(testCtx(t), h.ID, original.ID)
	if err != nil {
		t.Fatalf("Get after CompleteAndAward: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}
	if got.ClaimedBy != nil || got.ClaimedAt != nil || got.ClaimExpiresAt != nil {
		t.Errorf("completed standing instance claim fields = (%v, %v, %v), want all nil",
			got.ClaimedBy, got.ClaimedAt, got.ClaimExpiresAt)
	}

	// Points were still awarded and a fresh standing instance still replaced it.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 6 {
		t.Errorf("Balance = %d, want 6 (claim clearing must not interfere with the award)", balance)
	}
	after, err := instRepo.ListStanding(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListStanding (after): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("ListStanding (after) = %d instances, want 1 (respawn must still happen)", len(after))
	}
	if after[0].ID == original.ID {
		t.Error("respawned standing instance has the same id as the completed one, want a fresh row")
	}
	if after[0].ClaimedBy != nil || after[0].ClaimExpiresAt != nil {
		t.Errorf("respawned standing instance claim fields = (%v, %v), want both nil", after[0].ClaimedBy, after[0].ClaimExpiresAt)
	}
}

// ---------------------------------------------------------------------------
// SweepExpiredClaims — orphaned claim after member deletion (NES-117,
// CodeRabbit MAJOR finding)
// ---------------------------------------------------------------------------

// TestSweepExpiredClaims_OrphanedClaimAfterMemberDeletion simulates a member
// being deleted while they hold an active, not-yet-expired claim. ON DELETE
// SET NULL (claimed_by) nulls only claimed_by; claimed_at/claim_expires_at
// deliberately survive (task_instance_claim_consistency is directional to
// allow exactly this — see the migration). The deletion itself must succeed
// (no CHECK violation), and once the claim's original expiry passes, the
// sweep must revert the instance to the pool WITHOUT attempting a penalty
// (there is no member left to credit) and without erroring.
func TestSweepExpiredClaims_OrphanedClaimAfterMemberDeletion(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Delete the claimant directly (no repository method exists for member
	// deletion yet). This must succeed without violating
	// task_instance_claim_consistency or task_instance_claim_expiry_requires_claim.
	if _, err := pool.Exec(testCtx(t), "DELETE FROM member WHERE id = $1", m1.String()); err != nil {
		t.Fatalf("delete claimant member: %v (must not violate a task_instance claim CHECK constraint)", err)
	}

	// The row survives the deletion with claimed_by AND assignee_id nulled
	// (both FKs reference the same now-deleted member, each with their own
	// ON DELETE SET NULL) while claimed_at/claim_expires_at remain intact.
	orphaned, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after member deletion: %v", err)
	}
	if orphaned.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil (nulled by ON DELETE SET NULL)", orphaned.ClaimedBy)
	}
	if orphaned.AssigneeID != nil {
		t.Errorf("AssigneeID = %v, want nil (nulled by ON DELETE SET NULL, same deleted member)", orphaned.AssigneeID)
	}
	if orphaned.ClaimedAt == nil {
		t.Error("ClaimedAt = nil, want still set (survives the claimant's deletion)")
	}
	if orphaned.ClaimExpiresAt == nil {
		t.Error("ClaimExpiresAt = nil, want still set (survives the claimant's deletion)")
	}

	// The sweep must revert the orphaned claim cleanly: no error, no penalty
	// (nothing added to the ledger since there is no claimant), and no
	// notification-worthy claim returned.
	claims, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
	if err != nil {
		t.Fatalf("SweepExpiredClaims: %v (must handle an orphaned claim without error)", err)
	}
	if len(claims) != 0 {
		t.Errorf("SweepExpiredClaims returned %d claims, want 0 (no claimant to penalize or notify)", len(claims))
	}

	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after sweep: %v", err)
	}
	if got.AssigneeID != nil {
		t.Errorf("AssigneeID = %v, want nil (reverted to pool)", got.AssigneeID)
	}
	if got.ClaimedAt != nil {
		t.Errorf("ClaimedAt = %v, want nil (reverted)", got.ClaimedAt)
	}
	if got.ClaimExpiresAt != nil {
		t.Errorf("ClaimExpiresAt = %v, want nil (reverted)", got.ClaimExpiresAt)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("Status = %v, want pending", got.Status)
	}

	// No ledger row was created for the orphaned claim (there is no member to
	// credit it to, and point_ledger.member_id is NOT NULL).
	var ledgerCount int
	if err := pool.QueryRow(testCtx(t),
		"SELECT count(*) FROM point_ledger WHERE household_id = $1 AND source_id = $2",
		h.ID.String(), inst.ID.String(),
	).Scan(&ledgerCount); err != nil {
		t.Fatalf("count point_ledger rows: %v", err)
	}
	if ledgerCount != 0 {
		t.Errorf("point_ledger rows for the orphaned claim = %d, want 0", ledgerCount)
	}
}
