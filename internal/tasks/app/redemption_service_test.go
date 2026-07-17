package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// fakeRedemptionFulfiller — in-memory implementation of app.RedemptionFulfiller
// ---------------------------------------------------------------------------

// fulfillCall, denyCall, and cancelCall record one invocation's full argument
// set — not just a count — so a test can assert the service passed through
// the exact household, redemption, and (for Cancel) member id it was given,
// not merely that the method was called some number of times (CodeRabbit
// finding, NES-127 round 4).
type fulfillCall struct {
	householdID household.HouseholdID
	id          domain.RewardRedemptionID
}

type denyCall struct {
	householdID household.HouseholdID
	id          domain.RewardRedemptionID
	reason      string
}

type cancelCall struct {
	householdID household.HouseholdID
	id          domain.RewardRedemptionID
	memberID    household.MemberID
}

// fakeRedemptionFulfiller is a configurable fake that covers every branch of
// RedemptionService without a database.
type fakeRedemptionFulfiller struct {
	// resolved is returned by Fulfill/Deny/Cancel on success.
	resolved domain.ResolvedRedemption
	// fulfillErr/denyErr/cancelErr override the respective method's error
	// return when non-nil.
	fulfillErr, denyErr, cancelErr error
	// fulfillCalls/denyCalls/cancelCalls record every invocation's arguments.
	fulfillCalls []fulfillCall
	denyCalls    []denyCall
	cancelCalls  []cancelCall
}

func (f *fakeRedemptionFulfiller) Fulfill(
	_ context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
) (domain.ResolvedRedemption, error) {
	f.fulfillCalls = append(f.fulfillCalls, fulfillCall{householdID: householdID, id: id})
	if f.fulfillErr != nil {
		return domain.ResolvedRedemption{}, f.fulfillErr
	}
	return f.resolved, nil
}

func (f *fakeRedemptionFulfiller) Deny(
	_ context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
	reason string,
) (domain.ResolvedRedemption, error) {
	f.denyCalls = append(f.denyCalls, denyCall{householdID: householdID, id: id, reason: reason})
	if f.denyErr != nil {
		return domain.ResolvedRedemption{}, f.denyErr
	}
	return f.resolved, nil
}

func (f *fakeRedemptionFulfiller) Cancel(
	_ context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
	memberID household.MemberID,
) (domain.ResolvedRedemption, error) {
	f.cancelCalls = append(f.cancelCalls, cancelCall{householdID: householdID, id: id, memberID: memberID})
	if f.cancelErr != nil {
		return domain.ResolvedRedemption{}, f.cancelErr
	}
	return f.resolved, nil
}

// Compile-time assertion.
var _ app.RedemptionFulfiller = (*fakeRedemptionFulfiller)(nil)

// ---------------------------------------------------------------------------
// RedemptionService constructor validation
// ---------------------------------------------------------------------------

func TestNewRedemptionService_NilRepo_ReturnsError(t *testing.T) {
	_, err := app.NewRedemptionService(nil, newFakeEnqueuer(), newTestLogger())
	if err == nil {
		t.Error("NewRedemptionService(nil repo) error = nil, want non-nil")
	}
}

func TestNewRedemptionService_NilEnqueuer_ReturnsError(t *testing.T) {
	_, err := app.NewRedemptionService(&fakeRedemptionFulfiller{}, nil, newTestLogger())
	if err == nil {
		t.Error("NewRedemptionService(nil enqueuer) error = nil, want non-nil")
	}
}

