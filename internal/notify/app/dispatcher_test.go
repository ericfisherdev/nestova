package app_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/app"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// ----------------------------------------------------------------------------
// Fakes
// ----------------------------------------------------------------------------

// fakeOutbox is an in-memory domain.Outbox for hermetic dispatcher tests.
type fakeOutbox struct {
	due       []*domain.Notification
	sentIDs   []domain.NotificationID
	failedIDs []domain.NotificationID
	claimErr  error
}

func (f *fakeOutbox) Enqueue(_ context.Context, n *domain.Notification) error {
	f.due = append(f.due, n)
	return nil
}

func (f *fakeOutbox) ClaimDue(_ context.Context, limit int) ([]*domain.Notification, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if len(f.due) == 0 {
		return nil, nil
	}
	end := limit
	if end > len(f.due) {
		end = len(f.due)
	}
	batch := f.due[:end]
	f.due = f.due[end:]
	return batch, nil
}

func (f *fakeOutbox) MarkSent(_ context.Context, id domain.NotificationID) error {
	f.sentIDs = append(f.sentIDs, id)
	return nil
}

func (f *fakeOutbox) MarkFailed(_ context.Context, id domain.NotificationID) error {
	f.failedIDs = append(f.failedIDs, id)
	return nil
}

// toggleFakeSender is a domain.Sender whose Send behaviour is determined by a
// closure, enabling per-call control in tests.
type toggleFakeSender struct {
	ch     domain.Channel
	sendFn func(n *domain.Notification) error
}

func (s *toggleFakeSender) Channel() domain.Channel { return s.ch }

func (s *toggleFakeSender) Send(_ context.Context, n *domain.Notification) error {
	return s.sendFn(n)
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

func silentLogger() *slog.Logger {
	// Discard all log output; tests should not produce noise.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}

func newInAppNotification() *domain.Notification {
	return &domain.Notification{
		ID:           domain.NewNotificationID(),
		HouseholdID:  household.NewHouseholdID(),
		Channel:      domain.ChannelInApp,
		Title:        "Test Title",
		Body:         "Test Body",
		ScheduledFor: time.Now().Add(-time.Second),
		Status:       domain.StatusPending,
	}
}

func newDispatcher(t *testing.T, outbox domain.Outbox, senders []domain.Sender) *app.Dispatcher {
	t.Helper()
	d, err := app.NewDispatcher(outbox, senders, silentLogger(), metrics.NopTickRecorder{}, 10, time.Minute)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

func alwaysSucceedSender() *toggleFakeSender {
	return &toggleFakeSender{
		ch:     domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error { return nil },
	}
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestRunOnce_Success_CallsMarkSent(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	outbox.due = []*domain.Notification{n}

	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 1 {
		t.Errorf("RunOnce count = %d, want 1", count)
	}
	if len(outbox.sentIDs) != 1 || outbox.sentIDs[0] != n.ID {
		t.Errorf("outbox.sentIDs = %v, want [%v]", outbox.sentIDs, n.ID)
	}
	if len(outbox.failedIDs) != 0 {
		t.Errorf("outbox.failedIDs = %v, want empty", outbox.failedIDs)
	}
}

func TestRunOnce_SendFailure_CallsMarkFailed(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error { return errors.New("network down") },
	}
	d := newDispatcher(t, outbox, []domain.Sender{sender})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 1 {
		t.Errorf("RunOnce count = %d, want 1", count)
	}
	if len(outbox.failedIDs) != 1 || outbox.failedIDs[0] != n.ID {
		t.Errorf("outbox.failedIDs = %v, want [%v]", outbox.failedIDs, n.ID)
	}
	if len(outbox.sentIDs) != 0 {
		t.Errorf("outbox.sentIDs = %v, want empty", outbox.sentIDs)
	}
}

func TestRunOnce_UnknownChannel_CallsMarkFailed(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	n.Channel = domain.ChannelEmail // no email sender registered
	outbox.due = []*domain.Notification{n}

	// Only inapp sender registered — email channel is unknown.
	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 1 {
		t.Errorf("RunOnce count = %d, want 1", count)
	}
	if len(outbox.failedIDs) != 1 || outbox.failedIDs[0] != n.ID {
		t.Errorf("outbox.failedIDs = %v, want [%v]", outbox.failedIDs, n.ID)
	}
	if len(outbox.sentIDs) != 0 {
		t.Errorf("outbox.sentIDs = %v, want empty", outbox.sentIDs)
	}
}

func TestRunOnce_OneFailureDoesNotAbortBatch(t *testing.T) {
	// Two notifications: first send fails, second succeeds. Both must be
	// processed: count must be 2, one in failedIDs, one in sentIDs.
	outbox := &fakeOutbox{}
	n1 := newInAppNotification()
	n2 := newInAppNotification()
	outbox.due = []*domain.Notification{n1, n2}

	calls := 0
	sender := &toggleFakeSender{
		ch: domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error {
			calls++
			if calls == 1 {
				return errors.New("transient error on first")
			}
			return nil
		},
	}
	d := newDispatcher(t, outbox, []domain.Sender{sender})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 2 {
		t.Errorf("RunOnce count = %d, want 2", count)
	}
	if len(outbox.failedIDs) != 1 {
		t.Errorf("failedIDs len = %d, want 1", len(outbox.failedIDs))
	}
	if len(outbox.sentIDs) != 1 {
		t.Errorf("sentIDs len = %d, want 1", len(outbox.sentIDs))
	}
}

func TestRunOnce_NoDueNotifications_ReturnsZero(t *testing.T) {
	outbox := &fakeOutbox{} // empty due slice
	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 0 {
		t.Errorf("RunOnce count = %d, want 0", count)
	}
}

func TestRunOnce_ClaimDueError_ReturnsError(t *testing.T) {
	outbox := &fakeOutbox{claimErr: errors.New("db error")}
	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Error("RunOnce error = nil, want non-nil when ClaimDue fails")
	}
}

func TestNewDispatcher_NilOutbox_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(nil, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil outbox) error = nil, want non-nil")
	}
}

