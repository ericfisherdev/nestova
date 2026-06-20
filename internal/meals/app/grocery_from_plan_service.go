package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// daysInWeek is the meal-planner window length; PlanForWeek / GenerateFromWeek
// span weekStart through weekStart + (daysInWeek - 1), inclusive.
const daysInWeek = 7

// GroceryFromPlanService turns a week's meal plan into shopping-list items: it
// aggregates the planned recipes' ingredients (scaled by servings) and adds them
// to the shopping list as meal-plan-sourced items, idempotently — re-running a
// week's generation adds nothing new.
type GroceryFromPlanService struct {
	plans    domain.MealPlanRepository
	recipes  domain.RecipeRepository
	shopping tracking.ShoppingListRepository
}

// NewGroceryFromPlanService constructs the service with an injected meal-plan
// repository, recipe repository, and shopping-list repository.
func NewGroceryFromPlanService(plans domain.MealPlanRepository, recipes domain.RecipeRepository, shopping tracking.ShoppingListRepository) (*GroceryFromPlanService, error) {
	if plans == nil {
		return nil, errors.New("app: NewGroceryFromPlanService requires a non-nil meal plan repository")
	}
	if recipes == nil {
		return nil, errors.New("app: NewGroceryFromPlanService requires a non-nil recipe repository")
	}
	if shopping == nil {
		return nil, errors.New("app: NewGroceryFromPlanService requires a non-nil shopping list repository")
	}
	return &GroceryFromPlanService{plans: plans, recipes: recipes, shopping: shopping}, nil
}

// aggregateKey groups planned ingredient amounts by ingredient and unit, so
// same-unit amounts sum while differing units stay separate lines (Quantity does
// no unit conversion).
type aggregateKey struct {
	ingredient tracking.IngredientID
	unit       household.Unit
}

// GenerateFromWeek aggregates the week's planned recipe ingredients (scaled by each
// entry's servings relative to the recipe's yield) and adds them to the shopping
// list as meal_plan items, returning how many new items were inserted. All planned
// ingredients are added (pantry-aware netting is out of scope); generation is
// idempotent per (household, ingredient), so a second run for the same plan inserts
// nothing.
func (s *GroceryFromPlanService) GenerateFromWeek(ctx context.Context, householdID household.HouseholdID, weekStart time.Time) (int, error) {
	entries, err := s.plans.ListByDateRange(ctx, householdID, weekStart, weekStart.AddDate(0, 0, daysInWeek-1))
	if err != nil {
		return 0, err
	}

	totals := make(map[aggregateKey]household.Quantity)
	// order preserves first-seen order for deterministic, testable adds (map
	// iteration order is unspecified).
	order := make([]aggregateKey, 0)
	for _, entry := range entries {
		recipe, err := s.recipes.Get(ctx, householdID, entry.RecipeID)
		if err != nil {
			return 0, err
		}
		// Both servings counts are guaranteed positive by domain validation and the
		// schema CHECKs. Fail loud rather than silently skip if a corrupt row ever
		// slips through, so the generated list can't quietly understate the plan.
		if recipe.Servings <= 0 || entry.Servings <= 0 {
			return 0, fmt.Errorf("app: cannot scale meal plan entry for recipe %s: recipe servings %d, entry servings %d",
				entry.RecipeID, recipe.Servings, entry.Servings)
		}
		scale := float64(entry.Servings) / float64(recipe.Servings)
		for _, line := range recipe.Ingredients {
			scaled, err := household.NewQuantity(line.Quantity.Amount*scale, line.Quantity.Unit)
			if err != nil {
				return 0, err
			}
			key := aggregateKey{ingredient: line.IngredientID, unit: line.Quantity.Unit}
			current, ok := totals[key]
			if !ok {
				totals[key] = scaled
				order = append(order, key)
				continue
			}
			sum, err := current.Add(scaled)
			if err != nil {
				return 0, err
			}
			totals[key] = sum
		}
	}

	added := 0
	for _, key := range order {
		ingredient := key.ingredient
		item := &tracking.ShoppingListItem{
			ID:           tracking.NewShoppingListItemID(),
			HouseholdID:  householdID,
			IngredientID: &ingredient,
			Quantity:     totals[key],
			Source:       tracking.SourceMealPlan,
			Status:       tracking.StatusNeeded,
		}
		inserted, err := s.shopping.AddMealPlanIfAbsent(ctx, item)
		if err != nil {
			return added, err
		}
		if inserted {
			added++
		}
	}
	return added, nil
}
