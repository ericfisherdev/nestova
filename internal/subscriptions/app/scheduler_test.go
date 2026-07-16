package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
	"github.com/ericfisherdev/nestova/internal/subscriptions/app"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type markCall struct {
	id         domain.SubscriptionID
	occurrence time.Time
}

type advanceCall struct {
	id      domain.SubscriptionID
	newNext time.Time
}

// fakeStore is an in-memory renewalStore for hermetic scheduler tests.
type fakeStore struct {
	due          []*domain.Subscription
	listErr      error
	claimed      bool // result MarkReminded returns
	markErr      error
	advanceErr   error
	markCalls    []markCall
	advanceCalls []advanceCall
}

func (f *fakeStore) ListDueForRenewal(context.Context, time.Time) ([]*domain.Subscription, error) {
	return f.due, f.listErr
}

func (f *fakeStore) MarkReminded(_ context.Context, id domain.SubscriptionID, occurrence time.Time) (bool, error) {
	f.markCalls = append(f.markCalls, markCall{id, occurrence})
	if f.markErr != nil {
		return false, f.markErr
	}
	return f.claimed, nil
}

func (f *fakeStore) AdvanceRenewal(_ context.Context, id domain.SubscriptionID, newNext time.Time) error {
	f.advanceCalls = append(f.advanceCalls, advanceCall{id, newNext})
	return f.advanceErr
}

type fakeEnqueuer struct {
	enqueued []*notifydomain.Notification
	err      error
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, n *notifydomain.Notification) error {
	if f.err != nil {
		return f.err
	}
	f.enqueued = append(f.enqueued, n)
	return nil
}

func mustScheduler(t *testing.T, store *fakeStore, enq notifydomain.Enqueuer) *app.RenewalScheduler {
	t.Helper()
	s, err := app.NewRenewalScheduler(store, enq, discardLogger(), metrics.NopTickRecorder{}, time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("NewRenewalScheduler: %v", err)
	}
	return s
}

func testSub(t *testing.T, next time.Time, payer *household.MemberID) *domain.Subscription {
	t.Helper()
	amount, err := household.NewMoney(1299, "USD")
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	return &domain.Subscription{
		ID:            domain.NewSubscriptionID(),
		HouseholdID:   household.NewHouseholdID(),
		Name:          "Streaming",
		Amount:        amount,
		Cycle:         domain.CycleMonthly,
		NextRenewalOn: next,
		PayerID:       payer,
		Active:        true,
	}
}

func TestRunOnceRemindsUpcomingOnce(t *testing.T) {
	asOf := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	next := day(2026, 7, 12) // upcoming, within window
	payer := household.NewMemberID()
	sub := testSub(t, next, &payer)
	store := &fakeStore{due: []*domain.Subscription{sub}, claimed: true}
	enq := &fakeEnqueuer{}

	count, err := mustScheduler(t, store, enq).RunOnce(context.Background(), asOf)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if count != 1 {
		t.Fatalf("RunOnce count = %d, want 1", count)
	}
	if len(store.markCalls) != 1 || !store.markCalls[0].occurrence.Equal(next) {
		t.Fatalf("MarkReminded calls = %+v, want one with occurrence %s", store.markCalls, next)
	}
	if len(store.advanceCalls) != 0 {
		t.Fatalf("AdvanceRenewal should not be called for an upcoming renewal, got %+v", store.advanceCalls)
	}
	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(enq.enqueued))
	}
	n := enq.enqueued[0]
	if n.SourceType != "subscription" || n.SourceID == nil {
		t.Fatalf("notification source = (%q, %v), want subscription/non-nil", n.SourceType, n.SourceID)
	}
	if n.MemberID == nil || *n.MemberID != payer {
		t.Fatalf("notification MemberID = %v, want payer %v", n.MemberID, payer)
	}
}

func TestRunOnceIdempotentWhenAlreadyReminded(t *testing.T) {
	asOf := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	sub := testSub(t, day(2026, 7, 12), nil)
	store := &fakeStore{due: []*domain.Subscription{sub}, claimed: false} // already reminded
	enq := &fakeEnqueuer{}

	count, err := mustScheduler(t, store, enq).RunOnce(context.Background(), asOf)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if count != 0 || len(enq.enqueued) != 0 {
		t.Fatalf("RunOnce count = %d, enqueued = %d, want 0/0", count, len(enq.enqueued))
	}
}

