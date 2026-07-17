package adapter_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// RewardPostgresRepository.RedeemWithDebit — finite-stock race (NES-127)
// ---------------------------------------------------------------------------

// TestRedeemWithDebit_FiniteStockRaceExactlyOneWins proves the FOR UPDATE
// reward-row lock serialises TWO DIFFERENT members concurrently redeeming the
// last unit of a finite-stock reward — the case the per-member advisory lock
// (see TestRedeemWithDebit_ConcurrentRedeemsSerialized) cannot cover, since
// that lock is scoped per (household, member), not per reward. Each member
// has ample individual balance, so ErrInsufficientPoints can never be the
// cause of either outcome — the only guard in play is the finite-stock check.
//
// Both goroutines are held at a shared start gate (readied via a WaitGroup,
// released by closing start) so neither can begin its transaction before the
// other is already spawned and waiting — without this, the Go scheduler could
// run the two RedeemWithDebit calls back-to-back with no real overlap, and a
// bug in the locking protocol could slip through undetected. The whole
// scenario runs several iterations, each against a fresh reward/redemption
// pair, for the same reason table-driven concurrency tests generally do: one
// lucky interleaving proves nothing about a race that only sometimes fires.
func TestRedeemWithDebit_FiniteStockRaceExactlyOneWins(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)

	const iterations = 5
	for i := range iterations {
		h, m1, m2 := seedHousehold(t, pool)

		const cost = 20
		seedBalanceForMember(t, ledgerRepo, h.ID, m1, 1000)
		seedBalanceForMember(t, ledgerRepo, h.ID, m2, 1000)

		reward := &domain.Reward{
			ID:                domain.NewRewardID(),
			HouseholdID:       h.ID,
			Name:              "Last unit",
			CostPoints:        cost,
			QuantityAvailable: intPtr(1), // exactly one unit — only one redeemer can win.
			Active:            true,
		}
		if err := rewardRepo.CreateReward(testCtx(t), reward); err != nil {
			t.Fatalf("iteration %d: CreateReward: %v", i, err)
		}

		redemptions := []*domain.RewardRedemption{
			buildRedemption(h.ID, m1, reward.ID),
			buildRedemption(h.ID, m2, reward.ID),
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)

		var (
			ready   sync.WaitGroup
			wg      sync.WaitGroup
			mu      sync.Mutex
			results []error
		)
		start := make(chan struct{})
		ready.Add(len(redemptions))
		wg.Add(len(redemptions))
		for _, red := range redemptions {
			go func(r *domain.RewardRedemption) {
				defer wg.Done()
				ready.Done()
				<-start // block until every goroutine has reached this point
				_, err := rewardRepo.RedeemWithDebit(ctx, r)
				mu.Lock()
				results = append(results, err)
				mu.Unlock()
			}(red)
		}
		ready.Wait() // every goroutine is spawned and waiting at the gate
		close(start) // release them all at once
		wg.Wait()
		cancel()

		var successCount, outOfStockCount int
		for _, err := range results {
			switch {
			case err == nil:
				successCount++
			case errors.Is(err, domain.ErrRewardOutOfStock):
				outOfStockCount++
			default:
				t.Fatalf("iteration %d: unexpected redeem error: %v", i, err)
			}
		}
		if successCount != 1 {
			t.Errorf("iteration %d: successCount = %d, want exactly 1", i, successCount)
		}
		if outOfStockCount != 1 {
			t.Errorf("iteration %d: outOfStockCount = %d, want exactly 1", i, outOfStockCount)
		}

		// Exactly one redemption row must exist for this reward — no overshoot.
		const countQ = `SELECT COUNT(*) FROM reward_redemption WHERE reward_id = $1`
		var redemptionCount int
		if err := pool.QueryRow(testCtx(t), countQ, reward.ID.String()).Scan(&redemptionCount); err != nil {
			t.Fatalf("iteration %d: count redemption rows: %v", i, err)
		}
		if redemptionCount != 1 {
			t.Errorf("iteration %d: redemption rows = %d, want exactly 1 (no overshoot)", i, redemptionCount)
		}
	}
}

