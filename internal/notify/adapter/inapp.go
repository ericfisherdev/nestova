package adapter

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// InAppSender is the domain.Sender implementation for the in-app notification
// channel. It logs the delivery event and returns nil (no external side-effect
// in the skeleton). PII (title, body) is never logged; only non-sensitive
// identifiers are included.
type InAppSender struct {
	logger *slog.Logger
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.Sender = (*InAppSender)(nil)

// NewInAppSender constructs an InAppSender with an injected logger.
func NewInAppSender(logger *slog.Logger) *InAppSender {
	if logger == nil {
		panic("adapter: NewInAppSender requires a non-nil logger")
	}
	return &InAppSender{logger: logger}
}

// Channel reports the delivery channel this sender handles.
func (s *InAppSender) Channel() domain.Channel { return domain.ChannelInApp }

// Send logs the in-app delivery event. No PII (title, body) is included in the
// log entry; only the notification ID, household ID, and channel are recorded.
func (s *InAppSender) Send(_ context.Context, n *domain.Notification) error {
	if n == nil {
		return errors.New("adapter: inapp send: nil notification")
	}
	s.logger.Info("inapp notification delivered",
		"notification_id", n.ID.String(),
		"household_id", n.HouseholdID.String(),
		"channel", n.Channel.String(),
	)
	return nil
}
