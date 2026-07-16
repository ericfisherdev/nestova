package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// Scheduler periodically materialises pending task instances, sweeps past-due
// pending instances to overdue, and emits due-soon, overdue, claim-warning,
// claim-expiry, and trade-expiry reminders through the notify outbox. It is
// designed to run as a single background goroutine alongside the notification
// dispatcher; the two workers are independent and share no state.
//
// Each poll cycle:
//  1. Calls [Generator.GenerateDue] to materialise upcoming instances.
//  2. Calls [domain.TaskInstanceRepository.MarkPendingOverdueAll] to flip
//     past-due pending instances to overdue and obtain the transitioned rows.
//  3. Calls [Reminders.EmitOverdue] to enqueue overdue notifications.
//  4. Calls [Reminders.EmitDueSoon] to claim and enqueue due-soon notifications.
//  5. Calls [Reminders.EmitClaimWarnings] to mark and enqueue "claim expiring
//     soon" notifications for claims entering their warning window (NES-118).
//  6. Calls [Reminders.EmitClaimExpiry] to revert expired chore claims,
//     penalize their claimants, and enqueue claim-expiry notifications
//     (NES-117). It runs on this same cadence rather than a separate
//     scheduler since it shares the same "sweep task_instance for a
//     time-based transition" shape as step 2.
//  7. Calls [TradeService.ExpireTrades] to resolve expired chore trade
//     proposals and enqueue trade-expiry notifications (NES-121). Like step
//     6, it shares the tasks scheduler's cadence rather than running as its
//     own background worker, since a trade's expiry is itself derived from
//     the same task_instance due dates the rest of this scheduler already
//     sweeps.
//
// A failure in step 1 is logged and the error is recorded, but steps 2–7 still
// run — a generation failure must not prevent the overdue sweep, reminder
// emission, claim-warning pass, claim-expiry sweep, or trade-expiry sweep. The
// first error encountered across all steps is surfaced by [Scheduler.RunOnce].
type Scheduler struct {
	generator    *Generator
	instanceRepo domain.TaskInstanceRepository
	reminders    *Reminders
	tradeService *TradeService
	logger       *slog.Logger
	ticks        metrics.TickRecorder
	pollInterval time.Duration
}

// NewScheduler constructs a Scheduler with injected dependencies.
//   - generator materialises upcoming task instances.
//   - instanceRepo is used for the overdue sweep
//     ([domain.TaskInstanceRepository.MarkPendingOverdueAll]) and due-soon
//     claim ([domain.TaskInstanceRepository.ClaimDueSoonReminders]).
//   - tradeRepo is used for the trade-expiry sweep
//     ([domain.ChoreTradeRepository.SweepExpiredTrades], NES-121); Scheduler
//     builds a [TradeService] internally, mirroring how it builds [Reminders]
//     from instanceRepo.
//   - enqueuer is the notify outbox producer port; Scheduler builds both
//     [Reminders] and [TradeService] internally from it.
//   - logger receives structured log lines; only task/count identifiers are
//     logged (not PII).
//   - ticks records each poll cycle's duration and outcome (NES-115); pass
//     [metrics.NopTickRecorder] when tick instrumentation is irrelevant.
//   - pollInterval controls how often [Scheduler.Run] polls. Must be positive.
//
// Returns an error when any dependency is nil or pollInterval is not positive.
func NewScheduler(
	generator *Generator,
	instanceRepo domain.TaskInstanceRepository,
	tradeRepo domain.ChoreTradeRepository,
	enqueuer notifydomain.Enqueuer,
	logger *slog.Logger,
	ticks metrics.TickRecorder,
	pollInterval time.Duration,
) (*Scheduler, error) {
	if generator == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil generator")
	}
	if instanceRepo == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil instance repository")
	}
	if tradeRepo == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil trade repository")
	}
	if enqueuer == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil enqueuer")
	}
	if logger == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil logger")
	}
	if ticks == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil tick recorder")
	}
	if pollInterval <= 0 {
		return nil, fmt.Errorf("app: NewScheduler pollInterval must be positive, got %v", pollInterval)
	}
	reminders, err := NewReminders(instanceRepo, enqueuer, logger)
	if err != nil {
		return nil, fmt.Errorf("app: NewScheduler: %w", err)
	}
	tradeService, err := NewTradeService(tradeRepo, enqueuer, logger)
	if err != nil {
		return nil, fmt.Errorf("app: NewScheduler: %w", err)
	}
	return &Scheduler{
		generator:    generator,
		instanceRepo: instanceRepo,
		reminders:    reminders,
		tradeService: tradeService,
		logger:       logger,
		ticks:        ticks,
		pollInterval: pollInterval,
	}, nil
}

