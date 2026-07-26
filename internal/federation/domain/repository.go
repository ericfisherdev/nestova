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

// MemberLinkRepository persists nestorage member links (NSTR-107): the
// record tying a household member to the Nestorage account
// ReconciliationService.Confirm resolved for them, one per member.
//
// Error contracts:
//   - Put returns ErrLinkConflict when link.RemoteUserID already names a
//     DIFFERENT member than link.MemberID.
type MemberLinkRepository interface {
	// ListByHousehold returns every existing link for householdID, in no
	// particular order — callers needing a deterministic view (Proposals)
	// sort or index it themselves.
	ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]MemberLink, error)
	// Put idempotently records link: creating it when link.MemberID has no
	// link yet, or succeeding unchanged when link.MemberID already names
	// exactly this RemoteUserID — which is what makes a retried Confirm
	// call after a partial failure losslessly re-runnable. Returns
	// ErrLinkConflict when link.RemoteUserID is already linked to a
	// DIFFERENT member.
	Put(ctx context.Context, link MemberLink) error
}
