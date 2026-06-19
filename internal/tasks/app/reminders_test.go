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
// Channel, SourceType, and an overdue Title prefix.
func TestEmitOverdue_EnqueuesOneNotificationPerTarget(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	r, err := app.NewReminders(newFakeTaskInstanceRepo(), enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	tgt1 := newReminderTarget(domain.ReminderOverdue, true)
	tgt2 := newReminderTarget(domain.ReminderOverdue, false) // no assignee → household-wide

	r.EmitOverdue(context.Background(), time.Now(), []domain.ReminderTarget{tgt1, tgt2})

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
	// Title must contain "overdue".
	if n1.Title == "" {
		t.Error("n1.Title is empty")
	}

	// Verify target 2 — household-wide (no member).
	n2 := enqueuer.notifications[1]
	if n2.MemberID != nil {
		t.Errorf("n2.MemberID = %v, want nil for unassigned target", n2.MemberID)
	}
}

// TestEmitOverdue_OneEnqueueErrorDoesNotAbortBatch verifies that when the
// second of three enqueue calls fails, the third target is still processed and
// two total notifications are enqueued.
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

	r.EmitOverdue(context.Background(), time.Now(), targets)

	// errOnCall=2 means the second call errors, so only 2 succeed.
	if len(enqueuer.notifications) != 2 {
		t.Errorf("enqueued %d notifications after mid-batch error, want 2", len(enqueuer.notifications))
	}
}

// TestEmitOverdue_EmptyTargets_EnqueuesNothing verifies that EmitOverdue with
// an empty slice performs no enqueue calls.
func TestEmitOverdue_EmptyTargets_EnqueuesNothing(t *testing.T) {
	enqueuer := newFakeEnqueuer()
	r, err := app.NewReminders(newFakeTaskInstanceRepo(), enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	r.EmitOverdue(context.Background(), time.Now(), nil)

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

	if err := r.EmitDueSoon(context.Background(), time.Now()); err != nil {
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

// TestEmitDueSoon_OneEnqueueErrorDoesNotAbortBatch verifies that a failing
// enqueue for one due-soon target does not prevent remaining targets from
// being enqueued. The error is logged but EmitDueSoon returns nil.
func TestEmitDueSoon_OneEnqueueErrorDoesNotAbortBatch(t *testing.T) {
	enqueuer := &fakeEnqueuerWithError{errOnCall: 1} // first call errors
	targets := []domain.ReminderTarget{
		newReminderTarget(domain.ReminderDueSoon, false),
		newReminderTarget(domain.ReminderDueSoon, false),
	}

	instRepo := &fakeInstanceRepoWithDueSoon{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		dueSoonTargets:       targets,
	}

	r, err := app.NewReminders(instRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewReminders: %v", err)
	}

	if err := r.EmitDueSoon(context.Background(), time.Now()); err != nil {
		t.Fatalf("EmitDueSoon returned error for mid-batch enqueue failure: %v", err)
	}

	// Only 1 of 2 enqueues succeeded (first errored, second succeeded).
	if len(enqueuer.notifications) != 1 {
		t.Errorf("enqueued %d notifications, want 1 (second target succeeded)", len(enqueuer.notifications))
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
	reminders.EmitOverdue(context.Background(), time.Now(), repo.overdueTargets)
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
