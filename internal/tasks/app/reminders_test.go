package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// fakeEnqueuerWithError records calls and can inject an error on a specific call.
// ---------------------------------------------------------------------------

type fakeEnqueuerWithError struct {
	fakeEnqueuer
	errOnCall int // 1-based; 0 means never error
	callCount int
}

func (e *fakeEnqueuerWithError) Enqueue(_ context.Context, n *notifydomain.Notification) error {
	e.callCount++
	if e.errOnCall > 0 && e.callCount == e.errOnCall {
		return errors.New("stub: enqueue failed")
	}
	e.notifications = append(e.notifications, n)
	return nil
}

// ---------------------------------------------------------------------------
// fakeInstanceRepoWithDueSoon extends fakeTaskInstanceRepo with a configurable
// ClaimDueSoonReminders that returns preset targets.
// ---------------------------------------------------------------------------

type fakeInstanceRepoWithDueSoon struct {
	*fakeTaskInstanceRepo
	dueSoonTargets []domain.ReminderTarget
	dueSoonErr     error
	// clearedIDs records the instance IDs passed to ClearDueSoonReminder so
	// recovery tests can assert the un-stamp path ran.
	clearedIDs []domain.TaskInstanceID
	// clearErr, when set, makes ClearDueSoonReminder fail.
	clearErr error
}

func (r *fakeInstanceRepoWithDueSoon) ClaimDueSoonReminders(_ context.Context, _ time.Time) ([]domain.ReminderTarget, error) {
	if r.dueSoonErr != nil {
		return nil, r.dueSoonErr
	}
	return r.dueSoonTargets, nil
}

func (r *fakeInstanceRepoWithDueSoon) MarkPendingOverdueAll(_ context.Context, _ time.Time) ([]domain.ReminderTarget, error) {
	return nil, nil
}

func (r *fakeInstanceRepoWithDueSoon) ClearDueSoonReminder(_ context.Context, id domain.TaskInstanceID) error {
	r.clearedIDs = append(r.clearedIDs, id)
	return r.clearErr
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newReminderTarget(kind domain.ReminderKind, withAssignee bool) domain.ReminderTarget {
	hhID := household.NewHouseholdID()
	instID := domain.NewTaskInstanceID()
	tgt := domain.ReminderTarget{
		InstanceID:  instID,
		HouseholdID: hhID,
		Title:       "Vacuum living room",
		Category:    domain.ChoreCategory,
		DueOn:       time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC),
		Kind:        kind,
	}
	if withAssignee {
		mid := household.NewMemberID()
		tgt.AssigneeID = &mid
	}
	return tgt
}

// ---------------------------------------------------------------------------
// NewReminders constructor validation
// ---------------------------------------------------------------------------

func TestNewReminders_NilInstanceRepo_ReturnsError(t *testing.T) {
	_, err := app.NewReminders(nil, newFakeEnqueuer(), discardLogger())
	if err == nil {
		t.Error("NewReminders(nil instanceRepo) error = nil, want non-nil")
	}
}

func TestNewReminders_NilEnqueuer_ReturnsError(t *testing.T) {
	_, err := app.NewReminders(newFakeTaskInstanceRepo(), nil, discardLogger())
	if err == nil {
		t.Error("NewReminders(nil enqueuer) error = nil, want non-nil")
	}
}

