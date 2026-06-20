package app_test

import (
	"context"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	trackingadapter "github.com/ericfisherdev/nestova/internal/tracking/adapter"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// TestFinderServiceFromPantryDBRoundTrip wires the real local source, recipe box,
// pantry, and catalogue: a recipe needs flour + eggs, the pantry stocks only
// flour, so the finder returns it at a 50% match with eggs missing.
func TestFinderServiceFromPantryDBRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hh := seedHousehold(t, pool)

	recipeRepo := mealsadapter.NewRecipeRepository(pool)
	ingredientRepo := trackingadapter.NewIngredientRepository(pool)
	pantryRepo := trackingadapter.NewPantryRepository(pool)

	recipeSvc, err := app.NewRecipeService(recipeRepo, ingredientRepo)
	if err != nil {
		t.Fatalf("NewRecipeService: %v", err)
	}
	if _, err := recipeSvc.CreateRecipe(ctx, hh, app.RecipeInput{
		Title: "Pancakes", Servings: 2,
		Ingredients: []app.IngredientLine{
			line("flour", 200, household.UnitGram, false),
			line("eggs", 2, household.UnitCount, false),
		},
	}); err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}

	// Stock only flour in the pantry.
	flour, err := ingredientRepo.EnsureIngredient(ctx, "flour")
	if err != nil {
		t.Fatalf("EnsureIngredient flour: %v", err)
	}
	if err := pantryRepo.Create(ctx, &tracking.PantryItem{
		ID: tracking.NewPantryItemID(), HouseholdID: hh, IngredientID: flour.ID,
		Quantity: household.Quantity{Amount: 500, Unit: household.UnitGram},
	}); err != nil {
		t.Fatalf("pantry Create: %v", err)
	}

	localSource, err := mealsadapter.NewLocalRecipeSource(recipeRepo)
	if err != nil {
		t.Fatalf("NewLocalRecipeSource: %v", err)
	}
	finder, err := app.NewFinderService(localSource, pantryRepo, ingredientRepo)
	if err != nil {
		t.Fatalf("NewFinderService: %v", err)
	}

	matches, err := finder.FindFromPantry(ctx, hh)
	if err != nil {
		t.Fatalf("FindFromPantry: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if matches[0].Recipe.Title != "Pancakes" || matches[0].MatchPct != 0.5 {
		t.Errorf("match = %q/%v, want Pancakes/0.5", matches[0].Recipe.Title, matches[0].MatchPct)
	}
	eggs, err := ingredientRepo.EnsureIngredient(ctx, "eggs")
	if err != nil {
		t.Fatalf("EnsureIngredient eggs: %v", err)
	}
	if len(matches[0].Missing) != 1 || matches[0].Missing[0] != eggs.ID {
		t.Errorf("missing = %v, want [eggs]", matches[0].Missing)
	}
}
