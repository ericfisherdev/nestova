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
// A single failing enqueue is logged and does not abort the batch — every
// target is attempted. It returns a non-nil aggregated error when any enqueue
// failed, so the caller can surface the failure rather than silently succeed.
//
// Unlike due-soon, an overdue reminder cannot be "un-transitioned": the row is
// already overdue and stays visibly overdue in the UI, so the user is not left
// unaware even when the notification enqueue fails. The returned error exists
// only to make the failure observable upstream.
func (r *Reminders) EmitOverdue(ctx context.Context, asOf time.Time, targets []domain.ReminderTarget) error {
	var failures int
	for _, tgt := range targets {
		if err := r.enqueueReminder(ctx, asOf, tgt); err != nil {
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("reminders: %d of %d overdue enqueues failed", failures, len(targets))
	}
	return nil
}

// EmitDueSoon calls [domain.TaskInstanceRepository.ClaimDueSoonReminders] to
// atomically claim pending instances inside the due-soon window that have not
// yet been reminded, then enqueues one in-app notification per claimed target.
//
// Recovery: ClaimDueSoonReminders stamps reminded_at BEFORE this method
// enqueues, so a failed enqueue would otherwise drop the reminder permanently.
// To make it recoverable, a failed enqueue triggers
// [domain.TaskInstanceRepository.ClearDueSoonReminder] to reset reminded_at to
// NULL, so the next tick re-claims and retries the row. A failed clear is
// logged (the reminder is then lost until the row's state changes, which is the
// best we can do).
//
// Returns a non-nil error when ClaimDueSoonReminders fails OR when any enqueue
// failed, so the scheduler surfaces the failure instead of masking it.
func (r *Reminders) EmitDueSoon(ctx context.Context, asOf time.Time) error {
	targets, err := r.instanceRepo.ClaimDueSoonReminders(ctx, asOf)
	if err != nil {
		return fmt.Errorf("reminders: claim due-soon: %w", err)
	}

	var failures int
	for _, tgt := range targets {
		if enqErr := r.enqueueReminder(ctx, asOf, tgt); enqErr != nil {
			failures++
			// Un-stamp reminded_at so the row is re-claimed and retried next tick.
			if clearErr := r.instanceRepo.ClearDueSoonReminder(ctx, tgt.InstanceID); clearErr != nil {
				r.logger.Error("reminders: clear due-soon reminder failed; reminder may be lost until row changes",
					"instance_id", tgt.InstanceID.String(),
					"error", clearErr,
				)
			}
		}
	}

	r.logger.Info("reminders: due-soon emitted",
		"count", len(targets), "failures", failures)

	if failures > 0 {
		return fmt.Errorf("reminders: %d of %d due-soon enqueues failed", failures, len(targets))
	}
	return nil
}

// enqueueReminder builds and enqueues a single notification for the target. It
// returns the enqueue error (nil on success) so callers can take recovery
// action; the error is also logged here. A blank-title target or unknown kind
// is logged and skipped, returning nil (nothing to retry).
//
// ScheduledFor is set to asOf (the reminder's emission time) so the dispatcher
// delivers it immediately — a due-soon reminder is an advance heads-up and an
// overdue reminder is already late, so neither should wait until the due date.
// No PII is logged — only task instance ID and kind.
func (r *Reminders) enqueueReminder(ctx context.Context, asOf time.Time, tgt domain.ReminderTarget) error {
	// A target with no title means its recurring_task could not be resolved
	// (should be impossible given the ON DELETE CASCADE FK). Skip rather than
	// enqueue a blank-content notification; there is nothing to retry.
	if tgt.Title == "" {
		r.logger.Warn("reminders: skipping target with no resolved task title",
			"instance_id", tgt.InstanceID.String(), "kind", tgt.Kind.String())
		return nil
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
		return nil
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
		return fmt.Errorf("reminders: enqueue instance %s: %w", tgt.InstanceID.String(), err)
	}
	return nil
}
