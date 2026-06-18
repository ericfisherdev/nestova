package domain

import (
	"context"
	"errors"
	"time"
)

// Household is the aggregate root for the household bounded context.
type Household struct {
	ID        HouseholdID
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Domain errors returned by HouseholdRepository implementations.
var (
	// ErrHouseholdNotFound is returned when a household does not exist.
	ErrHouseholdNotFound = errors.New("household: household not found")
	// ErrMemberNotFound is returned when a member does not exist.
	ErrMemberNotFound = errors.New("household: member not found")
	// ErrDuplicateMember is returned when adding a member whose display name
	// already exists (case-insensitively) within the household.
	ErrDuplicateMember = errors.New("household: duplicate member display name in household")
)

// HouseholdRepository persists households and their members. Member is a child
// entity of the Household aggregate, so its operations live on this one
// aggregate-scoped repository.
//
// Persistence contracts (the caller sets identity and valid enum values; the
// store sets timestamps):
//   - CreateHousehold expects h.ID and h.Name set; it populates CreatedAt/UpdatedAt.
//   - AddMember expects m.ID, m.HouseholdID, m.DisplayName, and valid m.Role /
//     m.Color set; it populates CreatedAt/UpdatedAt. The caller is responsible
//     for supplying valid enum values (the store does not re-validate on write).
//
// Error contracts:
//   - GetHousehold returns ErrHouseholdNotFound when id is unknown.
//   - GetMember returns ErrMemberNotFound when id is unknown.
//   - AddMember returns ErrDuplicateMember when the display name collides within
//     the household, and ErrHouseholdNotFound when m.HouseholdID does not exist.
//   - ListMembers returns an empty slice (not an error) for an unknown household.
//   - CreateHousehold/AddMember surface other failures (e.g. an id collision) as
//     a wrapped error, not a sentinel.
//   - HasAnyHousehold returns (false, nil) on an empty database and (true, nil)
//     once at least one household row exists. It never returns a sentinel error.
type HouseholdRepository interface {
	CreateHousehold(ctx context.Context, h *Household) error
	GetHousehold(ctx context.Context, id HouseholdID) (*Household, error)
	AddMember(ctx context.Context, m *Member) error
	GetMember(ctx context.Context, id MemberID) (*Member, error)
	ListMembers(ctx context.Context, householdID HouseholdID) ([]*Member, error)
	// HasAnyHousehold reports whether at least one household row exists. It is
	// used by the onboarding flow to gate the first-run setup page and to block
	// a second call to the public POST /onboarding route (open-registration guard).
	HasAnyHousehold(ctx context.Context) (bool, error)
}
