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
// TaskInstanceRepository.CompletionDays — gated tests
// ---------------------------------------------------------------------------

// TestCompletionDays_ReturnsDistinctCalendarDays verifies that CompletionDays
// returns distinct calendar days (midnight UTC) for all 'done' instances
// completed by the member on or after since, ordered ascending.
func TestCompletionDays_ReturnsDistinctCalendarDays(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 5)

	// Seed three instances completed on three different calendar days.
	type completion struct {
		dueOn       time.Time
		completedAt time.Time
	}
	instances := []completion{
		{dueOn: refDate.AddDate(0, 0, 10), completedAt: time.Date(2025, 6, 1, 9, 0, 0, 0, time.UTC)},
		{dueOn: refDate.AddDate(0, 0, 11), completedAt: time.Date(2025, 6, 2, 14, 0, 0, 0, time.UTC)},
		{dueOn: refDate.AddDate(0, 0, 12), completedAt: time.Date(2025, 6, 3, 8, 30, 0, 0, time.UTC)},
	}
	for i, c := range instances {
		inst := seedTaskInstance(t, instRepo, rt, c.dueOn)
		if err := instRepo.Complete(testCtx(t), h.ID, inst.ID, m1, c.completedAt); err != nil {
			t.Fatalf("Complete(inst %d): %v", i, err)
		}
	}

	since := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC) // before all completions
	days, err := instRepo.CompletionDays(testCtx(t), h.ID, m1, since)
	if err != nil {
		t.Fatalf("CompletionDays: %v", err)
	}
	if len(days) != 3 {
		t.Fatalf("CompletionDays = %d days, want 3", len(days))
	}
	// Results must be ascending calendar days, normalised to midnight UTC.
	wantDays := []time.Time{
		time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 6, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 6, 3, 0, 0, 0, 0, time.UTC),
	}
	for i, want := range wantDays {
		if !days[i].Equal(want) {
			t.Errorf("days[%d] = %v, want %v", i, days[i], want)
		}
	}
}

// TestCompletionDays_MultipleCompletionsOnSameDayReturnOnce verifies that
// two completions on the same calendar day (different instances) are
// deduplicated to a single day.
func TestCompletionDays_MultipleCompletionsOnSameDayReturnOnce(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 3)

	// Two instances, both completed on the same calendar day at different times.
	sameDay1 := time.Date(2025, 7, 10, 8, 0, 0, 0, time.UTC)
	sameDay2 := time.Date(2025, 7, 10, 20, 0, 0, 0, time.UTC)

	for i, completedAt := range []time.Time{sameDay1, sameDay2} {
		inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, i+20))
		if err := instRepo.Complete(testCtx(t), h.ID, inst.ID, m1, completedAt); err != nil {
			t.Fatalf("Complete (same-day %d): %v", i, err)
		}
	}

	since := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	days, err := instRepo.CompletionDays(testCtx(t), h.ID, m1, since)
	if err != nil {
		t.Fatalf("CompletionDays: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("CompletionDays = %d days, want 1 (dedup)", len(days))
	}
	wantDay := time.Date(2025, 7, 10, 0, 0, 0, 0, time.UTC)
	if !days[0].Equal(wantDay) {
		t.Errorf("days[0] = %v, want %v", days[0], wantDay)
	}
}

// TestCompletionDays_SinceFilterExcludesOldCompletions verifies that
// completions whose completed_at is before since are excluded.
func TestCompletionDays_SinceFilterExcludesOldCompletions(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 2)

	old := time.Date(2024, 12, 1, 10, 0, 0, 0, time.UTC)    // before since
	recent := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC) // after since

	for i, completedAt := range []time.Time{old, recent} {
		inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, i+30))
		if err := instRepo.Complete(testCtx(t), h.ID, inst.ID, m1, completedAt); err != nil {
			t.Fatalf("Complete(%d): %v", i, err)
		}
	}

	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) // after old, before recent
	days, err := instRepo.CompletionDays(testCtx(t), h.ID, m1, since)
	if err != nil {
		t.Fatalf("CompletionDays: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("CompletionDays = %d days, want 1 (since filter)", len(days))
	}
	wantDay := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	if !days[0].Equal(wantDay) {
		t.Errorf("days[0] = %v, want %v", days[0], wantDay)
	}
}

