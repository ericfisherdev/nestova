package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// IngredientID uniquely identifies a canonical ingredient in the shared
// ingredient catalogue.
type IngredientID uuid.UUID

// NewIngredientID returns a new time-ordered (UUIDv7) ingredient id, which gives
// better B-tree index locality than random v4 ids. uuid.NewV7 only errors if the
// crypto random source is unavailable — the same failure under which uuid.New
// itself panics — so Must is appropriate here, matching the household and notify
// id constructors.
func NewIngredientID() IngredientID { return IngredientID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id IngredientID) String() string { return uuid.UUID(id).String() }

// ParseIngredientID parses a canonical UUID string into an IngredientID.
func ParseIngredientID(s string) (IngredientID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return IngredientID{}, fmt.Errorf("parse ingredient id: %w", err)
	}
	return IngredientID(u), nil
}

// TrackedItemID uniquely identifies a tracked consumable item within a household.
type TrackedItemID uuid.UUID

// NewTrackedItemID returns a new time-ordered (UUIDv7) tracked-item id. See
// NewIngredientID for why Must is appropriate.
func NewTrackedItemID() TrackedItemID { return TrackedItemID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id TrackedItemID) String() string { return uuid.UUID(id).String() }

// ParseTrackedItemID parses a canonical UUID string into a TrackedItemID.
func ParseTrackedItemID(s string) (TrackedItemID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return TrackedItemID{}, fmt.Errorf("parse tracked item id: %w", err)
	}
	return TrackedItemID(u), nil
}

// UsageEventID uniquely identifies a single usage event.
type UsageEventID uuid.UUID

// NewUsageEventID returns a new time-ordered (UUIDv7) usage-event id. See
// NewIngredientID for why Must is appropriate.
func NewUsageEventID() UsageEventID { return UsageEventID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id UsageEventID) String() string { return uuid.UUID(id).String() }

// ParseUsageEventID parses a canonical UUID string into a UsageEventID.
func ParseUsageEventID(s string) (UsageEventID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return UsageEventID{}, fmt.Errorf("parse usage event id: %w", err)
	}
	return UsageEventID(u), nil
}

// PantryItemID uniquely identifies an on-hand pantry entry within a household.
type PantryItemID uuid.UUID

// NewPantryItemID returns a new time-ordered (UUIDv7) pantry-item id. See
// NewIngredientID for why Must is appropriate.
func NewPantryItemID() PantryItemID { return PantryItemID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id PantryItemID) String() string { return uuid.UUID(id).String() }

// ParsePantryItemID parses a canonical UUID string into a PantryItemID.
func ParsePantryItemID(s string) (PantryItemID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return PantryItemID{}, fmt.Errorf("parse pantry item id: %w", err)
	}
	return PantryItemID(u), nil
}
