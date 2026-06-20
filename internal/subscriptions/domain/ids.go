package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// SubscriptionID uniquely identifies a recurring subscription.
type SubscriptionID uuid.UUID

// NewSubscriptionID returns a new time-ordered (UUIDv7) subscription id, which
// gives better B-tree index locality than random v4 ids. uuid.NewV7 only errors
// if the crypto random source is unavailable — the same failure under which
// uuid.New itself panics — so Must is appropriate here.
func NewSubscriptionID() SubscriptionID { return SubscriptionID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id SubscriptionID) String() string { return uuid.UUID(id).String() }

// ParseSubscriptionID parses a canonical UUID string into a SubscriptionID.
func ParseSubscriptionID(s string) (SubscriptionID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return SubscriptionID{}, fmt.Errorf("parse subscription id: %w", err)
	}
	return SubscriptionID(u), nil
}
