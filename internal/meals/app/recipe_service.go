package app

import (
	"context"
	"errors"
	"strings"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// IngredientLine is a member-entered recipe ingredient line before normalization:
// a free-text Name resolved to the shared catalogue, plus an amount, unit, and
// whether the ingredient is optional.
type IngredientLine struct {
	Name     string
	Amount   float64
	Unit     household.Unit
	Optional bool
}

// RecipeInput is the data for creating or editing a box recipe.
type RecipeInput struct {
	Title        string
	Servings     int
	Instructions string
	Ingredients  []IngredientLine
}

// RecipeService is the recipe-box use-case boundary: members create, edit, delete,
// and list their household's recipes, with each ingredient line normalized to the
// shared catalogue (NES-38).
type RecipeService struct {
	recipes     domain.RecipeRepository
	ingredients tracking.IngredientEnsurer
}

// NewRecipeService constructs the service with an injected recipe repository and
// ingredient ensurer.
func NewRecipeService(recipes domain.RecipeRepository, ingredients tracking.IngredientEnsurer) (*RecipeService, error) {
	if recipes == nil {
		return nil, errors.New("app: NewRecipeService requires a non-nil recipe repository")
	}
	if ingredients == nil {
		return nil, errors.New("app: NewRecipeService requires a non-nil ingredient ensurer")
	}
	return &RecipeService{recipes: recipes, ingredients: ingredients}, nil
}

// CreateRecipe normalizes each ingredient line's name to the catalogue, builds the
// box recipe, and persists it with its lines. It returns tracking.ErrInvalidIngredient
// for a blank ingredient name, household.ErrInvalidQuantity for an invalid amount,
// and domain.ErrInvalidRecipe for a blank title or non-positive servings.
func (s *RecipeService) CreateRecipe(ctx context.Context, householdID household.HouseholdID, in RecipeInput) (*domain.Recipe, error) {
	recipe, err := s.buildRecipe(ctx, domain.NewRecipeID(), householdID, in)
	if err != nil {
		return nil, err
	}
	if err := s.recipes.Create(ctx, recipe); err != nil {
		return nil, err
	}
	return recipe, nil
}

// EditRecipe rewrites a box recipe's fields and replaces its ingredient set
// (re-normalizing each line). It returns domain.ErrRecipeNotFound when the id is
// unknown in the household, plus the same validation errors as CreateRecipe.
func (s *RecipeService) EditRecipe(ctx context.Context, householdID household.HouseholdID, recipeID domain.RecipeID, in RecipeInput) (*domain.Recipe, error) {
	recipe, err := s.buildRecipe(ctx, recipeID, householdID, in)
	if err != nil {
		return nil, err
	}
	if err := s.recipes.Update(ctx, recipe); err != nil {
		return nil, err
	}
	return recipe, nil
}

// DeleteRecipe removes a box recipe (and its lines), returning
// domain.ErrRecipeNotFound when the id is unknown in the household.
func (s *RecipeService) DeleteRecipe(ctx context.Context, householdID household.HouseholdID, recipeID domain.RecipeID) error {
	return s.recipes.Delete(ctx, householdID, recipeID)
}

// ListRecipeBox returns the household's box recipes ordered by title.
func (s *RecipeService) ListRecipeBox(ctx context.Context, householdID household.HouseholdID) ([]*domain.Recipe, error) {
	return s.recipes.ListByHousehold(ctx, householdID)
}

// buildRecipe normalizes the input's ingredient lines to the catalogue and
// assembles a validated local recipe with the given id.
func (s *RecipeService) buildRecipe(ctx context.Context, id domain.RecipeID, householdID household.HouseholdID, in RecipeInput) (*domain.Recipe, error) {
	ingredients, err := s.normalizeLines(ctx, in.Ingredients)
	if err != nil {
		return nil, err
	}
	recipe := &domain.Recipe{
		ID:           id,
		HouseholdID:  &householdID,
		Title:        strings.TrimSpace(in.Title),
		Source:       domain.SourceLocal,
		Servings:     in.Servings,
		Instructions: in.Instructions,
		Ingredients:  ingredients,
	}
	if err := recipe.Validate(); err != nil {
		return nil, err
	}
	return recipe, nil
}

// normalizeLines pairs each line with a validated Quantity and resolves its
// free-text name to a catalogue id (a race-safe upsert). Quantity is validated
// before EnsureIngredient so a rejected line never mutates the shared catalogue;
// a blank name is rejected by EnsureIngredient itself without a catalogue write.
func (s *RecipeService) normalizeLines(ctx context.Context, lines []IngredientLine) ([]domain.RecipeIngredient, error) {
	out := make([]domain.RecipeIngredient, 0, len(lines))
	for _, line := range lines {
		quantity, err := household.NewQuantity(line.Amount, line.Unit)
		if err != nil {
			return nil, err
		}
		ingredient, err := s.ingredients.EnsureIngredient(ctx, line.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.RecipeIngredient{
			IngredientID: ingredient.ID,
			Quantity:     quantity,
			Optional:     line.Optional,
		})
	}
	return out, nil
}
