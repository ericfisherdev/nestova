package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ErrInvalidExternalEvent is returned by Validate for a malformed cached event.
var ErrInvalidExternalEvent = errors.New("calendar: invalid external event")

// ExternalEvent is a cached external calendar event that feeds the unified
// calendar. It is a write-through cache of the provider's event, keyed per
// account by ExternalID (the provider's event id). Color is the provider color
// id, empty when the event carries none. AllDay marks a date-only event.
type ExternalEvent struct {
	ID                ExternalEventID
	CalendarAccountID CalendarAccountID
	ExternalID        string
	Title             string
	StartsAt          time.Time
	EndsAt            time.Time
	AllDay            bool
	Color             string
	UpdatedAt         time.Time
}

// Validate reports whether the event is well-formed, wrapping
// ErrInvalidExternalEvent with detail. ExternalID must be non-blank (it is the
// cache key), the start must be set, and the end must not precede the start.
func (e ExternalEvent) Validate() error {
	if e.ID == (ExternalEventID{}) {
		return fmt.Errorf("%w: id is required", ErrInvalidExternalEvent)
	}
	if e.CalendarAccountID == (CalendarAccountID{}) {
		return fmt.Errorf("%w: calendar account id is required", ErrInvalidExternalEvent)
	}
	if strings.TrimSpace(e.ExternalID) == "" {
		return fmt.Errorf("%w: external id must not be blank", ErrInvalidExternalEvent)
	}
	if e.StartsAt.IsZero() {
		return fmt.Errorf("%w: start time is required", ErrInvalidExternalEvent)
	}
	if e.EndsAt.IsZero() {
		return fmt.Errorf("%w: end time is required", ErrInvalidExternalEvent)
	}
	if e.EndsAt.Before(e.StartsAt) {
		return fmt.Errorf("%w: end time must not precede start time", ErrInvalidExternalEvent)
	}
	return nil
}

// ExternalEventRepository persists the external-event cache.
//
// Error contracts:
//   - UpsertByExternalID inserts or replaces the cache row for
//     (calendar_account_id, external_id); it is idempotent and preserves the
//     existing row's id on conflict. A returned error is wrapped, no sentinel.
//   - DeleteByExternalID removes the cached event for a provider event id; it is
//     a no-op (no error) when the event is not cached, so honoring a provider
//     deletion is idempotent.
//   - ListByHouseholdRange returns the household's cached events whose start
//     falls in [from, to], across all the household's calendar accounts, ordered
//     by start time; it returns an empty slice (not an error) when none match.
type ExternalEventRepository interface {
	UpsertByExternalID(ctx context.Context, event *ExternalEvent) error
	DeleteByExternalID(ctx context.Context, accountID CalendarAccountID, externalID string) error
	ListByHouseholdRange(ctx context.Context, householdID household.HouseholdID, from, to time.Time) ([]*ExternalEvent, error)
}
