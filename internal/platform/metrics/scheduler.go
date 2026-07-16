package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Canonical scheduler names for the TickRecorder's scheduler label. Every
// background loop passes exactly one of these constants; keeping the full set
// here (next to the port) bounds the label's cardinality to five known values
// and makes any new scheduler an explicit, reviewable addition.
const (
	// SchedulerDispatcher is the notification outbox dispatcher (NES-24).
	SchedulerDispatcher = "dispatcher"
	// SchedulerTasks is the task generation + overdue-sweep scheduler (NES-31).
	SchedulerTasks = "task_scheduler"
	// SchedulerRestock is the restock prediction scheduler (NES-44).
	SchedulerRestock = "restock"
	// SchedulerRenewal is the subscription renewal scheduler (NES-65).
	SchedulerRenewal = "renewal"
	// SchedulerCalendarSync is the calendar sync scheduler (NES-68).
	SchedulerCalendarSync = "calendar_sync"
)

// TickRecorder is the minimal port (ISP) a background scheduler records one
// completed poll cycle through: how long the cycle took and whether it failed.
// scheduler must be one of the Scheduler* constants above so the label set
// stays bounded. Implementations must be safe for concurrent use — the five
// schedulers each run in their own goroutine.
type TickRecorder interface {
	ObserveTick(scheduler string, d time.Duration, err error)
}

// Values for the tick counter's result label.
const (
	tickResultSuccess = "success"
	tickResultError   = "error"
)

// PromTickRecorder is the Prometheus-backed TickRecorder. The fields are
// exported so tests can assert on them with prometheus/testutil, but
// construction always goes through NewPromTickRecorder so every instance is
// registered.
type PromTickRecorder struct {
	// TicksTotal counts completed scheduler cycles, labelled by scheduler name
	// and result ("success" or "error").
	TicksTotal *prometheus.CounterVec
	// TickDuration observes cycle duration in seconds, labelled by scheduler
	// name. Result is intentionally omitted to bound the histogram's series
	// count (each series carries a full bucket set).
	TickDuration *prometheus.HistogramVec
	// LastSuccess gauges the Unix timestamp of each scheduler's most recent
	// successful cycle; a failing cycle leaves it untouched, so a stale value
	// signals a scheduler that has stopped succeeding.
	LastSuccess *prometheus.GaugeVec
}

// Compile-time check that the Prometheus recorder satisfies the port.
var _ TickRecorder = (*PromTickRecorder)(nil)

// NewPromTickRecorder constructs the scheduler tick metrics and registers them
// on reg. It panics when reg is nil (matching the platform convention of
// failing loudly at construction for required dependencies) and when a metric
// with the same name is already registered, so a double-wired registry
// surfaces at boot rather than as silently shared counters.
func NewPromTickRecorder(reg prometheus.Registerer) *PromTickRecorder {
	if reg == nil {
		panic("metrics: NewPromTickRecorder requires a non-nil registerer")
	}
	factory := promauto.With(reg)
	return &PromTickRecorder{
		TicksTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "nestova_scheduler_ticks_total",
			Help: "Total number of completed background scheduler cycles, by scheduler and result.",
		}, []string{"scheduler", "result"}),
		TickDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nestova_scheduler_tick_duration_seconds",
			Help:    "Background scheduler cycle duration in seconds, by scheduler.",
			Buckets: prometheus.DefBuckets,
		}, []string{"scheduler"}),
		LastSuccess: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nestova_scheduler_last_success_timestamp_seconds",
			Help: "Unix timestamp of the most recent successful cycle, by scheduler.",
		}, []string{"scheduler"}),
	}
}

// ObserveTick records one completed cycle: it increments the tick counter with
// the outcome derived from err, observes the cycle duration, and — on success
// only — moves the scheduler's last-success timestamp to now, so a failing
// scheduler's staleness is visible.
func (r *PromTickRecorder) ObserveTick(scheduler string, d time.Duration, err error) {
	result := tickResultSuccess
	if err != nil {
		result = tickResultError
	}
	r.TicksTotal.WithLabelValues(scheduler, result).Inc()
	r.TickDuration.WithLabelValues(scheduler).Observe(d.Seconds())
	if err == nil {
		r.LastSuccess.WithLabelValues(scheduler).SetToCurrentTime()
	}
}

// NopTickRecorder is a no-op TickRecorder for tests and optional wiring where
// tick instrumentation is irrelevant.
type NopTickRecorder struct{}

// Compile-time check that the no-op recorder satisfies the port.
var _ TickRecorder = NopTickRecorder{}

// ObserveTick discards the observation.
func (NopTickRecorder) ObserveTick(string, time.Duration, error) {}
