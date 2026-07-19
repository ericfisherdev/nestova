package domain

import (
	"context"
	"errors"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// EmailSender is the outbound port for sending a single email through
// whichever provider this deployment is configured with (NES-141). Like
// SMSSender, it is deliberately narrower than Sender: Sender's
// Send(ctx, *Notification) takes a whole outbox row, but an email provider
// needs only a destination address, a subject, and the two body parts —
// EmailSender is the channel-specific payload shape EmailNotificationSender
// (the Sender-implementing adapter for ChannelEmail) wraps, once it has
// resolved the notification's member down to an email address, mirroring
// SMSNotificationSender's identical wrapping of SMSSender.
//
// Send error contract: implementations return ErrRecipientRejected when
// the provider refuses the destination address outright — in this
// deployment's Amazon SES sandbox scope, this is what an unverified
// recipient's send attempt looks like (see SESEmailSender's own doc for
// why no more specific reason is distinguished) — a terminal,
// non-retryable outcome, and a wrapped provider error for every other
// failure.
type EmailSender interface {
	// Send delivers an email with subject to the destination to, with
	// separate HTML and plain-text bodies (a client renders whichever it
	// supports; every send carries both — see EmailNotificationSender's
	// own doc for why neither is optional), returning the provider's own
	// message identifier on success.
	Send(ctx context.Context, to, subject, htmlBody, textBody string) (providerMessageID string, err error)
}

// MemberEmailResolver is the narrow, cross-context port
// EmailNotificationSender uses to resolve a member's current email
// address at send time (NES-141). An email address lives on
// auth/domain.Credential (physically the member table's email column),
// not household.Member or any table this bounded context owns — this
// port lets notify depend on "a memberID resolves to an email" without
// ever importing internal/auth. The composition root
// (cmd/server/main.go) wires the auth context's own credential
// repository against this port STRUCTURALLY (it already has a method
// with this exact shape) — no new adapter type is needed, mirroring the
// same "adapters never import each other" convention documented there
// for the onboarding provisioner.
type MemberEmailResolver interface {
	// ResolveEmail returns memberID's current email address. Returns a
	// non-nil error when memberID does not exist OR exists but has no
	// email on file (e.g. a member with no login credentials of their
	// own — member.email and password_hash are set or cleared together,
	// see 00002_auth's own CHECK) — EmailNotificationSender.Send treats
	// EVERY resolution failure identically, as a terminal, non-retryable
	// outcome, so the two cases are deliberately not distinguished by a
	// sentinel.
	ResolveEmail(ctx context.Context, memberID household.MemberID) (string, error)
}

// ErrRecipientRejected is returned by EmailSender.Send when the provider
// refuses to send to the destination address at all (NES-141) — in this
// deployment's Amazon SES sandbox scope, the dominant real-world cause is
// an unverified recipient address, but see SESEmailSender's own doc for
// why SES's own error shape cannot distinguish that from other
// message-rejection causes. Treated as terminal and never retried,
// mirroring ErrRecipientOptedOut's identical SMS-side reasoning: retrying
// an outright rejection spends nothing but time.
var ErrRecipientRejected = errors.New("notify: email recipient rejected by provider")
