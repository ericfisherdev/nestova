package domain

import (
	"context"
	"errors"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ErrTrackedItemNotFound is returned when a tracked item does not exist.
var ErrTrackedItemNotFound = errors.New("tracking: tracked item not found")

// ErrTrackedItemNameTooLong is returned by UsageService.RegisterItem when the
// name exceeds MaxTrackedItemNameLength runes (NES-194). The restock scheduler
// resolves a tracked item's name to a catalogue ingredient, so the bound must
// not exceed MaxIngredientNameLength.
var ErrTrackedItemNameTooLong = errors.New("tracking: tracked item name is too long")

// MaxTrackedItemNameLength bounds a tracked item's name, counted in runes. The
// tracked_item.name CHECK in 00045_text_field_length_caps.sql carries the same
// number.
const MaxTrackedItemNameLength = MaxIngredientNameLength

// TrackedItem is a household consumable whose usage is tracked to predict when
// it needs restocking. RestockLeadDays is how many days before predicted
// depletion the item should appear on the shopping list. Inactive items are
// retained for history but excluded from active listings and restock runs.
type TrackedItem struct {
	ID              TrackedItemID
	HouseholdID     household.HouseholdID
	Name            string
	Category        string
	RestockLeadDays int
	Active          bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TrackedItemRepository persists tracked items.
//
// Persistence contracts (the caller sets identity and valid field values; the
// store sets timestamps):
//   - Create expects ID, HouseholdID, and a non-empty Name; it populates
//     CreatedAt/UpdatedAt.
//   - Update expects an existing ID and rewrites Name, Category, RestockLeadDays,
//     and Active; it refreshes UpdatedAt.
//
// Error contracts:
//   - Get and Update return ErrTrackedItemNotFound when the id is unknown.
//   - ListActiveByHousehold and ListDueForRestock return an empty slice (not an
//     error) when nothing matches.
type TrackedItemRepository interface {
	Create(ctx context.Context, item *TrackedItem) error
	Get(ctx context.Context, id TrackedItemID) (*TrackedItem, error)
	Update(ctx context.Context, item *TrackedItem) error
	ListActiveByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*TrackedItem, error)
	// ListAllActive returns every active tracked item across all households,
	// ordered by household then name. The restock scheduler iterates these to
	// recompute predictions and raise restock entries.
	ListAllActive(ctx context.Context) ([]*TrackedItem, error)
	// ListDueForRestock returns active items in the household whose cached
	// prediction puts predicted depletion within the item's lead window of asOf
	// (predicted_depletion_on <= asOf + RestockLeadDays). asOf is injected so the
	// query is deterministic and testable.
	ListDueForRestock(ctx context.Context, householdID household.HouseholdID, asOf time.Time) ([]*TrackedItem, error)
}
