package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

// renewalReminderSource is the notification source_type for subscription renewal
// reminders, mirroring "restock"/"task_instance".
const renewalReminderSource = "subscription"

// renewalStore is the slice of the subscription repository the scheduler needs
// (ISP): listing due subscriptions, claiming a reminder idempotently, and
// advancing a past-due renewal. The pgx SubscriptionRepository satisfies it.
type renewalStore interface {
	ListDueForRenewal(ctx context.Context, asOf time.Time) ([]*domain.Subscription, error)
	MarkReminded(ctx context.Context, id domain.SubscriptionID, occurrence time.Time) (bool, error)
	AdvanceRenewal(ctx context.Context, id domain.SubscriptionID, newNext time.Time) error
}

// RenewalScheduler emits renewal reminders and advances past-due renewals. Each
// run it lists subscriptions within their reminder lead window and, for those
// whose renewal is still upcoming, raises exactly one reminder per occurrence
// (idempotent via MarkReminded); subscriptions whose renewal date has passed are
// rolled forward by their cycle. It runs as a background goroutine (see Run)
// alongside the other schedulers.
type RenewalScheduler struct {
	store        renewalStore
	enqueuer     notifydomain.Enqueuer
	logger       *slog.Logger
	ticks        metrics.TickRecorder
	pollInterval time.Duration
	tickTimeout  time.Duration
}

// NewRenewalScheduler constructs the scheduler with injected dependencies.
// ticks records each poll cycle's duration and outcome (NES-115); pass
// [metrics.NopTickRecorder] when tick instrumentation is irrelevant.
// pollInterval is how often Run polls; tickTimeout bounds a single cycle's work
// (and so how long an in-flight cycle can delay shutdown). Both must be positive
// and are kept separate so the poll cadence can be long without making shutdown
// wait a full interval for a stalled tick.
// Returns an error when any dependency is nil or either duration is not
// positive.
func NewRenewalScheduler(
	store renewalStore,
	enqueuer notifydomain.Enqueuer,
	logger *slog.Logger,
	ticks metrics.TickRecorder,
	pollInterval time.Duration,
	tickTimeout time.Duration,
) (*RenewalScheduler, error) {
	if store == nil {
		return nil, errors.New("app: NewRenewalScheduler requires a non-nil store")
	}
	if enqueuer == nil {
		return nil, errors.New("app: NewRenewalScheduler requires a non-nil enqueuer")
	}
	if logger == nil {
		return nil, errors.New("app: NewRenewalScheduler requires a non-nil logger")
	}
	if ticks == nil {
		return nil, errors.New("app: NewRenewalScheduler requires a non-nil tick recorder")
	}
	if pollInterval <= 0 {
		return nil, fmt.Errorf("app: NewRenewalScheduler pollInterval must be positive, got %v", pollInterval)
	}
	if tickTimeout <= 0 {
		return nil, fmt.Errorf("app: NewRenewalScheduler tickTimeout must be positive, got %v", tickTimeout)
	}
	return &RenewalScheduler{
		store:        store,
		enqueuer:     enqueuer,
		logger:       logger,
		ticks:        ticks,
		pollInterval: pollInterval,
		tickTimeout:  tickTimeout,
	}, nil
}

// RunOnce processes all subscriptions due within their reminder lead window as of
// asOf, returning the number of reminders raised. A failure on one subscription
// is logged and recorded, but the rest of the batch still runs; the first error
// encountered is returned.
func (s *RenewalScheduler) RunOnce(ctx context.Context, asOf time.Time) (int, error) {
	subs, err := s.store.ListDueForRenewal(ctx, asOf)
	if err != nil {
		return 0, fmt.Errorf("renewal: list due: %w", err)
	}

	var (
		reminders int
		firstErr  error
	)
	for _, sub := range subs {
		if err := ctx.Err(); err != nil {
			return reminders, err
		}
		raised, subErr := s.process(ctx, sub, asOf)
		if subErr != nil {
			s.logger.Error("renewal: process subscription failed",
				"subscription_id", sub.ID.String(), "error", subErr)
			if firstErr == nil {
				firstErr = subErr
			}
			continue
		}
		reminders += raised
	}
	return reminders, firstErr
}

