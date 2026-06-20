package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Meal-plan entry errors.
var (
	// ErrMealPlanEntryNotFound is returned when a meal-plan entry does not exist
	// for the given (household, date, meal) slot.
	ErrMealPlanEntryNotFound = errors.New("meals: meal plan entry not found")
	// ErrInvalidMealPlanEntry is returned when a meal-plan entry violates its
	// invariants: an unknown meal slot or non-positive servings.
	ErrInvalidMealPlanEntry = errors.New("meals: invalid meal plan entry")
)

// MealPlanEntry assigns one of the household's box recipes to a (Date, Meal) slot
// on the weekly plan. A slot holds a single entry per household. Servings is the
// portion count planned for the slot, used to scale the recipe when generating a
// shopping list (NES-61). Only Date's calendar date is significant. To avoid
// timezone drift when it is reduced to the DATE column the repository persists to,
// callers supply Date as a UTC calendar date (the planner constructs slots at
// midnight UTC); the date is then taken in UTC so 2026-06-21 names the same slot
// however the value was built.
type MealPlanEntry struct {
	ID          MealPlanEntryID
	HouseholdID household.HouseholdID
	Date        time.Time
	Meal        Meal
	RecipeID    RecipeID
	Servings    int
}

// Validate reports whether the entry is well-formed: a set ID (the primary key,
// caller-assigned via NewMealPlanEntryID), a set Date, a known Meal slot, and a
// positive Servings count, returning ErrInvalidMealPlanEntry otherwise.
// (HouseholdID and RecipeID identity are enforced by the schema's foreign keys,
// matching how the other contexts validate their entries.)
func (e *MealPlanEntry) Validate() error {
	if e.ID == (MealPlanEntryID{}) {
		return fmt.Errorf("%w: id is required", ErrInvalidMealPlanEntry)
	}
	if e.Date.IsZero() {
		return fmt.Errorf("%w: date is required", ErrInvalidMealPlanEntry)
	}
	if !e.Meal.Valid() {
		return fmt.Errorf("%w: unknown meal %q", ErrInvalidMealPlanEntry, e.Meal)
	}
	if e.Servings <= 0 {
		return fmt.Errorf("%w: servings must be positive, got %d", ErrInvalidMealPlanEntry, e.Servings)
	}
	return nil
}

// MealPlanRepository persists weekly meal-plan entries.
//
// Persistence contracts:
//   - Upsert assigns a recipe to the (HouseholdID, Date, Meal) slot, replacing any
//     existing entry in that slot (one recipe per slot). The caller sets a valid
//     entry whose RecipeID names a recipe owned by the household; an unknown
//     recipe yields ErrRecipeNotFound. The slot — not entry.ID — is the identity:
//     re-assigning an occupied slot updates that row in place (its existing ID and
//     CreatedAt are preserved), so entry.ID is consumed only when the slot is
//     empty and a new row is inserted.
//
// Error contracts:
//   - Delete removes the entry in the (householdID, date, meal) slot and returns
//     ErrMealPlanEntryNotFound when the slot is empty.
//   - ListByDateRange returns the household's entries whose Date falls in the
//     inclusive window [start, end] ordered by Date then Meal, or an empty slice
//     when none match. The caller chooses the window (e.g. a week).
//
// All time.Time parameters are interpreted as UTC calendar dates (only the
// year/month/day are significant), consistent with MealPlanEntry.Date.
type MealPlanRepository interface {
	Upsert(ctx context.Context, entry *MealPlanEntry) error
	Delete(ctx context.Context, householdID household.HouseholdID, date time.Time, meal Meal) error
	ListByDateRange(ctx context.Context, householdID household.HouseholdID, start, end time.Time) ([]*MealPlanEntry, error)
}
