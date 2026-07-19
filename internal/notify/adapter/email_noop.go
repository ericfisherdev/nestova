package adapter

import (
	"context"
	"log/slog"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// NoopEmailSender is the domain.EmailSender used when
// NOTIFY_EMAIL_ENABLED=false (NES-141) — the safe zero-config default,
// mirroring NoopSMSSender's identical "always available, no external
// dependency" role. It logs the delivery attempt and returns a synthetic
// message id with no external side effect: a deployment with email
// disabled must run with ZERO AWS dependency, so this type imports
// nothing from aws-sdk-go-v2 at all.
type NoopEmailSender struct {
	logger *slog.Logger
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.EmailSender = (*NoopEmailSender)(nil)

// NewNoopEmailSender constructs a NoopEmailSender with an injected logger.
func NewNoopEmailSender(logger *slog.Logger) *NoopEmailSender {
	if logger == nil {
		panic("adapter: NewNoopEmailSender requires a non-nil logger")
	}
	return &NoopEmailSender{logger: logger}
}

// Send logs the would-be delivery and returns a fixed synthetic message id
// with no external side effect. No PII (the destination address, subject,
// or body) is logged; only the body lengths are recorded, mirroring
// NoopSMSSender's own no-PII-in-logs convention.
func (s *NoopEmailSender) Send(ctx context.Context, _, _, htmlBody, textBody string) (string, error) {
	s.logger.InfoContext(ctx, "email send skipped: NOTIFY_EMAIL_ENABLED is false",
		"html_body_length", len(htmlBody),
		"text_body_length", len(textBody),
	)
	return "noop", nil
}
