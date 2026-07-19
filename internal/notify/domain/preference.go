package domain

import (
	"context"
	"errors"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// MemberPreference is one member's chosen delivery Channel for one
// EventType (NES-139, the member_notification_pref table). A member with
// no MemberPreference row for a given EventType gets that event type's
// default channel (ChannelInApp) — a routing decision resolved by
// routing.RoutingEnqueuer, not a value ever returned by
// PreferenceRepository itself (see that interface's own doc).
type MemberPreference struct {
	HouseholdID household.HouseholdID
	MemberID    household.MemberID
	EventType   EventType
	Channel     Channel
}

// ErrPreferenceNotFound is returned by PreferenceRepository.Get when the
// member has no explicit preference for the given event type — the
// sparse-table default. Callers resolving a channel for delivery treat
// this as ChannelInApp; callers rendering the settings UI treat it the
// same way for display purposes.
var ErrPreferenceNotFound = errors.New("notify: no preference set for this event type")

// PreferenceRepository is the notify context's port onto member
// notification preferences (NES-139), implemented in the adapter layer
// against the member_notification_pref table.
//
// Error contracts:
//   - Get returns ErrPreferenceNotFound when the member has no explicit
//     preference for eventType.
//   - Set upserts unconditionally (no not-found error — inserting the
//     first preference for a (member, event type) pair is the common
//     case, not an error).
//   - ListForMember returns an empty slice (not an error) when the member
//     has no explicit preferences at all.
type PreferenceRepository interface {
	// Get returns memberID's explicit channel preference for eventType.
	Get(ctx context.Context, memberID household.MemberID, eventType EventType) (Channel, error)

	// Set upserts pref (household_id, member_id, event_type) -> channel.
	Set(ctx context.Context, pref MemberPreference) error

	// ListForMember returns every explicit preference memberID has set,
	// in no particular order — the caller (the settings UI's section
	// view builder) is responsible for merging this sparse list against
	// AllEventTypes to render a complete, defaulted table.
	ListForMember(ctx context.Context, memberID household.MemberID) ([]MemberPreference, error)
}
