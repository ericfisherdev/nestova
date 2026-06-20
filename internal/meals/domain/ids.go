package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// RecipeID uniquely identifies a recipe — either a household's box recipe or an
// external/cached one.
type RecipeID uuid.UUID

// NewRecipeID returns a new time-ordered (UUIDv7) recipe id, which gives better
// B-tree index locality than random v4 ids. uuid.NewV7 only errors if the crypto
// random source is unavailable — the same failure under which uuid.New itself
// panics — so Must is appropriate here, matching the other id constructors.
func NewRecipeID() RecipeID { return RecipeID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id RecipeID) String() string { return uuid.UUID(id).String() }

// ParseRecipeID parses a canonical UUID string into a RecipeID.
func ParseRecipeID(s string) (RecipeID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return RecipeID{}, fmt.Errorf("parse recipe id: %w", err)
	}
	return RecipeID(u), nil
}

// MealPlanEntryID uniquely identifies a single assignment of a recipe to a
// (date, meal) slot on a household's weekly plan.
type MealPlanEntryID uuid.UUID

// NewMealPlanEntryID returns a new time-ordered (UUIDv7) meal-plan-entry id. See
// NewRecipeID for why Must is appropriate.
func NewMealPlanEntryID() MealPlanEntryID { return MealPlanEntryID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id MealPlanEntryID) String() string { return uuid.UUID(id).String() }

// ParseMealPlanEntryID parses a canonical UUID string into a MealPlanEntryID.
func ParseMealPlanEntryID(s string) (MealPlanEntryID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return MealPlanEntryID{}, fmt.Errorf("parse meal plan entry id: %w", err)
	}
	return MealPlanEntryID(u), nil
}
