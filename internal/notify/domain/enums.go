package domain

import "fmt"

// Channel is the delivery mechanism for a notification. Stored as text,
// validated here. The values match the notification.channel CHECK constraint.
type Channel string

// Notification delivery channels.
const (
	// ChannelPush delivers via a device push notification.
	ChannelPush Channel = "push"
	// ChannelEmail delivers via email.
	ChannelEmail Channel = "email"
	// ChannelInApp delivers as an in-app notification.
	ChannelInApp Channel = "inapp"
	// ChannelSMS delivers via SMS text message (NES-138). The schema and
	// domain.SMSSender port both exist as of NES-138; no code enqueues a
	// ChannelSMS notification yet — routing a notification to it (member
	// phone numbers, delivery preferences) is NES-139.
	ChannelSMS Channel = "sms"
)

// Valid reports whether c is a known channel.
func (c Channel) Valid() bool {
	switch c {
	case ChannelPush, ChannelEmail, ChannelInApp, ChannelSMS:
		return true
	default:
		return false
	}
}

// String returns the channel's stored value.
func (c Channel) String() string { return string(c) }

// ParseChannel validates and returns a Channel, or an error for an unknown value.
func ParseChannel(s string) (Channel, error) {
	c := Channel(s)
	if !c.Valid() {
		return "", fmt.Errorf("invalid channel %q", s)
	}
	return c, nil
}

// EventType identifies WHY a notification was raised — the semantic kind
// of event, distinct from Channel (HOW it's delivered) and SourceType
// (WHICH broader entity kind triggered it, e.g. "task_instance" covers
// both EventTypeTaskDueSoon and EventTypeTaskOverdue). A member's
// per-event-type channel preference (NES-139,
// member_notification_pref.event_type) is keyed on this value: a member
// can route "claim expiring" to SMS while leaving "task due soon" on the
// in-app default, even though both originate from the same task_instance
// source.
//
// Every EventType here corresponds to an existing notification-raising
// call site as of NES-139; a future notification kind (e.g. NES-141's
// email channel needs none, but a genuinely new EVENT would) adds a new
// constant here and updates AllEventTypes.
type EventType string

// Notification event types, grouped by originating bounded context.
const (
	// EventTypeClaimExpiring fires when a claimed chore's completion
	// window is about to expire (a warning, before the penalty).
	EventTypeClaimExpiring EventType = "claim_expiring"
	// EventTypeClaimExpired fires when a claimed chore's completion
	// window has expired and the claimant was penalized.
	EventTypeClaimExpired EventType = "claim_expired"
	// EventTypeTaskDueSoon fires ahead of a recurring task instance's due
	// date.
	EventTypeTaskDueSoon EventType = "task_due_soon"
	// EventTypeTaskOverdue fires once a recurring task instance's due
	// date has passed uncompleted.
	EventTypeTaskOverdue EventType = "task_overdue"
	// EventTypeChoreTradeProposed fires when another member proposes a
	// chore trade.
	EventTypeChoreTradeProposed EventType = "chore_trade_proposed"
	// EventTypeChoreTradeAccepted fires when a proposed chore trade is
	// accepted.
	EventTypeChoreTradeAccepted EventType = "chore_trade_accepted"
	// EventTypeChoreTradeDeclined fires when a proposed chore trade is
	// declined.
	EventTypeChoreTradeDeclined EventType = "chore_trade_declined"
	// EventTypeChoreTradeExpired fires when a chore trade proposal
	// expires without a response.
	EventTypeChoreTradeExpired EventType = "chore_trade_expired"
	// EventTypeRewardRedemptionRequested fires to a parent when a member
	// requests a reward redemption.
	EventTypeRewardRedemptionRequested EventType = "reward_redemption_requested"
	// EventTypeRewardRedemptionResolved fires to the requesting member
	// when their redemption is approved or denied.
	EventTypeRewardRedemptionResolved EventType = "reward_redemption_resolved"
	// EventTypeRestockSoon fires when a tracked item is predicted to run
	// out soon.
	EventTypeRestockSoon EventType = "restock_soon"
	// EventTypeSubscriptionRenewalDue fires ahead of a subscription's
	// next renewal date.
	EventTypeSubscriptionRenewalDue EventType = "subscription_renewal_due"
)

// AllEventTypes returns every known EventType in a stable display order,
// for rendering the preferences settings UI's event-type list.
func AllEventTypes() []EventType {
	return []EventType{
		EventTypeClaimExpiring,
		EventTypeClaimExpired,
		EventTypeTaskDueSoon,
		EventTypeTaskOverdue,
		EventTypeChoreTradeProposed,
		EventTypeChoreTradeAccepted,
		EventTypeChoreTradeDeclined,
		EventTypeChoreTradeExpired,
		EventTypeRewardRedemptionRequested,
		EventTypeRewardRedemptionResolved,
		EventTypeRestockSoon,
		EventTypeSubscriptionRenewalDue,
	}
}

// Valid reports whether e is a known event type.
func (e EventType) Valid() bool {
	for _, known := range AllEventTypes() {
		if e == known {
			return true
		}
	}
	return false
}

// String returns the event type's stored value.
func (e EventType) String() string { return string(e) }

// Label returns a short, member-facing description of the event type, for
// the preferences settings UI.
func (e EventType) Label() string {
	switch e {
	case EventTypeClaimExpiring:
		return "Claim expiring soon"
	case EventTypeClaimExpired:
		return "Claim expired"
	case EventTypeTaskDueSoon:
		return "Task due soon"
	case EventTypeTaskOverdue:
		return "Task overdue"
	case EventTypeChoreTradeProposed:
		return "New chore trade proposal"
	case EventTypeChoreTradeAccepted:
		return "Chore trade accepted"
	case EventTypeChoreTradeDeclined:
		return "Chore trade declined"
	case EventTypeChoreTradeExpired:
		return "Chore trade proposal expired"
	case EventTypeRewardRedemptionRequested:
		return "New reward redemption request"
	case EventTypeRewardRedemptionResolved:
		return "Reward redemption approved/denied"
	case EventTypeRestockSoon:
		return "Item predicted to run out soon"
	case EventTypeSubscriptionRenewalDue:
		return "Subscription renewing soon"
	default:
		return string(e)
	}
}

// ParseEventType validates and returns an EventType, or an error for an
// unknown value.
func ParseEventType(s string) (EventType, error) {
	e := EventType(s)
	if !e.Valid() {
		return "", fmt.Errorf("invalid event type %q", s)
	}
	return e, nil
}

// Status is the lifecycle state of a notification in the outbox. Stored as
// text, validated here. The values match the notification.status CHECK
// constraint.
type Status string

// Notification outbox statuses.
const (
	// StatusPending marks a notification that has not yet been claimed for
	// delivery.
	StatusPending Status = "pending"
	// StatusSent marks a notification that has been successfully delivered.
	StatusSent Status = "sent"
	// StatusFailed marks a notification whose delivery attempt failed.
	StatusFailed Status = "failed"
	// StatusCancelled marks a notification that was explicitly cancelled before
	// delivery.
	StatusCancelled Status = "cancelled"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusSent, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// String returns the status's stored value.
func (s Status) String() string { return string(s) }

// ParseStatus validates and returns a Status, or an error for an unknown value.
func ParseStatus(s string) (Status, error) {
	st := Status(s)
	if !st.Valid() {
		return "", fmt.Errorf("invalid status %q", s)
	}
	return st, nil
}
