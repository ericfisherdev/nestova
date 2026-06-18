package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// callCountingInstanceRepo wraps fakeTaskInstanceRepo and adds a configurable
// MarkPendingOverdueAll that counts invocations and returns a preset error.
// ---------------------------------------------------------------------------

type callCountingInstanceRepo struct {
	*fakeTaskInstanceRepo
	overdueAllCalls atomic.Int64
	overdueAllErr   error
	overdueAllCount int
}

func newCallCountingInstanceRepo() *callCountingInstanceRepo {
	return &callCountingInstanceRepo{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
	}
}

func (r *callCountingInstanceRepo) MarkPendingOverdueAll(_ context.Context, _ time.Time) (int, error) {
	r.overdueAllCalls.Add(1)
	return r.overdueAllCount, r.overdueAllErr
}

// ---------------------------------------------------------------------------
// errListAllActive is the sentinel returned by failListAllActiveRepo so tests
// can assert errors.Is(err, errListAllActive).
// ---------------------------------------------------------------------------

var errListAllActive = errors.New("stub: ListAllActive failed")

// failListAllActiveTaskRepo embeds fakeRecurringTaskRepo and overrides
// ListAllActive to return errListAllActive, simulating a database failure
// during the generation step.
type failListAllActiveTaskRepo struct {
	*fakeRecurringTaskRepo
}

func (r *failListAllActiveTaskRepo) ListAllActive(_ context.Context) ([]*domain.RecurringTask, error) {
	return nil, errListAllActive
}

// ---------------------------------------------------------------------------
// blockingOverdueRepo is a domain.TaskInstanceRepository whose
// MarkPendingOverdueAll blocks until the release channel is closed. It is used
// to simulate an in-flight tick during scheduler shutdown, verifying that the
// per-tick context is decoupled from the Run shutdown context.
// ---------------------------------------------------------------------------

type blockingOverdueRepo struct {
	*fakeTaskInstanceRepo
	release <-chan struct{}
	called  chan<- struct{}
}

