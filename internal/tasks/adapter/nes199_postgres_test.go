package adapter_test

import (
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// TestCompleteAndAward_LapsedClaimIncursPenalty proves the claim window is
// enforced at completion, not only by the sweep (NES-199): completing one
// minute past the window is charged the claim-expiry penalty, and completing
// one minute inside it is not.
func TestCompleteAndAward_LapsedClaimIncursPenalty(t *testing.T) {
	tests := []struct {
		name        string
		offset      time.Duration // completion time relative to claim expiry
		wantBalance int           // starting balance 20, award 10, penalty 5 if lapsed
		wantPenalty bool
	}{
		{name: "11:59 awards in full", offset: -time.Minute, wantBalance: 30},
		{name: "12:01 is penalized", offset: time.Minute, wantBalance: 25, wantPenalty: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			taskRepo := adapter.NewRecurringTaskRepository(pool)
			instRepo := adapter.NewTaskInstanceRepository(pool)
			ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
			h, m1, _ := seedHousehold(t, pool)
			seedBalanceForMember(t, ledgerRepo, h.ID, m1, 20)

			rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10)
			inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
			if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			at := claimed.ClaimExpiresAt.Add(tc.offset)
			if err := instRepo.CompleteAndAward(testCtx(t), h.ID, inst.ID, m1, at); err != nil {
				t.Fatalf("CompleteAndAward: %v", err)
			}

			balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
			if err != nil {
				t.Fatalf("Balance: %v", err)
			}
			if balance != tc.wantBalance {
				t.Errorf("Balance = %d, want %d", balance, tc.wantBalance)
			}

			// The sweep that follows must not penalize the same window twice.
			swept, err := instRepo.SweepExpiredClaims(testCtx(t), farFutureAsOf())
			if err != nil {
				t.Fatalf("SweepExpiredClaims: %v", err)
			}
			if len(swept) != 0 {
				t.Errorf("sweep returned %d claims after completion, want 0", len(swept))
			}
			after, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
			if err != nil {
				t.Fatalf("Balance after sweep: %v", err)
			}
			if after != tc.wantBalance {
				t.Errorf("Balance after sweep = %d, want %d", after, tc.wantBalance)
			}
		})
	}
}

// TestComplete_LapsedClaimIncursPenalty covers the non-awarding Complete path.
func TestComplete_LapsedClaimIncursPenalty(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	ledgerRepo := adapter.NewPointLedgerPostgresRepository(pool)
	h, m1, _ := seedHousehold(t, pool)
	seedBalanceForMember(t, ledgerRepo, h.ID, m1, 20)

	rt := seedRecurringTaskWithPoints(t, taskRepo, h.ID, 10)
	inst := seedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 7))
	if err := instRepo.Claim(testCtx(t), h.ID, inst.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := instRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	at := claimed.ClaimExpiresAt.Add(time.Minute)
	if err := instRepo.Complete(testCtx(t), h.ID, inst.ID, m1, at); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	balance, err := ledgerRepo.Balance(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := 20 - domain.ClaimExpiryPenalty(10); balance != want {
		t.Errorf("Balance = %d, want %d", balance, want)
	}
}
