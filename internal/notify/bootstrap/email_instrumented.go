package bootstrap

import (
	"context"
	"errors"

	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// instrumentedEmailSender decorates a domain.EmailSender with
// EmailRecorder metrics around every Send call (NES-141) — mirrors
// instrumentedSMSSender's identical decorator role: a plain wrap, not a
// modification to SESEmailSender itself, so it applies equally to that
// adapter or any future EmailSender implementation without either
// depending on internal/platform/metrics (see NewEmailSender's own doc).
type instrumentedEmailSender struct {
	next     notifydomain.EmailSender
	recorder metrics.EmailRecorder
}

// Compile-time assurance the decorator satisfies the port.
var _ notifydomain.EmailSender = (*instrumentedEmailSender)(nil)

// newInstrumentedEmailSender wraps next, recording every Send outcome
// through recorder.
func newInstrumentedEmailSender(next notifydomain.EmailSender, recorder metrics.EmailRecorder) *instrumentedEmailSender {
	return &instrumentedEmailSender{next: next, recorder: recorder}
}

// Send delegates to the wrapped sender and records exactly one outcome:
// IncRejected when the wrapped sender returns
// notifydomain.ErrRecipientRejected, IncFailed for any other error, and
// IncSent on success.
func (s *instrumentedEmailSender) Send(ctx context.Context, to, subject, htmlBody, textBody string) (string, error) {
	id, err := s.next.Send(ctx, to, subject, htmlBody, textBody)
	switch {
	case err == nil:
		s.recorder.IncSent()
	case errors.Is(err, notifydomain.ErrRecipientRejected):
		s.recorder.IncRejected()
	default:
		s.recorder.IncFailed()
	}
	return id, err
}
