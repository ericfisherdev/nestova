package metrics_test

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// TestNewPromTickRecorderRegistersOnRegistry verifies the constructor registers
// all three metrics on the given registerer. The vectors are exercised with one
// observation first: vector families are lazy and appear in a gather only once
// at least one child series exists.
func TestNewPromTickRecorderRegistersOnRegistry(t *testing.T) {
	reg := metrics.NewRegistry()
	r := metrics.NewPromTickRecorder(reg)
	r.ObserveTick(metrics.SchedulerDispatcher, time.Millisecond, nil)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{
		"nestova_scheduler_ticks_total",
		"nestova_scheduler_tick_duration_seconds",
		"nestova_scheduler_last_success_timestamp_seconds",
	} {
		if !names[want] {
			t.Errorf("%s not registered on the provided registry", want)
		}
	}
}

// TestNewPromTickRecorderNilRegistererPanics pins the platform convention of
// failing loudly at construction when a required dependency is missing.
func TestNewPromTickRecorderNilRegistererPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewPromTickRecorder(nil) did not panic")
		}
	}()
	metrics.NewPromTickRecorder(nil)
}

// TestObserveTickErrorCountsErrorAndKeepsLastSuccess is the canonical
// failing-tick behaviour check, asserted once here for all schedulers (the
// per-scheduler tests use a spy to prove ObserveTick is called with the tick's
// error): an error increments ticks_total{result="error"}, observes the
// duration, and does NOT move the last-success timestamp — neither on a
// first-ever failure (no series is created) nor on a failure that follows a
// success (the recorded timestamp stays put).
func TestObserveTickErrorCountsErrorAndKeepsLastSuccess(t *testing.T) {
	r := metrics.NewPromTickRecorder(metrics.NewRegistry())

	r.ObserveTick(metrics.SchedulerTasks, 50*time.Millisecond, errors.New("db down"))

	if got := testutil.ToFloat64(r.TicksTotal.WithLabelValues(string(metrics.SchedulerTasks), "error")); got != 1 {
		t.Errorf(`ticks_total{result="error"} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(r.TicksTotal.WithLabelValues(string(metrics.SchedulerTasks), "success")); got != 0 {
		t.Errorf(`ticks_total{result="success"} = %v, want 0`, got)
	}
	if got := testutil.CollectAndCount(r.TickDuration); got != 1 {
		t.Errorf("tick_duration series count = %d, want 1", got)
	}
	// A first-ever failing tick must not create a last-success series.
	if got := testutil.CollectAndCount(r.LastSuccess); got != 0 {
		t.Errorf("last_success series count after a failing tick = %d, want 0", got)
	}

	// A failure AFTER a prior success must leave the recorded timestamp
	// untouched — this catches an implementation that overwrites the gauge on
	// every tick regardless of outcome. Pin the gauge to a known value first so
	// the comparison cannot be fooled by two SetToCurrentTime calls landing on
	// the same clock reading.
	r.ObserveTick(metrics.SchedulerTasks, 50*time.Millisecond, nil)
	const pinnedLastSuccess = 12345.0
	r.LastSuccess.WithLabelValues(string(metrics.SchedulerTasks)).Set(pinnedLastSuccess)

	r.ObserveTick(metrics.SchedulerTasks, 50*time.Millisecond, errors.New("db down again"))

	if got := testutil.ToFloat64(r.LastSuccess.WithLabelValues(string(metrics.SchedulerTasks))); got != pinnedLastSuccess {
		t.Errorf("last_success after a failure following a success = %v, want unchanged %v", got, pinnedLastSuccess)
	}
	if got := testutil.ToFloat64(r.TicksTotal.WithLabelValues(string(metrics.SchedulerTasks), "error")); got != 2 {
		t.Errorf(`ticks_total{result="error"} after second failure = %v, want 2`, got)
	}
}

// TestObserveTickUnknownSchedulerCollapsesToOther verifies the cardinality
// guard: a SchedulerName outside the canonical set must land in the fixed
// "other" series rather than minting a new label value.
func TestObserveTickUnknownSchedulerCollapsesToOther(t *testing.T) {
	r := metrics.NewPromTickRecorder(metrics.NewRegistry())

	r.ObserveTick(metrics.SchedulerName("rogue"), time.Millisecond, nil)

	// Exactly one counter series must exist — the "other" one. Asserting the
	// series count (rather than reading a "rogue" child, which would itself
	// instantiate that series) proves no rogue label value was minted.
	if got := testutil.CollectAndCount(r.TicksTotal); got != 1 {
		t.Errorf("ticks_total series count = %d, want 1 (only the collapsed 'other' series)", got)
	}
	if got := testutil.ToFloat64(r.TicksTotal.WithLabelValues("other", "success")); got != 1 {
		t.Errorf(`ticks_total{scheduler="other",result="success"} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(r.LastSuccess.WithLabelValues("other")); got <= 0 {
		t.Errorf(`last_success{scheduler="other"} = %v, want a positive Unix timestamp`, got)
	}
}

// TestObserveTickSuccessCountsSuccessAndMovesLastSuccess verifies the success
// path: ticks_total{result="success"} increments and the last-success gauge is
// set to (approximately) now.
func TestObserveTickSuccessCountsSuccessAndMovesLastSuccess(t *testing.T) {
	r := metrics.NewPromTickRecorder(metrics.NewRegistry())

	before := float64(time.Now().Add(-time.Second).Unix())
	r.ObserveTick(metrics.SchedulerRenewal, 50*time.Millisecond, nil)

	if got := testutil.ToFloat64(r.TicksTotal.WithLabelValues(string(metrics.SchedulerRenewal), "success")); got != 1 {
		t.Errorf(`ticks_total{result="success"} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(r.LastSuccess.WithLabelValues(string(metrics.SchedulerRenewal))); got < before {
		t.Errorf("last_success = %v, want a recent Unix timestamp (>= %v)", got, before)
	}
}
