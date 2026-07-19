package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// TestNewEmailMetricsRegistersOnRegistry verifies the constructor
// registers the send counter on the given registerer. The vector is
// exercised with a zero-value observation first: vector families are
// lazy and appear in a gather only once at least one child series
// exists.
func TestNewEmailMetricsRegistersOnRegistry(t *testing.T) {
	reg := metrics.NewRegistry()
	m := metrics.NewEmailMetrics(reg)
	m.SendsTotal.WithLabelValues("sent").Add(0)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	if !names["nestova_email_sends_total"] {
		t.Error("nestova_email_sends_total not registered on the provided registry")
	}
	// FallbacksTotal is a plain Counter (not a vector), so it appears in a
	// gather immediately — no zero-value observation needed first.
	if !names["nestova_email_fallbacks_total"] {
		t.Error("nestova_email_fallbacks_total not registered on the provided registry")
	}
}

// TestNewEmailMetricsNilRegistererPanics pins the platform convention of
// failing loudly at construction when a required dependency is missing.
func TestNewEmailMetricsNilRegistererPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewEmailMetrics(nil) did not panic")
		}
	}()
	metrics.NewEmailMetrics(nil)
}

// TestEmailMetrics_IncrementsAreIndependentPerResult verifies each of
// IncSent/IncFailed/IncRejected increments its OWN "result" label series
// on SendsTotal without affecting the others.
func TestEmailMetrics_IncrementsAreIndependentPerResult(t *testing.T) {
	m := metrics.NewEmailMetrics(metrics.NewRegistry())

	m.IncSent()
	m.IncSent()
	m.IncFailed()
	m.IncRejected()
	m.IncRejected()
	m.IncRejected()

	if got := testutil.ToFloat64(m.SendsTotal.WithLabelValues("sent")); got != 2 {
		t.Errorf("sent = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.SendsTotal.WithLabelValues("failed")); got != 1 {
		t.Errorf("failed = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.SendsTotal.WithLabelValues("rejected")); got != 3 {
		t.Errorf("rejected = %v, want 3", got)
	}
}

// TestEmailMetrics_IncFallback_UsesADedicatedCounter_NotSendsTotal is the
// CodeRabbit round-2 regression test (major finding #2): IncFallback must
// increment its own FallbacksTotal counter, never a "fallback" value on
// SendsTotal — folding it in there would let sum(nestova_email_sends_total)
// over-count real email send attempts by however many fell back.
func TestEmailMetrics_IncFallback_UsesADedicatedCounter_NotSendsTotal(t *testing.T) {
	m := metrics.NewEmailMetrics(metrics.NewRegistry())

	m.IncSent()
	m.IncFallback()
	m.IncFallback()
	m.IncFallback()
	m.IncFallback()

	if got := testutil.ToFloat64(m.FallbacksTotal); got != 4 {
		t.Errorf("FallbacksTotal = %v, want 4", got)
	}
	// SendsTotal must reflect ONLY the real send attempt (IncSent above),
	// not the four fallback attempts.
	if got := testutil.ToFloat64(m.SendsTotal.WithLabelValues("sent")); got != 1 {
		t.Errorf("SendsTotal{result=sent} = %v, want 1 (unaffected by IncFallback)", got)
	}
}

// TestNopEmailRecorder_DoesNotPanic exercises every NopEmailRecorder
// method — they must all be safe, inert no-ops. A panic here fails the
// test on its own, so no assertions (and no use of *testing.T) are
// needed.
func TestNopEmailRecorder_DoesNotPanic(_ *testing.T) {
	var r metrics.NopEmailRecorder
	r.IncSent()
	r.IncFailed()
	r.IncRejected()
	r.IncFallback()
}

// TestSMSMetricsAndEmailMetrics_SatisfyFallbackRecorder confirms both
// concrete recorder types satisfy the shared, channel-agnostic
// FallbackRecorder port Dispatcher uses (NES-141) — a compile-time
// assertion made explicit in a test so a future signature change to
// either recorder's IncFallback surfaces here immediately.
func TestSMSMetricsAndEmailMetrics_SatisfyFallbackRecorder(_ *testing.T) {
	var _ metrics.FallbackRecorder = metrics.NewSMSMetrics(metrics.NewRegistry())
	var _ metrics.FallbackRecorder = metrics.NewEmailMetrics(metrics.NewRegistry())
	var _ metrics.FallbackRecorder = metrics.NopSMSRecorder{}
	var _ metrics.FallbackRecorder = metrics.NopEmailRecorder{}
}
