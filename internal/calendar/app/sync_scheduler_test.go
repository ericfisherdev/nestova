package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/calendar/app"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// spyTickRecorder records ObserveTick calls for assertion. A spy is used
// instead of asserting on a real PromTickRecorder because the ObserveTick seam
// lives in the unexported runTick; PromTickRecorder's own error/success
// behaviour is covered once in the metrics package.
type spyTickRecorder struct {
	mu    sync.Mutex
	calls []spyTickCall
}

type spyTickCall struct {
	scheduler string
	err       error
}

func (s *spyTickRecorder) ObserveTick(scheduler string, _ time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, spyTickCall{scheduler: scheduler, err: err})
}

// waitForCall polls until at least one call is recorded or the deadline hits.
func (s *spyTickRecorder) waitForCall(t *testing.T) spyTickCall {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.calls) > 0 {
			call := s.calls[0]
			s.mu.Unlock()
			return call
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("ObserveTick was never called")
	return spyTickCall{}
}

func TestNewSyncSchedulerValidatesDeps(t *testing.T) {
	svc := mustSyncService(t, &fakeSyncAccountStore{}, &fakeEventRepo{}, &fakeEventSource{}, &fakeTokenProvider{})
	log := syncLogger()
	rec := metrics.NopTickRecorder{}
	cases := []struct {
		name string
		fn   func() (*app.SyncScheduler, error)
	}{
		{"nil sync service", func() (*app.SyncScheduler, error) {
			return app.NewSyncScheduler(nil, log, rec, time.Hour, time.Minute)
		}},
		{"nil logger", func() (*app.SyncScheduler, error) {
			return app.NewSyncScheduler(svc, nil, rec, time.Hour, time.Minute)
		}},
		{"nil tick recorder", func() (*app.SyncScheduler, error) {
			return app.NewSyncScheduler(svc, log, nil, time.Hour, time.Minute)
		}},
		{"non-positive poll interval", func() (*app.SyncScheduler, error) {
			return app.NewSyncScheduler(svc, log, rec, 0, time.Minute)
		}},
		{"non-positive tick timeout", func() (*app.SyncScheduler, error) {
			return app.NewSyncScheduler(svc, log, rec, time.Hour, 0)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.fn(); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestSyncScheduler_Run_FailingTickObservedWithError(t *testing.T) {
	// A failing account list makes the whole sync cycle error.
	store := &fakeSyncAccountStore{listErr: errors.New("db error")}
	svc := mustSyncService(t, store, &fakeEventRepo{}, &fakeEventSource{}, &fakeTokenProvider{})
	spy := &spyTickRecorder{}
	s, err := app.NewSyncScheduler(svc, syncLogger(), spy, 10*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatalf("NewSyncScheduler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	call := spy.waitForCall(t)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if call.scheduler != metrics.SchedulerCalendarSync {
		t.Errorf("ObserveTick scheduler = %q, want %q", call.scheduler, metrics.SchedulerCalendarSync)
	}
	if call.err == nil {
		t.Error("ObserveTick err = nil, want the failing tick's error")
	}
}
