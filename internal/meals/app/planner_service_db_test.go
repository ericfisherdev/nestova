package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

// TestPlannerAssignReadClearRoundTrip exercises the planner over the real
// repositories: a box recipe is assigned to a slot, read back in the week's plan,
// then cleared.
func TestPlannerAssignReadClearRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hh := seedHousehold(t, pool)

	recipeRepo := mealsadapter.NewRecipeRepository(pool)
	planRepo := mealsadapter.NewMealPlanRepository(pool)
	recipe := &domain.Recipe{ID: domain.NewRecipeID(), HouseholdID: &hh, Title: "Bread", Source: domain.SourceLocal, Servings: 2}
	if err := recipeRepo.Create(ctx, recipe); err != nil {
		t.Fatalf("Create recipe: %v", err)
	}

	svc, err := app.NewPlannerService(planRepo, recipeRepo)
	if err != nil {
		t.Fatalf("NewPlannerService: %v", err)
	}

	day := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	weekStart := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC) // Sunday
	if err := svc.AssignMeal(ctx, hh, day, domain.MealDinner, recipe.ID, 4); err != nil {
		t.Fatalf("AssignMeal: %v", err)
	}

	week, err := svc.PlanForWeek(ctx, hh, weekStart)
	if err != nil {
		t.Fatalf("PlanForWeek: %v", err)
	}
	if len(week) != 1 || week[0].RecipeID != recipe.ID || week[0].Meal != domain.MealDinner || week[0].Servings != 4 || !week[0].Date.Equal(day) {
		t.Fatalf("week = %+v, want one dinner of Bread on the 24th with 4 servings", week)
	}

	if err := svc.ClearMeal(ctx, hh, day, domain.MealDinner); err != nil {
		t.Fatalf("ClearMeal: %v", err)
	}
	week, err = svc.PlanForWeek(ctx, hh, weekStart)
	if err != nil {
		t.Fatalf("PlanForWeek after clear: %v", err)
	}
	if len(week) != 0 {
		t.Errorf("week after clear = %d entries, want 0", len(week))
	}
}

// TestPlannerRejectsForeignRecipeAtDB confirms the household scope holds end-to-end:
// a recipe owned by another household cannot be planned.
func TestPlannerRejectsForeignRecipeAtDB(t *testing.T) {
	pool := newTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := seedHousehold(t, pool)
	other := seedHousehold(t, pool)

	recipeRepo := mealsadapter.NewRecipeRepository(pool)
	planRepo := mealsadapter.NewMealPlanRepository(pool)
	foreign := &domain.Recipe{ID: domain.NewRecipeID(), HouseholdID: &other, Title: "Theirs", Source: domain.SourceLocal, Servings: 2}
	if err := recipeRepo.Create(ctx, foreign); err != nil {
		t.Fatalf("Create foreign recipe: %v", err)
	}

	svc, err := app.NewPlannerService(planRepo, recipeRepo)
	if err != nil {
		t.Fatalf("NewPlannerService: %v", err)
	}
	day := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	if err := svc.AssignMeal(ctx, owner, day, domain.MealDinner, foreign.ID, 2); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("AssignMeal(foreign recipe) = %v, want ErrRecipeNotFound", err)
	}
}
