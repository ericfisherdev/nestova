package adapter_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

// seedRecurringTaskWithPoints creates and persists a recurring task whose
// points award is configurable. All other fields use sensible defaults.
func seedRecurringTaskWithPoints(
	t *testing.T,
	repo *adapter.RecurringTaskRepository,
	householdID household.HouseholdID,
	points int,
) *domain.RecurringTask {
	t.Helper()
	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    householdID,
		Title:          "Test chore",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Points:         points,
		LeadTimeDays:   1,
		Active:         true,
	}
	if err := repo.Create(testCtx(t), rt); err != nil {
		t.Fatalf("seedRecurringTaskWithPoints: %v", err)
	}
	return rt
}

// seedReward creates and persists an active reward for the household.
func seedReward(
	t *testing.T,
	repo *adapter.RewardPostgresRepository,
	householdID household.HouseholdID,
	name string,
	costPoints int,
) *domain.Reward {
	t.Helper()
	r := &domain.Reward{
		ID:          domain.NewRewardID(),
		HouseholdID: householdID,
		Name:        name,
		CostPoints:  costPoints,
		Active:      true,
	}
	if err := repo.CreateReward(testCtx(t), r); err != nil {
		t.Fatalf("seedReward(%q): %v", name, err)
	}
	return r
}

// ---------------------------------------------------------------------------
// CompleteAndAward — atomic completion + point award
// ---------------------------------------------------------------------------

// TestCompleteAndAward_CreditsPoints verifies the core NES-36 behaviour:
// completing a pending instance with points > 0 atomically transitions it to
// done and appends one point_ledger row, so Balance reflects the award.
func TestCompleteAndAward_CreditsPoints(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 5)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 1))

	// Claim before completing so the NES-117 claim-clearing assertion below is
	// meaningful — an instance that was never claimed trivially has nil claim
	// fields regardless of whether CompleteAndAward clears them.
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward: %v", err)
	}

	// Instance must be done.
	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after CompleteAndAward: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}
	if got.CompletedBy == nil || *got.CompletedBy != m1 {
		t.Errorf("CompletedBy = %v, want %v", got.CompletedBy, m1)
	}
	if got.CompletedAt == nil || got.CompletedAt.IsZero() {
		t.Errorf("CompletedAt = %v, want a non-zero timestamp", got.CompletedAt)
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

	// Balance must reflect exactly one award.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 5 {
		t.Errorf("Balance = %d, want 5", balance)
	}
}

// TestCompleteAndAward_ZeroPointsNoLedgerRow verifies that completing a task
// with points = 0 transitions the instance to done but appends no ledger row
// (Balance stays 0, not 0 from a zero-point entry).
func TestCompleteAndAward_ZeroPointsNoLedgerRow(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 0)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 1))

	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward(points=0): %v", err)
	}

	// Instance must be done.
	got, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after CompleteAndAward(points=0): %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}

	// No ledger row — balance stays zero.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance(points=0): %v", err)
	}
	if balance != 0 {
		t.Errorf("Balance(points=0) = %d, want 0", balance)
	}
}

// TestCompleteAndAward_RecompletionNoSecondAward verifies the idempotency
// invariant: re-completing an already-done instance returns
// ErrInstanceInTerminalState and does NOT append a second ledger row.
func TestCompleteAndAward_RecompletionNoSecondAward(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 5)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 1))

	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward (first): %v", err)
	}

	// Re-completing must return the terminal-state sentinel.
	err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, time.Now())
	if !errors.Is(err, domain.ErrInstanceInTerminalState) {
		t.Errorf("CompleteAndAward (re-completion) = %v, want ErrInstanceInTerminalState", err)
	}

	// Balance must still be exactly 5 — no double-award.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance after re-completion: %v", err)
	}
	if balance != 5 {
		t.Errorf("Balance after re-completion = %d, want 5 (no double-award)", balance)
	}
}

// TestCompleteAndAward_OverdueInstance verifies that an overdue instance is
// still completable and its award is credited.
func TestCompleteAndAward_OverdueInstance(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 3)
	inst := seedOverdueTaskInstance(t, instRepo, rt)

	if err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, time.Now()); err != nil {
		t.Fatalf("CompleteAndAward(overdue): %v", err)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance(overdue): %v", err)
	}
	if balance != 3 {
		t.Errorf("Balance(overdue) = %d, want 3", balance)
	}
}