// TestRedeemWithDebit_OutOfStockRollsBack verifies that a redeem attempt
// against an already-exhausted finite-stock reward returns
// ErrRewardOutOfStock and writes NO rows at all for the rejected attempt —
// neither a reward_redemption row nor a point_ledger row keyed by its own id,
// not merely an unchanged aggregate balance (which alone would not catch a
// rollback that left an orphaned row behind).
func TestRedeemWithDebit_OutOfStockRollsBack(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 100)
	seedBalanceForMember(t, ledgerRepo, h.ID, m2, 100)

	reward := &domain.Reward{
		ID:                domain.NewRewardID(),
		HouseholdID:       h.ID,
		Name:              "One only",
		CostPoints:        10,
		QuantityAvailable: intPtr(1),
		Active:            true,
	}
	if err := rewardRepo.CreateReward(testCtx(t), reward); err != nil {
		t.Fatalf("CreateReward: %v", err)
	}

	// First redemption consumes the only unit.
	first := buildRedemption(h.ID, m1, reward.ID)
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), first); err != nil {
		t.Fatalf("RedeemWithDebit (first): %v", err)
	}

	// Second, distinct member is rejected — no stock left.
	second := buildRedemption(h.ID, m2, reward.ID)
	_, err := rewardRepo.RedeemWithDebit(testCtx(t), second)
	if !errors.Is(err, domain.ErrRewardOutOfStock) {
		t.Fatalf("RedeemWithDebit (second): %v, want ErrRewardOutOfStock", err)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m2)
	if err != nil {
		t.Fatalf("Balance(m2): %v", err)
	}
	if balance != 100 {
		t.Errorf("Balance(m2) = %d, want 100 (unchanged after rollback)", balance)
	}

	// The rejected attempt's OWN redemption row must not exist at all.
	const redemptionCountQ = `SELECT COUNT(*) FROM reward_redemption WHERE id = $1`
	var redemptionCount int
	if err := pool.QueryRow(testCtx(t), redemptionCountQ, second.ID.String()).Scan(&redemptionCount); err != nil {
		t.Fatalf("count second's redemption rows: %v", err)
	}
	if redemptionCount != 0 {
		t.Errorf("second's redemption rows = %d, want 0 (rollback)", redemptionCount)
	}

	// Nor must a debit ledger row keyed by the rejected attempt's id.
	const debitCountQ = `SELECT COUNT(*) FROM point_ledger WHERE source_type = 'redemption' AND source_id = $1`
	var debitCount int
	if err := pool.QueryRow(testCtx(t), debitCountQ, second.ID.String()).Scan(&debitCount); err != nil {
		t.Fatalf("count second's debit ledger rows: %v", err)
	}
	if debitCount != 0 {
		t.Errorf("second's debit ledger rows = %d, want 0 (rollback)", debitCount)
	}
}

// TestRedeemWithDebit_DeniedRedemptionFreesStock verifies that a denied
// redemption does not count against a finite-stock reward's cap at the
// RedeemWithDebit write path, mirroring
// TestRewardRepository_ListStorefrontRewards_IgnoresCancelledRedemptions'
// (NES-126) read-path coverage for the same NOT IN ('cancelled', 'denied')
// predicate.
func TestRedeemWithDebit_DeniedRedemptionFreesStock(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 100)
	seedBalanceForMember(t, ledgerRepo, h.ID, m2, 100)

	reward := &domain.Reward{
		ID:                domain.NewRewardID(),
		HouseholdID:       h.ID,
		Name:              "One only",
		CostPoints:        10,
		QuantityAvailable: intPtr(1),
		Active:            true,
	}
	if err := rewardRepo.CreateReward(testCtx(t), reward); err != nil {
		t.Fatalf("CreateReward: %v", err)
	}

	first := buildRedemption(h.ID, m1, reward.ID)
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), first); err != nil {
		t.Fatalf("RedeemWithDebit (first): %v", err)
	}

	// Deny the first redemption — this must free its reserved unit.
	if _, err := rewardRepo.Deny(testCtx(t), h.ID, first.ID, "changed my mind"); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	// A second, distinct member can now redeem the freed unit.
	second := buildRedemption(h.ID, m2, reward.ID)
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), second); err != nil {
		t.Fatalf("RedeemWithDebit (after deny): %v, want success (denied redemption frees stock)", err)
	}
}

// ---------------------------------------------------------------------------
// RewardPostgresRepository.RedeemWithDebit — TOCTOU (CodeRabbit finding, NES-127)
// ---------------------------------------------------------------------------

