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
) (int, error) {
	f.redeemCalls++
	if f.redeemErr != nil {
		return 0, f.redeemErr
	}
	// Mirror the real adapter: the debited cost comes from the (here, fake)
	// locked reward row, never from a caller-supplied value.
	if f.reward != nil {
		return f.reward.CostPoints, nil
	}
	return 0, nil
}

// Compile-time assertion.
var _ app.RewardRedeemer = (*fakeRewardRedeemer)(nil)

// ---------------------------------------------------------------------------
// fakeMemberLister — in-memory implementation of app.HouseholdMemberLister
// ---------------------------------------------------------------------------

// fakeMemberLister is a configurable fake used by RewardService tests to
// control which household members are candidates for the parent-notification
// step (NES-127) without a database.
type fakeMemberLister struct {
	members []*household.Member
	err     error
}

func (f *fakeMemberLister) ListMembers(
	_ context.Context,
	_ household.HouseholdID,
) ([]*household.Member, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.members, nil
}

// Compile-time assertion.
var _ app.HouseholdMemberLister = (*fakeMemberLister)(nil)

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

// newRewardService constructs a RewardService with a fresh no-op member
// lister and enqueuer, for tests that do not care about the parent
// notification step. Tests that DO care build the service directly.
func newRewardService(repo app.RewardRedeemer) *app.RewardService {
	return app.NewRewardService(repo, &fakeMemberLister{}, newFakeEnqueuer(), newTestLogger())
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
	app.NewRewardService(nil, &fakeMemberLister{}, newFakeEnqueuer(), newTestLogger())
}

func TestNewRewardService_NilMembers_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewRewardService(nil members) did not panic")
		}
	}()
	app.NewRewardService(&fakeRewardRedeemer{}, nil, newFakeEnqueuer(), newTestLogger())
}

func TestNewRewardService_NilEnqueuer_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewRewardService(nil enqueuer) did not panic")
		}
	}()
	app.NewRewardService(&fakeRewardRedeemer{}, &fakeMemberLister{}, nil, newTestLogger())
}

func TestNewRewardService_NilLogger_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewRewardService(nil logger) did not panic")
		}
	}()
	app.NewRewardService(&fakeRewardRedeemer{}, &fakeMemberLister{}, newFakeEnqueuer(), nil)
}

// ---------------------------------------------------------------------------
// RewardService.Redeem — success path
// ---------------------------------------------------------------------------

