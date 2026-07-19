package app

import (
	"context"
	"errors"
	"log/slog"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// RoutingEnqueuer decorates a domain.Enqueuer with per-member,
// per-event-type channel routing and household quiet-hours deferral
// (NES-139). It is the ONLY place a Notification's Channel is resolved
// from member preference — every scheduler/service that raises a
// notification (reminders, chore trades, reward redemptions, restock,
// subscription renewals) is wired against a RoutingEnqueuer instead of
// the raw Outbox at composition time (cmd/server/main.go), so none of
// their own call sites need to change beyond setting EventType.
//
// Resolution happens ONLY when BOTH n.MemberID and n.EventType are set: a
// household-wide notification (MemberID nil) has no single member to
// resolve a preference for, and a notification whose EventType is empty
// (not yet migrated to the preference system) is left exactly as its
// caller set Channel — both cases pass through unchanged, never an error.
//
// A failure resolving the channel (a contact or preference lookup error)
// is logged and swallowed, NOT propagated: a routing failure must never
// block the notification from being enqueued at all — it falls back to
// whatever Channel the caller already set (ChannelInApp, by every current
// call site's own convention), which is always a safe, always-delivered
// channel.
type RoutingEnqueuer struct {
	next        domain.Enqueuer
	preferences domain.PreferenceRepository
	contacts    domain.ContactDirectory
	households  householdReader
	logger      *slog.Logger
}

// Compile-time assurance the decorator satisfies the port it wraps.
var _ domain.Enqueuer = (*RoutingEnqueuer)(nil)

// NewRoutingEnqueuer constructs a RoutingEnqueuer wrapping next. Panics
// when any dependency is nil.
func NewRoutingEnqueuer(
	next domain.Enqueuer,
	preferences domain.PreferenceRepository,
	contacts domain.ContactDirectory,
	households householdReader,
	logger *slog.Logger,
) *RoutingEnqueuer {
	if next == nil {
		panic("app: NewRoutingEnqueuer requires a non-nil Enqueuer")
	}
	if preferences == nil {
		panic("app: NewRoutingEnqueuer requires a non-nil PreferenceRepository")
	}
	if contacts == nil {
		panic("app: NewRoutingEnqueuer requires a non-nil ContactDirectory")
	}
	if households == nil {
		panic("app: NewRoutingEnqueuer requires a non-nil household reader")
	}
	if logger == nil {
		panic("app: NewRoutingEnqueuer requires a non-nil logger")
	}
	return &RoutingEnqueuer{next: next, preferences: preferences, contacts: contacts, households: households, logger: logger}
}

// Enqueue resolves n's Channel from member preference (when routable —
// see the type's own doc), shifts n.ScheduledFor to the end of the
// household's quiet hours when the resolved channel is SMS and
// ScheduledFor falls inside that window, and delegates to the wrapped
// Enqueuer. n's other fields are never modified.
func (e *RoutingEnqueuer) Enqueue(ctx context.Context, n *domain.Notification) error {
	if n != nil && n.MemberID != nil && n.EventType != "" {
		e.route(ctx, n)
	}
	return e.next.Enqueue(ctx, n)
}

// route resolves and applies n's Channel (and, for SMS, its quiet-hours
// deferral) in place. Errors are logged, not returned — see Enqueue's own
// doc on why a routing failure must never block enqueueing.
func (e *RoutingEnqueuer) route(ctx context.Context, n *domain.Notification) {
	channel, err := e.resolveChannel(ctx, *n.MemberID, n.EventType)
	if err != nil {
		e.logger.ErrorContext(ctx, "routing: resolve channel failed, keeping default",
			"member_id", n.MemberID.String(),
			"event_type", n.EventType.String(),
			"error", err,
		)
		return
	}
	n.Channel = channel
	if channel != domain.ChannelSMS {
		return
	}

	hh, err := e.households.GetHousehold(ctx, n.HouseholdID)
	if err != nil {
		e.logger.ErrorContext(ctx, "routing: load household for quiet hours failed, sending without deferral",
			"household_id", n.HouseholdID.String(),
			"error", err,
		)
		return
	}
	if hh.InQuietHours(n.ScheduledFor) {
		n.ScheduledFor = hh.QuietHoursEndAfter(n.ScheduledFor)
	}
}

// resolveChannel returns the channel a notification for (memberID,
// eventType) should use: the member's explicit preference when one
// exists AND (for an sms preference specifically) the member is
// CURRENTLY sms-ready, ChannelInApp otherwise — the sparse-table default,
// and the safe fallback when a member's sms preference has gone stale
// (e.g. they opted out after setting it). This is what makes the NES-139
// AC "removing a phone number or opting out stops SMS immediately without
// losing notifications" true for every notification enqueued from that
// point on (SMSNotificationSender's own re-check at send time closes the
// remaining race for anything already enqueued before the change — see
// that type's own doc).
func (e *RoutingEnqueuer) resolveChannel(ctx context.Context, memberID household.MemberID, eventType domain.EventType) (domain.Channel, error) {
	pref, err := e.preferences.Get(ctx, memberID, eventType)
	switch {
	case errors.Is(err, domain.ErrPreferenceNotFound):
		return domain.ChannelInApp, nil
	case err != nil:
		return "", err
	}
	if pref != domain.ChannelSMS {
		return pref, nil
	}

	contact, err := e.contacts.GetContact(ctx, memberID)
	if err != nil {
		return "", err
	}
	if !contact.ReadyForSMS() {
		return domain.ChannelInApp, nil
	}
	return domain.ChannelSMS, nil
}