// TestCompletionDays_CrossHouseholdIsolation verifies that completions from a
// different household or a different member are not included.
func TestCompletionDays_CrossHouseholdIsolation(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	hA, mA, mA2 := seedHousehold(t, pool)
	hB, mB, _ := seedHousehold(t, pool)

	rtA := seedRecurringTaskWithPoints(t, taskRepo, hA.ID, 1)
	rtB := seedRecurringTaskWithPoints(t, taskRepo, hB.ID, 1)

	completedAt := time.Date(2025, 6, 5, 10, 0, 0, 0, time.UTC)

	// mA2 (a different member of hA) completes a task.
	instA2 := seedTaskInstance(t, instRepo, rtA, refDate.AddDate(0, 0, 1))
	if err := instRepo.Complete(testCtx(t), hA.ID, instA2.ID, mA2, completedAt); err != nil {
		t.Fatalf("Complete(mA2): %v", err)
	}

	// mB (member of hB) completes a task.
	instB := seedTaskInstance(t, instRepo, rtB, refDate.AddDate(0, 0, 2))
	if err := instRepo.Complete(testCtx(t), hB.ID, instB.ID, mB, completedAt); err != nil {
		t.Fatalf("Complete(mB): %v", err)
	}

	since := time.Time{} // no lower bound
	// mA itself has no completions: expect empty slice.
	days, err := instRepo.CompletionDays(testCtx(t), hA.ID, mA, since)
	if err != nil {
		t.Fatalf("CompletionDays(mA): %v", err)
	}
	if len(days) != 0 {
		t.Errorf("CompletionDays(mA) = %d, want 0 (isolation)", len(days))
	}
}

// TestCompletionDays_EmptyReturnsSlice verifies that CompletionDays returns an
// empty slice (not an error) when no completions exist.
func TestCompletionDays_EmptyReturnsSlice(t *testing.T) {
	pool := newTestPool(t)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	days, err := instRepo.CompletionDays(testCtx(t), h.ID, m1, time.Time{})
	if err != nil {
		t.Fatalf("CompletionDays(empty): %v", err)
	}
	// Contract: empty result is a non-nil zero-length slice, never nil. Callers
	// (CurrentStreak) and JSON encoders rely on a real slice.
	if days == nil {
		t.Fatal("CompletionDays(empty) = nil, want non-nil empty slice")
	}
	if len(days) != 0 {
		t.Errorf("CompletionDays(empty) = %d, want 0", len(days))
	}
}

// ---------------------------------------------------------------------------
// RewardPostgresRepository.RedeemWithDebit — gated tests
// ---------------------------------------------------------------------------

// seedBalanceForMember appends a manual credit to give the member a starting
// balance. Used to set up the pre-condition for RedeemWithDebit tests.
func seedBalanceForMember(
	t *testing.T,
	ledgerRepo *adapter.PointLedgerPostgresRepository,
	householdID household.HouseholdID,
	memberID household.MemberID,
	points int,
) {
	t.Helper()
	appendEntry(t, ledgerRepo, householdID, memberID, points, time.Now().UTC())
}

