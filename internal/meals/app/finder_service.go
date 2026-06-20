package app

import (
	"context"
	"errors"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// FinderService is the "what can I make?" use-case boundary. It assembles the cook's
// on-hand ingredient set — from the pantry (the default) or from an ad-hoc list of
// ingredient names — and delegates ranking to the injected RecipeSource, so the
// finder is agnostic to which source (local, external, or hybrid) produced the
// matches.
type FinderService struct {
	source      domain.RecipeSource
	pantry      tracking.PantryRepository
	ingredients tracking.IngredientEnsurer
}

// NewFinderService constructs the service with an injected recipe source, pantry
// repository, and ingredient ensurer.
func NewFinderService(source domain.RecipeSource, pantry tracking.PantryRepository, ingredients tracking.IngredientEnsurer) (*FinderService, error) {
	if source == nil {
		return nil, errors.New("app: NewFinderService requires a non-nil recipe source")
	}
	if pantry == nil {
		return nil, errors.New("app: NewFinderService requires a non-nil pantry repository")
	}
	if ingredients == nil {
		return nil, errors.New("app: NewFinderService requires a non-nil ingredient ensurer")
	}
	return &FinderService{source: source, pantry: pantry, ingredients: ingredients}, nil
}

// FindFromPantry ranks recipes against the household's current pantry contents.
// A household may stock several entries for the same ingredient, so the ingredient
// ids are de-duplicated before ranking.
func (s *FinderService) FindFromPantry(ctx context.Context, householdID household.HouseholdID) ([]domain.RecipeMatch, error) {
	items, err := s.pantry.ListByHousehold(ctx, householdID)
	if err != nil {
		return nil, err
	}
	have := make([]tracking.IngredientID, 0, len(items))
	seen := make(map[tracking.IngredientID]struct{}, len(items))
	for _, item := range items {
		if _, ok := seen[item.IngredientID]; ok {
			continue
		}
		seen[item.IngredientID] = struct{}{}
		have = append(have, item.IngredientID)
	}
	return s.source.FindByIngredients(ctx, householdID, have)
}

// FindFromIngredients ranks recipes against an ad-hoc list of ingredient names,
// each normalized to the shared catalogue (and de-duplicated) before ranking. A
// blank name yields tracking.ErrInvalidIngredient.
func (s *FinderService) FindFromIngredients(ctx context.Context, householdID household.HouseholdID, names []string) ([]domain.RecipeMatch, error) {
	have := make([]tracking.IngredientID, 0, len(names))
	seen := make(map[tracking.IngredientID]struct{}, len(names))
	for _, name := range names {
		ingredient, err := s.ingredients.EnsureIngredient(ctx, name)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[ingredient.ID]; ok {
			continue
		}
		seen[ingredient.ID] = struct{}{}
		have = append(have, ingredient.ID)
	}
	return s.source.FindByIngredients(ctx, householdID, have)
}
