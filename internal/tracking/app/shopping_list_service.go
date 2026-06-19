package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// ShoppingListService is the shopping-list use-case boundary for member-facing
// actions: adding manual items, transitioning their status, and listing by
// status. System restock generation is the restock automation's concern and uses
// the repository's AddRestockIfAbsent directly.
type ShoppingListService struct {
	items domain.ShoppingListRepository
}

// NewShoppingListService constructs the service with an injected repository.
func NewShoppingListService(items domain.ShoppingListRepository) (*ShoppingListService, error) {
	if items == nil {
		return nil, errors.New("app: NewShoppingListService requires a non-nil shopping list repository")
	}
	return &ShoppingListService{items: items}, nil
}

// AddManualItem adds a member-entered item in the needed state, identified by
// either a catalogue ingredient or a free-text name (exactly one). It returns
// domain.ErrInvalidShoppingListItem when that rule is broken or
// domain.ErrInvalidQuantity for an invalid quantity. addedBy is the member adding
// it (nil if unattributed).
func (s *ShoppingListService) AddManualItem(ctx context.Context, householdID household.HouseholdID, ingredientID *domain.IngredientID, name string, quantity household.Quantity, addedBy *household.MemberID) (*domain.ShoppingListItem, error) {
	item := &domain.ShoppingListItem{
		ID:           domain.NewShoppingListItemID(),
		HouseholdID:  householdID,
		IngredientID: ingredientID,
		Name:         strings.TrimSpace(name),
		Quantity:     quantity,
		Source:       domain.SourceManual,
		Status:       domain.StatusNeeded,
		AddedBy:      addedBy,
	}
	if err := item.Validate(); err != nil {
		return nil, err
	}
	if err := s.items.Add(ctx, item); err != nil {
		return nil, err
	}
	return item, nil
}

// TransitionStatus moves an item to status (needed/in_cart/purchased) within
// householdID, returning the updated item. It rejects an unknown status value and
// returns domain.ErrShoppingListItemNotFound when the id is unknown in the
// household.
func (s *ShoppingListService) TransitionStatus(ctx context.Context, householdID household.HouseholdID, itemID domain.ShoppingListItemID, status domain.ItemStatus) (*domain.ShoppingListItem, error) {
	if !status.Valid() {
		return nil, fmt.Errorf("transition status: invalid status %q", status)
	}
	return s.items.UpdateStatus(ctx, householdID, itemID, status)
}

// ListByStatus returns the household's items in the given status.
func (s *ShoppingListService) ListByStatus(ctx context.Context, householdID household.HouseholdID, status domain.ItemStatus) ([]*domain.ShoppingListItem, error) {
	if !status.Valid() {
		return nil, fmt.Errorf("list by status: invalid status %q", status)
	}
	return s.items.ListByStatus(ctx, householdID, status)
}