// buildRedemption constructs a RewardRedemption entity ready to be passed to
// RedeemWithDebit. The caller supplies the household, member, and reward IDs.
func buildRedemption(
	householdID household.HouseholdID,
	memberID household.MemberID,
	rewardID domain.RewardID,
) *domain.RewardRedemption {
	now := time.Now().UTC()
	return &domain.RewardRedemption{
		ID:          domain.NewRewardRedemptionID(),
		HouseholdID: householdID,
		RewardID:    rewardID,
		MemberID:    memberID,
		Status:      domain.RedemptionRequested,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// TestRedeemWithDebit_SuccessDebitsBalance verifies the core NES-37 behaviour:
// a successful redeem inserts a redemption row and a negative ledger entry so
// the balance drops by exactly costPoints.
func TestRedeemWithDebit_SuccessDebitsBalance(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 100)

	reward := seedReward(t, rewardRepo, h.ID, "Coffee", 30)
	redemption := buildRedemption(h.ID, m1, reward.ID)

	if err := rewardRepo.RedeemWithDebit(testCtx(t), redemption, 30); err != nil {
		t.Fatalf("RedeemWithDebit: %v", err)
	}

	// Balance must drop by exactly 30.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance after redeem: %v", err)
	}
	if balance != 70 {
		t.Errorf("Balance = %d, want 70 (100 - 30)", balance)
	}

	// The redemption row must exist.
	const q = `SELECT status FROM reward_redemption WHERE id = $1`
	var status string
	if err := pool.QueryRow(testCtx(t), q, redemption.ID.String()).Scan(&status); err != nil {
		t.Fatalf("read redemption row: %v", err)
	}
	if status != "requested" {
		t.Errorf("status = %q, want requested", status)
	}

	// The debit ledger row must exist with source_type = 'redemption'.
	const debitQ = `SELECT points FROM point_ledger WHERE source_id = $1 AND source_type = 'redemption'`
	var pts int
	if err := pool.QueryRow(testCtx(t), debitQ, redemption.ID.String()).Scan(&pts); err != nil {
		t.Fatalf("read debit ledger row: %v", err)
	}
	if pts != -30 {
		t.Errorf("debit ledger points = %d, want -30", pts)
	}
}

// TestRedeemWithDebit_InsufficientPointsRollback verifies that when the
// member's balance is below costPoints, RedeemWithDebit returns
// ErrInsufficientPoints and writes NO rows (neither redemption nor ledger).
func TestRedeemWithDebit_InsufficientPointsRollback(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 10) // only 10 pts

	reward := seedReward(t, rewardRepo, h.ID, "Expensive", 100) // costs 100
	redemption := buildRedemption(h.ID, m1, reward.ID)

	err := rewardRepo.RedeemWithDebit(testCtx(t), redemption, 100)
	if !errors.Is(err, domain.ErrInsufficientPoints) {
		t.Fatalf("RedeemWithDebit(insufficient) = %v, want ErrInsufficientPoints", err)
	}

	// Balance unchanged.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance after failed redeem: %v", err)
	}
	if balance != 10 {
		t.Errorf("Balance = %d, want 10 (unchanged after rollback)", balance)
	}

	// No redemption row.
	const redemptionQ = `SELECT COUNT(*) FROM reward_redemption WHERE id = $1`
	var count int
	if err := pool.QueryRow(testCtx(t), redemptionQ, redemption.ID.String()).Scan(&count); err != nil {
		t.Fatalf("count redemption rows: %v", err)
	}
	if count != 0 {
		t.Errorf("redemption rows = %d, want 0 (rollback)", count)
	}

	// No debit ledger row.
	const debitQ = `SELECT COUNT(*) FROM point_ledger WHERE source_type = 'redemption' AND source_id = $1`
	var debitCount int
	if err := pool.QueryRow(testCtx(t), debitQ, redemption.ID.String()).Scan(&debitCount); err != nil {
		t.Fatalf("count debit ledger rows: %v", err)
	}
	if debitCount != 0 {
		t.Errorf("debit ledger rows = %d, want 0 (rollback)", debitCount)
	}
}

// TestRedeemWithDebit_UnknownRewardFK verifies that RedeemWithDebit returns
// ErrRewardNotFound when the reward does not exist in the household (FK
// violation on reward_redemption_reward_fk).
func TestRedeemWithDebit_UnknownRewardFK(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 200)

	// Use a reward ID that does not exist in the household.
	nonExistentRewardID := domain.NewRewardID()
	redemption := buildRedemption(h.ID, m1, nonExistentRewardID)

	err := rewardRepo.RedeemWithDebit(testCtx(t), redemption, 50)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Fatalf("RedeemWithDebit(unknown reward) = %v, want ErrRewardNotFound", err)
	}

	// Balance unchanged (rollback happened).
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance after FK error: %v", err)
	}
	if balance != 200 {
		t.Errorf("Balance = %d, want 200 (unchanged)", balance)
	}
}

