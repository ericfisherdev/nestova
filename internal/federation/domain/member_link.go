package domain

import (
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// MemberLink connects one household member to the Nestorage account
// NSTR-101's federation endpoints found or created for them (NSTR-107) —
// nestorage_member_link's domain representation. HouseholdID is
// denormalized from the member so every read and write MemberLinkRepository
// serves is scoped by it directly, without a join back to member.
type MemberLink struct {
	MemberID     household.MemberID
	HouseholdID  household.HouseholdID
	RemoteUserID string
	LinkedVia    LinkOrigin
	LinkedAt     time.Time
}

// LinkOrigin records why a MemberLink was created — the value persisted as
// nestorage_member_link.linked_via. Stored as text, validated here,
// mirroring household/domain/enums.go's own Valid/Parse convention for a
// small stored-as-text enum.
type LinkOrigin string

// The three ways a MemberLink can come to exist.
const (
	// LinkOriginEmail is a link ProposeMatches' own case-insensitive exact
	// email match proposed, that a human then confirmed.
	LinkOriginEmail LinkOrigin = "email"
	// LinkOriginManual is a link a human paired by hand — for a member and
	// remote account ProposeMatches itself left unmatched or ambiguous.
	LinkOriginManual LinkOrigin = "manual"
	// LinkOriginCreated is a link ReconciliationService.Confirm made by
	// provisioning a brand new Nestorage account for a confirmed no-match.
	LinkOriginCreated LinkOrigin = "created"
)

// Valid reports whether o is a known link origin.
func (o LinkOrigin) Valid() bool {
	switch o {
	case LinkOriginEmail, LinkOriginManual, LinkOriginCreated:
		return true
	default:
		return false
	}
}

// String returns the origin's stored value.
func (o LinkOrigin) String() string { return string(o) }

// ParseLinkOrigin validates and returns a LinkOrigin, or an error for an
// unknown value.
func ParseLinkOrigin(s string) (LinkOrigin, error) {
	o := LinkOrigin(s)
	if !o.Valid() {
		return "", fmt.Errorf("invalid link origin %q", s)
	}
	return o, nil
}