// RunOnce executes one generation+sweep+reminder cycle as of asOf.
//
// Steps (each step still runs even if a previous step fails; the first
// non-nil error is returned):
//  1. [Generator.GenerateDue] — materialise upcoming instances.
//  2. [domain.TaskInstanceRepository.MarkPendingOverdueAll] — transition
//     past-due pending rows to overdue and collect the targets.
//  3. [Reminders.EmitOverdue] — enqueue overdue notifications for the targets
//     returned by step 2.
//  4. [Reminders.EmitDueSoon] — claim and enqueue due-soon notifications.
//  5. [Reminders.EmitClaimWarnings] — mark and enqueue "claim expiring soon"
//     notifications (NES-118).
//  6. [Reminders.EmitClaimExpiry] — revert expired chore claims, penalize
//     their claimants, and enqueue claim-expiry notifications (NES-117).
//  7. [TradeService.ExpireTrades] — resolve expired chore trade proposals and
//     enqueue trade-expiry notifications (NES-121).
func (s *Scheduler) RunOnce(ctx context.Context, asOf time.Time) error {
	var firstErr error

	generated, genErr := s.generator.GenerateDue(ctx, asOf)
	if genErr != nil {
		s.logger.Error("scheduler: generate due failed", "error", genErr)
		firstErr = fmt.Errorf("scheduler: generate due: %w", genErr)
	} else {
		s.logger.Info("scheduler: generated instances", "count", generated)
	}

	overdueTargets, overdueErr := s.instanceRepo.MarkPendingOverdueAll(ctx, asOf)
	if overdueErr != nil {
		s.logger.Error("scheduler: overdue sweep failed", "error", overdueErr)
		if firstErr == nil {
			firstErr = fmt.Errorf("scheduler: overdue sweep: %w", overdueErr)
		}
	} else {
		s.logger.Info("scheduler: marked overdue", "count", len(overdueTargets))
		if emitErr := s.reminders.EmitOverdue(ctx, asOf, overdueTargets); emitErr != nil {
			s.logger.Error("scheduler: overdue reminders failed", "error", emitErr)
			if firstErr == nil {
				firstErr = fmt.Errorf("scheduler: overdue reminders: %w", emitErr)
			}
		}
	}

	if dueSoonErr := s.reminders.EmitDueSoon(ctx, asOf); dueSoonErr != nil {
		s.logger.Error("scheduler: due-soon reminders failed", "error", dueSoonErr)
		if firstErr == nil {
			firstErr = fmt.Errorf("scheduler: due-soon reminders: %w", dueSoonErr)
		}
	}

	if claimWarningErr := s.reminders.EmitClaimWarnings(ctx, asOf); claimWarningErr != nil {
		s.logger.Error("scheduler: claim warnings failed", "error", claimWarningErr)
		if firstErr == nil {
			firstErr = fmt.Errorf("scheduler: claim warnings: %w", claimWarningErr)
		}
	}

	if claimExpiryErr := s.reminders.EmitClaimExpiry(ctx, asOf); claimExpiryErr != nil {
		s.logger.Error("scheduler: claim expiry processing failed", "error", claimExpiryErr)
		if firstErr == nil {
			firstErr = fmt.Errorf("scheduler: claim expiry processing: %w", claimExpiryErr)
		}
	}

	if tradeExpiryErr := s.tradeService.ExpireTrades(ctx, asOf); tradeExpiryErr != nil {
		s.logger.Error("scheduler: trade expiry processing failed", "error", tradeExpiryErr)
		if firstErr == nil {
			firstErr = fmt.Errorf("scheduler: trade expiry processing: %w", tradeExpiryErr)
		}
	}

	return firstErr
}

// Run polls on every pollInterval until ctx is cancelled. It logs start and
// stop events. Errors from RunOnce are logged but do not stop the loop —
// transient database failures resolve on the next tick.
//
// Cancelling ctx stops the loop but does NOT abort a cycle already in progress:
// each tick runs under its own context (see runTick), so an in-flight cycle
// finishes its database writes cleanly before Run returns. Callers can therefore
// wait for Run to return to know the scheduler has fully drained.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("scheduler: starting", "poll_interval", s.pollInterval)
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler: stopped")
			return
		case <-ticker.C:
			// When ctx is cancelled and a tick fires in the same select, Go may
			// pick either case; re-check so no extra tick runs after shutdown.
			if ctx.Err() != nil {
				s.logger.Info("scheduler: stopped")
				return
			}
			s.runTick()
		}
	}
}

// runTick executes a single RunOnce under a fresh bounded context that is
// independent of Run's lifecycle context. Decoupling the work context from the
// shutdown signal lets an in-flight cycle complete its database writes even
// while the process is shutting down, while the timeout still caps how long a
// stalled cycle can delay shutdown.
func (s *Scheduler) runTick() {
	runCtx, cancel := context.WithTimeout(context.Background(), s.pollInterval)
	defer cancel()

	start := time.Now()
	err := s.RunOnce(runCtx, start)
	s.ticks.ObserveTick(metrics.SchedulerTasks, time.Since(start), err)
	if err != nil {
		s.logger.Error("scheduler: run once failed", "error", err)
	}
}
