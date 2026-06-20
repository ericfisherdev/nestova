package adapter

import (
	"context"
	"errors"
	"fmt"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// LocalRecipeSource is the recipe-box implementation of domain.RecipeSource: it
// ranks the household's own recipes against the on-hand ingredients. It composes
// the RecipeRepository (which loads recipes with their ingredient lines) and the
// shared MatchByIngredients ranking, so the query and the scoring each have a
// single home.
type LocalRecipeSource struct {
	recipes domain.RecipeRepository
}

// Compile-time assurance the source satisfies the port.
var _ domain.RecipeSource = (*LocalRecipeSource)(nil)

// NewLocalRecipeSource constructs the source with an injected recipe repository.
func NewLocalRecipeSource(recipes domain.RecipeRepository) (*LocalRecipeSource, error) {
	if recipes == nil {
		return nil, errors.New("adapter: NewLocalRecipeSource requires a non-nil recipe repository")
	}
	return &LocalRecipeSource{recipes: recipes}, nil
}

// FindByIngredients ranks the household's box recipes by ingredient overlap with
// have, dropping recipes the cook cannot make any of and ordering the rest by
// match percentage then title.
func (s *LocalRecipeSource) FindByIngredients(ctx context.Context, householdID household.HouseholdID, have []tracking.IngredientID) ([]domain.RecipeMatch, error) {
	recipes, err := s.recipes.ListByHousehold(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("local recipe source: %w", err)
	}
	return domain.MatchByIngredients(recipes, have), nil
}