// TestRewardService_Redeem_Success verifies that a successful redemption
// returns a RewardRedemption with status 'pending' and calls RedeemWithDebit
// exactly once.
func TestRewardService_Redeem_Success(t *testing.T) {
	hhID := household.NewHouseholdID()
	reward := newActiveReward(hhID, 50)
	repo := &fakeRewardRedeemer{reward: reward}
	svc := newRewardService(repo)

	memberID := household.NewMemberID()
	redemption, err := svc.Redeem(t.Context(), hhID, memberID, reward.ID)
	if err != nil {
		t.Fatalf("Redeem: unexpected error: %v", err)
	}
	if redemption == nil {
		t.Fatal("Redeem: returned nil redemption")
	}
	if redemption.Status != domain.RedemptionPending {
		t.Errorf("Status = %v, want %v", redemption.Status, domain.RedemptionPending)
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
	svc := newRewardService(repo)

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
	svc := newRewardService(repo)

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
	svc := newRewardService(repo)

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
	svc := newRewardService(repo)

	_, err := svc.Redeem(t.Context(), hhID, household.NewMemberID(), reward.ID)
	if !errors.Is(err, domain.ErrRewardNotFound) {
		t.Errorf("Redeem(FK race) = %v, want ErrRewardNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// RewardService.Redeem — ErrRewardOutOfStock from RedeemWithDebit (NES-127)
// ---------------------------------------------------------------------------

// TestRewardService_Redeem_OutOfStock verifies that ErrRewardOutOfStock
// returned by RedeemWithDebit (finite-stock cap reached) is propagated
// correctly and no notification is enqueued for a failed redemption.
func TestRewardService_Redeem_OutOfStock(t *testing.T) {
	hhID := household.NewHouseholdID()
	reward := newActiveReward(hhID, 15)
	repo := &fakeRewardRedeemer{
		reward:    reward,
		redeemErr: domain.ErrRewardOutOfStock,
	}
	members := &fakeMemberLister{members: []*household.Member{
		{ID: household.NewMemberID(), Role: household.RoleOwner},
	}}
	enqueuer := newFakeEnqueuer()
	svc := app.NewRewardService(repo, members, enqueuer, newTestLogger())

	_, err := svc.Redeem(t.Context(), hhID, household.NewMemberID(), reward.ID)
	if !errors.Is(err, domain.ErrRewardOutOfStock) {
		t.Errorf("Redeem(out of stock) = %v, want ErrRewardOutOfStock", err)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("notifications enqueued = %d, want 0 (failed redemption)", len(enqueuer.notifications))
	}
}

// ---------------------------------------------------------------------------
// RewardService.Redeem — parent notification (NES-127)
// ---------------------------------------------------------------------------

// TestRewardService_Redeem_NotifiesOnlyParents verifies that a successful
// redemption enqueues exactly one notification per parent (owner/adult)
// member and none for a child member, addressed individually to each parent.
func TestRewardService_Redeem_NotifiesOnlyParents(t *testing.T) {
	hhID := household.NewHouseholdID()
	reward := newActiveReward(hhID, 25)
	repo := &fakeRewardRedeemer{reward: reward}

	owner := &household.Member{ID: household.NewMemberID(), DisplayName: "Owner Olivia", Role: household.RoleOwner}
	adult := &household.Member{ID: household.NewMemberID(), DisplayName: "Adult Alex", Role: household.RoleAdult}
	child := &household.Member{ID: household.NewMemberID(), DisplayName: "Child Charlie", Role: household.RoleChild}
	members := &fakeMemberLister{members: []*household.Member{owner, adult, child}}
	enqueuer := newFakeEnqueuer()
	svc := app.NewRewardService(repo, members, enqueuer, newTestLogger())

	if _, err := svc.Redeem(t.Context(), hhID, child.ID, reward.ID); err != nil {
		t.Fatalf("Redeem: unexpected error: %v", err)
	}

	if len(enqueuer.notifications) != 2 {
		t.Fatalf("notifications enqueued = %d, want 2 (owner + adult only)", len(enqueuer.notifications))
	}
	notified := make(map[household.MemberID]bool, 2)
	for _, n := range enqueuer.notifications {
		if n.MemberID == nil {
			t.Fatal("notification MemberID is nil, want a specific parent")
		}
		notified[*n.MemberID] = true
		if n.HouseholdID != hhID {
			t.Errorf("notification HouseholdID = %v, want %v", n.HouseholdID, hhID)
		}
	}
	if !notified[owner.ID] {
		t.Error("owner was not notified")
	}
	if !notified[adult.ID] {
		t.Error("adult was not notified")
	}
	if notified[child.ID] {
		t.Error("child was notified, want only parents notified")
	}
}

// TestRewardService_Redeem_NotificationFailureDoesNotFailRedeem verifies that
// an enqueue failure is swallowed (logged only): the redemption itself still
// succeeds and is returned to the caller.
func TestRewardService_Redeem_NotificationFailureDoesNotFailRedeem(t *testing.T) {
	hhID := household.NewHouseholdID()
	reward := newActiveReward(hhID, 10)
	repo := &fakeRewardRedeemer{reward: reward}
	members := &fakeMemberLister{members: []*household.Member{
		{ID: household.NewMemberID(), Role: household.RoleOwner},
	}}
	svc := app.NewRewardService(repo, members, &fakeEnqueuerWithError{errOnCall: 1}, newTestLogger())

	redemption, err := svc.Redeem(t.Context(), hhID, household.NewMemberID(), reward.ID)
	if err != nil {
		t.Fatalf("Redeem: unexpected error despite notification failure: %v", err)
	}
	if redemption == nil {
		t.Fatal("Redeem: returned nil redemption despite notification failure")
	}
}
