package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// SyncRecorder is the minimal port (ISP) the calendar sync engine records
// through: events applied to the external-event cache and per-account sync
// failures. It exists (rather than the sync service holding the counters
// directly) so prometheus imports stay confined to this package and tests can
// substitute a spy. Implementations must be safe for concurrent use.
type SyncRecorder interface {
	AddEventsSynced(count int)
	IncAccountError()
}

// SyncMetrics is the Prometheus-backed SyncRecorder for the calendar sync
// engine. The fields are exported so tests can assert on them with
// prometheus/testutil, but construction always goes through NewSyncMetrics so
// every instance is registered; consumers record through the SyncRecorder
// methods.
type SyncMetrics struct {
	// EventsTotal counts external calendar events applied to the cache
	// (upserts and deletes) across all accounts.
	EventsTotal prometheus.Counter
	// AccountErrorsTotal counts per-account sync failures. Each failed account
	// in a sync pass increments it once.
	AccountErrorsTotal prometheus.Counter
}

// Compile-time check that the Prometheus metrics satisfy the port.
var _ SyncRecorder = (*SyncMetrics)(nil)

// NewSyncMetrics constructs the calendar sync metrics and registers them on
// reg. It panics when reg is nil (matching the platform convention of failing
// loudly at construction for required dependencies) and when a metric with the
// same name is already registered, so a double-wired registry surfaces at boot
// rather than as silently shared counters.
func NewSyncMetrics(reg prometheus.Registerer) *SyncMetrics {
	if reg == nil {
		panic("metrics: NewSyncMetrics requires a non-nil registerer")
	}
	factory := promauto.With(reg)
	return &SyncMetrics{
		EventsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "nestova_calendar_sync_events_total",
			Help: "Total number of external calendar events applied to the cache (upserts and deletes).",
		}),
		AccountErrorsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "nestova_calendar_sync_account_errors_total",
			Help: "Total number of per-account calendar sync failures.",
		}),
	}
}

// AddEventsSynced adds count to the synced-events counter. Non-positive counts
// are ignored: zero-event passes carry no signal, and a Counter panics on a
// negative add.
func (m *SyncMetrics) AddEventsSynced(count int) {
	if count <= 0 {
		return
	}
	m.EventsTotal.Add(float64(count))
}

// IncAccountError increments the per-account sync failure counter.
func (m *SyncMetrics) IncAccountError() {
	m.AccountErrorsTotal.Inc()
}

// NopSyncRecorder is a no-op SyncRecorder for tests and optional wiring where
// sync instrumentation is irrelevant.
type NopSyncRecorder struct{}

// Compile-time check that the no-op recorder satisfies the port.
var _ SyncRecorder = NopSyncRecorder{}

// AddEventsSynced discards the observation.
func (NopSyncRecorder) AddEventsSynced(int) {}

// IncAccountError discards the observation.
func (NopSyncRecorder) IncAccountError() {}
