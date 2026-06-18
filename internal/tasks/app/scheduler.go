package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// Scheduler periodically materialises pending task instances and sweeps
// past-due pending instances to overdue across all households. It is designed
// to run as a single background goroutine alongside the notification dispatcher;
// the two workers are independent and share no state.
//
// Each poll cycle:
//  1. Calls [Generator.GenerateDue] to materialise upcoming instances.
//  2. Calls [domain.TaskInstanceRepository.MarkPendingOverdueAll] to flip
//     past-due pending instances to overdue.
//
// A failure in step 1 is logged and the error is recorded, but step 2 still
// runs — a generation failure must not prevent the overdue sweep. The first
// error encountered across both steps is surfaced by [Scheduler.RunOnce].
type Scheduler struct {
	generator    *Generator
	instanceRepo domain.TaskInstanceRepository
	logger       *slog.Logger
	pollInterval time.Duration
}

// NewScheduler constructs a Scheduler with injected dependencies.
//   - generator materialises upcoming task instances; it encapsulates the
//     recurring-task and instance repositories so the Scheduler never touches
//     them directly for generation.
//   - instanceRepo is used only for the overdue sweep
//     ([domain.TaskInstanceRepository.MarkPendingOverdueAll]).
//   - logger receives structured log lines; only task/count identifiers are
//     logged (not PII).
//   - pollInterval controls how often [Scheduler.Run] polls. Must be positive.
func NewScheduler(
	generator *Generator,
	instanceRepo domain.TaskInstanceRepository,
	logger *slog.Logger,
	pollInterval time.Duration,
) (*Scheduler, error) {
	if generator == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil generator")
	}
	if instanceRepo == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil instance repository")
	}
	if logger == nil {
		return nil, errors.New("app: NewScheduler requires a non-nil logger")
	}
	if pollInterval <= 0 {
		return nil, fmt.Errorf("app: NewScheduler pollInterval must be positive, got %v", pollInterval)
	}
	return &Scheduler{
		generator:    generator,
		instanceRepo: instanceRepo,
		logger:       logger,
		pollInterval: pollInterval,
	}, nil
}

// RunOnce executes one generation+sweep cycle as of asOf.
//
// It calls [Generator.GenerateDue] then
// [domain.TaskInstanceRepository.MarkPendingOverdueAll]. A failure in the
// generation step is logged; the overdue sweep still runs. The first non-nil
// error from either step is returned.
func (s *Scheduler) RunOnce(ctx context.Context, asOf time.Time) error {
	var firstErr error

	generated, genErr := s.generator.GenerateDue(ctx, asOf)
	if genErr != nil {
		s.logger.Error("scheduler: generate due failed", "error", genErr)
		firstErr = fmt.Errorf("scheduler: generate due: %w", genErr)
	} else {
		s.logger.Info("scheduler: generated instances", "count", generated)
	}

	overdue, overdueErr := s.instanceRepo.MarkPendingOverdueAll(ctx, asOf)
	if overdueErr != nil {
		s.logger.Error("scheduler: overdue sweep failed", "error", overdueErr)
		if firstErr == nil {
			firstErr = fmt.Errorf("scheduler: overdue sweep: %w", overdueErr)
		}
	} else {
		s.logger.Info("scheduler: marked overdue", "count", overdue)
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

	if err := s.RunOnce(runCtx, time.Now()); err != nil {
		s.logger.Error("scheduler: run once failed", "error", err)
	}
}
