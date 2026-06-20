package app_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

// fakeMealPlanRepo is an in-memory domain.MealPlanRepository keyed by slot.
type fakeMealPlanRepo struct {
	entries map[string]*domain.MealPlanEntry
}

func newFakeMealPlanRepo() *fakeMealPlanRepo {
	return &fakeMealPlanRepo{entries: map[string]*domain.MealPlanEntry{}}
}

func slotKey(hh household.HouseholdID, date time.Time, meal domain.Meal) string {
	return hh.String() + "|" + date.UTC().Format("2006-01-02") + "|" + meal.String()
}

func (f *fakeMealPlanRepo) Upsert(_ context.Context, entry *domain.MealPlanEntry) error {
	f.entries[slotKey(entry.HouseholdID, entry.Date, entry.Meal)] = entry
	return nil
}

func (f *fakeMealPlanRepo) Delete(_ context.Context, hh household.HouseholdID, date time.Time, meal domain.Meal) error {
	key := slotKey(hh, date, meal)
	if _, ok := f.entries[key]; !ok {
		return domain.ErrMealPlanEntryNotFound
	}
	delete(f.entries, key)
	return nil
}

func (f *fakeMealPlanRepo) ListByDateRange(_ context.Context, hh household.HouseholdID, start, end time.Time) ([]*domain.MealPlanEntry, error) {
	mealOrder := map[domain.Meal]int{domain.MealBreakfast: 0, domain.MealLunch: 1, domain.MealDinner: 2, domain.MealSnack: 3}
	var out []*domain.MealPlanEntry
	for _, entry := range f.entries {
		if entry.HouseholdID == hh && !entry.Date.Before(start) && !entry.Date.After(end) {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return mealOrder[out[i].Meal] < mealOrder[out[j].Meal]
	})
	return out, nil
}

func mustPlanner(t *testing.T, plans domain.MealPlanRepository, recipes domain.RecipeRepository) *app.PlannerService {
	t.Helper()
	svc, err := app.NewPlannerService(plans, recipes)
	if err != nil {
		t.Fatalf("NewPlannerService: %v", err)
	}
	return svc
}

func seedFakeRecipe(t *testing.T, repo *fakeRecipeRepo, hh household.HouseholdID) domain.RecipeID {
	t.Helper()
	id := domain.NewRecipeID()
	if err := repo.Create(context.Background(), &domain.Recipe{
		ID: id, HouseholdID: &hh, Title: "Soup", Source: domain.SourceLocal, Servings: 2,
	}); err != nil {
		t.Fatalf("seed recipe: %v", err)
	}
	return id
}

func planDate(d int) time.Time {
	return time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC)
}

func TestNewPlannerServiceRejectsNilDeps(t *testing.T) {
	if _, err := app.NewPlannerService(nil, newFakeRecipeRepo()); err == nil {
		t.Error("nil plans = nil error, want error")
	}
	if _, err := app.NewPlannerService(newFakeMealPlanRepo(), nil); err == nil {
		t.Error("nil recipes = nil error, want error")
	}
}

func TestAssignMealAssignsKnownRecipe(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo()
	hh := household.NewHouseholdID()
	recipeID := seedFakeRecipe(t, recipes, hh)
	svc := mustPlanner(t, plans, recipes)

	if err := svc.AssignMeal(context.Background(), hh, planDate(22), domain.MealDinner, recipeID, 4); err != nil {
		t.Fatalf("AssignMeal: %v", err)
	}
	week, err := svc.PlanForWeek(context.Background(), hh, planDate(21))
	if err != nil {
		t.Fatalf("PlanForWeek: %v", err)
	}
	if len(week) != 1 || week[0].RecipeID != recipeID || week[0].Servings != 4 || week[0].Meal != domain.MealDinner {
		t.Errorf("stored slot = %+v, want one dinner with recipe %v / 4 servings", week, recipeID)
	}
}

func TestAssignMealRejectsUnknownRecipe(t *testing.T) {
	svc := mustPlanner(t, newFakeMealPlanRepo(), newFakeRecipeRepo())
	hh := household.NewHouseholdID()
	if err := svc.AssignMeal(context.Background(), hh, planDate(22), domain.MealDinner, domain.NewRecipeID(), 2); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("AssignMeal(unknown recipe) = %v, want ErrRecipeNotFound", err)
	}
}

