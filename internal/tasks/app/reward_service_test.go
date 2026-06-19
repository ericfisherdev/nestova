package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// fakeRewardRedeemer — in-memory implementation of app.RewardRedeemer
// ---------------------------------------------------------------------------

// fakeRewardRedeemer is a configurable fake that covers all branches of
// RewardService.Redeem without a database. Its fields allow per-test
// injection of sentinel errors and a pre-set reward record.
type fakeRewardRedeemer struct {
	// reward is returned by GetReward when not nil and rewardErr is nil.
	reward *domain.Reward
	// rewardErr overrides GetReward's return when non-nil.
	rewardErr error
	// redeemErr overrides RedeemWithDebit's return when non-nil.
	redeemErr error
	// redeemCalls counts how many times RedeemWithDebit was called so tests
	// can assert that a failed balance/notfound guard skips the debit.
	redeemCalls int
}

func (f *fakeRewardRedeemer) GetReward(
	_ context.Context,
	_ household.HouseholdID,
	_ domain.RewardID,
) (*domain.Reward, error) {
	if f.rewardErr != nil {
		return nil, f.rewardErr
	}
	return f.reward, nil
}

func (f *fakeRewardRedeemer) RedeemWithDebit(
	_ context.Context,
	_ *domain.RewardRedemption,
	_ int,
) error {
	f.redeemCalls++
	return f.redeemErr
}

// Compile-time assertion.
var _ app.RewardRedeemer = (*fakeRewardRedeemer)(nil)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newActiveReward(householdID household.HouseholdID, cost int) *domain.Reward {
	return &domain.Reward{
		ID:          domain.NewRewardID(),
		HouseholdID: householdID,
		Name:        "Test reward",
		CostPoints:  cost,
		Active:      true,
	}
}

// ---------------------------------------------------------------------------
// RewardService constructor validation
// ---------------------------------------------------------------------------

func TestNewRewardService_NilRepo_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewRewardService(nil repo) did not panic")
		}
	}()
	app.NewRewardService(nil, newTestLogger())
}

func TestNewRewardService_NilLogger_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewRewardService(nil logger) did not panic")
		}
	}()
	app.NewRewardService(&fakeRewardRedeemer{}, nil)
}

// ---------------------------------------------------------------------------
// RewardService.Redeem — success path
// ---------------------------------------------------------------------------

// TestRewardService_Redeem_Success verifies that a successful redemption
// returns a RewardRedemption with status 'requested' and calls RedeemWithDebit
// exactly once.
func TestRewardService_Redeem_Success(t *testing.T) {
	hhID := household.NewHouseholdID()
	reward := newActiveReward(hhID, 50)
	repo := &fakeRewardRedeemer{reward: reward}
	svc := app.NewRewardService(repo, newTestLogger())

	memberID := household.NewMemberID()
	redemption, err := svc.Redeem(t.Context(), hhID, memberID, reward.ID)
	if err != nil {
		t.Fatalf("Redeem: unexpected error: %v", err)
	}
	if redemption == nil {
		t.Fatal("Redeem: returned nil redemption")
	}
	if redemption.Status != domain.RedemptionRequested {
		t.Errorf("Status = %v, want %v", redemption.Status, domain.RedemptionRequested)
	}
	if redemption.HouseholdID != hhID {
		t.Errorf("HouseholdID = %v, want %v", redemption.HouseholdID, hhID)
	}
	if redemption.MemberID != memberID {
		t.Errorf("MemberID = %v, want %v", redemption.MemberID, memberID)
	}
	if redemption.RewardID != reward.ID {
		t.Errorf("RewardID = %v, want %v", redemption.RewardID, reward.ID)
	}
	if repo.redeemCalls != 1 {
		t.Errorf("RedeemWithDebit called %d times, want 1", repo.redeemCalls)
	}
}

// ---------------------------------------------------------------------------
// RewardService.Redeem — ErrRewardNotFound from GetReward
// ---------------------------------------------------------------------------