func TestNewReminders_NilLogger_ReturnsError(t *testing.T) {
	_, err := app.NewReminders(newFakeTaskInstanceRepo(), newFakeEnqueuer(), nil)
	if err == nil {
		t.Error("NewReminders(nil logger) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// EmitOverdue
// ---------------------------------------------------------------------------

// TestEmitOverdue_EnqueuesOneNotificationPerTarget verifies that EmitOverdue
// calls Enqueue once for each target with the correct SourceID, MemberID,
// Channel, SourceType, an overdue Title prefix, and the immediate-delivery
// contract (ScheduledFor == asOf, Status == StatusPending).
func TestEmitOverdue_EnqueuesOneNotificationPerTarget(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	r, err := app.NewReminders(newFakeTaskInstanceRepo(), enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	// asOf differs from the targets' DueOn so the ScheduledFor==asOf check is
	// meaningful (DueOn is 2025-03-10; asOf is a distinct, later instant).
	asOf := time.Date(2025, 4, 1, 9, 30, 0, 0, time.UTC)
	tgt1 := newReminderTarget(domain.ReminderOverdue, true)
	tgt2 := newReminderTarget(domain.ReminderOverdue, false) // no assignee → household-wide

	if emitErr := r.EmitOverdue(context.Background(), asOf, []domain.ReminderTarget{tgt1, tgt2}); emitErr != nil {
		t.Fatalf("EmitOverdue returned error on all-success batch: %v", emitErr)
	}

	if len(enqueuer.notifications) != 2 {
		t.Fatalf("enqueued %d notifications, want 2", len(enqueuer.notifications))
	}

	// Verify target 1 — assigned.
	n1 := enqueuer.notifications[0]
	if n1.Channel != notifydomain.ChannelInApp {
		t.Errorf("n1.Channel = %v, want inapp", n1.Channel)
	}
	if n1.SourceType != "task_instance" {
		t.Errorf("n1.SourceType = %q, want task_instance", n1.SourceType)
	}
	wantSourceID := uuid.UUID(tgt1.InstanceID)
	if n1.SourceID == nil || *n1.SourceID != wantSourceID {
		t.Errorf("n1.SourceID = %v, want %v", n1.SourceID, wantSourceID)
	}
	if n1.MemberID == nil || *n1.MemberID != *tgt1.AssigneeID {
		t.Errorf("n1.MemberID = %v, want %v", n1.MemberID, tgt1.AssigneeID)
	}
	if n1.HouseholdID != tgt1.HouseholdID {
		t.Errorf("n1.HouseholdID = %v, want %v", n1.HouseholdID, tgt1.HouseholdID)
	}
	// Title must be populated.
	if n1.Title == "" {
		t.Error("n1.Title is empty")
	}
	// Immediate-delivery contract: ScheduledFor is asOf (not the due date), and
	// the notification is enqueued pending.
	if !n1.ScheduledFor.Equal(asOf) {
		t.Errorf("n1.ScheduledFor = %v, want asOf %v", n1.ScheduledFor, asOf)
	}
	if n1.ScheduledFor.Equal(tgt1.DueOn) {
		t.Error("n1.ScheduledFor == DueOn, want it set to asOf for immediate delivery")
	}
	if n1.Status != notifydomain.StatusPending {
		t.Errorf("n1.Status = %v, want StatusPending", n1.Status)
	}

	// Verify target 2 — household-wide (no member).
	n2 := enqueuer.notifications[1]
	if n2.MemberID != nil {
		t.Errorf("n2.MemberID = %v, want nil for unassigned target", n2.MemberID)
	}
	if !n2.ScheduledFor.Equal(asOf) {
		t.Errorf("n2.ScheduledFor = %v, want asOf %v", n2.ScheduledFor, asOf)
	}
	if n2.Status != notifydomain.StatusPending {
		t.Errorf("n2.Status = %v, want StatusPending", n2.Status)
	}
}

// TestEmitOverdue_OneEnqueueErrorDoesNotAbortBatch verifies that when the
// second of three enqueue calls fails, the third target is still processed
// (two total notifications enqueued) AND EmitOverdue returns a non-nil
// aggregated error so the failure is surfaced rather than masked.
func TestEmitOverdue_OneEnqueueErrorDoesNotAbortBatch(t *testing.T) {
	enqueuer := &fakeEnqueuerWithError{errOnCall: 2}
	r, err := app.NewReminders(newFakeTaskInstanceRepo(), enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	targets := []domain.ReminderTarget{
		newReminderTarget(domain.ReminderOverdue, false),
		newReminderTarget(domain.ReminderOverdue, false),
		newReminderTarget(domain.ReminderOverdue, false),
	}

	emitErr := r.EmitOverdue(context.Background(), time.Now(), targets)
	if emitErr == nil {
		t.Error("EmitOverdue error = nil, want non-nil when an enqueue failed")
	}

	// errOnCall=2 means the second call errors, so only 2 succeed.
	if len(enqueuer.notifications) != 2 {
		t.Errorf("enqueued %d notifications after mid-batch error, want 2", len(enqueuer.notifications))
	}
}

// TestEmitOverdue_EmptyTargets_EnqueuesNothing verifies that EmitOverdue with
// an empty slice performs no enqueue calls and returns nil.
func TestEmitOverdue_EmptyTargets_EnqueuesNothing(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	r, err := app.NewReminders(newFakeTaskInstanceRepo(), enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	if emitErr := r.EmitOverdue(context.Background(), time.Now(), nil); emitErr != nil {
		t.Errorf("EmitOverdue(empty) error = %v, want nil", emitErr)
	}

	if len(enqueuer.notifications) != 0 {
		t.Errorf("enqueued %d notifications for empty targets, want 0", len(enqueuer.notifications))
	}
}

// ---------------------------------------------------------------------------
// EmitDueSoon
// ---------------------------------------------------------------------------

// TestEmitDueSoon_EnqueuesOneNotificationPerClaimedTarget verifies that
// EmitDueSoon enqueues one notification per target returned by
// ClaimDueSoonReminders, with correct SourceID and due-soon Title.
func TestEmitDueSoon_EnqueuesOneNotificationPerClaimedTarget(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	tgt := newReminderTarget(domain.ReminderDueSoon, true)

	instRepo := &fakeInstanceRepoWithDueSoon{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		dueSoonTargets:       []domain.ReminderTarget{tgt},
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	asOf := time.Date(2025, 3, 8, 7, 15, 0, 0, time.UTC) // distinct from tgt.DueOn
	if err := r.EmitDueSoon(context.Background(), asOf); err != nil {
		t.Fatalf("EmitDueSoon: %v", err)
	}

	if len(enqueuer.notifications) != 1 {
		t.Fatalf("enqueued %d notifications, want 1", len(enqueuer.notifications))
	}

	n := enqueuer.notifications[0]
	wantSourceID := uuid.UUID(tgt.InstanceID)
	if n.SourceID == nil || *n.SourceID != wantSourceID {
		t.Errorf("SourceID = %v, want %v", n.SourceID, wantSourceID)
	}
	if n.Channel != notifydomain.ChannelInApp {
		t.Errorf("Channel = %v, want inapp", n.Channel)
	}
	if n.SourceType != "task_instance" {
		t.Errorf("SourceType = %q, want task_instance", n.SourceType)
	}
	if n.MemberID == nil || *n.MemberID != *tgt.AssigneeID {
		t.Errorf("MemberID = %v, want %v", n.MemberID, tgt.AssigneeID)
	}
	if n.Title == "" {
		t.Error("Title is empty")
	}
	// Immediate-delivery contract: ScheduledFor is asOf (not the due date), and
	// the notification is enqueued pending.
	if !n.ScheduledFor.Equal(asOf) {
		t.Errorf("ScheduledFor = %v, want asOf %v", n.ScheduledFor, asOf)
	}
	if n.ScheduledFor.Equal(tgt.DueOn) {
		t.Error("ScheduledFor == DueOn, want it set to asOf for immediate delivery")
	}
	if n.Status != notifydomain.StatusPending {
		t.Errorf("Status = %v, want StatusPending", n.Status)
	}

	// Happy path must NOT clear any reminders.
	if len(instRepo.clearedIDs) != 0 {
		t.Errorf("ClearDueSoonReminder called %d times on success, want 0", len(instRepo.clearedIDs))
	}
}

// TestEmitDueSoon_ClaimError_ReturnsError verifies that an error from
// ClaimDueSoonReminders is propagated and no notifications are enqueued.
func TestEmitDueSoon_ClaimError_ReturnsError(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	wantErr := errors.New("db: claim failed")

	instRepo := &fakeInstanceRepoWithDueSoon{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		dueSoonErr:           wantErr,
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	err = r.EmitDueSoon(context.Background(), time.Now())
	if !errors.Is(err, wantErr) {
		t.Errorf("EmitDueSoon error = %v, want to wrap %v", err, wantErr)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("enqueued %d notifications after claim error, want 0", len(enqueuer.notifications))
	}
}

// TestEmitDueSoon_OneEnqueueErrorClearsAndSurfaces verifies that a failing
// enqueue for one due-soon target (a) does not prevent remaining targets from
// being enqueued, (b) calls ClearDueSoonReminder for the failed target so the
// reminder is retried next tick, and (c) returns a non-nil aggregated error so
// the failure is surfaced rather than masked.
func TestEmitDueSoon_OneEnqueueErrorClearsAndSurfaces(t *testing.T) {
	enqueuer := &fakeEnqueuerWithError{errOnCall: 1} // first call errors
	failedTarget := newReminderTarget(domain.ReminderDueSoon, false)
	okTarget := newReminderTarget(domain.ReminderDueSoon, false)
	targets := []domain.ReminderTarget{failedTarget, okTarget}

	instRepo := &fakeInstanceRepoWithDueSoon{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		dueSoonTargets:       targets,
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	emitErr := r.EmitDueSoon(context.Background(), time.Now())
	if emitErr == nil {
		t.Error("EmitDueSoon error = nil, want non-nil when an enqueue failed")
	}

	// Only 1 of 2 enqueues succeeded (first errored, second succeeded).
	if len(enqueuer.notifications) != 1 {
		t.Errorf("enqueued %d notifications, want 1 (second target succeeded)", len(enqueuer.notifications))
	}

	// The failed target's reminded_at must have been cleared for retry.
	if len(instRepo.clearedIDs) != 1 {
		t.Fatalf("ClearDueSoonReminder called %d times, want 1", len(instRepo.clearedIDs))
	}
	if instRepo.clearedIDs[0] != failedTarget.InstanceID {
		t.Errorf("cleared instance = %v, want failed target %v", instRepo.clearedIDs[0], failedTarget.InstanceID)
	}
}

// TestEmitDueSoon_ClearFailureStillSurfacesEnqueueError verifies that when both
// the enqueue and the recovery clear fail, EmitDueSoon still returns a non-nil
// error (the enqueue failure is what matters; the clear failure is only logged).
func TestEmitDueSoon_ClearFailureStillSurfacesEnqueueError(t *testing.T) {
	enqueuer := &fakeEnqueuerWithError{errOnCall: 1}
	tgt := newReminderTarget(domain.ReminderDueSoon, false)

	instRepo := &fakeInstanceRepoWithDueSoon{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		dueSoonTargets:       []domain.ReminderTarget{tgt},
		clearErr:             errors.New("db: clear failed"),
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	emitErr := r.EmitDueSoon(context.Background(), time.Now())
	if emitErr == nil {
		t.Error("EmitDueSoon error = nil, want non-nil when enqueue failed (even though clear also failed)")
	}
	// The clear was still attempted.
	if len(instRepo.clearedIDs) != 1 {
		t.Errorf("ClearDueSoonReminder called %d times, want 1", len(instRepo.clearedIDs))
	}
}

// ---------------------------------------------------------------------------
// fakeInstanceRepoWithClaimExpiry extends fakeTaskInstanceRepo with a
// configurable SweepExpiredClaims (NES-117), mirroring
// fakeInstanceRepoWithDueSoon.
// ---------------------------------------------------------------------------

type fakeInstanceRepoWithClaimExpiry struct {
	*fakeTaskInstanceRepo
	claims []domain.ExpiredClaim
	err    error
}

func (r *fakeInstanceRepoWithClaimExpiry) SweepExpiredClaims(_ context.Context, _ time.Time) ([]domain.ExpiredClaim, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.claims, nil
}

func (r *fakeInstanceRepoWithClaimExpiry) MarkPendingOverdueAll(_ context.Context, _ time.Time) ([]domain.ReminderTarget, error) {
	return nil, nil
}

// newExpiredClaim builds a domain.ExpiredClaim with a fresh instance,
// household, and claimant for use across EmitClaimExpiry tests.
func newExpiredClaim(penaltyPoints int) domain.ExpiredClaim {
	return domain.ExpiredClaim{
		InstanceID:      domain.NewTaskInstanceID(),
		HouseholdID:     household.NewHouseholdID(),
		RecurringTaskID: domain.NewRecurringTaskID(),
		ClaimedBy:       household.NewMemberID(),
		Title:           "Mow the lawn",
		PenaltyPoints:   penaltyPoints,
	}
}

// ---------------------------------------------------------------------------
// EmitClaimExpiry (NES-117)
// ---------------------------------------------------------------------------

// TestEmitClaimExpiry_EnqueuesOneNotificationPerClaim verifies that
// EmitClaimExpiry calls Enqueue once per claim returned by
// SweepExpiredClaims, addressed to the claimant, with the penalty reflected
// in the notification body.
func TestEmitClaimExpiry_EnqueuesOneNotificationPerClaim(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	claim := newExpiredClaim(5)

	instRepo := &fakeInstanceRepoWithClaimExpiry{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		claims:               []domain.ExpiredClaim{claim},
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	asOf := time.Date(2025, 4, 1, 9, 30, 0, 0, time.UTC)
	if emitErr := r.EmitClaimExpiry(context.Background(), asOf); emitErr != nil {
		t.Fatalf("EmitClaimExpiry: %v", emitErr)
	}

	if len(enqueuer.notifications) != 1 {
		t.Fatalf("enqueued %d notifications, want 1", len(enqueuer.notifications))
	}
	n := enqueuer.notifications[0]
	if n.HouseholdID != claim.HouseholdID {
		t.Errorf("HouseholdID = %v, want %v", n.HouseholdID, claim.HouseholdID)
	}
	if n.MemberID == nil || *n.MemberID != claim.ClaimedBy {
		t.Errorf("MemberID = %v, want %v", n.MemberID, claim.ClaimedBy)
	}
	if n.Channel != notifydomain.ChannelInApp {
		t.Errorf("Channel = %v, want inapp", n.Channel)
	}
	wantSourceID := uuid.UUID(claim.InstanceID)
	if n.SourceID == nil || *n.SourceID != wantSourceID {
		t.Errorf("SourceID = %v, want %v", n.SourceID, wantSourceID)
	}
	if n.SourceType != "task_instance" {
		t.Errorf("SourceType = %q, want task_instance", n.SourceType)
	}
	if !n.ScheduledFor.Equal(asOf) {
		t.Errorf("ScheduledFor = %v, want asOf %v", n.ScheduledFor, asOf)
	}
	if n.Status != notifydomain.StatusPending {
		t.Errorf("Status = %v, want StatusPending", n.Status)
	}
	if want := "Your claim on Mow the lawn expired, -5 points."; n.Body != want {
		t.Errorf("Body = %q, want %q", n.Body, want)
	}
}

// TestEmitClaimExpiry_SweepError_ReturnsError verifies that an error from
// SweepExpiredClaims is propagated and no notifications are enqueued.
func TestEmitClaimExpiry_SweepError_ReturnsError(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	wantErr := errors.New("db: sweep failed")

	instRepo := &fakeInstanceRepoWithClaimExpiry{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		err:                  wantErr,
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	err = r.EmitClaimExpiry(context.Background(), time.Now())
	if !errors.Is(err, wantErr) {
		t.Errorf("EmitClaimExpiry error = %v, want to wrap %v", err, wantErr)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("enqueued %d notifications after sweep error, want 0", len(enqueuer.notifications))
	}
}

// TestEmitClaimExpiry_OneEnqueueErrorDoesNotAbortBatch verifies that a
// failing enqueue for one claim does not prevent the remaining claim from
// being enqueued, and that EmitClaimExpiry returns a non-nil aggregated
// error so the failure is surfaced rather than masked.
func TestEmitClaimExpiry_OneEnqueueErrorDoesNotAbortBatch(t *testing.T) {
	enqueuer := &fakeEnqueuerWithError{errOnCall: 1}
	instRepo := &fakeInstanceRepoWithClaimExpiry{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		claims:               []domain.ExpiredClaim{newExpiredClaim(1), newExpiredClaim(2)},
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	emitErr := r.EmitClaimExpiry(context.Background(), time.Now())
	if emitErr == nil {
		t.Error("EmitClaimExpiry error = nil, want non-nil when an enqueue failed")
	}
	if len(enqueuer.notifications) != 1 {
		t.Errorf("enqueued %d notifications, want 1 (second claim succeeded)", len(enqueuer.notifications))
	}
}

// TestEmitClaimExpiry_EmptyClaims_EnqueuesNothing verifies that
// EmitClaimExpiry with no expired claims performs no enqueue calls and
// returns nil.
func TestEmitClaimExpiry_EmptyClaims_EnqueuesNothing(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	instRepo := &fakeInstanceRepoWithClaimExpiry{fakeTaskInstanceRepo: newFakeTaskInstanceRepo()}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	if emitErr := r.EmitClaimExpiry(context.Background(), time.Now()); emitErr != nil {
		t.Errorf("EmitClaimExpiry(empty) error = %v, want nil", emitErr)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("enqueued %d notifications for no expired claims, want 0", len(enqueuer.notifications))
	}
}

// ---------------------------------------------------------------------------
// fakeInstanceRepoWithClaimWarnings extends fakeTaskInstanceRepo with a
// configurable ClaimWarnings (NES-118), mirroring
// fakeInstanceRepoWithClaimExpiry.
// ---------------------------------------------------------------------------

// clearedClaimWarning records one ClearClaimWarning call's arguments so
// recovery tests can assert not just that the un-stamp path ran, but that it
// was scoped to the correct claim window (the InstanceID/ExpiresAt pair
// EmitClaimWarnings actually received from ClaimWarnings).
type clearedClaimWarning struct {
	instanceID domain.TaskInstanceID
	expiresAt  time.Time
}

type fakeInstanceRepoWithClaimWarnings struct {
	*fakeTaskInstanceRepo
	warnings []domain.ClaimWarning
	err      error
	// clearedWarnings records every ClearClaimWarning call's arguments,
	// mirroring fakeInstanceRepoWithDueSoon's clearedIDs but also capturing
	// ExpiresAt so tests can verify the window-scope guard is exercised with
	// the right value, not just that a clear happened.
	clearedWarnings []clearedClaimWarning
	// clearErr, when set, makes ClearClaimWarning fail.
	clearErr error
}

func (r *fakeInstanceRepoWithClaimWarnings) ClaimWarnings(_ context.Context, _ time.Time) ([]domain.ClaimWarning, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.warnings, nil
}

func (r *fakeInstanceRepoWithClaimWarnings) ClearClaimWarning(_ context.Context, id domain.TaskInstanceID, expiresAt time.Time) error {
	r.clearedWarnings = append(r.clearedWarnings, clearedClaimWarning{instanceID: id, expiresAt: expiresAt})
	return r.clearErr
}

func (r *fakeInstanceRepoWithClaimWarnings) MarkPendingOverdueAll(_ context.Context, _ time.Time) ([]domain.ReminderTarget, error) {
	return nil, nil
}

// newClaimWarning builds a domain.ClaimWarning with a fresh instance,
// household, and claimant for use across EmitClaimWarnings tests.
func newClaimWarning(expiresAt time.Time) domain.ClaimWarning {
	return domain.ClaimWarning{
		InstanceID:  domain.NewTaskInstanceID(),
		HouseholdID: household.NewHouseholdID(),
		ClaimedBy:   household.NewMemberID(),
		Title:       "Mow the lawn",
		ExpiresAt:   expiresAt,
	}
}

// ---------------------------------------------------------------------------
// EmitClaimWarnings (NES-118)
// ---------------------------------------------------------------------------

// TestEmitClaimWarnings_EnqueuesOneNotificationPerWarning verifies that
// EmitClaimWarnings calls Enqueue once per warning returned by ClaimWarnings,
// addressed to the claimant, with the claim window named in the body.
func TestEmitClaimWarnings_EnqueuesOneNotificationPerWarning(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	warning := newClaimWarning(time.Date(2025, 4, 1, 11, 30, 0, 0, time.UTC))

	instRepo := &fakeInstanceRepoWithClaimWarnings{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		warnings:             []domain.ClaimWarning{warning},
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	asOf := time.Date(2025, 4, 1, 9, 30, 0, 0, time.UTC)
	if emitErr := r.EmitClaimWarnings(context.Background(), asOf); emitErr != nil {
		t.Fatalf("EmitClaimWarnings: %v", emitErr)
	}

	if len(enqueuer.notifications) != 1 {
		t.Fatalf("enqueued %d notifications, want 1", len(enqueuer.notifications))
	}
	n := enqueuer.notifications[0]
	if n.HouseholdID != warning.HouseholdID {
		t.Errorf("HouseholdID = %v, want %v", n.HouseholdID, warning.HouseholdID)
	}
	if n.MemberID == nil || *n.MemberID != warning.ClaimedBy {
		t.Errorf("MemberID = %v, want %v", n.MemberID, warning.ClaimedBy)
	}
	if n.Channel != notifydomain.ChannelInApp {
		t.Errorf("Channel = %v, want inapp", n.Channel)
	}
	wantSourceID := uuid.UUID(warning.InstanceID)
	if n.SourceID == nil || *n.SourceID != wantSourceID {
		t.Errorf("SourceID = %v, want %v", n.SourceID, wantSourceID)
	}
	if n.SourceType != "task_instance" {
		t.Errorf("SourceType = %q, want task_instance", n.SourceType)
	}
	if !n.ScheduledFor.Equal(asOf) {
		t.Errorf("ScheduledFor = %v, want asOf %v", n.ScheduledFor, asOf)
	}
	if n.Status != notifydomain.StatusPending {
		t.Errorf("Status = %v, want StatusPending", n.Status)
	}
	if want := "Your claim on Mow the lawn expires within 2 hours — complete it soon to avoid a point penalty."; n.Body != want {
		t.Errorf("Body = %q, want %q", n.Body, want)
	}
	if want := "Claim expiring soon: Mow the lawn"; n.Title != want {
		t.Errorf("Title = %q, want %q", n.Title, want)
	}
}

// TestEmitClaimWarnings_ClaimWarningsError_ReturnsError verifies that an
// error from ClaimWarnings is propagated and no notifications are enqueued.
func TestEmitClaimWarnings_ClaimWarningsError_ReturnsError(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	wantErr := errors.New("db: claim warnings failed")

	instRepo := &fakeInstanceRepoWithClaimWarnings{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		err:                  wantErr,
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	err = r.EmitClaimWarnings(context.Background(), time.Now())
	if !errors.Is(err, wantErr) {
		t.Errorf("EmitClaimWarnings error = %v, want to wrap %v", err, wantErr)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("enqueued %d notifications after claim-warnings error, want 0", len(enqueuer.notifications))
	}
}

// TestEmitClaimWarnings_OneEnqueueErrorDoesNotAbortBatch verifies that a
// failing enqueue for one warning does not prevent the remaining warning from
// being enqueued, and that EmitClaimWarnings returns a non-nil aggregated
// error so the failure is surfaced rather than masked.
func TestEmitClaimWarnings_OneEnqueueErrorDoesNotAbortBatch(t *testing.T) {
	enqueuer := &fakeEnqueuerWithError{errOnCall: 1}
	instRepo := &fakeInstanceRepoWithClaimWarnings{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		warnings: []domain.ClaimWarning{
			newClaimWarning(time.Now().Add(time.Hour)),
			newClaimWarning(time.Now().Add(90 * time.Minute)),
		},
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	emitErr := r.EmitClaimWarnings(context.Background(), time.Now())
	if emitErr == nil {
		t.Error("EmitClaimWarnings error = nil, want non-nil when an enqueue failed")
	}
	if len(enqueuer.notifications) != 1 {
		t.Errorf("enqueued %d notifications, want 1 (second warning succeeded)", len(enqueuer.notifications))
	}
}

// TestEmitClaimWarnings_EmptyWarnings_EnqueuesNothing verifies that
// EmitClaimWarnings with no claims entering the warning window performs no
// enqueue calls and returns nil.
func TestEmitClaimWarnings_EmptyWarnings_EnqueuesNothing(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	instRepo := &fakeInstanceRepoWithClaimWarnings{fakeTaskInstanceRepo: newFakeTaskInstanceRepo()}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	if emitErr := r.EmitClaimWarnings(context.Background(), time.Now()); emitErr != nil {
		t.Errorf("EmitClaimWarnings(empty) error = %v, want nil", emitErr)
	}
	if len(enqueuer.notifications) != 0 {
		t.Errorf("enqueued %d notifications for no claim warnings, want 0", len(enqueuer.notifications))
	}
}

// TestEmitClaimWarnings_OneEnqueueErrorClearsAndSurfaces verifies that when
// the first of two warning enqueues fails, EmitClaimWarnings (a) still
// enqueues the remaining warning, (b) calls ClearClaimWarning for the failed
// warning's instance so it is retried next tick, and (c) returns a non-nil
// aggregated error so the failure is surfaced rather than masked. Mirrors
// TestEmitDueSoon_OneEnqueueErrorClearsAndSurfaces.
func TestEmitClaimWarnings_OneEnqueueErrorClearsAndSurfaces(t *testing.T) {
	enqueuer := &fakeEnqueuerWithError{errOnCall: 1} // first call errors
	failedWarning := newClaimWarning(time.Now().Add(90 * time.Minute))
	okWarning := newClaimWarning(time.Now().Add(time.Hour))
	warnings := []domain.ClaimWarning{failedWarning, okWarning}

	instRepo := &fakeInstanceRepoWithClaimWarnings{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		warnings:             warnings,
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	emitErr := r.EmitClaimWarnings(context.Background(), time.Now())
	if emitErr == nil {
		t.Error("EmitClaimWarnings error = nil, want non-nil when an enqueue failed")
	}

	// Only 1 of 2 enqueues succeeded (first errored, second succeeded).
	if len(enqueuer.notifications) != 1 {
		t.Errorf("enqueued %d notifications, want 1 (second warning succeeded)", len(enqueuer.notifications))
	}

	// The failed warning's claim_warned_at must have been cleared for retry,
	// scoped to the exact claim window (ExpiresAt) the warning was generated
	// for.
	if len(instRepo.clearedWarnings) != 1 {
		t.Fatalf("ClearClaimWarning called %d times, want 1", len(instRepo.clearedWarnings))
	}
	cleared := instRepo.clearedWarnings[0]
	if cleared.instanceID != failedWarning.InstanceID {
		t.Errorf("cleared instance = %v, want failed warning %v", cleared.instanceID, failedWarning.InstanceID)
	}
	if !cleared.expiresAt.Equal(failedWarning.ExpiresAt) {
		t.Errorf("cleared expiresAt = %v, want %v", cleared.expiresAt, failedWarning.ExpiresAt)
	}
}

// TestEmitClaimWarnings_ClearFailureStillSurfacesEnqueueError verifies that
// when both the enqueue and the recovery clear fail, EmitClaimWarnings still
// returns a non-nil error (the enqueue failure is what matters; the clear
// failure is only logged). Mirrors
// TestEmitDueSoon_ClearFailureStillSurfacesEnqueueError.
func TestEmitClaimWarnings_ClearFailureStillSurfacesEnqueueError(t *testing.T) {
	enqueuer := &fakeEnqueuerWithError{errOnCall: 1}
	warning := newClaimWarning(time.Now().Add(90 * time.Minute))

	instRepo := &fakeInstanceRepoWithClaimWarnings{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		warnings:             []domain.ClaimWarning{warning},
		clearErr:             errors.New("db: clear failed"),
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	emitErr := r.EmitClaimWarnings(context.Background(), time.Now())
	if emitErr == nil {
		t.Error("EmitClaimWarnings error = nil, want non-nil when enqueue failed (even though clear also failed)")
	}
	// The clear was still attempted, with the correct claim window.
	if len(instRepo.clearedWarnings) != 1 {
		t.Fatalf("ClearClaimWarning called %d times, want 1", len(instRepo.clearedWarnings))
	}
	if !instRepo.clearedWarnings[0].expiresAt.Equal(warning.ExpiresAt) {
		t.Errorf("cleared expiresAt = %v, want %v", instRepo.clearedWarnings[0].expiresAt, warning.ExpiresAt)
	}
}

// TestEmitClaimWarnings_RetriesAfterClear verifies the end-to-end recovery
// contract: a failed enqueue clears claim_warned_at for that claim window, so
// a subsequent EmitClaimWarnings call (representing the next scheduler tick,
// where ClaimWarnings would now re-select the un-stamped row) can enqueue the
// same warning successfully.
func TestEmitClaimWarnings_RetriesAfterClear(t *testing.T) {
	warning := newClaimWarning(time.Now().Add(90 * time.Minute))

	// First tick: the enqueuer fails once, forcing the recovery clear.
	failingEnqueuer := &fakeEnqueuerWithError{errOnCall: 1}
	instRepo := &fakeInstanceRepoWithClaimWarnings{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		warnings:             []domain.ClaimWarning{warning},
	}
	r, err := app.NewReminders(instRepo, failingEnqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}
	if emitErr := r.EmitClaimWarnings(context.Background(), time.Now()); emitErr == nil {
		t.Fatal("EmitClaimWarnings (first tick) error = nil, want non-nil enqueue failure")
	}
	if len(instRepo.clearedWarnings) != 1 {
		t.Fatalf("ClearClaimWarning called %d times after first tick, want 1", len(instRepo.clearedWarnings))
	}

	// Second tick: a fresh (non-failing) enqueuer over the same, now-cleared
	// warning — simulating ClaimWarnings re-selecting the un-stamped row.
	retryEnqueuer := newFakeEnqueuer()
	r2, err := app.NewReminders(instRepo, retryEnqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders (retry): %v", err)
	}
	if emitErr := r2.EmitClaimWarnings(context.Background(), time.Now()); emitErr != nil {
		t.Fatalf("EmitClaimWarnings (retry tick): %v", emitErr)
	}
	if len(retryEnqueuer.notifications) != 1 {
		t.Fatalf("retry tick enqueued %d notifications, want 1 (the retried warning)", len(retryEnqueuer.notifications))
	}
}

// ---------------------------------------------------------------------------
// combinedFakeRepo satisfies domain.TaskInstanceRepository with configurable
// MarkPendingOverdueAll and ClaimDueSoonReminders return values, allowing
// end-to-end Reminders tests without a database.
// ---------------------------------------------------------------------------

type combinedFakeRepo struct {
	*fakeTaskInstanceRepo
	overdueTargets []domain.ReminderTarget
	dueSoonTargets []domain.ReminderTarget
	dueSoonErr     error
}

func (r *combinedFakeRepo) MarkPendingOverdueAll(_ context.Context, _ time.Time) ([]domain.ReminderTarget, error) {
	return r.overdueTargets, nil
}

func (r *combinedFakeRepo) ClaimDueSoonReminders(_ context.Context, _ time.Time) ([]domain.ReminderTarget, error) {
	return r.dueSoonTargets, r.dueSoonErr
}

// ---------------------------------------------------------------------------
// Scheduler.RunOnce — reminders integration
// ---------------------------------------------------------------------------

// TestReminders_Integration_EmitsOverdueAndDueSoon verifies the Reminders
// service end to end (calling EmitOverdue + EmitDueSoon directly, not via the
// Scheduler):
//   - 2 overdue targets → 2 overdue notifications.
//   - ClaimDueSoonReminders returns 1 due-soon target → 1 due-soon notification.
//   - Total 3 notifications enqueued.
func TestReminders_Integration_EmitsOverdueAndDueSoon(t *testing.T) {
	overdueTarget := newReminderTarget(domain.ReminderOverdue, true)
	dueSoonTarget := newReminderTarget(domain.ReminderDueSoon, true)

	repo := &combinedFakeRepo{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		overdueTargets:       []domain.ReminderTarget{overdueTarget, newReminderTarget(domain.ReminderOverdue, false)},
		dueSoonTargets:       []domain.ReminderTarget{dueSoonTarget},
	}

	enqueuer := newFakeEnqueuer()
	reminders, err := app.NewReminders(repo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	// Emit overdue for the 2 overdue targets.
	if err := reminders.EmitOverdue(context.Background(), time.Now(), repo.overdueTargets); err != nil {
		t.Fatalf("EmitOverdue: %v", err)
	}
	// Emit due-soon (calls ClaimDueSoonReminders on repo which returns 1 target).
	if err := reminders.EmitDueSoon(context.Background(), time.Now()); err != nil {
		t.Fatalf("EmitDueSoon: %v", err)
	}

	if len(enqueuer.notifications) != 3 {
		t.Fatalf("enqueued %d notifications, want 3 (2 overdue + 1 due-soon)", len(enqueuer.notifications))
	}

	// Verify SourceIDs match the expected targets.
	var overdueCount, dueSoonCount int
	for _, n := range enqueuer.notifications {
		if n.SourceID == nil {
			t.Error("notification SourceID is nil, want a task_instance UUID")
			continue
		}
		srcUUID := *n.SourceID
		if srcUUID == uuid.UUID(overdueTarget.InstanceID) {
			overdueCount++
		}
		if srcUUID == uuid.UUID(dueSoonTarget.InstanceID) {
			dueSoonCount++
		}
	}
	if overdueCount != 1 {
		t.Errorf("found %d notifications for overdueTarget, want 1", overdueCount)
	}
	if dueSoonCount != 1 {
		t.Errorf("found %d notifications for dueSoonTarget, want 1", dueSoonCount)
	}
}
