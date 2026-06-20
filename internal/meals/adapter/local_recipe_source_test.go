package adapter_test

import (
	"context"
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// erroringRecipeRepo is a domain.RecipeRepository whose reads fail, used to verify
// the source surfaces repository errors.
type erroringRecipeRepo struct{ err error }

func (r erroringRecipeRepo) Create(context.Context, *domain.Recipe) error { return r.err }
func (r erroringRecipeRepo) Update(context.Context, *domain.Recipe) error { return r.err }
func (r erroringRecipeRepo) UpsertExternal(context.Context, *domain.Recipe) (*domain.Recipe, error) {
	return nil, r.err
}

func (r erroringRecipeRepo) Get(context.Context, household.HouseholdID, domain.RecipeID) (*domain.Recipe, error) {
	return nil, r.err
}

func (r erroringRecipeRepo) Delete(context.Context, household.HouseholdID, domain.RecipeID) error {
	return r.err
}

func (r erroringRecipeRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*domain.Recipe, error) {
	return nil, r.err
}

func TestLocalRecipeSourceRejectsNilRepository(t *testing.T) {
	if _, err := adapter.NewLocalRecipeSource(nil); err == nil {
		t.Error("NewLocalRecipeSource(nil) = nil error, want error")
	}
}

func TestLocalRecipeSourcePropagatesRepoError(t *testing.T) {
	sentinel := errors.New("list failed")
	src, err := adapter.NewLocalRecipeSource(erroringRecipeRepo{err: sentinel})
	if err != nil {
		t.Fatalf("NewLocalRecipeSource: %v", err)
	}
	if _, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), nil); !errors.Is(err, sentinel) {
		t.Errorf("FindByIngredients error = %v, want wrapped %v", err, sentinel)
	}
}

func TestLocalRecipeSourceRanksHouseholdRecipes(t *testing.T) {
	pool := newTestPool(t)
	ctx := testCtx(t)
	ours := seedHousehold(t, pool)
	theirs := seedHousehold(t, pool)
	flour := seedIngredient(t, pool, "flour")
	eggs := seedIngredient(t, pool, "eggs")
	milk := seedIngredient(t, pool, "milk")

	repo := adapter.NewRecipeRepository(pool)
	if err := repo.Create(ctx, newLocalRecipe(ours, "Pancakes",
		line(flour, 200, household.UnitGram, false), line(eggs, 2, household.UnitCount, false))); err != nil {
		t.Fatalf("Create Pancakes: %v", err)
	}
	if err := repo.Create(ctx, newLocalRecipe(ours, "Scramble",
		line(eggs, 3, household.UnitCount, false), line(milk, 50, household.UnitMilliliter, false))); err != nil {
		t.Fatalf("Create Scramble: %v", err)
	}
	// A recipe we have none of, and a foreign household's recipe.
	if err := repo.Create(ctx, newLocalRecipe(ours, "Milkshake", line(milk, 1, household.UnitCount, false))); err != nil {
		t.Fatalf("Create Milkshake: %v", err)
	}
	if err := repo.Create(ctx, newLocalRecipe(theirs, "Their Omelette", line(eggs, 2, household.UnitCount, false))); err != nil {
		t.Fatalf("Create foreign: %v", err)
	}

	src, err := adapter.NewLocalRecipeSource(repo)
	if err != nil {
		t.Fatalf("NewLocalRecipeSource: %v", err)
	}

	matches, err := src.FindByIngredients(ctx, ours, []tracking.IngredientID{flour, eggs})
	if err != nil {
		t.Fatalf("FindByIngredients: %v", err)
	}
	// Pancakes (flour+eggs) = 1.0; Scramble (eggs have, milk missing) = 0.5;
	// Milkshake (milk only, none on hand) excluded; the foreign recipe is never seen.
	if len(matches) != 2 {
		t.Fatalf("matches = %d, want 2 (zero-match and foreign excluded)", len(matches))
	}
	if matches[0].Recipe.Title != "Pancakes" || matches[0].MatchPct != 1.0 {
		t.Errorf("rank 0 = %q/%v, want Pancakes/1.0", matches[0].Recipe.Title, matches[0].MatchPct)
	}
	if matches[1].Recipe.Title != "Scramble" || matches[1].MatchPct != 0.5 {
		t.Errorf("rank 1 = %q/%v, want Scramble/0.5", matches[1].Recipe.Title, matches[1].MatchPct)
	}
	if len(matches[1].Missing) != 1 || matches[1].Missing[0] != milk {
		t.Errorf("Scramble missing = %v, want [milk]", matches[1].Missing)
	}
}
