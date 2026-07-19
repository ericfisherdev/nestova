package bootstrap

import (
	"context"
	"errors"

	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// instrumentedSMSSender decorates a domain.SMSSender with SMSRecorder
// metrics around every Send call (NES-138) — a plain decorator, not a
// modification to AWSEndUserMessagingSender itself, so it applies equally
// to that adapter or any future SMSSender implementation without either
// depending on internal/platform/metrics (see NewSMSSender's own doc).
type instrumentedSMSSender struct {
	next     notifydomain.SMSSender
	recorder metrics.SMSRecorder
}

// Compile-time assurance the decorator satisfies the port.
var _ notifydomain.SMSSender = (*instrumentedSMSSender)(nil)

// newInstrumentedSMSSender wraps next, recording every Send outcome
// through recorder.
func newInstrumentedSMSSender(next notifydomain.SMSSender, recorder metrics.SMSRecorder) *instrumentedSMSSender {
	return &instrumentedSMSSender{next: next, recorder: recorder}
}

// Send delegates to the wrapped sender and records exactly one outcome:
// IncOptedOut when the wrapped sender returns
// notifydomain.ErrRecipientOptedOut, IncFailed for any other error, and
// IncSent on success.
func (s *instrumentedSMSSender) Send(ctx context.Context, to notifydomain.E164Phone, body string) (string, error) {
	id, err := s.next.Send(ctx, to, body)
	switch {
	case err == nil:
		s.recorder.IncSent()
	case errors.Is(err, notifydomain.ErrRecipientOptedOut):
		s.recorder.IncOptedOut()
	default:
		s.recorder.IncFailed()
	}
	return id, err
}
