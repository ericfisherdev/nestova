package bootstrap

import (
	"context"
	"testing"

	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// TestNewSMSSender_DisabledReturnsNoop verifies the NOTIFY_SMS_ENABLED=false
// default builds a NoopSMSSender — the AC that a disabled deployment has
// zero AWS dependency.
func TestNewSMSSender_DisabledReturnsNoop(t *testing.T) {
	sender, err := NewSMSSender(context.Background(), config.SMSConfig{Enabled: false}, metrics.NopSMSRecorder{}, testLogger())
	if err != nil {
		t.Fatalf("NewSMSSender: %v", err)
	}
	if _, ok := sender.(*notifyadapter.NoopSMSSender); !ok {
		t.Errorf("NewSMSSender(disabled) = %T, want *notifyadapter.NoopSMSSender", sender)
	}
}

// TestNewSMSSender_EnabledPropagatesConstructionError verifies an enabled
// config with invalid AWS params fails NewSMSSender rather than silently
// falling back to Noop — construction never reaches the network for these
// cases, since AWSEndUserMessagingSender validates params before touching
// the SDK (see that constructor's own test coverage).
func TestNewSMSSender_EnabledPropagatesConstructionError(t *testing.T) {
	cfg := config.SMSConfig{
		Enabled:             true,
		Region:              "", // invalid: blank region
		OriginationIdentity: "+15551234567",
		RetryMaxAttempts:    3,
	}
	_, err := NewSMSSender(context.Background(), cfg, metrics.NopSMSRecorder{}, testLogger())
	if err == nil {
		t.Fatal("NewSMSSender(enabled, invalid params) error = nil, want non-nil")
	}
}

// TestNewSMSSender_EnabledWithNilRecorderErrors verifies a nil recorder is
// rejected at construction, not left to panic later inside
// instrumentedSMSSender.Send the first time SMS is actually used.
func TestNewSMSSender_EnabledWithNilRecorderErrors(t *testing.T) {
	cfg := config.SMSConfig{
		Enabled:             true,
		Region:              "us-east-1",
		OriginationIdentity: "+15551234567",
		RetryMaxAttempts:    3,
	}
	_, err := NewSMSSender(context.Background(), cfg, nil, testLogger())
	if err == nil {
		t.Fatal("NewSMSSender(enabled, nil recorder) error = nil, want non-nil")
	}
}

// TestNewSMSSender_EnabledReturnsInstrumentedSender verifies an enabled,
// validly-configured sender is wrapped in instrumentedSMSSender rather than
// returned bare.
func TestNewSMSSender_EnabledReturnsInstrumentedSender(t *testing.T) {
	cfg := config.SMSConfig{
		Enabled:             true,
		Region:              "us-east-1",
		OriginationIdentity: "+15551234567",
		RetryMaxAttempts:    3,
	}
	sender, err := NewSMSSender(context.Background(), cfg, &fakeSMSRecorder{}, testLogger())
	if err != nil {
		t.Fatalf("NewSMSSender: %v", err)
	}
	if _, ok := sender.(*instrumentedSMSSender); !ok {
		t.Errorf("NewSMSSender(enabled) = %T, want *instrumentedSMSSender", sender)
	}
}
