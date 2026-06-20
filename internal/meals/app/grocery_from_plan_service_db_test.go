package app_test

import (
	"context"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	trackingadapter "github.com/ericfisherdev/nestova/internal/tracking/adapter"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// TestGenerateFromWeekDBRoundTrip wires the real planner, recipe box, catalogue,
// and shopping list: a planned recipe's ingredients land on the shopping list as
// meal_plan items, and a second generation for the same plan is a no-op.
func TestGenerateFromWeekDBRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hh := seedHousehold(t, pool)

	recipeRepo := mealsadapter.NewRecipeRepository(pool)
	planRepo := mealsadapter.NewMealPlanRepository(pool)
	ingredientRepo := trackingadapter.NewIngredientRepository(pool)
	shoppingRepo := trackingadapter.NewShoppingListRepository(pool)

	flour, err := ingredientRepo.EnsureIngredient(ctx, "flour")
	if err != nil {
		t.Fatalf("EnsureIngredient: %v", err)
	}
	recipe := &domain.Recipe{
		ID: domain.NewRecipeID(), HouseholdID: &hh, Title: "Bread", Source: domain.SourceLocal, Servings: 2,
		Ingredients: []domain.RecipeIngredient{{IngredientID: flour.ID, Quantity: household.Quantity{Amount: 100, Unit: household.UnitGram}}},
	}
	if err := recipeRepo.Create(ctx, recipe); err != nil {
		t.Fatalf("Create recipe: %v", err)
	}

	day := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	weekStart := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if err := planRepo.Upsert(ctx, &domain.MealPlanEntry{
		ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: day, Meal: domain.MealDinner, RecipeID: recipe.ID, Servings: 4,
	}); err != nil {
		t.Fatalf("Upsert plan entry: %v", err)
	}

	svc, err := app.NewGroceryFromPlanService(planRepo, recipeRepo, shoppingRepo)
	if err != nil {
		t.Fatalf("NewGroceryFromPlanService: %v", err)
	}

	added, err := svc.GenerateFromWeek(ctx, hh, weekStart)
	if err != nil {
		t.Fatalf("GenerateFromWeek: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}

	needed, err := shoppingRepo.ListByStatus(ctx, hh, tracking.StatusNeeded)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	mealPlanItems := 0
	for _, item := range needed {
		if item.Source != tracking.SourceMealPlan {
			continue
		}
		mealPlanItems++
		// 4 servings of a 2-serving recipe scales 100 g flour to 200 g.
		if item.IngredientID == nil || *item.IngredientID != flour.ID || item.Quantity.Amount != 200 || item.Quantity.Unit != household.UnitGram {
			t.Errorf("meal_plan item = %+v, want 200 g of flour", item)
		}
	}
	if mealPlanItems != 1 {
		t.Fatalf("meal_plan shopping items = %d, want 1", mealPlanItems)
	}

	// A second generation for the same plan adds nothing.
	added, err = svc.GenerateFromWeek(ctx, hh, weekStart)
	if err != nil {
		t.Fatalf("GenerateFromWeek re-run: %v", err)
	}
	if added != 0 {
		t.Errorf("re-run added = %d, want 0 (idempotent)", added)
	}
}
