package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Values for the email send counter's result label.
const (
	emailResultSent     = "sent"
	emailResultFailed   = "failed"
	emailResultRejected = "rejected"
)

// EmailRecorder is the minimal port (ISP) an email sender records each
// send attempt's outcome through (NES-141) — IncSent/IncFailed/IncRejected
// are MUTUALLY EXCLUSIVE results per Send call, mirroring SMSRecorder's
// identical single-event/categorical-outcome shape. IncFallback is a
// DIFFERENT kind of event, recorded through its own counter — see
// NewEmailMetrics's own doc for why (CodeRabbit PR NES-141 round 2, major
// finding #2). Implementations must be safe for concurrent use.
type EmailRecorder interface {
	// IncSent records a successful send.
	IncSent()
	// IncFailed records a send that failed for any reason OTHER than the
	// recipient being rejected by the provider (see IncRejected).
	IncFailed()
	// IncRejected records a send the provider refused outright (in this
	// deployment's Amazon SES sandbox scope, overwhelmingly an
	// unverified recipient address) — tracked separately from IncFailed
	// since a sandbox rejection is an expected, non-retryable outcome
	// while the deployment stays in sandbox mode, not a delivery failure
	// worth alerting on the same way a provider error is.
	IncRejected()
	// IncFallback records that the dispatcher ATTEMPTED to re-enqueue a
	// terminal email failure's content to the in-app channel instead of
	// losing it entirely (NES-141, Dispatcher.fallbackToInApp) — see
	// SMSRecorder.IncFallback's own doc for the identical attempt-vs-
	// confirmed-delivery distinction AND for why this is NOT one of the
	// mutually-exclusive Send-outcome results above, both of which apply
	// here unchanged.
	IncFallback()
}

// EmailMetrics is the Prometheus-backed EmailRecorder. The fields are
// exported so tests can assert on them with prometheus/testutil, but
// construction always goes through NewEmailMetrics so every instance is
// registered; consumers record through the EmailRecorder methods.
type EmailMetrics struct {
	// SendsTotal counts email send attempts, labelled by result (sent,
	// failed, rejected) — the outcome of an actual attempt to reach SES,
	// nothing else.
	SendsTotal *prometheus.CounterVec
	// FallbacksTotal counts terminal-email-failure fallback-to-in-app
	// ATTEMPTS (see IncFallback's own doc) — a separate counter, not a
	// "fallback" value on SendsTotal, mirroring SMSMetrics.FallbacksTotal's
	// identical reasoning: folding it into SendsTotal would let
	// sum(nestova_email_sends_total) over-count real email send attempts
	// by however many of them fell back (CodeRabbit PR NES-141 round 2,
	// major finding #2).
	FallbacksTotal prometheus.Counter
}

// Compile-time check that the Prometheus metrics satisfy the port.
var _ EmailRecorder = (*EmailMetrics)(nil)

// NewEmailMetrics constructs the email send metrics and registers them on
// reg. It panics when reg is nil (matching the platform convention of
// failing loudly at construction for required dependencies) and when a
// metric with the same name is already registered, so a double-wired
// registry surfaces at boot rather than as silently shared counters.
func NewEmailMetrics(reg prometheus.Registerer) *EmailMetrics {
	if reg == nil {
		panic("metrics: NewEmailMetrics requires a non-nil registerer")
	}
	factory := promauto.With(reg)
	return &EmailMetrics{
		SendsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "nestova_email_sends_total",
			Help: "Total number of email send attempts, by result (sent, failed, rejected).",
		}, []string{"result"}),
		FallbacksTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "nestova_email_fallbacks_total",
			Help: "Total number of terminal email failures for which the dispatcher attempted an in-app fallback.",
		}),
	}
}

// IncSent increments the sent-result counter.
func (m *EmailMetrics) IncSent() { m.SendsTotal.WithLabelValues(emailResultSent).Inc() }

// IncFailed increments the failed-result counter.
func (m *EmailMetrics) IncFailed() { m.SendsTotal.WithLabelValues(emailResultFailed).Inc() }

// IncRejected increments the rejected-result counter.
func (m *EmailMetrics) IncRejected() { m.SendsTotal.WithLabelValues(emailResultRejected).Inc() }

// IncFallback increments the dedicated fallback counter (see
// FallbacksTotal's own doc) — NOT a "fallback" value on SendsTotal.
func (m *EmailMetrics) IncFallback() { m.FallbacksTotal.Inc() }

// NopEmailRecorder is a no-op EmailRecorder for tests and optional wiring
// where email instrumentation is irrelevant.
type NopEmailRecorder struct{}

// Compile-time check that the no-op recorder satisfies the port.
var _ EmailRecorder = NopEmailRecorder{}

// IncSent discards the observation.
func (NopEmailRecorder) IncSent() {}

// IncFailed discards the observation.
func (NopEmailRecorder) IncFailed() {}

// IncRejected discards the observation.
func (NopEmailRecorder) IncRejected() {}

// IncFallback discards the observation.
func (NopEmailRecorder) IncFallback() {}

// FallbackRecorder is the narrow, channel-agnostic port Dispatcher uses
// to record a terminal channel failure's fallback-to-in-app ATTEMPT
// (NES-139, generalized to email in NES-141) — both SMSRecorder and
// EmailRecorder satisfy this structurally (each already declares its own
// IncFallback above), so Dispatcher can record a fallback attempt for
// whichever channel failed without depending on either recorder's full,
// channel-specific interface (ISP).
type FallbackRecorder interface {
	IncFallback()
}