func TestRunOncePastDueAdvances(t *testing.T) {
	asOf := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	sub := testSub(t, day(2026, 6, 12), nil) // past due
	store := &fakeStore{due: []*domain.Subscription{sub}, claimed: true}
	enq := &fakeEnqueuer{}

	count, err := mustScheduler(t, store, enq).RunOnce(context.Background(), asOf)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if count != 0 || len(enq.enqueued) != 0 {
		t.Fatalf("past-due should not remind: count = %d, enqueued = %d", count, len(enq.enqueued))
	}
	if len(store.advanceCalls) != 1 || !store.advanceCalls[0].newNext.Equal(day(2026, 7, 12)) {
		t.Fatalf("AdvanceRenewal calls = %+v, want one advancing to 2026-07-12", store.advanceCalls)
	}
	if len(store.markCalls) != 0 {
		t.Fatalf("past-due should not claim a reminder, got %+v", store.markCalls)
	}
}

func TestRunOnceEnqueueFailureSurfacesError(t *testing.T) {
	asOf := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	sub := testSub(t, day(2026, 7, 12), nil)
	store := &fakeStore{due: []*domain.Subscription{sub}, claimed: true}
	enq := &fakeEnqueuer{err: errors.New("outbox down")}

	count, err := mustScheduler(t, store, enq).RunOnce(context.Background(), asOf)
	if err == nil {
		t.Fatal("RunOnce error = nil, want the enqueue failure surfaced")
	}
	if count != 0 {
		t.Fatalf("RunOnce count = %d, want 0 on enqueue failure", count)
	}
	// The claim was still made (mark-first), so a re-run will not duplicate it.
	if len(store.markCalls) != 1 {
		t.Fatalf("MarkReminded calls = %d, want 1 (claim precedes enqueue)", len(store.markCalls))
	}
}

func TestRunOnceListErrorPropagates(t *testing.T) {
	store := &fakeStore{listErr: errors.New("db down")}
	_, err := mustScheduler(t, store, &fakeEnqueuer{}).RunOnce(context.Background(), time.Now())
	if err == nil {
		t.Fatal("RunOnce error = nil, want the list failure propagated")
	}
}

func TestRunOnceContinuesAfterPerSubscriptionError(t *testing.T) {
	asOf := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	// A past-due subscription whose AdvanceRenewal fails, followed by a healthy
	// upcoming subscription that should still be reminded.
	bad := testSub(t, day(2026, 6, 1), nil)
	good := testSub(t, day(2026, 7, 12), nil)
	store := &fakeStore{due: []*domain.Subscription{bad, good}, claimed: true, advanceErr: errors.New("advance failed")}
	enq := &fakeEnqueuer{}

	count, err := mustScheduler(t, store, enq).RunOnce(context.Background(), asOf)
	if err == nil {
		t.Fatal("RunOnce error = nil, want the per-subscription error recorded")
	}
	if count != 1 || len(enq.enqueued) != 1 {
		t.Fatalf("healthy subscription should still be reminded: count = %d, enqueued = %d", count, len(enq.enqueued))
	}
}

func TestNewRenewalSchedulerValidatesDeps(t *testing.T) {
	store := &fakeStore{}
	enq := &fakeEnqueuer{}
	log := discardLogger()
	rec := metrics.NopTickRecorder{}
	cases := []struct {
		name string
		fn   func() (*app.RenewalScheduler, error)
	}{
		{"nil store", func() (*app.RenewalScheduler, error) {
			return app.NewRenewalScheduler(nil, enq, log, rec, time.Hour, time.Minute)
		}},
		{"nil enqueuer", func() (*app.RenewalScheduler, error) {
			return app.NewRenewalScheduler(store, nil, log, rec, time.Hour, time.Minute)
		}},
		{"nil logger", func() (*app.RenewalScheduler, error) {
			return app.NewRenewalScheduler(store, enq, nil, rec, time.Hour, time.Minute)
		}},
		{"nil tick recorder", func() (*app.RenewalScheduler, error) {
			return app.NewRenewalScheduler(store, enq, log, nil, time.Hour, time.Minute)
		}},
		{"non-positive poll interval", func() (*app.RenewalScheduler, error) {
			return app.NewRenewalScheduler(store, enq, log, rec, 0, time.Minute)
		}},
		{"non-positive tick timeout", func() (*app.RenewalScheduler, error) {
			return app.NewRenewalScheduler(store, enq, log, rec, time.Hour, 0)
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

func TestRenewalScheduler_Run_FailingTickObservedWithError(t *testing.T) {
	store := &fakeStore{listErr: errors.New("db error")}
	spy := &spyTickRecorder{}
	s, err := app.NewRenewalScheduler(store, &fakeEnqueuer{}, discardLogger(), spy, 10*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatalf("NewRenewalScheduler: %v", err)
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

	if call.scheduler != metrics.SchedulerRenewal {
		t.Errorf("ObserveTick scheduler = %q, want %q", call.scheduler, metrics.SchedulerRenewal)
	}
	if call.err == nil {
		t.Error("ObserveTick err = nil, want the failing tick's error")
	}
}
