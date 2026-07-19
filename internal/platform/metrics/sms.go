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
// attempt's outcome through (NES-138) — IncSent/IncFailed/IncOptedOut are
// MUTUALLY EXCLUSIVE results per Send call, mirroring TickRecorder's own
// single-event/categorical-outcome shape (ObserveTick's "result" label)
// rather than SyncRecorder's separate-counters style, which fits distinct
// kinds of events rather than one event's alternative outcomes.
// IncFallback is a DIFFERENT kind of event (see its own doc) and is
// recorded through its own, separate counter — see NewSMSMetrics's own
// doc for why (CodeRabbit PR NES-141 round 2, major finding #2).
// Implementations must be safe for concurrent use.
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
	// IncFallback records that the dispatcher ATTEMPTED to re-enqueue a
	// terminal SMS failure's content to the in-app channel instead of
	// losing it entirely (NES-139, Dispatcher.fallbackToInApp). This is
	// an attempt count, not a confirmed-delivery count: the call happens
	// before the fallback's own outbox.Enqueue runs, and that enqueue can
	// itself fail — a rare, separately-logged condition this counter does
	// not distinguish from a successful fallback enqueue. It is NOT one
	// of the mutually-exclusive Send-outcome results above: a fallback is
	// an EFFECT of a terminal send failure (already counted once via
	// IncFailed/IncOptedOut for the SMS attempt itself), recorded on its
	// own counter rather than as another "result" value on the same
	// series family, so summing nestova_sms_sends_total across every
	// result still yields "total SMS send attempts" without a fallback
	// (which is not an SMS send at all — it is an in-app re-enqueue)
	// inflating that total.
	IncFallback()
}

// SMSMetrics is the Prometheus-backed SMSRecorder. The fields are exported
// so tests can assert on them with prometheus/testutil, but construction
// always goes through NewSMSMetrics so every instance is registered;
// consumers record through the SMSRecorder methods.
type SMSMetrics struct {
	// SendsTotal counts SMS send attempts, labelled by result (sent,
	// failed, opted_out) — the outcome of an actual attempt to reach the
	// SMS provider, nothing else.
	SendsTotal *prometheus.CounterVec
	// FallbacksTotal counts terminal-SMS-failure fallback-to-in-app
	// ATTEMPTS (see IncFallback's own doc) — a separate counter, not a
	// "fallback" value on SendsTotal, since a fallback is not itself an
	// SMS send: folding it into SendsTotal would let
	// sum(nestova_sms_sends_total) over-count real SMS send attempts by
	// however many of them fell back (CodeRabbit PR NES-141 round 2,
	// major finding #2).
	FallbacksTotal prometheus.Counter
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
		FallbacksTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "nestova_sms_fallbacks_total",
			Help: "Total number of terminal SMS failures for which the dispatcher attempted an in-app fallback.",
		}),
	}
}

// IncSent increments the sent-result counter.
func (m *SMSMetrics) IncSent() { m.SendsTotal.WithLabelValues(smsResultSent).Inc() }

// IncFailed increments the failed-result counter.
func (m *SMSMetrics) IncFailed() { m.SendsTotal.WithLabelValues(smsResultFailed).Inc() }

// IncOptedOut increments the opted-out-result counter.
func (m *SMSMetrics) IncOptedOut() { m.SendsTotal.WithLabelValues(smsResultOptedOut).Inc() }

// IncFallback increments the dedicated fallback counter (see
// FallbacksTotal's own doc) — NOT a "fallback" value on SendsTotal.
func (m *SMSMetrics) IncFallback() { m.FallbacksTotal.Inc() }

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

// IncFallback discards the observation.
func (NopSMSRecorder) IncFallback() {}
