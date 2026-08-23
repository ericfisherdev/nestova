package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	ncmetrics "github.com/ericfisherdev/nestcore/metrics"
)

// SchedulerName identifies a background scheduler in the tick metrics'
// scheduler label. It is a distinct type (not a bare string) so callers must
// deliberately mint a value rather than accidentally passing arbitrary text;
// combined with PromTickRecorder collapsing unknown names to "other", the
// label's cardinality stays bounded even against misuse.
type SchedulerName = ncmetrics.SchedulerName

// Canonical scheduler names for the TickRecorder's scheduler label. Every
// background loop passes exactly one of these constants; keeping the full set
// here (next to the port) bounds the label's cardinality to five known values
// and makes any new scheduler an explicit, reviewable addition (it must also
// be added to knownSchedulers below).
const (
	// SchedulerDispatcher is the notification outbox dispatcher (NES-24).
	SchedulerDispatcher SchedulerName = "dispatcher"
	// SchedulerTasks is the task generation + overdue-sweep scheduler (NES-31).
	SchedulerTasks SchedulerName = "task_scheduler"
	// SchedulerRestock is the restock prediction scheduler (NES-44).
	SchedulerRestock SchedulerName = "restock"
	// SchedulerRenewal is the subscription renewal scheduler (NES-65).
	SchedulerRenewal SchedulerName = "renewal"
	// SchedulerCalendarSync is the calendar sync scheduler (NES-68).
	SchedulerCalendarSync SchedulerName = "calendar_sync"
)

// knownSchedulers is the canonical allowlist passed to nestcore's
// NewPromTickRecorder; a name outside this set collapses to a fixed "other"
// label so a misbehaving caller cannot mint unbounded series.
var knownSchedulers = []SchedulerName{
	SchedulerDispatcher, SchedulerTasks, SchedulerRestock, SchedulerRenewal, SchedulerCalendarSync,
}

// TickRecorder is the minimal port (ISP) a background scheduler records one
// completed poll cycle through: how long the cycle took and whether it failed.
// name must be one of the Scheduler* constants above so the label set stays
// bounded; implementations collapse anything else to a fixed fallback value.
// Implementations must be safe for concurrent use — the five schedulers each
// run in their own goroutine.
type TickRecorder = ncmetrics.TickRecorder

// PromTickRecorder is the Prometheus-backed TickRecorder. The fields are
// exported so tests can assert on them with prometheus/testutil, but
// construction always goes through NewPromTickRecorder so every instance is
// registered.
type PromTickRecorder = ncmetrics.PromTickRecorder

// NewPromTickRecorder constructs the scheduler tick metrics and registers them
// on reg. It panics when reg is nil (matching the platform convention of
// failing loudly at construction for required dependencies) and when a metric
// with the same name is already registered, so a double-wired registry
// surfaces at boot rather than as silently shared counters.
func NewPromTickRecorder(reg prometheus.Registerer) *PromTickRecorder {
	return ncmetrics.NewPromTickRecorder(reg, namespace, knownSchedulers)
}

// NopTickRecorder is a no-op TickRecorder for tests and optional wiring where
// tick instrumentation is irrelevant.
type NopTickRecorder = ncmetrics.NopTickRecorder
