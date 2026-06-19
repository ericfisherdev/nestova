package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// categoryLabel returns a human-readable prefix for a task category, used in
// notification title text. It is kept simple and contains no PII.
func categoryLabel(cat domain.Category) string {
	if cat == domain.MaintenanceCategory {
		return "Maintenance item"
	}
	return "Chore"
}

// Reminders emits due-soon and overdue task notifications through the notify
// outbox. It is constructed by [NewScheduler] and driven from
// [Scheduler.RunOnce].
//
// A single failing enqueue is logged and does not abort the batch — remaining
// targets are still processed.
type Reminders struct {
	instanceRepo domain.TaskInstanceRepository
	enqueuer     notifydomain.Enqueuer
	logger       *slog.Logger
}

// NewReminders constructs a Reminders service with injected dependencies.
//
// Returns an error when any argument is nil.
func NewReminders(
	instanceRepo domain.TaskInstanceRepository,
	enqueuer notifydomain.Enqueuer,
	logger *slog.Logger,
) (*Reminders, error) {
	if instanceRepo == nil {
		return nil, errors.New("app: NewReminders requires a non-nil instance repository")
	}
	if enqueuer == nil {
		return nil, errors.New("app: NewReminders requires a non-nil enqueuer")
	}
	if logger == nil {
		return nil, errors.New("app: NewReminders requires a non-nil logger")
	}
	return &Reminders{
		instanceRepo: instanceRepo,
		enqueuer:     enqueuer,
		logger:       logger,
	}, nil
}

// EmitOverdue enqueues one in-app overdue notification per target. It is
// called by [Scheduler.RunOnce] with the targets returned by
// [domain.TaskInstanceRepository.MarkPendingOverdueAll].
//
// One failing enqueue is logged and does not abort the batch.
func (r *Reminders) EmitOverdue(ctx context.Context, asOf time.Time, targets []domain.ReminderTarget) {
	for _, tgt := range targets {
		r.enqueueReminder(ctx, asOf, tgt)
	}
}

// EmitDueSoon calls [domain.TaskInstanceRepository.ClaimDueSoonReminders] to
// atomically claim pending instances that have entered their lead-time window
// and not yet been reminded, then enqueues one in-app notification per claimed
// target.
//
// Returns any error from ClaimDueSoonReminders. Individual enqueue failures
// are logged but do not abort the batch or contribute to the returned error.
func (r *Reminders) EmitDueSoon(ctx context.Context, asOf time.Time) error {
	targets, err := r.instanceRepo.ClaimDueSoonReminders(ctx, asOf)
	if err != nil {
		return fmt.Errorf("reminders: claim due-soon: %w", err)
	}
	for _, tgt := range targets {
		r.enqueueReminder(ctx, asOf, tgt)
	}
	r.logger.Info("reminders: due-soon emitted", "count", len(targets))
	return nil
}

// enqueueReminder builds and enqueues a single notification for the target. A
// failing enqueue is logged and ignored so the caller's batch is never aborted
// by one notification failure.
//
// ScheduledFor is set to asOf (the reminder's emission time) so the dispatcher
// delivers it immediately — a due-soon reminder is an advance heads-up and an
// overdue reminder is already late, so neither should wait until the due date.
// No PII is logged — only task instance ID and kind.
func (r *Reminders) enqueueReminder(ctx context.Context, asOf time.Time, tgt domain.ReminderTarget) {
	// A target with no title means its recurring_task could not be resolved
	// (should be impossible given the ON DELETE CASCADE FK). Skip rather than
	// enqueue a blank-content notification.
	if tgt.Title == "" {
		r.logger.Warn("reminders: skipping target with no resolved task title",
			"instance_id", tgt.InstanceID.String(), "kind", tgt.Kind.String())
		return
	}

	label := categoryLabel(tgt.Category)

	var title, body string
	switch tgt.Kind {
	case domain.ReminderDueSoon:
		title = fmt.Sprintf("%s due soon: %s", label, tgt.Title)
		body = fmt.Sprintf("%s is due on %s.", tgt.Title, tgt.DueOn.Format("Jan 2"))
	case domain.ReminderOverdue:
		title = fmt.Sprintf("%s overdue: %s", label, tgt.Title)
		body = fmt.Sprintf("%s was due on %s and is now overdue.", tgt.Title, tgt.DueOn.Format("Jan 2"))
	default:
		r.logger.Error("reminders: unknown reminder kind",
			"kind", tgt.Kind,
			"instance_id", tgt.InstanceID.String(),
		)
		return
	}

	instUUID := uuid.UUID(tgt.InstanceID)
	n := &notifydomain.Notification{
		ID:           notifydomain.NewNotificationID(),
		HouseholdID:  tgt.HouseholdID,
		MemberID:     tgt.AssigneeID,
		Channel:      notifydomain.ChannelInApp,
		Title:        title,
		Body:         body,
		ScheduledFor: asOf,
		Status:       notifydomain.StatusPending,
		SourceType:   "task_instance",
		SourceID:     &instUUID,
	}

	if err := r.enqueuer.Enqueue(ctx, n); err != nil {
		r.logger.Error("reminders: enqueue failed",
			"instance_id", tgt.InstanceID.String(),
			"kind", tgt.Kind.String(),
			"error", err,
		)
	}
}
