package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// TestNewSMSMetricsRegistersOnRegistry verifies the constructor registers
// the send counter on the given registerer. The vector is exercised with a
// zero-value observation first: vector families are lazy and appear in a
// gather only once at least one child series exists.
func TestNewSMSMetricsRegistersOnRegistry(t *testing.T) {
	reg := metrics.NewRegistry()
	m := metrics.NewSMSMetrics(reg)
	m.SendsTotal.WithLabelValues("sent").Add(0)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	if !names["nestova_sms_sends_total"] {
		t.Error("nestova_sms_sends_total not registered on the provided registry")
	}
	// FallbacksTotal is a plain Counter (not a vector), so it appears in a
	// gather immediately — no zero-value observation needed first.
	if !names["nestova_sms_fallbacks_total"] {
		t.Error("nestova_sms_fallbacks_total not registered on the provided registry")
	}
}

// TestNewSMSMetricsNilRegistererPanics pins the platform convention of
// failing loudly at construction when a required dependency is missing.
func TestNewSMSMetricsNilRegistererPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewSMSMetrics(nil) did not panic")
		}
	}()
	metrics.NewSMSMetrics(nil)
}

// TestSMSMetrics_IncrementsAreIndependentPerResult verifies each of
// IncSent/IncFailed/IncOptedOut increments its OWN "result" label series
// on SendsTotal without affecting the others.
func TestSMSMetrics_IncrementsAreIndependentPerResult(t *testing.T) {
	m := metrics.NewSMSMetrics(metrics.NewRegistry())

	m.IncSent()
	m.IncSent()
	m.IncFailed()
	m.IncOptedOut()
	m.IncOptedOut()
	m.IncOptedOut()

	if got := testutil.ToFloat64(m.SendsTotal.WithLabelValues("sent")); got != 2 {
		t.Errorf("sent = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.SendsTotal.WithLabelValues("failed")); got != 1 {
		t.Errorf("failed = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.SendsTotal.WithLabelValues("opted_out")); got != 3 {
		t.Errorf("opted_out = %v, want 3", got)
	}
}

// TestSMSMetrics_IncFallback_UsesADedicatedCounter_NotSendsTotal is the
// CodeRabbit round-2 regression test (major finding #2): IncFallback must
// increment its own FallbacksTotal counter, never a "fallback" value on
// SendsTotal — folding it in there would let sum(nestova_sms_sends_total)
// over-count real SMS send attempts by however many fell back.
func TestSMSMetrics_IncFallback_UsesADedicatedCounter_NotSendsTotal(t *testing.T) {
	m := metrics.NewSMSMetrics(metrics.NewRegistry())

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

// TestNopSMSRecorder_DoesNotPanic exercises every NopSMSRecorder method —
// they must all be safe, inert no-ops. A panic here fails the test on its
// own, so no assertions (and no use of *testing.T) are needed.
func TestNopSMSRecorder_DoesNotPanic(_ *testing.T) {
	var r metrics.NopSMSRecorder
	r.IncSent()
	r.IncFailed()
	r.IncOptedOut()
	r.IncFallback()
}
