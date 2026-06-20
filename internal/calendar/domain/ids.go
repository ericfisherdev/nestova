package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// CalendarAccountID uniquely identifies a member's connected calendar account.
type CalendarAccountID uuid.UUID

// ExternalEventID uniquely identifies a cached external calendar event.
type ExternalEventID uuid.UUID

// NewCalendarAccountID returns a new time-ordered (UUIDv7) calendar account id,
// which gives better B-tree index locality than random v4 ids. uuid.NewV7 only
// errors if the crypto random source is unavailable — the same failure under
// which uuid.New itself panics — so Must is appropriate here.
func NewCalendarAccountID() CalendarAccountID { return CalendarAccountID(uuid.Must(uuid.NewV7())) }

// NewExternalEventID returns a new time-ordered (UUIDv7) external event id.
func NewExternalEventID() ExternalEventID { return ExternalEventID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id CalendarAccountID) String() string { return uuid.UUID(id).String() }

// String returns the canonical UUID string.
func (id ExternalEventID) String() string { return uuid.UUID(id).String() }

// ParseCalendarAccountID parses a canonical UUID string into a CalendarAccountID.
func ParseCalendarAccountID(s string) (CalendarAccountID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return CalendarAccountID{}, fmt.Errorf("parse calendar account id: %w", err)
	}
	return CalendarAccountID(u), nil
}

// ParseExternalEventID parses a canonical UUID string into an ExternalEventID.
func ParseExternalEventID(s string) (ExternalEventID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ExternalEventID{}, fmt.Errorf("parse external event id: %w", err)
	}
	return ExternalEventID(u), nil
}
