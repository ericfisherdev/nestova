package domain

import "fmt"

// RecipeSourceKind records where a recipe came from. Stored as text and validated
// here (typed-string convention, not iota), mirrored by a Postgres CHECK on
// recipe.source. SourceLocal is a household-owned box recipe; SourceExternal is
// an external/cached recipe shared across households (see RecipeSource, NES-58/59).
type RecipeSourceKind string

// Recipe source kinds.
const (
	SourceLocal    RecipeSourceKind = "local"
	SourceExternal RecipeSourceKind = "external"
)

// RecipeSourceKinds returns the supported source kinds in canonical order.
func RecipeSourceKinds() []RecipeSourceKind {
	return []RecipeSourceKind{SourceLocal, SourceExternal}
}

// Valid reports whether k is a known source kind.
func (k RecipeSourceKind) Valid() bool {
	switch k {
	case SourceLocal, SourceExternal:
		return true
	default:
		return false
	}
}

// String returns the source kind's stored value.
func (k RecipeSourceKind) String() string { return string(k) }

// ParseRecipeSourceKind validates and returns a RecipeSourceKind, or an error for
// an unknown value.
func ParseRecipeSourceKind(s string) (RecipeSourceKind, error) {
	k := RecipeSourceKind(s)
	if !k.Valid() {
		return "", fmt.Errorf("invalid recipe source kind %q", s)
	}
	return k, nil
}

// Meal is a slot in a day's plan. Stored as text and validated here (typed-string
// convention, not iota), mirrored by a Postgres CHECK on meal_plan_entry.meal.
type Meal string

// Meal slots, in daily order.
const (
	MealBreakfast Meal = "breakfast"
	MealLunch     Meal = "lunch"
	MealDinner    Meal = "dinner"
	MealSnack     Meal = "snack"
)

// Meals returns the supported meal slots in daily order.
func Meals() []Meal {
	return []Meal{MealBreakfast, MealLunch, MealDinner, MealSnack}
}

// Valid reports whether m is a known meal slot.
func (m Meal) Valid() bool {
	switch m {
	case MealBreakfast, MealLunch, MealDinner, MealSnack:
		return true
	default:
		return false
	}
}

// String returns the meal slot's stored value.
func (m Meal) String() string { return string(m) }

// ParseMeal validates and returns a Meal, or an error for an unknown value.
func ParseMeal(s string) (Meal, error) {
	m := Meal(s)
	if !m.Valid() {
		return "", fmt.Errorf("invalid meal %q", s)
	}
	return m, nil
}
