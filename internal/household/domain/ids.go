package domain

import (
	identity "github.com/ericfisherdev/nestcore/identity/domain"
)

// HouseholdID and MemberID are aliases (not new types) for nestcore's shared
// identity domain ids (NSTR-115): household and member rows now live in the
// identity schema, owned by nestcore, so every household/auth call site that
// passes a HouseholdID/MemberID into a nestcore-provided function (e.g. the
// shared session store) needs zero conversion. Re-declared here, rather than
// requiring every caller in this codebase to import
// github.com/ericfisherdev/nestcore/identity/domain directly, so the many
// existing household.HouseholdID / household.MemberID call sites across the
// other bounded contexts (tasks, calendar, media, subscriptions, tracking,
// kiosk, notify, auth) keep compiling unchanged.
type (
	// HouseholdID uniquely identifies a household.
	HouseholdID = identity.HouseholdID
	// MemberID uniquely identifies a member.
	MemberID = identity.MemberID
)

// NewHouseholdID returns a new time-ordered (UUIDv7) household id.
func NewHouseholdID() HouseholdID { return identity.NewHouseholdID() }

// NewMemberID returns a new time-ordered (UUIDv7) member id.
func NewMemberID() MemberID { return identity.NewMemberID() }

// ParseHouseholdID parses a canonical UUID string into a HouseholdID.
func ParseHouseholdID(s string) (HouseholdID, error) { return identity.ParseHouseholdID(s) }

// ParseMemberID parses a canonical UUID string into a MemberID.
func ParseMemberID(s string) (MemberID, error) { return identity.ParseMemberID(s) }