// TestRedeemWithDebit_CrossHouseholdRewardRejected verifies that a member in
// household B cannot redeem a reward that belongs to household A. The composite
// FK on reward_redemption (household_id, reward_id) → reward(household_id, id)
// rejects the insert, returning ErrRewardNotFound.
func TestRedeemWithDebit_CrossHouseholdRewardRejected(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	hA, _, _ := seedHousehold(t, pool)
	hB, mB, _ := seedHousehold(t, pool)

	seedBalanceForMember(t, ledgerRepo, hB.ID, mB, 500)

	rewardA := seedReward(t, rewardRepo, hA.ID, "hA reward", 10)

	// Attempt: mB (hB member) redeems a reward belonging to hA.
	redemption := buildRedemption(hB.ID, mB, rewardA.ID)

	err := rewardRepo.RedeemWithDebit(testCtx(t), redemption, 10)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Fatalf("RedeemWithDebit(cross-household) = %v, want ErrRewardNotFound", err)
	}
}

// TestRedeemWithDebit_ConcurrentRedeemsSerialized proves the advisory lock
// serialises competing redeems for the same (household, member): when a member
// has exactly enough points for ONE redeem and two redeems fire concurrently,
// exactly one succeeds and one returns ErrInsufficientPoints. The final balance
// must be debited exactly once (no overspend, no negative balance), and exactly
// one debit ledger row must exist.
func TestRedeemWithDebit_ConcurrentRedeemsSerialized(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	const cost = 40
	// Exactly enough for a single redeem of cost points.
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, cost)

	reward := seedReward(t, rewardRepo, h.ID, "Contested reward", cost)

	// Two independent redemption rows (distinct PKs) so the only thing that can
	// serialise them is the advisory lock + balance check, not a PK clash.
	redemptionA := buildRedemption(h.ID, m1, reward.ID)
	redemptionB := buildRedemption(h.ID, m1, reward.ID)

	// One shared, bounded context for both goroutines. testCtx registers cleanup
	// on t and is not safe to call from goroutines, so build the context here.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []error
	)
	for _, red := range []*domain.RewardRedemption{redemptionA, redemptionB} {
		wg.Add(1)
		go func(r *domain.RewardRedemption) {
			defer wg.Done()
			err := rewardRepo.RedeemWithDebit(ctx, r, cost)
			mu.Lock()
			results = append(results, err)
			mu.Unlock()
		}(red)
	}
	wg.Wait()

	// Exactly one success (nil) and one ErrInsufficientPoints.
	var successCount, insufficientCount int
	for _, err := range results {
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, domain.ErrInsufficientPoints):
			insufficientCount++
		default:
			t.Fatalf("unexpected redeem error: %v", err)
		}
	}
	if successCount != 1 {
		t.Errorf("successCount = %d, want exactly 1", successCount)
	}
	if insufficientCount != 1 {
		t.Errorf("insufficientCount = %d, want exactly 1", insufficientCount)
	}

	// Final balance must be exactly 0: cost credited, cost debited once.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance after concurrent redeem: %v", err)
	}
	if balance != 0 {
		t.Errorf("Balance = %d, want 0 (debited exactly once, no overspend)", balance)
	}

	// Exactly one debit ledger row must exist for this member.
	const debitCountQ = `
		SELECT COUNT(*) FROM point_ledger
		 WHERE household_id = $1 AND member_id = $2 AND source_type = 'redemption'`
	var debitCount int
	if err := pool.QueryRow(testCtx(t), debitCountQ, h.ID.String(), m1.String()).Scan(&debitCount); err != nil {
		t.Fatalf("count debit ledger rows: %v", err)
	}
	if debitCount != 1 {
		t.Errorf("debit ledger rows = %d, want exactly 1 (no duplicate debit)", debitCount)
	}

	// Exactly one redemption row must exist.
	const redemptionCountQ = `
		SELECT COUNT(*) FROM reward_redemption
		 WHERE household_id = $1 AND member_id = $2`
	var redemptionCount int
	if err := pool.QueryRow(testCtx(t), redemptionCountQ, h.ID.String(), m1.String()).Scan(&redemptionCount); err != nil {
		t.Fatalf("count redemption rows: %v", err)
	}
	if redemptionCount != 1 {
		t.Errorf("redemption rows = %d, want exactly 1", redemptionCount)
	}
}
