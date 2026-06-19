package domain

import (
	"context"
	"errors"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ErrPantryItemNotFound is returned when a pantry item does not exist.
var ErrPantryItemNotFound = errors.New("tracking: pantry item not found")

// PantryItem is an on-hand quantity of an ingredient in a household's pantry —
// the source of truth for what is currently stocked. A household may hold
// several entries for the same ingredient (e.g. separate batches with different
// expiry dates). ExpiresOn is nil for items without a tracked expiry.
type PantryItem struct {
	ID           PantryItemID
	HouseholdID  household.HouseholdID
	IngredientID IngredientID
	Quantity     household.Quantity
	ExpiresOn    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PantryRepository persists pantry items.
//
// Persistence contracts (the caller sets identity and a valid Quantity; the
// store sets timestamps):
//   - Create expects ID, HouseholdID, IngredientID, and a valid Quantity set;
//     it populates CreatedAt/UpdatedAt.
//
// Error contracts:
//   - Get, Adjust, and Consume return ErrPantryItemNotFound when the id is
//     unknown.
//   - ListByHousehold and ListExpiringWithin return an empty slice (not an
//     error) when nothing matches.
type PantryRepository interface {
	Create(ctx context.Context, item *PantryItem) error
	Get(ctx context.Context, id PantryItemID) (*PantryItem, error)
	// Adjust increases an item's on-hand quantity by delta and returns the
	// updated item. Consume decreases it by amount. Both are scoped to householdID
	// (a foreign id yields ErrPantryItemNotFound, so a member cannot mutate another
	// household's item) and apply the change atomically under a row lock so
	// concurrent mutations cannot lose updates, reusing the shared Quantity
	// arithmetic (units must match — ErrUnitMismatch — and Consume cannot drop
	// below zero — ErrInvalidQuantity).
	Adjust(ctx context.Context, householdID household.HouseholdID, id PantryItemID, delta household.Quantity) (*PantryItem, error)
	Consume(ctx context.Context, householdID household.HouseholdID, id PantryItemID, amount household.Quantity) (*PantryItem, error)
	ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*PantryItem, error)
	// ListExpiringWithin returns items in the household whose ExpiresOn falls in
	// the window [asOf, asOf + days] — items that have an expiry and are expiring
	// soon but not already expired before asOf — ordered by ExpiresOn ascending.
	// asOf is injected so the query is deterministic and testable. Already-expired
	// items are out of scope here; callers wanting those derive them from
	// ListByHousehold.
	ListExpiringWithin(ctx context.Context, householdID household.HouseholdID, asOf time.Time, days int) ([]*PantryItem, error)
}