func TestAssignMealValidation(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo()
	hh := household.NewHouseholdID()
	recipeID := seedFakeRecipe(t, recipes, hh)
	svc := mustPlanner(t, plans, recipes)
	ctx := context.Background()

	if err := svc.AssignMeal(ctx, hh, planDate(22), domain.Meal("brunch"), recipeID, 2); !errors.Is(err, domain.ErrInvalidMealPlanEntry) {
		t.Errorf("unknown meal = %v, want ErrInvalidMealPlanEntry", err)
	}
	if err := svc.AssignMeal(ctx, hh, planDate(22), domain.MealDinner, recipeID, 0); !errors.Is(err, domain.ErrInvalidMealPlanEntry) {
		t.Errorf("zero servings = %v, want ErrInvalidMealPlanEntry", err)
	}
	if err := svc.AssignMeal(ctx, hh, time.Time{}, domain.MealDinner, recipeID, 2); !errors.Is(err, domain.ErrInvalidMealPlanEntry) {
		t.Errorf("zero date = %v, want ErrInvalidMealPlanEntry", err)
	}
	// Validation fails before the recipe is consulted, so nothing is stored.
	if len(plans.entries) != 0 {
		t.Errorf("stored entries = %d, want 0 on validation failure", len(plans.entries))
	}
}

func TestAssignMealReplacesSlot(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo()
	hh := household.NewHouseholdID()
	first := seedFakeRecipe(t, recipes, hh)
	second := seedFakeRecipe(t, recipes, hh)
	svc := mustPlanner(t, plans, recipes)
	ctx := context.Background()

	if err := svc.AssignMeal(ctx, hh, planDate(22), domain.MealDinner, first, 2); err != nil {
		t.Fatalf("AssignMeal first: %v", err)
	}
	if err := svc.AssignMeal(ctx, hh, planDate(22), domain.MealDinner, second, 6); err != nil {
		t.Fatalf("AssignMeal second: %v", err)
	}
	week, err := svc.PlanForWeek(ctx, hh, planDate(21))
	if err != nil {
		t.Fatalf("PlanForWeek: %v", err)
	}
	if len(week) != 1 || week[0].RecipeID != second || week[0].Servings != 6 {
		t.Errorf("slot = %+v, want single entry with second recipe / 6 servings", week)
	}
}

func TestPlanForWeekReturnsOrderedWindow(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo()
	hh := household.NewHouseholdID()
	recipeID := seedFakeRecipe(t, recipes, hh)
	svc := mustPlanner(t, plans, recipes)
	ctx := context.Background()

	// Two entries within the week (out of order) and one outside it.
	if err := svc.AssignMeal(ctx, hh, planDate(23), domain.MealBreakfast, recipeID, 2); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignMeal(ctx, hh, planDate(22), domain.MealDinner, recipeID, 2); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignMeal(ctx, hh, planDate(30), domain.MealDinner, recipeID, 2); err != nil { // outside the week
		t.Fatal(err)
	}

	week, err := svc.PlanForWeek(ctx, hh, planDate(21)) // 21..27
	if err != nil {
		t.Fatalf("PlanForWeek: %v", err)
	}
	if len(week) != 2 {
		t.Fatalf("week entries = %d, want 2 (the 30th is out of window)", len(week))
	}
	if !week[0].Date.Equal(planDate(22)) || !week[1].Date.Equal(planDate(23)) {
		t.Errorf("week order = [%v, %v], want [22nd, 23rd]", week[0].Date, week[1].Date)
	}
}

func TestClearMeal(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo()
	hh := household.NewHouseholdID()
	recipeID := seedFakeRecipe(t, recipes, hh)
	svc := mustPlanner(t, plans, recipes)
	ctx := context.Background()

	if err := svc.AssignMeal(ctx, hh, planDate(22), domain.MealLunch, recipeID, 2); err != nil {
		t.Fatalf("AssignMeal: %v", err)
	}
	if err := svc.ClearMeal(ctx, hh, planDate(22), domain.MealLunch); err != nil {
		t.Fatalf("ClearMeal: %v", err)
	}
	if len(plans.entries) != 0 {
		t.Errorf("entries after clear = %d, want 0", len(plans.entries))
	}
	if err := svc.ClearMeal(ctx, hh, planDate(22), domain.MealLunch); !errors.Is(err, domain.ErrMealPlanEntryNotFound) {
		t.Errorf("ClearMeal(empty) = %v, want ErrMealPlanEntryNotFound", err)
	}
}