// TestRedeemWithDebit_UsesLockedPriceNotStaleValue proves RedeemWithDebit
// never trusts a caller-supplied price: it debits whatever cost_points the
// reward row holds AT THE MOMENT it locks that row, not whatever an earlier
// read (e.g. the app layer's GetReward, before this call) might have seen.
// The reward's price is changed via UpdateReward — simulating a concurrent
// price edit that committed in the gap between an app-layer pre-read and
// this call — and the debited amount and ledger entry must reflect only the
// NEW price.
func TestRedeemWithDebit_UsesLockedPriceNotStaleValue(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Movie night", 10)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 100)

	// Simulate a concurrent price edit that committed after some caller last
	// read this reward but before RedeemWithDebit runs: bump the price up.
	reward.CostPoints = 50
	if err := rewardRepo.UpdateReward(testCtx(t), reward); err != nil {
		t.Fatalf("UpdateReward (price change): %v", err)
	}

	redemption := buildRedemption(h.ID, m1, reward.ID)
	debited, err := rewardRepo.RedeemWithDebit(testCtx(t), redemption)
	if err != nil {
		t.Fatalf("RedeemWithDebit: %v", err)
	}
	if debited != 50 {
		t.Errorf("RedeemWithDebit debited = %d, want 50 (the current, locked price — not the original 10)", debited)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 50 {
		t.Errorf("Balance after redeem = %d, want 50 (100 - the new price of 50)", balance)
	}
}

// TestRedeemWithDebit_ArchivedRewardRejected proves RedeemWithDebit rejects a
// reward that was archived after an earlier read might have seen it as
// active — the FOR UPDATE lock re-reads Active fresh every time, so a
// concurrent archive that committed in the gap between an app-layer pre-read
// and this call is caught here, not missed.
func TestRedeemWithDebit_ArchivedRewardRejected(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Retiring soon", 10)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 100)

	// Simulate a concurrent archive that committed after some caller last
	// read this reward as active but before RedeemWithDebit runs.
	if err := rewardRepo.ArchiveReward(testCtx(t), h.ID, reward.ID); err != nil {
		t.Fatalf("ArchiveReward: %v", err)
	}

	redemption := buildRedemption(h.ID, m1, reward.ID)
	_, err := rewardRepo.RedeemWithDebit(testCtx(t), redemption)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("RedeemWithDebit(archived reward) = %v, want ErrRewardNotFound", err)
	}

	// No debit must have occurred.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 100 {
		t.Errorf("Balance after rejected redeem = %d, want 100 (unchanged)", balance)
	}
}

// ---------------------------------------------------------------------------
// RewardPostgresRepository.Fulfill (NES-127)
// ---------------------------------------------------------------------------

// TestRewardRepository_Fulfill_Success verifies that Fulfill transitions a
// pending redemption to fulfilled and returns the enriched ResolvedRedemption
// without touching the member's balance (points are debited at redemption
// time, not fulfilment).
func TestRewardRepository_Fulfill_Success(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Movie night", 20)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 20)
	redemption := buildRedemption(h.ID, m1, reward.ID)
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), redemption); err != nil {
		t.Fatalf("RedeemWithDebit: %v", err)
	}

	resolved, err := rewardRepo.Fulfill(testCtx(t), h.ID, redemption.ID)
	if err != nil {
		t.Fatalf("Fulfill: %v", err)
	}
	if resolved.Status != domain.RedemptionFulfilled {
		t.Errorf("Status = %v, want RedemptionFulfilled", resolved.Status)
	}
	if resolved.MemberID != m1 {
		t.Errorf("MemberID = %v, want %v", resolved.MemberID, m1)
	}
	if resolved.RewardName != "Movie night" {
		t.Errorf("RewardName = %q, want %q", resolved.RewardName, "Movie night")
	}

	// Balance is unaffected — the debit already happened at redemption time.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 0 {
		t.Errorf("Balance after fulfil = %d, want 0 (no change from fulfilment)", balance)
	}
}

// TestRewardRepository_Fulfill_AlreadyFulfilledReturnsNotPending verifies
// that fulfilling an already-fulfilled redemption returns
// ErrRedemptionNotPending (no double-fulfilment).
func TestRewardRepository_Fulfill_AlreadyFulfilledReturnsNotPending(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Coffee", 5)
	redemption := seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionFulfilled)

	_, err := rewardRepo.Fulfill(testCtx(t), h.ID, redemption.ID)
	if !errors.Is(err, domain.ErrRedemptionNotPending) {
		t.Errorf("Fulfill(already fulfilled) = %v, want ErrRedemptionNotPending", err)
	}
}

