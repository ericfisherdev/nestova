package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// TestNewSyncMetricsRegistersOnRegistry verifies the constructor registers both
// counters on the given registerer.
func TestNewSyncMetricsRegistersOnRegistry(t *testing.T) {
	reg := metrics.NewRegistry()
	metrics.NewSyncMetrics(reg)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{
		"nestova_calendar_sync_events_total",
		"nestova_calendar_sync_account_errors_total",
	} {
		if !names[want] {
			t.Errorf("%s not registered on the provided registry", want)
		}
	}
}

// TestNewSyncMetricsNilRegistererPanics pins the platform convention of failing
// loudly at construction when a required dependency is missing.
func TestNewSyncMetricsNilRegistererPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewSyncMetrics(nil) did not panic")
		}
	}()
	metrics.NewSyncMetrics(nil)
}

// TestAddEventsSyncedAddsPositiveCountsOnly verifies positive counts accumulate
// and non-positive counts are ignored (a Counter panics on a negative add).
func TestAddEventsSyncedAddsPositiveCountsOnly(t *testing.T) {
	m := metrics.NewSyncMetrics(metrics.NewRegistry())

	m.AddEventsSynced(3)
	m.AddEventsSynced(0)
	m.AddEventsSynced(-5)
	m.AddEventsSynced(2)

	if got := testutil.ToFloat64(m.EventsTotal); got != 5 {
		t.Errorf("events_total = %v, want 5 (3+2; zero and negative ignored)", got)
	}
}

// TestIncAccountErrorIncrements verifies each call adds one failure.
func TestIncAccountErrorIncrements(t *testing.T) {
	m := metrics.NewSyncMetrics(metrics.NewRegistry())

	m.IncAccountError()
	m.IncAccountError()

	if got := testutil.ToFloat64(m.AccountErrorsTotal); got != 2 {
		t.Errorf("account_errors_total = %v, want 2", got)
	}
}