// TestRewardService_Redeem_RewardNotFound verifies that ErrRewardNotFound
// from GetReward is propagated without calling RedeemWithDebit.
func TestRewardService_Redeem_RewardNotFound(t *testing.T) {
	repo := &fakeRewardRedeemer{rewardErr: domain.ErrRewardNotFound}
	svc := app.NewRewardService(repo, newTestLogger())

	_, err := svc.Redeem(t.Context(), household.NewHouseholdID(), household.NewMemberID(), domain.NewRewardID())
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("Redeem(unknown reward) = %v, want ErrRewardNotFound", err)
	}
	if repo.redeemCalls != 0 {
		t.Errorf("RedeemWithDebit called %d times, want 0", repo.redeemCalls)
	}
}

// ---------------------------------------------------------------------------
// RewardService.Redeem — inactive reward treated as ErrRewardNotFound
// ---------------------------------------------------------------------------

// TestRewardService_Redeem_InactiveReward verifies that a reward with
// Active=false is treated as ErrRewardNotFound, protecting retired rewards
// from further redemptions.
func TestRewardService_Redeem_InactiveReward(t *testing.T) {
	hhID := household.NewHouseholdID()
	inactiveReward := &domain.Reward{
		ID:          domain.NewRewardID(),
		HouseholdID: hhID,
		Name:        "Retired reward",
		CostPoints:  20,
		Active:      false, // retired
	}
	repo := &fakeRewardRedeemer{reward: inactiveReward}
	svc := app.NewRewardService(repo, newTestLogger())

	_, err := svc.Redeem(t.Context(), hhID, household.NewMemberID(), inactiveReward.ID)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("Redeem(inactive reward) = %v, want ErrRewardNotFound", err)
	}
	if repo.redeemCalls != 0 {
		t.Errorf("RedeemWithDebit called %d times, want 0 (inactive guard must fire first)", repo.redeemCalls)
	}
}

// ---------------------------------------------------------------------------
// RewardService.Redeem — ErrInsufficientPoints from RedeemWithDebit
// ---------------------------------------------------------------------------

// TestRewardService_Redeem_InsufficientPoints verifies that ErrInsufficientPoints
// returned by RedeemWithDebit is propagated correctly.
func TestRewardService_Redeem_InsufficientPoints(t *testing.T) {
	hhID := household.NewHouseholdID()
	reward := newActiveReward(hhID, 100)
	repo := &fakeRewardRedeemer{
		reward:    reward,
		redeemErr: domain.ErrInsufficientPoints,
	}
	svc := app.NewRewardService(repo, newTestLogger())

	_, err := svc.Redeem(t.Context(), hhID, household.NewMemberID(), reward.ID)
	if !errors.Is(err, domain.ErrInsufficientPoints) {
		t.Errorf("Redeem(insufficient) = %v, want ErrInsufficientPoints", err)
	}
	if repo.redeemCalls != 1 {
		t.Errorf("RedeemWithDebit called %d times, want 1", repo.redeemCalls)
	}
}

// ---------------------------------------------------------------------------
// RewardService.Redeem — ErrRewardNotFound from RedeemWithDebit (FK race)
// ---------------------------------------------------------------------------

// TestRewardService_Redeem_FKViolationFromDebit verifies that ErrRewardNotFound
// returned by RedeemWithDebit (FK race between GetReward and insert) is
// propagated correctly.
func TestRewardService_Redeem_FKViolationFromDebit(t *testing.T) {
	hhID := household.NewHouseholdID()
	reward := newActiveReward(hhID, 30)
	repo := &fakeRewardRedeemer{
		reward:    reward,
		redeemErr: domain.ErrRewardNotFound,
	}
	svc := app.NewRewardService(repo, newTestLogger())

	_, err := svc.Redeem(t.Context(), hhID, household.NewMemberID(), reward.ID)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("Redeem(FK race) = %v, want ErrRewardNotFound", err)
	}
}
