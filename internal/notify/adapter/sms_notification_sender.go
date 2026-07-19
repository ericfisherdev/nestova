package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// SMSNotificationSender is the domain.Sender implementation for the SMS
// channel (NES-139), wrapping the narrower domain.SMSSender (NES-138,
// which only knows how to send a body to an already-validated phone
// number) with the member-resolution step a full Sender needs: looking up
// the notification's member's current phone number and opt-in consent
// before ever calling the underlying SMSSender.
//
// This resolution happens HERE, at send time, deliberately — NOT only
// once at enqueue time (routing.RoutingEnqueuer already checks opt-in
// status before routing to SMS in the first place): a member can remove
// their phone number or opt out between when a notification was enqueued
// and when the dispatcher actually attempts delivery. Re-checking here
// closes that race and is what makes the NES-139 AC "removing a phone
// number or opting out stops SMS immediately" true even for
// already-queued notifications, not just future ones.
type SMSNotificationSender struct {
	sms      domain.SMSSender
	contacts domain.ContactDirectory
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.Sender = (*SMSNotificationSender)(nil)

// NewSMSNotificationSender constructs an SMSNotificationSender with
// injected dependencies. Panics when either is nil.
func NewSMSNotificationSender(sms domain.SMSSender, contacts domain.ContactDirectory) *SMSNotificationSender {
	if sms == nil {
		panic("adapter: NewSMSNotificationSender requires a non-nil SMSSender")
	}
	if contacts == nil {
		panic("adapter: NewSMSNotificationSender requires a non-nil ContactDirectory")
	}
	return &SMSNotificationSender{sms: sms, contacts: contacts}
}

// Channel reports the delivery channel this sender handles.
func (s *SMSNotificationSender) Channel() domain.Channel { return domain.ChannelSMS }

// Send resolves n.MemberID's current contact details and, only when the
// member has BOTH a phone number on file AND current opt-in consent
// (domain.MemberContact.ReadyForSMS), sends n's title and body through
// the wrapped SMSSender.
//
// Every failure branch here is terminal (never retried by this sender
// itself — the underlying SMSSender already exhausted its own retry
// budget for transient AWS failures before ever returning an error to
// this method): a nil MemberID (a household-wide notification was
// somehow routed to SMS, which should never happen — there is no single
// destination phone for a household), a member with no phone or no
// consent (domain.ErrMemberNotSMSReady), or the SMSSender's own terminal
// outcome (domain.ErrRecipientOptedOut or a wrapped provider error) all
// return a non-nil error, which Dispatcher.deliver maps to a terminal
// failure with an in-app fallback (NES-139) — see that method's own doc.
func (s *SMSNotificationSender) Send(ctx context.Context, n *domain.Notification) error {
	if n == nil {
		return errors.New("adapter: sms send: nil notification")
	}
	if n.MemberID == nil {
		return errors.New("adapter: sms send: notification has no member to address")
	}

	contact, err := s.contacts.GetContact(ctx, *n.MemberID)
	if err != nil {
		return fmt.Errorf("adapter: sms send: resolve contact: %w", err)
	}
	if !contact.ReadyForSMS() {
		return domain.ErrMemberNotSMSReady
	}

	body := n.Title
	if n.Body != "" {
		body = n.Title + ": " + n.Body
	}
	_, err = s.sms.Send(ctx, *contact.Phone, body)
	return err
}
