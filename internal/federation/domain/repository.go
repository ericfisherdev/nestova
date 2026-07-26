package domain

import (
	"context"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// InstanceLinkRepository persists federation instance links, one per
// household (NSTR-106). Every method is scoped by household id or base
// url — never a bare InstanceLinkID lookup — since every caller already
// knows which household or instance it cares about.
type InstanceLinkRepository interface {
	// Create persists link. Returns ErrHouseholdAlreadyBound when
	// link.HouseholdID already has a link, or ErrInstanceAlreadyBound when
	// link's (already-normalized) BaseURL is already linked to a different
	// household.
	Create(ctx context.Context, link *InstanceLink) error
	// GetByHousehold returns householdID's link, or ErrLinkNotFound.
	GetByHousehold(ctx context.Context, householdID household.HouseholdID) (*InstanceLink, error)
	// GetByBaseURL returns the link bound to the already-normalized
	// baseURL, or ErrLinkNotFound.
	GetByBaseURL(ctx context.Context, baseURL string) (*InstanceLink, error)
	// DeleteByHousehold removes householdID's link. It does not error when
	// no link exists — Detach is idempotent.
	DeleteByHousehold(ctx context.Context, householdID household.HouseholdID) error
}
