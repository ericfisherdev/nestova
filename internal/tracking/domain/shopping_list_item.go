package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Shopping-list item errors.
var (
	// ErrShoppingListItemNotFound is returned when a shopping-list item does not exist.
	ErrShoppingListItemNotFound = errors.New("tracking: shopping list item not found")
	// ErrInvalidShoppingListItem is returned when an item is not identified by
	// exactly one of an ingredient or a free-text name.
	ErrInvalidShoppingListItem = errors.New("tracking: shopping list item must have exactly one of ingredient or name")
)

// ShoppingListItem is one entry on a household's unified shopping list. It is
// identified by either a catalogue IngredientID (system-generated or a catalogue
// pick) or a free-text Name (ad-hoc manual entry) — exactly one is set. Source
// records its origin and Status its lifecycle (needed → in_cart → purchased).
// AddedBy is the member who added a manual item, nil for system-generated ones.
type ShoppingListItem struct {
	ID           ShoppingListItemID
	HouseholdID  household.HouseholdID
	IngredientID *IngredientID
	Name         string
	Quantity     household.Quantity
	Source       ItemSource
	Status       ItemStatus
	AddedBy      *household.MemberID
	CreatedAt    time.Time
}

// Validate reports whether the item is well-formed: identified by exactly one of
// IngredientID or a non-blank Name (ErrInvalidShoppingListItem otherwise), with a
// valid Quantity (ErrInvalidQuantity) and known Source and Status.
func (i *ShoppingListItem) Validate() error {
	hasIngredient := i.IngredientID != nil
	hasName := strings.TrimSpace(i.Name) != ""
	if hasIngredient == hasName {
		return ErrInvalidShoppingListItem
	}
	if err := i.Quantity.Validate(); err != nil {
		return err
	}
	if !i.Source.Valid() {
		return fmt.Errorf("shopping list item: invalid source %q", i.Source)
	}
	if !i.Status.Valid() {
		return fmt.Errorf("shopping list item: invalid status %q", i.Status)
	}
	return nil
}

// ShoppingListRepository persists shopping-list items.
//
// Contracts:
//   - Add inserts an item (the caller sets ID, HouseholdID, exactly one of
//     IngredientID/Name, a valid Quantity, Source, and Status); it populates
//     CreatedAt.
//   - AddRestockIfAbsent inserts a system restock item idempotently: it requires
//     Source == SourceRestock and a non-nil IngredientID, and inserts only when
//     no open (non-purchased) restock entry already exists for that
//     (household, ingredient). It reports whether a new row was inserted.
//   - AddMealPlanIfAbsent inserts a meal-plan-sourced item idempotently: it
//     requires Source == SourceMealPlan and a non-nil IngredientID (an error is
//     returned otherwise, as for a database failure), and inserts only when no open
//     (non-purchased) meal_plan entry already exists for that (household,
//     ingredient). It reports whether a new row was inserted, so a week's
//     plan-to-grocery generation can be re-run without duplicating items.
//   - UpdateStatus transitions an item's status within householdID and returns
//     the updated item, or ErrShoppingListItemNotFound when the id is unknown in
//     that household (so a member cannot transition another household's item).
//   - ListByStatus returns the household's items in the given status ordered by
//     creation, or an empty slice when none match.
type ShoppingListRepository interface {
	Add(ctx context.Context, item *ShoppingListItem) error
	AddRestockIfAbsent(ctx context.Context, item *ShoppingListItem) (inserted bool, err error)
	AddMealPlanIfAbsent(ctx context.Context, item *ShoppingListItem) (inserted bool, err error)
	UpdateStatus(ctx context.Context, householdID household.HouseholdID, id ShoppingListItemID, status ItemStatus) (*ShoppingListItem, error)
	ListByStatus(ctx context.Context, householdID household.HouseholdID, status ItemStatus) ([]*ShoppingListItem, error)
}