// TestCompleteAndAward_NotFound verifies that completing an unknown instance
// returns ErrInstanceNotFound and no ledger row is written.
func TestCompleteAndAward_NotFound(t *testing.T) {
	pool := newTestPool(t)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	err := instRepo.CompleteAndAward(testCtx(t), h.ID, domain.NewTaskInstanceID(), m1, time.Now())
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("CompleteAndAward(not found) = %v, want ErrInstanceNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// PointLedgerRepository — Append
// ---------------------------------------------------------------------------

// TestPointLedger_AppendAndBalance verifies that Append persists a point entry
// and Balance reflects it.
func TestPointLedger_AppendAndBalance(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	srcID := uuid.Must(uuid.NewV7())
	entry := &domain.PointEntry{
		ID:          domain.NewPointEntryID(),
		HouseholdID: h.ID,
		MemberID:    m1,
		SourceType:  "task_instance",
		SourceID:    &srcID,
		Points:      10,
		CreatedAt:   time.Now().UTC(),
	}
	if err := ledgerRepo.Append(testCtx(t), entry); err != nil {
		t.Fatalf("Append: %v", err)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 10 {
		t.Errorf("Balance = %d, want 10", balance)
	}
}

// TestPointLedger_AppendDuplicateReturnsErrDuplicatePointEntry verifies that
// appending a second entry with the same source_id and source_type =
// 'task_instance' returns ErrDuplicatePointEntry.
func TestPointLedger_AppendDuplicateReturnsErrDuplicatePointEntry(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	srcID := uuid.Must(uuid.NewV7())
	entry1 := &domain.PointEntry{
		ID:          domain.NewPointEntryID(),
		HouseholdID: h.ID,
		MemberID:    m1,
		SourceType:  "task_instance",
		SourceID:    &srcID,
		Points:      5,
		CreatedAt:   time.Now().UTC(),
	}
	if err := ledgerRepo.Append(testCtx(t), entry1); err != nil {
		t.Fatalf("Append (first): %v", err)
	}

	entry2 := &domain.PointEntry{
		ID:          domain.NewPointEntryID(), // different entry id
		HouseholdID: h.ID,
		MemberID:    m1,
		SourceType:  "task_instance",
		SourceID:    &srcID, // same source_id
		Points:      5,
		CreatedAt:   time.Now().UTC(),
	}
	err := ledgerRepo.Append(testCtx(t), entry2)
	if !errors.Is(err, domain.ErrDuplicatePointEntry) {
		t.Errorf("Append(duplicate) = %v, want ErrDuplicatePointEntry", err)
	}
}

// TestPointLedger_BalanceZeroForMemberWithNoEntries verifies that Balance
// returns 0, nil for a member with no history (not an error).
func TestPointLedger_BalanceZeroForMemberWithNoEntries(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance(no entries): %v", err)
	}
	if balance != 0 {
		t.Errorf("Balance(no entries) = %d, want 0", balance)
	}
}

// ---------------------------------------------------------------------------
// PointLedgerRepository — Leaderboard
// ---------------------------------------------------------------------------

// TestPointLedger_LeaderboardOrderingAndSinceFilter verifies that Leaderboard
// orders results by total points descending and respects the since filter.
func TestPointLedger_LeaderboardOrderingAndSinceFilter(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	// Anchor timestamps: old is before the filter; recent is after.
	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sinceFilter := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	// m1 earns 20 total: 15 recent + 5 old.
	appendEntry(t, ledgerRepo, h.ID, m1, 15, recent)
	appendEntry(t, ledgerRepo, h.ID, m1, 5, old)

	// m2 earns 10 total: all recent.
	appendEntry(t, ledgerRepo, h.ID, m2, 10, recent)

	// Leaderboard since sinceFilter: includes recent entries only.
	// m1 = 15, m2 = 10.
	result, err := ledgerRepo.Leaderboard(testCtx(t), h.ID, sinceFilter)
	if err != nil {
		t.Fatalf("Leaderboard: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("Leaderboard = %d rows, want 2", len(result))
	}
	// First entry must be m1 (highest total).
	if result[0].MemberID != m1 {
		t.Errorf("result[0].MemberID = %v, want %v (m1 has highest score)", result[0].MemberID, m1)
	}
	if result[0].Points != 15 {
		t.Errorf("result[0].Points = %d, want 15", result[0].Points)
	}
	if result[1].MemberID != m2 {
		t.Errorf("result[1].MemberID = %v, want %v", result[1].MemberID, m2)
	}
	if result[1].Points != 10 {
		t.Errorf("result[1].Points = %d, want 10", result[1].Points)
	}
}

// TestPointLedger_LeaderboardDeterministicOrderingWhenTied verifies that when
// two members have identical point totals, the leaderboard ordering is
// deterministic across calls. The query's `ORDER BY <sum> DESC, member_id`
// tiebreak guarantees a stable order regardless of row-arrival order, so two
// successive calls must return identical results.
func TestPointLedger_LeaderboardDeterministicOrderingWhenTied(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	// Both members earn 10 points at the same instant — a total tie.
	at := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	appendEntry(t, ledgerRepo, h.ID, m1, 10, at)
	appendEntry(t, ledgerRepo, h.ID, m2, 10, at)

	first, err := ledgerRepo.Leaderboard(testCtx(t), h.ID, time.Time{})
	if err != nil {
		t.Fatalf("Leaderboard (first): %v", err)
	}
	second, err := ledgerRepo.Leaderboard(testCtx(t), h.ID, time.Time{})
	if err != nil {
		t.Fatalf("Leaderboard (second): %v", err)
	}

	if len(first) != 2 {
		t.Fatalf("Leaderboard = %d rows, want 2", len(first))
	}
	if len(first) != len(second) {
		t.Fatalf("row counts differ: first = %d, second = %d", len(first), len(second))
	}

	// The two tied members must appear in the same order both times — the
	// member_id tiebreak makes this deterministic.
	for i := range first {
		if first[i].MemberID != second[i].MemberID {
			t.Errorf("ordering not deterministic at index %d: first = %v, second = %v",
				i, first[i].MemberID, second[i].MemberID)
		}
		if first[i].Points != 10 {
			t.Errorf("first[%d].Points = %d, want 10", i, first[i].Points)
		}
	}
}

// TestPointLedger_LeaderboardSinceExcludesOldEntries verifies that entries
// older than the since filter are excluded: a member whose only entries predate
// the filter does not appear on the leaderboard.
func TestPointLedger_LeaderboardSinceExcludesOldEntries(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	sinceFilter := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	appendEntry(t, ledgerRepo, h.ID, m1, 50, old)

	result, err := ledgerRepo.Leaderboard(testCtx(t), h.ID, sinceFilter)
	if err != nil {
		t.Fatalf("Leaderboard(old only): %v", err)
	}
	if len(result) != 0 {
		t.Errorf("Leaderboard(old only) = %d rows, want 0", len(result))
	}
}

// TestPointLedger_LeaderboardEmptyReturnsSlice verifies that Leaderboard
// returns an empty slice (not an error) when no entries exist.
func TestPointLedger_LeaderboardEmptyReturnsSlice(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	result, err := ledgerRepo.Leaderboard(testCtx(t), h.ID, time.Time{})
	if err != nil {
		t.Fatalf("Leaderboard(empty): %v", err)
	}
	if len(result) != 0 {
		t.Errorf("Leaderboard(empty) = %d rows, want 0", len(result))
	}
}

// ---------------------------------------------------------------------------
// Cross-household isolation
// ---------------------------------------------------------------------------

// TestPointLedger_CrossHouseholdIsolation verifies that a member's balance and
// leaderboard in one household never include entries from another household.
func TestPointLedger_CrossHouseholdIsolation(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	hA, mA, _ := seedHousehold(t, pool)
	hB, mB, _ := seedHousehold(t, pool)

	now := time.Now().UTC()
	appendEntry(t, ledgerRepo, hA.ID, mA, 100, now)
	appendEntry(t, ledgerRepo, hB.ID, mB, 200, now)

	// Household A sees only mA's points.
	balA, err := ledgerRepo.Balance(testCtx(t), hA.ID, mA)
	if err != nil {
		t.Fatalf("Balance(hA, mA): %v", err)
	}
	if balA != 100 {
		t.Errorf("Balance(hA, mA) = %d, want 100", balA)
	}

	// mB's points are not visible from hA.
	balBFromA, err := ledgerRepo.Balance(testCtx(t), hA.ID, mB)
	if err != nil {
		t.Fatalf("Balance(hA, mB): %v", err)
	}
	if balBFromA != 0 {
		t.Errorf("Balance(hA, mB) = %d, want 0 (cross-household isolation)", balBFromA)
	}

	// Leaderboard for hA must not contain mB.
	leaderA, err := ledgerRepo.Leaderboard(testCtx(t), hA.ID, time.Time{})
	if err != nil {
		t.Fatalf("Leaderboard(hA): %v", err)
	}
	for _, mp := range leaderA {
		if mp.MemberID == mB {
			t.Error("Leaderboard(hA) contains mB — cross-household leak")
		}
	}
}

// ---------------------------------------------------------------------------
// RewardRepository — CRUD
// ---------------------------------------------------------------------------

// TestRewardRepository_CreateAndGet verifies that CreateReward persists a
// reward and GetReward retrieves it with timestamps populated.
func TestRewardRepository_CreateAndGet(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Movie night pick", 100)

	got, err := rewardRepo.GetReward(testCtx(t), h.ID, reward.ID)
	if err != nil {
		t.Fatalf("GetReward: %v", err)
	}
	if got.ID != reward.ID {
		t.Errorf("ID = %v, want %v", got.ID, reward.ID)
	}
	if got.Name != reward.Name {
		t.Errorf("Name = %q, want %q", got.Name, reward.Name)
	}
	if got.CostPoints != reward.CostPoints {
		t.Errorf("CostPoints = %d, want %d", got.CostPoints, reward.CostPoints)
	}
	if !got.Active {
		t.Error("Active = false, want true")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
}

// TestRewardRepository_GetNotFound verifies that GetReward returns
// ErrRewardNotFound for an unknown id.
func TestRewardRepository_GetNotFound(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	_, err := rewardRepo.GetReward(testCtx(t), h.ID, domain.NewRewardID())
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("GetReward(unknown) = %v, want ErrRewardNotFound", err)
	}
}

// TestRewardRepository_GetCrossHousehold verifies that a reward belonging to
// one household is invisible to a query scoped to a different household.
func TestRewardRepository_GetCrossHousehold(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	hA, _, _ := seedHousehold(t, pool)
	hB, _, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, hA.ID, "hA reward", 50)

	_, err := rewardRepo.GetReward(testCtx(t), hB.ID, reward.ID)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("GetReward(cross-household) = %v, want ErrRewardNotFound", err)
	}
}

// TestRewardRepository_ListActiveRewards verifies that ListActiveRewards
// returns only active rewards for the household, ordered by cost_points, and
// excludes inactive rewards and rewards from other households.
func TestRewardRepository_ListActiveRewards(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	hA, _, _ := seedHousehold(t, pool)
	hB, _, _ := seedHousehold(t, pool)

	active1 := seedReward(t, rewardRepo, hA.ID, "Small treat", 10)
	active2 := seedReward(t, rewardRepo, hA.ID, "Movie night", 50)

	// Create an inactive reward for hA.
	inactive := &domain.Reward{
		ID:          domain.NewRewardID(),
		HouseholdID: hA.ID,
		Name:        "Retired reward",
		CostPoints:  25,
		Active:      false,
	}
	if err := rewardRepo.CreateReward(testCtx(t), inactive); err != nil {
		t.Fatalf("CreateReward(inactive): %v", err)
	}

	// Active reward in a different household — must NOT appear.
	seedReward(t, rewardRepo, hB.ID, "hB reward", 5)

	got, err := rewardRepo.ListActiveRewards(testCtx(t), hA.ID)
	if err != nil {
		t.Fatalf("ListActiveRewards: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListActiveRewards = %d rewards, want 2", len(got))
	}

	// Results must be ordered by cost_points ascending.
	if got[0].ID != active1.ID {
		t.Errorf("got[0].ID = %v, want %v (cheaper first)", got[0].ID, active1.ID)
	}
	if got[1].ID != active2.ID {
		t.Errorf("got[1].ID = %v, want %v", got[1].ID, active2.ID)
	}

	// Inactive and other-household rewards must not appear.
	ids := map[domain.RewardID]bool{got[0].ID: true, got[1].ID: true}
	if ids[inactive.ID] {
		t.Error("ListActiveRewards includes inactive reward")
	}
}

// TestRewardRepository_ListActiveRewardsEmpty verifies that ListActiveRewards
// returns an empty slice (not an error) when no active rewards exist.
func TestRewardRepository_ListActiveRewardsEmpty(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	got, err := rewardRepo.ListActiveRewards(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListActiveRewards(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListActiveRewards(empty) = %d rows, want 0", len(got))
	}
}

// TestRewardRepository_Redeem verifies that Redeem persists a redemption row
// with the expected fields and status.
func TestRewardRepository_Redeem(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Free coffee", 20)

	now := time.Now().UTC()
	redemption := &domain.RewardRedemption{
		ID:          domain.NewRewardRedemptionID(),
		HouseholdID: h.ID,
		RewardID:    reward.ID,
		MemberID:    m1,
		Status:      domain.RedemptionRequested,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := rewardRepo.Redeem(testCtx(t), redemption); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	// Verify the row was persisted by reading it back directly.
	const q = `SELECT status FROM reward_redemption WHERE id = $1`
	var status string
	if err := pool.QueryRow(testCtx(t), q, redemption.ID.String()).Scan(&status); err != nil {
		t.Fatalf("read redemption row: %v", err)
	}
	if status != "requested" {
		t.Errorf("status = %q, want requested", status)
	}
}

// TestRewardRepository_RedeemUnknownRewardReturnsErrRewardNotFound verifies
// that Redeem returns ErrRewardNotFound when the RewardID does not exist in
// the household.
func TestRewardRepository_RedeemUnknownRewardReturnsErrRewardNotFound(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	now := time.Now().UTC()
	redemption := &domain.RewardRedemption{
		ID:          domain.NewRewardRedemptionID(),
		HouseholdID: h.ID,
		RewardID:    domain.NewRewardID(), // does not exist
		MemberID:    m1,
		Status:      domain.RedemptionRequested,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	err := rewardRepo.Redeem(testCtx(t), redemption)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("Redeem(unknown reward) = %v, want ErrRewardNotFound", err)
	}
}

// TestRewardRepository_CrossHouseholdIsolation verifies that rewards and
// redemptions from one household are not visible to queries scoped to another
// household.
func TestRewardRepository_CrossHouseholdIsolation(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	hA, _, _ := seedHousehold(t, pool)
	hB, mB, _ := seedHousehold(t, pool)

	rewardA := seedReward(t, rewardRepo, hA.ID, "hA reward", 30)

	// hB sees no active rewards.
	got, err := rewardRepo.ListActiveRewards(testCtx(t), hB.ID)
	if err != nil {
		t.Fatalf("ListActiveRewards(hB): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListActiveRewards(hB) = %d rows, want 0 (cross-household isolation)", len(got))
	}

	// A member of hB cannot redeem a reward that belongs to hA: the composite FK
	// on reward_redemption (household_id, reward_id) → reward rejects the insert,
	// surfacing as ErrRewardNotFound.
	now := time.Now().UTC()
	redemption := &domain.RewardRedemption{
		ID:          domain.NewRewardRedemptionID(),
		HouseholdID: hB.ID,
		RewardID:    rewardA.ID,
		MemberID:    mB,
		Status:      domain.RedemptionRequested,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := rewardRepo.Redeem(testCtx(t), redemption); !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("Redeem(hB member, hA reward) = %v, want ErrRewardNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// appendEntry helper
// ---------------------------------------------------------------------------

// appendEntry is a test convenience that appends a manual point entry with a
// specific created_at timestamp. It uses source_type = "manual" with a nil
// source_id so the partial unique index (which applies only to source_type =
// 'task_instance' rows with a non-null source_id) is never triggered, letting
// each call insert an independent ledger row.
func appendEntry(
	t *testing.T,
	repo *adapter.PointLedgerPostgresRepository,
	householdID household.HouseholdID,
	memberID household.MemberID,
	points int,
	at time.Time,
) {
	t.Helper()
	entry := &domain.PointEntry{
		ID:          domain.NewPointEntryID(),
		HouseholdID: householdID,
		MemberID:    memberID,
		SourceType:  "manual",
		SourceID:    nil,
		Points:      points,
		CreatedAt:   at,
	}
	if err := repo.Append(testCtx(t), entry); err != nil {
		t.Fatalf("appendEntry: %v", err)
	}
}
