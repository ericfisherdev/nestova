package adapter

import (
	"context"
	"log/slog"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// NoopSMSSender is the domain.SMSSender used when NOTIFY_SMS_ENABLED=false
// (NES-138) — the safe zero-config default, mirroring
// media/adapter.LocalPhotoStore's own "always available, no external
// dependency" role for photo storage. It logs the delivery attempt and
// returns a synthetic message id with no external side effect: a
// deployment with SMS disabled must run with ZERO AWS dependency (NES-138
// AC), so this type imports nothing from aws-sdk-go-v2 at all.
type NoopSMSSender struct {
	logger *slog.Logger
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.SMSSender = (*NoopSMSSender)(nil)

// NewNoopSMSSender constructs a NoopSMSSender with an injected logger.
func NewNoopSMSSender(logger *slog.Logger) *NoopSMSSender {
	if logger == nil {
		panic("adapter: NewNoopSMSSender requires a non-nil logger")
	}
	return &NoopSMSSender{logger: logger}
}

// Send logs the would-be delivery and returns a fixed synthetic message id
// with no external side effect. No PII (the destination number, the body)
// is logged; only the body's length is recorded, mirroring InAppSender's
// own no-PII-in-logs convention.
func (s *NoopSMSSender) Send(ctx context.Context, _ domain.E164Phone, body string) (string, error) {
	s.logger.InfoContext(ctx, "sms send skipped: NOTIFY_SMS_ENABLED is false", "body_length", len(body))
	return "noop", nil
}