// TestRewardRepository_Fulfill_UnknownReturnsNotFound verifies that
// fulfilling an unknown redemption id returns ErrRedemptionNotFound —
// disambiguated from ErrRedemptionNotPending by the existence check.
func TestRewardRepository_Fulfill_UnknownReturnsNotFound(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	_, err := rewardRepo.Fulfill(testCtx(t), h.ID, domain.NewRewardRedemptionID())
	if !errors.Is(err, domain.ErrRedemptionNotFound) {
		t.Errorf("Fulfill(unknown) = %v, want ErrRedemptionNotFound", err)
	}
}

// TestRewardRepository_Fulfill_CrossHouseholdReturnsNotFound verifies that a
// redemption belonging to another household is treated as not found.
func TestRewardRepository_Fulfill_CrossHouseholdReturnsNotFound(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	hA, mA, _ := seedHousehold(t, pool)
	hB, _, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, hA.ID, "hA reward", 10)
	redemption := seedRedemption(t, rewardRepo, hA.ID, reward.ID, mA, domain.RedemptionPending)

	_, err := rewardRepo.Fulfill(testCtx(t), hB.ID, redemption.ID)
	if !errors.Is(err, domain.ErrRedemptionNotFound) {
		t.Errorf("Fulfill(cross-household) = %v, want ErrRedemptionNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// RewardPostgresRepository.Deny (NES-127)
// ---------------------------------------------------------------------------

// TestRewardRepository_Deny_RefundsExactAmountAndRecordsReason verifies that
// Deny transitions the redemption to denied, stores the reason, and appends a
// compensating ledger entry that restores the balance exactly — the original
// debit row is left untouched (history preserved, no mutated rows) — and that
// the refund is visible through PointLedgerPostgresRepository.History with
// the reward's name resolved, proving the refund join
// (source_type = 'redemption_refund') actually works end to end.
func TestRewardRepository_Deny_RefundsExactAmountAndRecordsReason(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Extra dessert", 15)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 15)
	redemption := buildRedemption(h.ID, m1, reward.ID)
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), redemption); err != nil {
		t.Fatalf("RedeemWithDebit: %v", err)
	}

	resolved, err := rewardRepo.Deny(testCtx(t), h.ID, redemption.ID, "out of ingredients")
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if resolved.Status != domain.RedemptionDenied {
		t.Errorf("Status = %v, want RedemptionDenied", resolved.Status)
	}
	if resolved.DeniedReason == nil || *resolved.DeniedReason != "out of ingredients" {
		t.Errorf("DeniedReason = %v, want %q", resolved.DeniedReason, "out of ingredients")
	}

	// Balance must be restored to exactly the pre-redemption amount.
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 15 {
		t.Errorf("Balance after deny = %d, want 15 (fully refunded)", balance)
	}

	// The original debit row must still exist, unmutated.
	const debitQ = `SELECT points FROM point_ledger WHERE source_type = 'redemption' AND source_id = $1`
	var debitPoints int
	if err := pool.QueryRow(testCtx(t), debitQ, redemption.ID.String()).Scan(&debitPoints); err != nil {
		t.Fatalf("read debit row: %v", err)
	}
	if debitPoints != -15 {
		t.Errorf("original debit points = %d, want -15 (unmutated)", debitPoints)
	}

	// A separate refund row must exist with source_type = 'redemption_refund'.
	const refundQ = `SELECT points FROM point_ledger WHERE source_type = $1 AND source_id = $2`
	var refundPoints int
	if err := pool.QueryRow(testCtx(t), refundQ, domain.SourceTypeRedemptionRefund, redemption.ID.String()).Scan(&refundPoints); err != nil {
		t.Fatalf("read refund row: %v", err)
	}
	if refundPoints != 15 {
		t.Errorf("refund points = %d, want 15", refundPoints)
	}

	// The refund must also surface through History, joined to the reward's
	// name via the reward_redemption→reward chain, exactly like the original
	// debit row.
	history, err := ledgerRepo.History(testCtx(t), h.ID, m1, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	var refundEntry *domain.PointHistoryEntry
	for i := range history {
		if history[i].SourceType == domain.SourceTypeRedemptionRefund {
			refundEntry = &history[i]
			break
		}
	}
	if refundEntry == nil {
		t.Fatal("History does not contain a redemption_refund entry")
	}
	if refundEntry.Points != 15 {
		t.Errorf("refund history entry Points = %d, want 15", refundEntry.Points)
	}
	if refundEntry.RewardName != "Extra dessert" {
		t.Errorf("refund history entry RewardName = %q, want %q", refundEntry.RewardName, "Extra dessert")
	}
}

// TestRewardRepository_Deny_EmptyReasonStoresNull verifies that denying with
// an empty reason string stores a NULL denied_reason, not an empty string
// row.
func TestRewardRepository_Deny_EmptyReasonStoresNull(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Snack", 5)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 5)
	redemption := buildRedemption(h.ID, m1, reward.ID)
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), redemption); err != nil {
		t.Fatalf("RedeemWithDebit: %v", err)
	}

	resolved, err := rewardRepo.Deny(testCtx(t), h.ID, redemption.ID, "")
	if err != nil {
		t.Fatalf("Deny(empty reason): %v", err)
	}
	if resolved.DeniedReason != nil {
		t.Errorf("DeniedReason = %v, want nil for an empty reason", resolved.DeniedReason)
	}
}

