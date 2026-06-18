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
)

// Valid reports whether c is a known channel.
func (c Channel) Valid() bool {
	switch c {
	case ChannelPush, ChannelEmail, ChannelInApp:
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
