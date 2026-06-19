package app

import (
	"context"
	"errors"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// PantryService is the pantry use-case boundary. Quantity mutations route through
// the shared Quantity value object, so unit mismatches and below-zero results are
// rejected (ErrUnitMismatch / ErrInvalidQuantity) without duplicated checks here.
type PantryService struct {
	pantry domain.PantryRepository
}

// NewPantryService constructs the service with an injected pantry repository.
func NewPantryService(pantry domain.PantryRepository) (*PantryService, error) {
	if pantry == nil {
		return nil, errors.New("app: NewPantryService requires a non-nil pantry repository")
	}
	return &PantryService{pantry: pantry}, nil
}

// Add stores a new pantry item with the given quantity and optional expiry,
// rejecting an invalid quantity with ErrInvalidQuantity.
func (s *PantryService) Add(ctx context.Context, householdID household.HouseholdID, ingredientID domain.IngredientID, quantity household.Quantity, expiresOn *time.Time) (*domain.PantryItem, error) {
	if err := quantity.Validate(); err != nil {
		return nil, err
	}
	item := &domain.PantryItem{
		ID:           domain.NewPantryItemID(),
		HouseholdID:  householdID,
		IngredientID: ingredientID,
		Quantity:     quantity,
		ExpiresOn:    expiresOn,
	}
	if err := s.pantry.Create(ctx, item); err != nil {
		return nil, err
	}
	return item, nil
}

// Adjust increases an item's on-hand quantity by delta (e.g. a restock). The
// repository applies the change atomically; units must match or it returns
// ErrUnitMismatch.
func (s *PantryService) Adjust(ctx context.Context, itemID domain.PantryItemID, delta household.Quantity) (*domain.PantryItem, error) {
	if err := delta.Validate(); err != nil {
		return nil, err
	}
	return s.pantry.Adjust(ctx, itemID, delta)
}

// Consume decreases an item's on-hand quantity by amount (e.g. using some up).
// The repository applies the change atomically; units must match (ErrUnitMismatch)
// and the result must not drop below zero (ErrInvalidQuantity).
func (s *PantryService) Consume(ctx context.Context, itemID domain.PantryItemID, amount household.Quantity) (*domain.PantryItem, error) {
	if err := amount.Validate(); err != nil {
		return nil, err
	}
	return s.pantry.Consume(ctx, itemID, amount)
}

// List returns all of the household's pantry items.
func (s *PantryService) List(ctx context.Context, householdID household.HouseholdID) ([]*domain.PantryItem, error) {
	return s.pantry.ListByHousehold(ctx, householdID)
}

// ListExpiringWithin returns the household's items expiring on or before
// asOf + days, ordered by expiry.
func (s *PantryService) ListExpiringWithin(ctx context.Context, householdID household.HouseholdID, asOf time.Time, days int) ([]*domain.PantryItem, error) {
	return s.pantry.ListExpiringWithin(ctx, householdID, asOf, days)
}
