package domain

import (
	"context"
	"errors"
	"time"
)

// ErrSyncTokenInvalid is returned by a CalendarEventSource when the stored sync
// token is no longer usable (e.g. expired) and the caller must discard it and
// perform a full resync.
var ErrSyncTokenInvalid = errors.New("calendar: sync token invalid, full resync required")

// SyncedEvent is a provider calendar event observed during a sync. Cancelled
// marks a deletion (the event should be removed from the cache); otherwise it is
// an upsert.
type SyncedEvent struct {
	ExternalID string
	Title      string
	StartsAt   time.Time
	EndsAt     time.Time
	AllDay     bool
	Color      string
	Cancelled  bool
}

// CalendarEventSource fetches a calendar's events from a provider. It is the
// outbound port the sync engine depends on; the Google adapter implements it.
type CalendarEventSource interface {
	// ListEvents returns the calendar's events for accessToken. When syncToken is
	// empty it performs a full sync; otherwise an incremental sync from that
	// cursor. It returns the events and the next sync token to persist, or
	// ErrSyncTokenInvalid when the token must be discarded and a full resync
	// performed with an empty syncToken.
	ListEvents(ctx context.Context, accessToken, calendarID, syncToken string) (events []SyncedEvent, nextSyncToken string, err error)
}
