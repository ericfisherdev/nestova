package app_test

import (
	"context"
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// fakeRedemptionFulfiller — in-memory implementation of app.RedemptionFulfiller
// ---------------------------------------------------------------------------

// fakeRedemptionFulfiller is a configurable fake that covers every branch of
// RedemptionService without a database.
type fakeRedemptionFulfiller struct {
	// resolved is returned by Fulfill/Deny/Cancel on success.
	resolved domain.ResolvedRedemption
	// fulfillErr/denyErr/cancelErr override the respective method's error
	// return when non-nil.
	fulfillErr, denyErr, cancelErr error
	// fulfillCalls/denyReasons/cancelCalls record arguments for assertion.
	fulfillCalls int
	denyReasons  []string
	cancelCalls  int
}

func (f *fakeRedemptionFulfiller) Fulfill(
	_ context.Context,
	_ household.HouseholdID,
	_ domain.RewardRedemptionID,
) (domain.ResolvedRedemption, error) {
	f.fulfillCalls++
	if f.fulfillErr != nil {
		return domain.ResolvedRedemption{}, f.fulfillErr
	}
	return f.resolved, nil
}

func (f *fakeRedemptionFulfiller) Deny(
	_ context.Context,
	_ household.HouseholdID,
	_ domain.RewardRedemptionID,
	reason string,
) (domain.ResolvedRedemption, error) {
	f.denyReasons = append(f.denyReasons, reason)
	if f.denyErr != nil {
		return domain.ResolvedRedemption{}, f.denyErr
	}
	return f.resolved, nil
}

func (f *fakeRedemptionFulfiller) Cancel(
	_ context.Context,
	_ household.HouseholdID,
	_ domain.RewardRedemptionID,
	_ household.MemberID,
) (domain.ResolvedRedemption, error) {
	f.cancelCalls++
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
// enqueues exactly one "reward fulfilled" notification addressed to the
// redeeming member.
func TestRedemptionService_Fulfill_Success(t *testing.T) {
	hhID := household.NewHouseholdID()
	memberID := household.NewMemberID()
	repo := &fakeRedemptionFulfiller{resolved: domain.ResolvedRedemption{
		RedemptionID: domain.NewRewardRedemptionID(),
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

	if err := svc.Fulfill(t.Context(), hhID, domain.NewRewardRedemptionID()); err != nil {
		t.Fatalf("Fulfill: unexpected error: %v", err)
	}
	if repo.fulfillCalls != 1 {
		t.Errorf("fulfillCalls = %d, want 1", repo.fulfillCalls)
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
// enqueues a notification that mentions the reason, and passes the reason
// through to the repository unchanged.
func TestRedemptionService_Deny_Success(t *testing.T) {
	hhID := household.NewHouseholdID()
	memberID := household.NewMemberID()
	reason := "out of stock"
	repo := &fakeRedemptionFulfiller{resolved: domain.ResolvedRedemption{
		RedemptionID: domain.NewRewardRedemptionID(),
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

	if err := svc.Deny(t.Context(), hhID, domain.NewRewardRedemptionID(), reason); err != nil {
		t.Fatalf("Deny: unexpected error: %v", err)
	}
	if len(repo.denyReasons) != 1 || repo.denyReasons[0] != reason {
		t.Errorf("denyReasons = %v, want [%q]", repo.denyReasons, reason)
	}
	if len(enqueuer.notifications) != 1 {
		t.Fatalf("notifications enqueued = %d, want 1", len(enqueuer.notifications))
	}
	n := enqueuer.notifications[0]
	if n.MemberID == nil || *n.MemberID != memberID {
		t.Errorf("notification MemberID = %v, want %v", n.MemberID, memberID)
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
// successful cancel enqueues NO notification — the member already knows,
// since they just performed the action themselves.
func TestRedemptionService_Cancel_Success_NoNotification(t *testing.T) {
	hhID := household.NewHouseholdID()
	memberID := household.NewMemberID()
	repo := &fakeRedemptionFulfiller{resolved: domain.ResolvedRedemption{
		RedemptionID: domain.NewRewardRedemptionID(),
		HouseholdID:  hhID,
		MemberID:     memberID,
		Status:       domain.RedemptionCancelled,
	}}
	enqueuer := newFakeEnqueuer()
	svc, err := app.NewRedemptionService(repo, enqueuer, newTestLogger())
	if err != nil {
		t.Fatalf("NewRedemptionService: %v", err)
	}

	if err := svc.Cancel(t.Context(), hhID, domain.NewRewardRedemptionID(), memberID); err != nil {
		t.Fatalf("Cancel: unexpected error: %v", err)
	}
	if repo.cancelCalls != 1 {
		t.Errorf("cancelCalls = %d, want 1", repo.cancelCalls)
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
