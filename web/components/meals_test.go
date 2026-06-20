package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func sampleMealsView() components.MealsView {
	return components.MealsView{
		CSRFToken: "csrf-token-abc",
		Units:     []components.UnitOption{{Value: "count", Label: "count"}, {Value: "g", Label: "g"}},
		Recipes: []components.MealRecipeView{
			{
				ID: "recipe-1", Title: "Pancakes", Servings: 4, ServingsLabel: "Serves 4",
				Ingredients: []components.MealIngredientView{{Name: "flour", Quantity: "200 g"}},
				EditLines:   []components.MealEditLineView{{Name: "flour", Amount: "200", Unit: "g"}},
			},
		},
		RecipeOptions: []components.MealRecipeOption{{ID: "recipe-1", Title: "Pancakes"}},
		Week: components.MealWeekView{
			WeekStart: "2026-06-21", RangeLabel: "Jun 21 – Jun 27",
			Days: []components.MealDayView{
				{
					Date: "2026-06-21", Label: "Sun 21",
					Slots: []components.MealSlotView{
						{Meal: "breakfast", MealLabel: "Breakfast"},
						{Meal: "dinner", MealLabel: "Dinner", RecipeTitle: "Pancakes", ServingsLabel: "4 servings", Filled: true},
					},
				},
			},
		},
		Finder: &components.MealFinderView{
			Source: "ingredients", Query: "flour, eggs",
			Matches: []components.MealMatchView{{Title: "Pancakes", MatchLabel: "67% match", Missing: []string{"eggs"}}},
		},
	}
}

func TestMealsPageRendersKeyElements(t *testing.T) {
	out := renderString(t, components.MealsPage(sampleMealsView()))
	for _, want := range []string{
		"Meals &amp; Recipes", // page heading
		"Recipe box",          // recipe box card
		"Pancakes",            // recipe + planner slot + match
		"Serves 4",            // recipe servings label
		"Weekly planner",      // planner card
		"Jun 21 – Jun 27",     // week range
		"4 servings",          // filled slot servings
		"What can I make?",    // finder card
		"67% match",           // finder match label
		"eggs",                // finder missing ingredient
	} {
		if !strings.Contains(out, want) {
			t.Errorf("MealsPage missing %q", want)
		}
	}
}

func TestMealsPagePostFormsCarryHTMXAndCSRF(t *testing.T) {
	out := renderString(t, components.MealsPage(sampleMealsView()))
	for _, want := range []string{
		`hx-post="/meals/recipes"`,                 // create recipe
		`hx-post="/meals/recipes/recipe-1/delete"`, // delete recipe
		`hx-post="/meals/plan"`,                    // assign meal
		`hx-post="/meals/plan/clear"`,              // clear a slot
		`hx-post="/meals/plan/generate"`,           // generate grocery list
		`name="csrf_token"`,                        // CSRF field present
		`value="csrf-token-abc"`,                   // CSRF token value
	} {
		if !strings.Contains(out, want) {
			t.Errorf("MealsPage missing %q", want)
		}
	}
}

func TestMealsPageFinderIsReadOnlyGet(t *testing.T) {
	out := renderString(t, components.MealsPage(sampleMealsView()))
	// The finder is a read-only GET to /meals, so its form posts no CSRF token.
	if !strings.Contains(out, `method="get" action="/meals"`) {
		t.Error("finder form should be a GET to /meals")
	}
}
