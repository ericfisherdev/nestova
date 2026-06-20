package app

import (
	"context"
	"errors"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

// PlannerService is the weekly meal-planner use-case boundary: members assign their
// box recipes to (date, meal) slots, read a week's plan, and clear a slot.
type PlannerService struct {
	plans   domain.MealPlanRepository
	recipes domain.RecipeRepository
}

// NewPlannerService constructs the service with an injected meal-plan repository and
// recipe repository.
func NewPlannerService(plans domain.MealPlanRepository, recipes domain.RecipeRepository) (*PlannerService, error) {
	if plans == nil {
		return nil, errors.New("app: NewPlannerService requires a non-nil meal plan repository")
	}
	if recipes == nil {
		return nil, errors.New("app: NewPlannerService requires a non-nil recipe repository")
	}
	return &PlannerService{plans: plans, recipes: recipes}, nil
}

// AssignMeal assigns recipeID to the (householdID, date, meal) slot, replacing any
// existing entry there. It returns domain.ErrInvalidMealPlanEntry for an unknown
// meal or non-positive servings and domain.ErrRecipeNotFound when the recipe is not
// one of the household's box recipes. The slot — (date, meal) — is the identity, so
// a re-assignment updates the existing row in place; callers read the resulting
// plan back with PlanForWeek rather than relying on a returned entry.
func (s *PlannerService) AssignMeal(ctx context.Context, householdID household.HouseholdID, date time.Time, meal domain.Meal, recipeID domain.RecipeID, servings int) error {
	entry := &domain.MealPlanEntry{
		ID:          domain.NewMealPlanEntryID(),
		HouseholdID: householdID,
		Date:        date,
		Meal:        meal,
		RecipeID:    recipeID,
		Servings:    servings,
	}
	if err := entry.Validate(); err != nil {
		return err
	}
	// Confirm the recipe is a box recipe of this household before planning it; Get
	// is household-scoped, so a foreign or external recipe yields ErrRecipeNotFound.
	if _, err := s.recipes.Get(ctx, householdID, recipeID); err != nil {
		return err
	}
	return s.plans.Upsert(ctx, entry)
}

// PlanForWeek returns the household's plan for the seven days starting at weekStart
// (inclusive), ordered by date then meal (daily order). weekStart is supplied by
// the caller so the window is deterministic.
func (s *PlannerService) PlanForWeek(ctx context.Context, householdID household.HouseholdID, weekStart time.Time) ([]*domain.MealPlanEntry, error) {
	weekEnd := weekStart.AddDate(0, 0, 6)
	return s.plans.ListByDateRange(ctx, householdID, weekStart, weekEnd)
}

// ClearMeal removes the entry in the (householdID, date, meal) slot, returning
// domain.ErrMealPlanEntryNotFound when the slot is already empty.
func (s *PlannerService) ClearMeal(ctx context.Context, householdID household.HouseholdID, date time.Time, meal domain.Meal) error {
	return s.plans.Delete(ctx, householdID, date, meal)
}