func TestNewDispatcher_EmptySenders_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{}, silentLogger(), metrics.NopTickRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(empty senders) error = nil, want non-nil")
	}
}

func TestNewDispatcher_NilLogger_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, nil, metrics.NopTickRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil logger) error = nil, want non-nil")
	}
}

func TestNewDispatcher_InvalidBatchSize_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, 0, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(batchSize=0) error = nil, want non-nil")
	}
}

func TestNewDispatcher_InvalidPollInterval_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, 10, 0)
	if err == nil {
		t.Error("NewDispatcher(pollInterval=0) error = nil, want non-nil")
	}
}

func TestNewDispatcher_DuplicateChannel_ReturnsError(t *testing.T) {
	s := alwaysSucceedSender()
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{s, s}, silentLogger(), metrics.NopTickRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(duplicate channel) error = nil, want non-nil")
	}
}

func TestNewDispatcher_NilSenderEntry_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{nil}, silentLogger(), metrics.NopTickRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil sender) error = nil, want non-nil")
	}
}

func TestRun_ReturnsWhenContextCancelled(t *testing.T) {
	d, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, 10, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestNewDispatcher_NilTickRecorder_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), nil, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil tick recorder) error = nil, want non-nil")
	}
}

// spyTickRecorder records ObserveTick calls for assertion. It is used instead
// of asserting on a real PromTickRecorder because the ObserveTick seam lives in
// the unexported runTick, so the observable contract here is "the tick recorder
// saw the failing cycle"; PromTickRecorder's own error/success behaviour is
// covered once in the metrics package.
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

// snapshot returns a copy of the recorded calls.
func (s *spyTickRecorder) snapshot() []spyTickCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spyTickCall(nil), s.calls...)
}

// waitForCall polls until at least one call is recorded or the deadline hits.
func (s *spyTickRecorder) waitForCall(t *testing.T) spyTickCall {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls := s.snapshot(); len(calls) > 0 {
			return calls[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("ObserveTick was never called")
	return spyTickCall{}
}

func TestRun_FailingTick_ObservedWithError(t *testing.T) {
	outbox := &fakeOutbox{claimErr: errors.New("db error")}
	spy := &spyTickRecorder{}
	d, err := app.NewDispatcher(outbox, []domain.Sender{alwaysSucceedSender()}, silentLogger(), spy, 10, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	call := spy.waitForCall(t)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if call.scheduler != metrics.SchedulerDispatcher {
		t.Errorf("ObserveTick scheduler = %q, want %q", call.scheduler, metrics.SchedulerDispatcher)
	}
	if call.err == nil {
		t.Error("ObserveTick err = nil, want the failing tick's error")
	}
}
