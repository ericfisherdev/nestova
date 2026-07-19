package bootstrap

import (
	"context"
	"testing"

	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// TestNewEmailSender_DisabledReturnsNoop verifies the
// NOTIFY_EMAIL_ENABLED=false default builds a NoopEmailSender — zero AWS
// dependency for a deployment with email disabled.
func TestNewEmailSender_DisabledReturnsNoop(t *testing.T) {
	sender, err := NewEmailSender(context.Background(), config.EmailConfig{Enabled: false}, metrics.NopEmailRecorder{}, testLogger())
	if err != nil {
		t.Fatalf("NewEmailSender: %v", err)
	}
	if _, ok := sender.(*notifyadapter.NoopEmailSender); !ok {
		t.Errorf("NewEmailSender(disabled) = %T, want *notifyadapter.NoopEmailSender", sender)
	}
}

// TestNewEmailSender_EnabledPropagatesConstructionError verifies an
// enabled config with invalid AWS params fails NewEmailSender rather
// than silently falling back to Noop — construction never reaches the
// network for these cases, since SESEmailSender validates params before
// touching the SDK (see that constructor's own test coverage).
func TestNewEmailSender_EnabledPropagatesConstructionError(t *testing.T) {
	cfg := config.EmailConfig{
		Enabled:     true,
		Region:      "", // invalid: blank region
		FromAddress: "sender@example.com",
	}
	_, err := NewEmailSender(context.Background(), cfg, metrics.NopEmailRecorder{}, testLogger())
	if err == nil {
		t.Fatal("NewEmailSender(enabled, invalid params) error = nil, want non-nil")
	}
}

// TestNewEmailSender_EnabledWithNilRecorderErrors verifies a nil recorder
// is rejected at construction, not left to panic later inside
// instrumentedEmailSender.Send the first time email is actually used.
func TestNewEmailSender_EnabledWithNilRecorderErrors(t *testing.T) {
	cfg := config.EmailConfig{
		Enabled:     true,
		Region:      "us-east-1",
		FromAddress: "sender@example.com",
	}
	_, err := NewEmailSender(context.Background(), cfg, nil, testLogger())
	if err == nil {
		t.Fatal("NewEmailSender(enabled, nil recorder) error = nil, want non-nil")
	}
}

// TestNewEmailSender_EnabledReturnsInstrumentedSender verifies an
// enabled, validly-configured sender is wrapped in
// instrumentedEmailSender rather than returned bare.
func TestNewEmailSender_EnabledReturnsInstrumentedSender(t *testing.T) {
	cfg := config.EmailConfig{
		Enabled:     true,
		Region:      "us-east-1",
		FromAddress: "sender@example.com",
	}
	sender, err := NewEmailSender(context.Background(), cfg, &fakeEmailRecorder{}, testLogger())
	if err != nil {
		t.Fatalf("NewEmailSender: %v", err)
	}
	if _, ok := sender.(*instrumentedEmailSender); !ok {
		t.Errorf("NewEmailSender(enabled) = %T, want *instrumentedEmailSender", sender)
	}
}
