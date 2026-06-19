package domain

import (
	"context"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// UsageEvent records a single handling of a tracked item at a point in time.
// MemberID is nil for system-generated events (e.g. an automated depletion);
// when set, it attributes the event to the member who logged it.
type UsageEvent struct {
	ID            UsageEventID
	HouseholdID   household.HouseholdID
	TrackedItemID TrackedItemID
	Type          UsageType
	OccurredAt    time.Time
	MemberID      *household.MemberID
	CreatedAt     time.Time
}

// UsageEventRepository appends usage events and reads the depletion history a
// prediction needs.
//
// Contracts:
//   - Append expects ID, HouseholdID, TrackedItemID, a valid Type, and
//     OccurredAt set; MemberID is optional. It populates CreatedAt.
//   - ListDepletionEvents returns only Depleted events for the item, ordered by
//     OccurredAt ascending, and an empty slice (not an error) when there are
//     none. The prediction engine averages the intervals between these.
type UsageEventRepository interface {
	Append(ctx context.Context, event *UsageEvent) error
	ListDepletionEvents(ctx context.Context, trackedItemID TrackedItemID) ([]*UsageEvent, error)
}
