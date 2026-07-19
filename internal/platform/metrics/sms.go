package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Values for the SMS send counter's result label.
const (
	smsResultSent     = "sent"
	smsResultFailed   = "failed"
	smsResultOptedOut = "opted_out"
)

// SMSRecorder is the minimal port (ISP) an SMS sender records each send
// attempt's outcome through (NES-138) — one of three MUTUALLY EXCLUSIVE
// results per Send call, mirroring TickRecorder's own single-event/
// categorical-outcome shape (ObserveTick's "result" label) rather than
// SyncRecorder's separate-counters style, which fits distinct kinds of
// events rather than one event's alternative outcomes. Implementations
// must be safe for concurrent use.
type SMSRecorder interface {
	// IncSent records a successful send.
	IncSent()
	// IncFailed records a send that failed for any reason OTHER than the
	// recipient having opted out (see IncOptedOut).
	IncFailed()
	// IncOptedOut records a send rejected because the destination has
	// opted out of SMS — tracked separately from IncFailed since an
	// opt-out is an expected, non-retryable outcome, not a delivery
	// failure worth alerting on the same way a provider error is.
	IncOptedOut()
}

// SMSMetrics is the Prometheus-backed SMSRecorder. The field is exported so
// tests can assert on it with prometheus/testutil, but construction always
// goes through NewSMSMetrics so every instance is registered; consumers
// record through the SMSRecorder methods.
type SMSMetrics struct {
	// SendsTotal counts SMS send attempts, labelled by result (sent,
	// failed, opted_out).
	SendsTotal *prometheus.CounterVec
}

// Compile-time check that the Prometheus metrics satisfy the port.
var _ SMSRecorder = (*SMSMetrics)(nil)

// NewSMSMetrics constructs the SMS send metrics and registers them on reg.
// It panics when reg is nil (matching the platform convention of failing
// loudly at construction for required dependencies) and when a metric with
// the same name is already registered, so a double-wired registry surfaces
// at boot rather than as silently shared counters.
func NewSMSMetrics(reg prometheus.Registerer) *SMSMetrics {
	if reg == nil {
		panic("metrics: NewSMSMetrics requires a non-nil registerer")
	}
	factory := promauto.With(reg)
	return &SMSMetrics{
		SendsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "nestova_sms_sends_total",
			Help: "Total number of SMS send attempts, by result (sent, failed, opted_out).",
		}, []string{"result"}),
	}
}

// IncSent increments the sent-result counter.
func (m *SMSMetrics) IncSent() { m.SendsTotal.WithLabelValues(smsResultSent).Inc() }

// IncFailed increments the failed-result counter.
func (m *SMSMetrics) IncFailed() { m.SendsTotal.WithLabelValues(smsResultFailed).Inc() }

// IncOptedOut increments the opted-out-result counter.
func (m *SMSMetrics) IncOptedOut() { m.SendsTotal.WithLabelValues(smsResultOptedOut).Inc() }

// NopSMSRecorder is a no-op SMSRecorder for tests and optional wiring where
// SMS instrumentation is irrelevant.
type NopSMSRecorder struct{}

// Compile-time check that the no-op recorder satisfies the port.
var _ SMSRecorder = NopSMSRecorder{}

// IncSent discards the observation.
func (NopSMSRecorder) IncSent() {}

// IncFailed discards the observation.
func (NopSMSRecorder) IncFailed() {}

// IncOptedOut discards the observation.
func (NopSMSRecorder) IncOptedOut() {}