// process raises a reminder for an upcoming renewal (exactly once per occurrence)
// or rolls a past-due renewal forward. It returns 1 when a new reminder was
// raised.
func (s *RenewalScheduler) process(ctx context.Context, sub *domain.Subscription, asOf time.Time) (int, error) {
	// A renewal whose date has passed is rolled forward; AdvanceRenewal clears the
	// reminder guard so the new occurrence starts un-reminded and is reminded on a
	// later tick once it enters its lead window.
	if sub.NextRenewalOn.Before(dateOf(asOf)) {
		newNext, err := AdvancePastDue(sub.Cycle, sub.NextRenewalOn, asOf)
		if err != nil {
			return 0, fmt.Errorf("advance past-due renewal: %w", err)
		}
		if err := s.store.AdvanceRenewal(ctx, sub.ID, newNext); err != nil {
			return 0, fmt.Errorf("persist advanced renewal: %w", err)
		}
		return 0, nil
	}

	// Upcoming renewal within the lead window: claim the reminder idempotently.
	claimed, err := s.store.MarkReminded(ctx, sub.ID, sub.NextRenewalOn)
	if err != nil {
		return 0, fmt.Errorf("claim renewal reminder: %w", err)
	}
	if !claimed {
		// A reminder was already emitted for this occurrence.
		return 0, nil
	}

	// The claim is the dedup artifact: it is committed before the enqueue so a
	// re-run cannot duplicate the reminder. If the enqueue then fails, the
	// reminder for this occurrence is lost rather than duplicated — a deliberate
	// trade-off matching the restock scheduler, since the renewal date itself is
	// unaffected. Surface the error so the run records it.
	if err := s.enqueueReminder(ctx, sub, asOf); err != nil {
		return 0, fmt.Errorf("enqueue renewal reminder: %w", err)
	}
	return 1, nil
}

// enqueueReminder queues an in-app renewal reminder for the subscription, sourced
// to it (source_type "subscription", source_id = the subscription's UUID) and
// addressed to the payer when set (household-wide otherwise).
func (s *RenewalScheduler) enqueueReminder(ctx context.Context, sub *domain.Subscription, asOf time.Time) error {
	sourceID := uuid.UUID(sub.ID)
	n := &notifydomain.Notification{
		ID:           notifydomain.NewNotificationID(),
		HouseholdID:  sub.HouseholdID,
		MemberID:     sub.PayerID,
		Channel:      notifydomain.ChannelInApp,
		Title:        fmt.Sprintf("Subscription renewing soon: %s", sub.Name),
		Body:         fmt.Sprintf("%s (%s) renews on %s.", sub.Name, sub.Amount.String(), sub.NextRenewalOn.Format("Jan 2, 2006")),
		ScheduledFor: asOf,
		Status:       notifydomain.StatusPending,
		SourceType:   renewalReminderSource,
		SourceID:     &sourceID,
		EventType:    notifydomain.EventTypeSubscriptionRenewalDue,
	}
	return s.enqueuer.Enqueue(ctx, n)
}

// Run polls every pollInterval until ctx is cancelled, logging start and stop.
// Errors from RunOnce are logged but do not stop the loop. Cancelling ctx stops
// the loop but does not abort an in-flight cycle: each tick runs under its own
// context (see runTick), so callers can wait for Run to return to know the
// scheduler has fully drained.
func (s *RenewalScheduler) Run(ctx context.Context) {
	s.logger.Info("renewal scheduler: starting", "poll_interval", s.pollInterval)
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("renewal scheduler: stopped")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				s.logger.Info("renewal scheduler: stopped")
				return
			}
			s.runTick()
		}
	}
}

// runTick executes one RunOnce under a fresh bounded context independent of Run's
// lifecycle context, so an in-flight cycle finishes its writes during shutdown
// while the timeout still caps how long a stalled cycle delays shutdown.
func (s *RenewalScheduler) runTick() {
	runCtx, cancel := context.WithTimeout(context.Background(), s.tickTimeout)
	defer cancel()

	start := time.Now()
	reminders, err := s.RunOnce(runCtx, start)
	s.ticks.ObserveTick(metrics.SchedulerRenewal, time.Since(start), err)
	if err != nil {
		s.logger.Error("renewal scheduler: run once failed", "error", err)
	}
	if reminders > 0 {
		s.logger.Info("renewal scheduler: raised renewal reminders", "count", reminders)
	}
}