// TestRewardRepository_Deny_NotPendingReturnsError verifies that denying a
// non-pending redemption (already fulfilled) returns ErrRedemptionNotPending
// and writes no refund row.
func TestRewardRepository_Deny_NotPendingReturnsError(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Coffee", 5)
	redemption := seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionFulfilled)

	_, err := rewardRepo.Deny(testCtx(t), h.ID, redemption.ID, "too late")
	if !errors.Is(err, domain.ErrRedemptionNotPending) {
		t.Errorf("Deny(already fulfilled) = %v, want ErrRedemptionNotPending", err)
	}

	const refundCountQ = `SELECT COUNT(*) FROM point_ledger WHERE source_type = $1 AND source_id = $2`
	var count int
	if err := pool.QueryRow(testCtx(t), refundCountQ, domain.SourceTypeRedemptionRefund, redemption.ID.String()).Scan(&count); err != nil {
		t.Fatalf("count refund rows: %v", err)
	}
	if count != 0 {
		t.Errorf("refund rows = %d, want 0 (denial rejected, no refund written)", count)
	}
}

// ---------------------------------------------------------------------------
// RewardPostgresRepository.Cancel (NES-127)
// ---------------------------------------------------------------------------

// TestRewardRepository_Cancel_RefundsExactAmount verifies that a member
// cancelling their own pending redemption is refunded exactly, mirroring
// Deny's refund guarantee.
func TestRewardRepository_Cancel_RefundsExactAmount(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Comic book", 12)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 12)
	redemption := buildRedemption(h.ID, m1, reward.ID)
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), redemption); err != nil {
		t.Fatalf("RedeemWithDebit: %v", err)
	}

	resolved, err := rewardRepo.Cancel(testCtx(t), h.ID, redemption.ID, m1)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if resolved.Status != domain.RedemptionCancelled {
		t.Errorf("Status = %v, want RedemptionCancelled", resolved.Status)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 12 {
		t.Errorf("Balance after cancel = %d, want 12 (fully refunded)", balance)
	}
}

// TestRewardRepository_Cancel_WrongMemberReturnsNotPending verifies that a
// member cannot cancel another member's redemption — the sentinel is
// ErrRedemptionNotPending (undisambiguated from "not found"), matching
// ChoreTradeRepository.Decline's precedent.
func TestRewardRepository_Cancel_WrongMemberReturnsNotPending(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Book", 8)
	redemption := seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionPending)

	_, err := rewardRepo.Cancel(testCtx(t), h.ID, redemption.ID, m2)
	if !errors.Is(err, domain.ErrRedemptionNotPending) {
		t.Errorf("Cancel(wrong member) = %v, want ErrRedemptionNotPending", err)
	}
}