func TestNewRedemptionService_NilLogger_ReturnsError(t *testing.T) {
	_, err := app.NewRedemptionService(&fakeRedemptionFulfiller{}, newFakeEnqueuer(), nil)
	if err == nil {
		t.Error("NewRedemptionService(nil logger) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// RedemptionService.Fulfill
// ---------------------------------------------------------------------------

// TestRedemptionService_Fulfill_Success verifies that a successful fulfil
// passes the exact household/redemption ids through to the repository, and
// enqueues exactly one "reward fulfilled" notification addressed to the
// redeeming member whose body names the reward.
func TestRedemptionService_Fulfill_Success(t *testing.T) {
	hhID := household.NewHouseholdID()
	memberID := household.NewMemberID()
	callHouseholdID := household.NewHouseholdID() // distinct from hhID: scope mix-ups must not pass
	callRedemptionID := domain.NewRewardRedemptionID()
	repo := &fakeRedemptionFulfiller{resolved: domain.ResolvedRedemption{
		RedemptionID: domain.NewRewardRedemptionID(), // distinct from callRedemptionID
		HouseholdID:  hhID,
		MemberID:     memberID,
		RewardName:   "Movie night",
		Status:       domain.RedemptionFulfilled,
	}}
	enqueuer := newFakeEnqueuer()
	svc, err := app.NewRedemptionService(repo, enqueuer, newTestLogger())
	if err != nil {
		t.Fatalf("NewRedemptionService: %v", err)
	}

	if err := svc.Fulfill(t.Context(), callHouseholdID, callRedemptionID); err != nil {
		t.Fatalf("Fulfill: unexpected error: %v", err)
	}
	if len(repo.fulfillCalls) != 1 {
		t.Fatalf("fulfillCalls = %d, want 1", len(repo.fulfillCalls))
	}
	got := repo.fulfillCalls[0]
	if got.householdID != callHouseholdID {
		t.Errorf("Fulfill householdID = %v, want %v", got.householdID, callHouseholdID)
	}
	if got.id != callRedemptionID {
		t.Errorf("Fulfill id = %v, want %v", got.id, callRedemptionID)
	}

	if len(enqueuer.notifications) != 1 {
		t.Fatalf("notifications enqueued = %d, want 1", len(enqueuer.notifications))
	}
	n := enqueuer.notifications[0]
	if n.MemberID == nil || *n.MemberID != memberID {
		t.Errorf("notification MemberID = %v, want %v", n.MemberID, memberID)
	}
	if n.HouseholdID != hhID {
		t.Errorf("notification HouseholdID = %v, want %v", n.HouseholdID, hhID)
	}
	if !strings.Contains(n.Body, "Movie night") {
		t.Errorf("notification Body = %q, want it to mention the reward name %q", n.Body, "Movie night")
	}
}

// TestRedemptionService_Fulfill_ErrorPropagated verifies that a repository
// error is wrapped and returned, and no notification is enqueued.
func TestRedemptionService_Fulfill_ErrorPropagated(t *testing.T) {
	repo := &fakeRedemptionFulfiller{fulfillErr: domain.ErrRedemptionNotPending}
	enqueuer := newFakeEnqueuer()
	svc, err := app.NewRedemptionService(repo, enqueuer, newTestLogger())
	if err != nil {
		t.Fatalf("NewRedemptionService: %v", err)
	}

	err = svc.Fulfill(t.Context(), household.NewHouseholdID(), domain.NewRewardRedemptionID())
	if !errors.Is(err, domain.ErrRedemptionNotPending) {
		t.Errorf("Fulfill = %v, want ErrRedemptionNotPending", err)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("notifications enqueued = %d, want 0 (failed fulfil)", len(enqueuer.notifications))
	}
}

// ---------------------------------------------------------------------------
// RedemptionService.Deny
// ---------------------------------------------------------------------------

// TestRedemptionService_Deny_Success verifies that a successful denial
// passes the exact household/redemption ids and reason through to the
// repository unchanged, and enqueues a notification whose body names both
// the reward and the denial reason.
func TestRedemptionService_Deny_Success(t *testing.T) {
	hhID := household.NewHouseholdID()
	memberID := household.NewMemberID()
	reason := "out of stock"
	callHouseholdID := household.NewHouseholdID() // distinct from hhID: scope mix-ups must not pass
	callRedemptionID := domain.NewRewardRedemptionID()
	repo := &fakeRedemptionFulfiller{resolved: domain.ResolvedRedemption{
		RedemptionID: domain.NewRewardRedemptionID(), // distinct from callRedemptionID
		HouseholdID:  hhID,
		MemberID:     memberID,
		RewardName:   "Comic book",
		Status:       domain.RedemptionDenied,
		DeniedReason: &reason,
	}}
	enqueuer := newFakeEnqueuer()
	svc, err := app.NewRedemptionService(repo, enqueuer, newTestLogger())
	if err != nil {
		t.Fatalf("NewRedemptionService: %v", err)
	}

	if err := svc.Deny(t.Context(), callHouseholdID, callRedemptionID, reason); err != nil {
		t.Fatalf("Deny: unexpected error: %v", err)
	}
	if len(repo.denyCalls) != 1 {
		t.Fatalf("denyCalls = %d, want 1", len(repo.denyCalls))
	}
	got := repo.denyCalls[0]
	if got.householdID != callHouseholdID {
		t.Errorf("Deny householdID = %v, want %v", got.householdID, callHouseholdID)
	}
	if got.id != callRedemptionID {
		t.Errorf("Deny id = %v, want %v", got.id, callRedemptionID)
	}
	if got.reason != reason {
		t.Errorf("Deny reason = %q, want %q", got.reason, reason)
	}

	if len(enqueuer.notifications) != 1 {
		t.Fatalf("notifications enqueued = %d, want 1", len(enqueuer.notifications))
	}
	n := enqueuer.notifications[0]
	if n.MemberID == nil || *n.MemberID != memberID {
		t.Errorf("notification MemberID = %v, want %v", n.MemberID, memberID)
	}
	if !strings.Contains(n.Body, "Comic book") {
		t.Errorf("notification Body = %q, want it to mention the reward name %q", n.Body, "Comic book")
	}
	if !strings.Contains(n.Body, reason) {
		t.Errorf("notification Body = %q, want it to mention the denial reason %q", n.Body, reason)
	}
}

// TestRedemptionService_Deny_ErrorPropagated verifies that a repository
// error is wrapped and returned, and no notification is enqueued.
func TestRedemptionService_Deny_ErrorPropagated(t *testing.T) {
	repo := &fakeRedemptionFulfiller{denyErr: domain.ErrRedemptionNotFound}
	enqueuer := newFakeEnqueuer()
	svc, err := app.NewRedemptionService(repo, enqueuer, newTestLogger())
	if err != nil {
		t.Fatalf("NewRedemptionService: %v", err)
	}

	err = svc.Deny(t.Context(), household.NewHouseholdID(), domain.NewRewardRedemptionID(), "")
	if !errors.Is(err, domain.ErrRedemptionNotFound) {
		t.Errorf("Deny = %v, want ErrRedemptionNotFound", err)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("notifications enqueued = %d, want 0 (failed deny)", len(enqueuer.notifications))
	}
}

// ---------------------------------------------------------------------------
// RedemptionService.Cancel
// ---------------------------------------------------------------------------

// TestRedemptionService_Cancel_Success_NoNotification verifies that a
// successful cancel passes the exact household/redemption/member ids through
// to the repository, and enqueues NO notification — the member already
// knows, since they just performed the action themselves.
func TestRedemptionService_Cancel_Success_NoNotification(t *testing.T) {
	hhID := household.NewHouseholdID()
	memberID := household.NewMemberID()
	callHouseholdID := household.NewHouseholdID() // distinct from hhID: scope mix-ups must not pass
	callRedemptionID := domain.NewRewardRedemptionID()
	callMemberID := household.NewMemberID() // distinct from memberID: scope mix-ups must not pass
	repo := &fakeRedemptionFulfiller{resolved: domain.ResolvedRedemption{
		RedemptionID: domain.NewRewardRedemptionID(), // distinct from callRedemptionID
		HouseholdID:  hhID,
		MemberID:     memberID,
		Status:       domain.RedemptionCancelled,
	}}
	enqueuer := newFakeEnqueuer()
	svc, err := app.NewRedemptionService(repo, enqueuer, newTestLogger())
	if err != nil {
		t.Fatalf("NewRedemptionService: %v", err)
	}

	if err := svc.Cancel(t.Context(), callHouseholdID, callRedemptionID, callMemberID); err != nil {
		t.Fatalf("Cancel: unexpected error: %v", err)
	}
	if len(repo.cancelCalls) != 1 {
		t.Fatalf("cancelCalls = %d, want 1", len(repo.cancelCalls))
	}
	got := repo.cancelCalls[0]
	if got.householdID != callHouseholdID {
		t.Errorf("Cancel householdID = %v, want %v", got.householdID, callHouseholdID)
	}
	if got.id != callRedemptionID {
		t.Errorf("Cancel id = %v, want %v", got.id, callRedemptionID)
	}
	if got.memberID != callMemberID {
		t.Errorf("Cancel memberID = %v, want %v", got.memberID, callMemberID)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("notifications enqueued = %d, want 0 (cancel never notifies)", len(enqueuer.notifications))
	}
}

// TestRedemptionService_Cancel_ErrorPropagated verifies that a repository
// error is wrapped and returned.
func TestRedemptionService_Cancel_ErrorPropagated(t *testing.T) {
	repo := &fakeRedemptionFulfiller{cancelErr: domain.ErrRedemptionNotPending}
	svc, err := app.NewRedemptionService(repo, newFakeEnqueuer(), newTestLogger())
	if err != nil {
		t.Fatalf("NewRedemptionService: %v", err)
	}

	err = svc.Cancel(t.Context(), household.NewHouseholdID(), domain.NewRewardRedemptionID(), household.NewMemberID())
	if !errors.Is(err, domain.ErrRedemptionNotPending) {
		t.Errorf("Cancel = %v, want ErrRedemptionNotPending", err)
	}
}
