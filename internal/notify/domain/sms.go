package domain

import "context"

// SMSSender is the outbound port for sending a single SMS text message
// through whichever provider this deployment is configured with (NES-138).
// It is deliberately narrower than Sender: Sender's Send(ctx, *Notification)
// takes a whole outbox row, but an SMS provider needs only a destination
// phone number and a message body — SMSSender is the channel-specific
// payload shape a FUTURE Sender-implementing SMS adapter wraps (NES-139,
// once member phone numbers and delivery routing exist to resolve a
// Notification down to a `to`), mirroring how Enqueuer is Outbox's own
// narrower producer-side port.
//
// Send error contract: implementations validate to via ParseE164Phone
// before this method is ever called (the caller constructs an E164Phone,
// which cannot itself be malformed — see that type's own doc), so Send
// itself never validates the destination's format. Send returns
// ErrRecipientOptedOut when the provider reports the destination has
// opted out of SMS — a terminal, non-retryable outcome — and a wrapped
// provider error for every other failure (throttling, service errors,
// etc., after the provider's own configured retry budget is exhausted).
type SMSSender interface {
	// Send delivers body to the destination to, returning the provider's
	// own message identifier on success (useful for later correlating a
	// delivery-status webhook or support inquiry against this specific
	// send, though nothing in this ticket persists it yet).
	Send(ctx context.Context, to E164Phone, body string) (providerMessageID string, err error)
}
