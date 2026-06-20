package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// SyncScheduler runs the calendar SyncService on a poll interval as a background
// goroutine (see Run), alongside the other schedulers.
type SyncScheduler struct {
	sync         *SyncService
	logger       *slog.Logger
	pollInterval time.Duration
	tickTimeout  time.Duration
}

// NewSyncScheduler constructs the scheduler. pollInterval is how often Run polls;
// tickTimeout bounds a single sync cycle (and so how long an in-flight cycle can
// delay shutdown). Both must be positive and are kept separate so the poll
// cadence can be long without making shutdown wait a full interval.
func NewSyncScheduler(sync *SyncService, logger *slog.Logger, pollInterval, tickTimeout time.Duration) (*SyncScheduler, error) {
	if sync == nil {
		return nil, errors.New("calendar: NewSyncScheduler requires a non-nil sync service")
	}
	if logger == nil {
		return nil, errors.New("calendar: NewSyncScheduler requires a non-nil logger")
	}
	if pollInterval <= 0 {
		return nil, fmt.Errorf("calendar: NewSyncScheduler pollInterval must be positive, got %v", pollInterval)
	}
	if tickTimeout <= 0 {
		return nil, fmt.Errorf("calendar: NewSyncScheduler tickTimeout must be positive, got %v", tickTimeout)
	}
	return &SyncScheduler{sync: sync, logger: logger, pollInterval: pollInterval, tickTimeout: tickTimeout}, nil
}

// Run polls every pollInterval until ctx is cancelled, logging start and stop.
// Errors from a sync cycle are logged but do not stop the loop. Each tick runs
// under its own context (see runTick), so callers can wait for Run to return to
// know the scheduler has fully drained.
func (s *SyncScheduler) Run(ctx context.Context) {
	s.logger.Info("calendar sync scheduler: starting", "poll_interval", s.pollInterval)
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("calendar sync scheduler: stopped")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				s.logger.Info("calendar sync scheduler: stopped")
				return
			}
			s.runTick()
		}
	}
}

// runTick executes one sync cycle under a fresh bounded context independent of
// Run's lifecycle context, so an in-flight cycle finishes its writes during
// shutdown while the timeout still caps how long a stalled cycle delays shutdown.
func (s *SyncScheduler) runTick() {
	runCtx, cancel := context.WithTimeout(context.Background(), s.tickTimeout)
	defer cancel()

	processed, err := s.sync.RunOnce(runCtx)
	if err != nil {
		s.logger.Error("calendar sync scheduler: run once failed", "error", err)
	}
	if processed > 0 {
		s.logger.Info("calendar sync scheduler: synced events", "count", processed)
	}
}
