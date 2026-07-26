package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// InstanceLinkID uniquely identifies a federation instance link.
type InstanceLinkID uuid.UUID

// NewInstanceLinkID returns a new time-ordered (UUIDv7) instance link id,
// mirroring household.NewHouseholdID's rationale (better B-tree index
// locality than a random v4 id).
func NewInstanceLinkID() InstanceLinkID { return InstanceLinkID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id InstanceLinkID) String() string { return uuid.UUID(id).String() }

// ParseInstanceLinkID parses a canonical UUID string into an InstanceLinkID.
func ParseInstanceLinkID(s string) (InstanceLinkID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return InstanceLinkID{}, fmt.Errorf("parse instance link id: %w", err)
	}
	return InstanceLinkID(u), nil
}
