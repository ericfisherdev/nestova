package domain_test

import (
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func req(id tracking.IngredientID) domain.RecipeIngredient {
	return domain.RecipeIngredient{IngredientID: id, Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount}, Optional: false}
}

func opt(id tracking.IngredientID) domain.RecipeIngredient {
	return domain.RecipeIngredient{IngredientID: id, Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount}, Optional: true}
}

func recipeWith(title string, lines ...domain.RecipeIngredient) *domain.Recipe {
	hh := household.NewHouseholdID()
	return &domain.Recipe{
		ID: domain.NewRecipeID(), HouseholdID: &hh, Title: title,
		Source: domain.SourceLocal, Servings: 2, Ingredients: lines,
	}
}

func TestMatchByIngredientsFullMatch(t *testing.T) {
	flour, eggs := tracking.NewIngredientID(), tracking.NewIngredientID()
	recipe := recipeWith("Pancakes", req(flour), req(eggs))

	matches := domain.MatchByIngredients([]*domain.Recipe{recipe}, []tracking.IngredientID{flour, eggs})
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if matches[0].MatchPct != 1.0 {
		t.Errorf("MatchPct = %v, want 1.0", matches[0].MatchPct)
	}
	if len(matches[0].Missing) != 0 {
		t.Errorf("Missing = %v, want empty", matches[0].Missing)
	}
}

func TestMatchByIngredientsPartialMatch(t *testing.T) {
	flour, eggs, milk := tracking.NewIngredientID(), tracking.NewIngredientID(), tracking.NewIngredientID()
	recipe := recipeWith("Pancakes", req(flour), req(eggs), req(milk))

	matches := domain.MatchByIngredients([]*domain.Recipe{recipe}, []tracking.IngredientID{flour, eggs})
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if matches[0].MatchPct < 0.66 || matches[0].MatchPct > 0.67 {
		t.Errorf("MatchPct = %v, want ~0.667 (2/3)", matches[0].MatchPct)
	}
	if len(matches[0].Missing) != 1 || matches[0].Missing[0] != milk {
		t.Errorf("Missing = %v, want [milk]", matches[0].Missing)
	}
}

func TestMatchByIngredientsExcludesOptionalFromMissing(t *testing.T) {
	flour, herbs := tracking.NewIngredientID(), tracking.NewIngredientID()
	// All required are on hand; the only unmet ingredient is optional.
	recipe := recipeWith("Bread", req(flour), opt(herbs))

	matches := domain.MatchByIngredients([]*domain.Recipe{recipe}, []tracking.IngredientID{flour})
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if matches[0].MatchPct != 1.0 {
		t.Errorf("MatchPct = %v, want 1.0 (optional ignored)", matches[0].MatchPct)
	}
	if len(matches[0].Missing) != 0 {
		t.Errorf("Missing = %v, want empty (optional never missing)", matches[0].Missing)
	}
}

func TestMatchByIngredientsRanksAndExcludesZero(t *testing.T) {
	a, b, c := tracking.NewIngredientID(), tracking.NewIngredientID(), tracking.NewIngredientID()
	full := recipeWith("Zucchini Bake", req(a))         // 1.0
	half := recipeWith("Apple Crumble", req(a), req(b)) // 0.5
	halfTie := recipeWith("Apple Tart", req(a), req(c)) // 0.5, ties with half -> title order
	none := recipeWith("Beef Stew", req(b), req(c))     // 0.0 -> excluded

	matches := domain.MatchByIngredients(
		[]*domain.Recipe{none, halfTie, half, full},
		[]tracking.IngredientID{a},
	)
	if len(matches) != 3 {
		t.Fatalf("matches = %d, want 3 (zero-match recipe excluded)", len(matches))
	}
	gotTitles := []string{matches[0].Recipe.Title, matches[1].Recipe.Title, matches[2].Recipe.Title}
	want := []string{"Zucchini Bake", "Apple Crumble", "Apple Tart"}
	for i := range want {
		if gotTitles[i] != want[i] {
			t.Errorf("rank %d = %q, want %q (order = %v)", i, gotTitles[i], want[i], gotTitles)
		}
	}
}

func TestMatchByIngredientsSkipsNilRecipe(t *testing.T) {
	flour := tracking.NewIngredientID()
	good := recipeWith("Bread", req(flour))
	matches := domain.MatchByIngredients([]*domain.Recipe{nil, good}, []tracking.IngredientID{flour})
	if len(matches) != 1 || matches[0].Recipe.Title != "Bread" {
		t.Errorf("matches = %+v, want one match (nil entry skipped)", matches)
	}
}

func TestMatchByIngredientsNoRequiredScoresFull(t *testing.T) {
	herbs := tracking.NewIngredientID()
	recipe := recipeWith("Garnish", opt(herbs)) // only optional lines

	matches := domain.MatchByIngredients([]*domain.Recipe{recipe}, nil)
	if len(matches) != 1 || matches[0].MatchPct != 1.0 {
		t.Errorf("no-required recipe = %+v, want one match at 1.0", matches)
	}
}
