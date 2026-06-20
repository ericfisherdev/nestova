package domain_test

import (
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

func validMealPlanEntry() *domain.MealPlanEntry {
	return &domain.MealPlanEntry{
		ID:          domain.NewMealPlanEntryID(),
		HouseholdID: household.NewHouseholdID(),
		Date:        time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC),
		Meal:        domain.MealDinner,
		RecipeID:    domain.NewRecipeID(),
		Servings:    4,
	}
}

func TestMealPlanEntryValidateAccepts(t *testing.T) {
	if err := validMealPlanEntry().Validate(); err != nil {
		t.Errorf("valid entry: Validate() = %v, want nil", err)
	}
}

func TestMealPlanEntryValidateRejectsUnknownMeal(t *testing.T) {
	e := validMealPlanEntry()
	e.Meal = domain.Meal("brunch")
	if err := e.Validate(); !errors.Is(err, domain.ErrInvalidMealPlanEntry) {
		t.Errorf("unknown meal: Validate() = %v, want ErrInvalidMealPlanEntry", err)
	}
}

func TestMealPlanEntryValidateRejectsNonPositiveServings(t *testing.T) {
	for _, servings := range []int{0, -1} {
		e := validMealPlanEntry()
		e.Servings = servings
		if err := e.Validate(); !errors.Is(err, domain.ErrInvalidMealPlanEntry) {
			t.Errorf("servings %d: Validate() = %v, want ErrInvalidMealPlanEntry", servings, err)
		}
	}
}

func TestMealPlanEntryValidateRejectsZeroDate(t *testing.T) {
	e := validMealPlanEntry()
	e.Date = time.Time{}
	if err := e.Validate(); !errors.Is(err, domain.ErrInvalidMealPlanEntry) {
		t.Errorf("zero date: Validate() = %v, want ErrInvalidMealPlanEntry", err)
	}
}

func TestMealPlanEntryValidateRejectsZeroID(t *testing.T) {
	e := validMealPlanEntry()
	e.ID = domain.MealPlanEntryID{}
	if err := e.Validate(); !errors.Is(err, domain.ErrInvalidMealPlanEntry) {
		t.Errorf("zero id: Validate() = %v, want ErrInvalidMealPlanEntry", err)
	}
}
