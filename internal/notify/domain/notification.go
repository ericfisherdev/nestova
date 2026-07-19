package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Notification is a scheduled delivery item in the outbox. It is the aggregate
// root of the notify bounded context.
//
// MemberID is nil for household-wide notifications; when set, the member must
// belong to HouseholdID (enforced by the composite FK in the notification table).
type Notification struct {
	ID          NotificationID
	HouseholdID household.HouseholdID
	// MemberID is nil for a household-wide notification.
	MemberID     *household.MemberID
	Channel      Channel
	Title        string
	Body         string
	ScheduledFor time.Time
	Status       Status
	// SentAt is populated by the outbox when the notification is successfully
	// delivered.
	SentAt *time.Time
	// SourceType optionally identifies the domain entity that triggered this
	// notification (e.g. "task", "meal").
	SourceType string
	// SourceID optionally holds the UUID of the triggering entity.
	SourceID  *uuid.UUID
	CreatedAt time.Time
}

// Domain errors for the notify bounded context.
var (
	// ErrNotificationNotFound is returned by Outbox when the requested
	// notification id does not exist.
	ErrNotificationNotFound = errors.New("notify: notification not found")
	// ErrUnknownChannel is returned by the sender registry when no Sender is
	// registered for the notification's channel.
	ErrUnknownChannel = errors.New("notify: unknown channel")
	// ErrRecipientOptedOut is returned by SMSSender.Send when the
	// destination phone number has opted out of SMS (NES-138) — a
	// terminal, non-retryable outcome: the carrier will not deliver to
	// that number regardless of how many times the send is retried, so a
	// caller must map this to a failed notification immediately rather
	// than exhausting its retry budget on a send that can never succeed.
	ErrRecipientOptedOut = errors.New("notify: recipient has opted out of sms")
)

// Enqueuer is the narrow producer port: callers that only need to queue a
// notification (feature code raising events) depend on this, not on the full
// dispatcher lifecycle in Outbox (interface segregation).
type Enqueuer interface {
	// Enqueue persists a new notification in pending status.
	Enqueue(ctx context.Context, n *Notification) error
}

// Outbox is the outbound persistence port for the notification outbox. It is
// implemented in the adapter layer and injected into the application layer.
// It embeds Enqueuer and adds the dispatcher-side lifecycle methods.
//
// Persistence contracts:
//   - Enqueue expects n.ID, n.HouseholdID, n.Channel, n.Title, n.Body,
//     n.ScheduledFor, and n.Status set. MemberID, SourceType, and SourceID are
//     optional (nullable). The store sets CreatedAt.
//
// Error contracts:
//   - Enqueue returns a wrapped error on constraint violation or network
//     failure; no sentinel.
//   - ClaimDue returns a wrapped error on failure; returns an empty slice (not
//     an error) when no notifications are due.
//   - MarkSent returns ErrNotificationNotFound when id is unknown.
//   - MarkFailed returns ErrNotificationNotFound when id is unknown.
type Outbox interface {
	Enqueuer

	// ClaimDue atomically selects up to limit pending notifications with
	// scheduled_for <= now(), transitions them to StatusSent (optimistic claim,
	// leaving sent_at NULL), and returns them. The caller delivers each and then
	// calls MarkSent (which stamps sent_at) or MarkFailed. A crash between
	// ClaimDue and delivery leaves the row as (StatusSent, sent_at IS NULL) —
	// claimed but not delivered — which this skeleton does not retry (effectively
	// at-most-once). That state is left detectable for a future recovery sweep.
	ClaimDue(ctx context.Context, limit int) ([]*Notification, error)

	// MarkSent sets the notification status to StatusSent and records sent_at.
	// Returns ErrNotificationNotFound when id is unknown.
	MarkSent(ctx context.Context, id NotificationID) error

	// MarkFailed sets the notification status to StatusFailed.
	// Returns ErrNotificationNotFound when id is unknown.
	MarkFailed(ctx context.Context, id NotificationID) error
}

// Sender is the outbound side-effect port for a single delivery channel. One
// implementation exists per channel; the dispatcher resolves the right sender
// from a registry keyed by Channel.
type Sender interface {
	// Channel returns the delivery channel this sender handles.
	Channel() Channel

	// Send delivers the notification. Returns a non-nil error when delivery
	// fails; the dispatcher will call Outbox.MarkFailed in that case.
	Send(ctx context.Context, n *Notification) error
}