func (r *blockingOverdueRepo) MarkPendingOverdueAll(_ context.Context, _ time.Time) (int, error) {
	// Signal that this method has been entered.
	select {
	case r.called <- struct{}{}:
	default:
	}
	// Block until released. We intentionally ignore any context here because
	// the correct behaviour under test is that runTick uses context.Background()
	// (decoupled from the Run shutdown signal), so our block must survive the
	// outer ctx cancellation.
	<-r.release
	return 0, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestScheduler constructs a Scheduler over the provided fakes with the
// given poll interval.
func newTestScheduler(
	t *testing.T,
	taskRepo *fakeRecurringTaskRepo,
	instRepo *callCountingInstanceRepo,
	pollInterval time.Duration,
) *app.Scheduler {
	t.Helper()
	gen, err := app.NewGenerator(taskRepo, instRepo.fakeTaskInstanceRepo, discardLogger(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	s, err := app.NewScheduler(gen, instRepo, discardLogger(), pollInterval)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Constructor validation
// ---------------------------------------------------------------------------

func TestNewScheduler_NilGenerator_ReturnsError(t *testing.T) {
	instRepo := newCallCountingInstanceRepo()
	_, err := app.NewScheduler(nil, instRepo, discardLogger(), time.Minute)
	if err == nil {
		t.Error("NewScheduler(nil generator) error = nil, want non-nil")
	}
}

func TestNewScheduler_NilInstanceRepo_ReturnsError(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	gen, err := app.NewGenerator(taskRepo, newFakeTaskInstanceRepo(), discardLogger(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	_, err = app.NewScheduler(gen, nil, discardLogger(), time.Minute)
	if err == nil {
		t.Error("NewScheduler(nil instanceRepo) error = nil, want non-nil")
	}
}

func TestNewScheduler_NilLogger_ReturnsError(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newCallCountingInstanceRepo()
	gen, err := app.NewGenerator(taskRepo, instRepo.fakeTaskInstanceRepo, discardLogger(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	_, err = app.NewScheduler(gen, instRepo, nil, time.Minute)
	if err == nil {
		t.Error("NewScheduler(nil logger) error = nil, want non-nil")
	}
}

func TestNewScheduler_NonPositivePollInterval_ReturnsError(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newCallCountingInstanceRepo()
	gen, err := app.NewGenerator(taskRepo, instRepo.fakeTaskInstanceRepo, discardLogger(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	_, err = app.NewScheduler(gen, instRepo, discardLogger(), 0)
	if err == nil {
		t.Error("NewScheduler(pollInterval=0) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// RunOnce
// ---------------------------------------------------------------------------

// TestScheduler_RunOnce_CallsBothSteps verifies that RunOnce with an empty task
// repo (zero generation) still calls MarkPendingOverdueAll exactly once and
// returns no error.
func TestScheduler_RunOnce_CallsBothSteps(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newCallCountingInstanceRepo()
	instRepo.overdueAllCount = 3

	s := newTestScheduler(t, taskRepo, instRepo, time.Minute)

	if err := s.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := instRepo.overdueAllCalls.Load(); got != 1 {
		t.Errorf("MarkPendingOverdueAll calls = %d, want 1", got)
	}
}

// TestScheduler_RunOnce_OverdueSweepRunsEvenWhenGenerateFails verifies that
// when GenerateDue fails, MarkPendingOverdueAll is still called and RunOnce
// returns a non-nil error.
func TestScheduler_RunOnce_OverdueSweepRunsEvenWhenGenerateFails(t *testing.T) {
	failingTaskRepo := &failListAllActiveTaskRepo{
		fakeRecurringTaskRepo: newFakeRecurringTaskRepo(),
	}
	gen, err := app.NewGenerator(failingTaskRepo, newFakeTaskInstanceRepo(), discardLogger(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}

	instRepo := newCallCountingInstanceRepo()
	s, err := app.NewScheduler(gen, instRepo, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}

	err = s.RunOnce(context.Background(), time.Now())
	if err == nil {
		t.Error("RunOnce error = nil, want non-nil when GenerateDue fails")
	}
	if got := instRepo.overdueAllCalls.Load(); got != 1 {
		t.Errorf("MarkPendingOverdueAll calls = %d, want 1 even on generate failure", got)
	}
}

// TestScheduler_RunOnce_OverdueSweepError_ReturnsError verifies that when
// MarkPendingOverdueAll fails and GenerateDue succeeds, RunOnce returns the
// sweep error.
func TestScheduler_RunOnce_OverdueSweepError_ReturnsError(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newCallCountingInstanceRepo()
	instRepo.overdueAllErr = errors.New("db: overdue sweep failed")

	s := newTestScheduler(t, taskRepo, instRepo, time.Minute)

	err := s.RunOnce(context.Background(), time.Now())
	if err == nil {
		t.Error("RunOnce error = nil, want non-nil when overdue sweep fails")
	}
}

// TestScheduler_RunOnce_GenerateFailFirst_ReturnsGenerateError verifies that
// when both GenerateDue and the overdue sweep fail, the generate error is
// returned (first-error wins).
func TestScheduler_RunOnce_GenerateFailFirst_ReturnsGenerateError(t *testing.T) {
	failingTaskRepo := &failListAllActiveTaskRepo{
		fakeRecurringTaskRepo: newFakeRecurringTaskRepo(),
	}
	gen, err := app.NewGenerator(failingTaskRepo, newFakeTaskInstanceRepo(), discardLogger(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}

	instRepo := newCallCountingInstanceRepo()
	instRepo.overdueAllErr = errors.New("db: sweep also failed")

	s, err := app.NewScheduler(gen, instRepo, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}

	err = s.RunOnce(context.Background(), time.Now())
	if err == nil {
		t.Fatal("RunOnce error = nil, want non-nil")
	}
	if !errors.Is(err, errListAllActive) {
		t.Errorf("RunOnce error = %v, want to wrap errListAllActive", err)
	}
}

// ---------------------------------------------------------------------------
// Run (lifecycle)
// ---------------------------------------------------------------------------

// TestScheduler_Run_ReturnsWhenContextCancelled verifies that Run exits cleanly
// after ctx is cancelled and does not hang.
func TestScheduler_Run_ReturnsWhenContextCancelled(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()
	instRepo := newCallCountingInstanceRepo()

	s := newTestScheduler(t, taskRepo, instRepo, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	// Cancelling before the 10ms tick interval elapsed must not start a tick.
	if n := instRepo.overdueAllCalls.Load(); n != 0 {
		t.Errorf("scheduler ran %d ticks after immediate cancellation, want 0", n)
	}
}

// TestScheduler_Run_InFlightTickCompletesBeforeStop verifies that a tick
// already executing when ctx is cancelled runs to completion before Run returns.
// This is the key proof that runTick uses context.Background() (decoupled from
// the shutdown signal) so an in-flight DB write is never interrupted by the
// process shutdown signal.
func TestScheduler_Run_InFlightTickCompletesBeforeStop(t *testing.T) {
	taskRepo := newFakeRecurringTaskRepo()

	release := make(chan struct{})
	called := make(chan struct{}, 1)
	blockRepo := &blockingOverdueRepo{
		fakeTaskInstanceRepo: newFakeTaskInstanceRepo(),
		release:              release,
		called:               called,
	}

	gen, err := app.NewGenerator(taskRepo, blockRepo.fakeTaskInstanceRepo, discardLogger(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	s, err := app.NewScheduler(gen, blockRepo, discardLogger(), 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(runDone)
	}()

	// Wait until a tick has entered MarkPendingOverdueAll.
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("MarkPendingOverdueAll was never entered")
	}

	// Cancel the Run context while the tick is still blocked.
	cancel()

	// Run must NOT return until the in-flight tick is released.
	select {
	case <-runDone:
		t.Fatal("Run returned before in-flight tick completed — tick context was incorrectly coupled to shutdown signal")
	case <-time.After(100 * time.Millisecond):
		// Correct behaviour: Run is still waiting.
	}

	// Release the tick; Run must now exit cleanly.
	close(release)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after in-flight tick completed")
	}
}