// TestRewardRepository_Cancel_NotPendingReturnsError verifies that cancelling
// an already-cancelled redemption returns ErrRedemptionNotPending and does
// not double-refund.
func TestRewardRepository_Cancel_NotPendingReturnsError(t *testing.T) {
	pool := newTestPool(t)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Toy", 9)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 9)
	redemption := buildRedemption(h.ID, m1, reward.ID)
	if _, err := rewardRepo.RedeemWithDebit(testCtx(t), redemption); err != nil {
		t.Fatalf("RedeemWithDebit: %v", err)
	}
	if _, err := rewardRepo.Cancel(testCtx(t), h.ID, redemption.ID, m1); err != nil {
		t.Fatalf("Cancel (first): %v", err)
	}

	_, err := rewardRepo.Cancel(testCtx(t), h.ID, redemption.ID, m1)
	if !errors.Is(err, domain.ErrRedemptionNotPending) {
		t.Errorf("Cancel (second) = %v, want ErrRedemptionNotPending", err)
	}

	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 9 {
		t.Errorf("Balance after double-cancel attempt = %d, want 9 (refunded exactly once)", balance)
	}
}

// ---------------------------------------------------------------------------
// RewardRepository — ListPendingRedemptions / ListMemberRedemptions (NES-127)
// ---------------------------------------------------------------------------

// TestRewardRepository_ListPendingRedemptions_OnlyPendingOldestFirst verifies
// that ListPendingRedemptions returns only pending redemptions, oldest first,
// each enriched with its reward's name.
func TestRewardRepository_ListPendingRedemptions_OnlyPendingOldestFirst(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Ice cream", 10)

	older := seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionPending)
	time.Sleep(5 * time.Millisecond) // ensure a distinct created_at ordering
	newer := seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionPending)
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionFulfilled) // must be excluded

	got, err := rewardRepo.ListPendingRedemptions(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListPendingRedemptions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListPendingRedemptions = %d rows, want 2", len(got))
	}
	if got[0].ID != older.ID {
		t.Errorf("got[0].ID = %v, want %v (oldest first)", got[0].ID, older.ID)
	}
	if got[1].ID != newer.ID {
		t.Errorf("got[1].ID = %v, want %v", got[1].ID, newer.ID)
	}
	if got[0].RewardName != "Ice cream" {
		t.Errorf("got[0].RewardName = %q, want %q", got[0].RewardName, "Ice cream")
	}
}

// TestRewardRepository_ListPendingRedemptions_EmptyReturnsSlice verifies that
// ListPendingRedemptions returns an empty slice (not an error) when nothing
// is pending.
func TestRewardRepository_ListPendingRedemptions_EmptyReturnsSlice(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, _, _ := seedHousehold(t, pool)

	got, err := rewardRepo.ListPendingRedemptions(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListPendingRedemptions(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListPendingRedemptions(empty) = %d rows, want 0", len(got))
	}
}

// TestRewardRepository_ListMemberRedemptions_NewestFirstAllStatuses verifies
// that ListMemberRedemptions returns the member's own redemptions regardless
// of status, newest first, and excludes another member's redemptions.
func TestRewardRepository_ListMemberRedemptions_NewestFirstAllStatuses(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Sticker pack", 3)

	older := seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionFulfilled)
	time.Sleep(5 * time.Millisecond)
	newer := seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionPending)
	seedRedemption(t, rewardRepo, h.ID, reward.ID, m2, domain.RedemptionPending) // different member — excluded

	got, err := rewardRepo.ListMemberRedemptions(testCtx(t), h.ID, m1, 10)
	if err != nil {
		t.Fatalf("ListMemberRedemptions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListMemberRedemptions = %d rows, want 2", len(got))
	}
	if got[0].ID != newer.ID {
		t.Errorf("got[0].ID = %v, want %v (newest first)", got[0].ID, newer.ID)
	}
	if got[1].ID != older.ID {
		t.Errorf("got[1].ID = %v, want %v", got[1].ID, older.ID)
	}
}

// TestRewardRepository_ListMemberRedemptions_LimitApplied verifies that the
// limit parameter caps the number of returned rows.
func TestRewardRepository_ListMemberRedemptions_LimitApplied(t *testing.T) {
	pool := newTestPool(t)
	rewardRepo := adapter.NewRewardPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)

	reward := seedReward(t, rewardRepo, h.ID, "Badge", 1)
	for range 3 {
		seedRedemption(t, rewardRepo, h.ID, reward.ID, m1, domain.RedemptionPending)
	}

	got, err := rewardRepo.ListMemberRedemptions(testCtx(t), h.ID, m1, 2)
	if err != nil {
		t.Fatalf("ListMemberRedemptions(limit=2): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListMemberRedemptions(limit=2) = %d rows, want 2", len(got))
	}
}
