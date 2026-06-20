package domain_test

import (
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func mustQuantity(t *testing.T, amount float64, unit household.Unit) household.Quantity {
	t.Helper()
	q, err := household.NewQuantity(amount, unit)
	if err != nil {
		t.Fatalf("NewQuantity(%v, %q): %v", amount, unit, err)
	}
	return q
}

func validLocalRecipe(t *testing.T) *domain.Recipe {
	t.Helper()
	hid := household.NewHouseholdID()
	return &domain.Recipe{
		ID:           domain.NewRecipeID(),
		HouseholdID:  &hid,
		Title:        "Pancakes",
		Source:       domain.SourceLocal,
		Servings:     4,
		Instructions: "Mix and cook.",
		Ingredients: []domain.RecipeIngredient{
			{IngredientID: tracking.NewIngredientID(), Quantity: mustQuantity(t, 2, household.UnitCount)},
		},
	}
}

func validExternalRecipe(t *testing.T) *domain.Recipe {
	t.Helper()
	ref := "spoonacular-12345"
	return &domain.Recipe{
		ID:          domain.NewRecipeID(),
		Title:       "Discovered Stew",
		Source:      domain.SourceExternal,
		ExternalRef: &ref,
		Servings:    2,
	}
}

func TestRecipeValidateAcceptsLocalAndExternal(t *testing.T) {
	if err := validLocalRecipe(t).Validate(); err != nil {
		t.Errorf("valid local recipe: Validate() = %v, want nil", err)
	}
	if err := validExternalRecipe(t).Validate(); err != nil {
		t.Errorf("valid external recipe: Validate() = %v, want nil", err)
	}
}

func TestRecipeValidateRejectsBlankTitleAndServings(t *testing.T) {
	blank := validLocalRecipe(t)
	blank.Title = "   "
	if err := blank.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
		t.Errorf("blank title: Validate() = %v, want ErrInvalidRecipe", err)
	}

	for _, servings := range []int{0, -1} {
		r := validLocalRecipe(t)
		r.Servings = servings
		if err := r.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
			t.Errorf("servings %d: Validate() = %v, want ErrInvalidRecipe", servings, err)
		}
	}
}

func TestRecipeValidateEnforcesSourceOwnership(t *testing.T) {
	// Local recipe must be household-owned with no external ref.
	localNoHousehold := validLocalRecipe(t)
	localNoHousehold.HouseholdID = nil
	if err := localNoHousehold.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
		t.Errorf("local without household: Validate() = %v, want ErrInvalidRecipe", err)
	}

	ref := "x"
	localWithRef := validLocalRecipe(t)
	localWithRef.ExternalRef = &ref
	if err := localWithRef.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
		t.Errorf("local with external ref: Validate() = %v, want ErrInvalidRecipe", err)
	}

	// External recipe must be household-agnostic and carry an external ref.
	externalWithHousehold := validExternalRecipe(t)
	hid := household.NewHouseholdID()
	externalWithHousehold.HouseholdID = &hid
	if err := externalWithHousehold.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
		t.Errorf("external with household: Validate() = %v, want ErrInvalidRecipe", err)
	}

	externalNoRef := validExternalRecipe(t)
	externalNoRef.ExternalRef = nil
	if err := externalNoRef.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
		t.Errorf("external without ref: Validate() = %v, want ErrInvalidRecipe", err)
	}

	// A whitespace-only external ref is treated as absent and rejected.
	blankRef := "   "
	externalBlankRef := validExternalRecipe(t)
	externalBlankRef.ExternalRef = &blankRef
	if err := externalBlankRef.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
		t.Errorf("external with blank ref: Validate() = %v, want ErrInvalidRecipe", err)
	}

	// A padded (non-empty but untrimmed) external ref is rejected to keep the
	// external-recipe cache key normalized.
	untrimmedRef := "  spoonacular-12345  "
	externalUntrimmedRef := validExternalRecipe(t)
	externalUntrimmedRef.ExternalRef = &untrimmedRef
	if err := externalUntrimmedRef.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
		t.Errorf("external with untrimmed ref: Validate() = %v, want ErrInvalidRecipe", err)
	}
}

func TestRecipeValidateRejectsUnknownSource(t *testing.T) {
	r := validLocalRecipe(t)
	r.Source = domain.RecipeSourceKind("imported")
	if err := r.Validate(); !errors.Is(err, domain.ErrInvalidRecipe) {
		t.Errorf("unknown source: Validate() = %v, want ErrInvalidRecipe", err)
	}
}

func TestRecipeValidatePropagatesQuantityError(t *testing.T) {
	r := validLocalRecipe(t)
	r.Ingredients = []domain.RecipeIngredient{
		{IngredientID: tracking.NewIngredientID(), Quantity: household.Quantity{Amount: 1, Unit: "bogus"}},
	}
	if err := r.Validate(); !errors.Is(err, household.ErrInvalidQuantity) {
		t.Errorf("invalid quantity line: Validate() = %v, want ErrInvalidQuantity", err)
	}
}

func TestRecipeIngredientValidate(t *testing.T) {
	good := domain.RecipeIngredient{IngredientID: tracking.NewIngredientID(), Quantity: mustQuantity(t, 3, household.UnitGram)}
	if err := good.Validate(); err != nil {
		t.Errorf("valid line: Validate() = %v, want nil", err)
	}
	bad := domain.RecipeIngredient{IngredientID: tracking.NewIngredientID(), Quantity: household.Quantity{Amount: -1, Unit: household.UnitGram}}
	if err := bad.Validate(); !errors.Is(err, household.ErrInvalidQuantity) {
		t.Errorf("negative line: Validate() = %v, want ErrInvalidQuantity", err)
	}
	// A zero amount is a valid Quantity but does not describe a recipe ingredient.
	zero := domain.RecipeIngredient{IngredientID: tracking.NewIngredientID(), Quantity: mustQuantity(t, 0, household.UnitGram)}
	if err := zero.Validate(); !errors.Is(err, household.ErrInvalidQuantity) {
		t.Errorf("zero line: Validate() = %v, want ErrInvalidQuantity", err)
	}
}
