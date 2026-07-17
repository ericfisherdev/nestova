package app_test

import (
	"context"
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// fakeMealPlanShoppingRepo is an in-memory tracking.ShoppingListRepository whose
// AddMealPlanIfAbsent de-duplicates open meal_plan items per (household, ingredient),
// mirroring the partial-unique guard.
type fakeMealPlanShoppingRepo struct {
	added map[string]*tracking.ShoppingListItem
}

func newFakeMealPlanShoppingRepo() *fakeMealPlanShoppingRepo {
	return &fakeMealPlanShoppingRepo{added: map[string]*tracking.ShoppingListItem{}}
}

func (f *fakeMealPlanShoppingRepo) Add(context.Context, *tracking.ShoppingListItem) error { return nil }
func (f *fakeMealPlanShoppingRepo) AddRestockIfAbsent(context.Context, *tracking.ShoppingListItem) (bool, error) {
	return true, nil
}

func (f *fakeMealPlanShoppingRepo) AddMealPlanIfAbsent(_ context.Context, item *tracking.ShoppingListItem) (bool, error) {
	if item.IngredientID == nil {
		// The service must always set the ingredient id; surface a clear error if
		// not, rather than panicking on the nil dereference below.
		return false, errors.New("fake: meal_plan item requires an ingredient id")
	}
	// Mirror the partial unique index key: (household, ingredient, unit), so the
	// same ingredient in different units coexists while each line de-duplicates.
	key := item.HouseholdID.String() + "|" + item.IngredientID.String() + "|" + item.Quantity.Unit.String()
	if _, exists := f.added[key]; exists {
		return false, nil
	}
	f.added[key] = item
	return true, nil
}

func (f *fakeMealPlanShoppingRepo) UpdateStatus(context.Context, household.HouseholdID, tracking.ShoppingListItemID, tracking.ItemStatus) (*tracking.ShoppingListItem, error) {
	return nil, tracking.ErrShoppingListItemNotFound
}

func (f *fakeMealPlanShoppingRepo) MarkInCart(context.Context, household.HouseholdID, tracking.ShoppingListItemID) (*tracking.ShoppingListItem, error) {
	return nil, tracking.ErrShoppingListItemNotFound
}

func (f *fakeMealPlanShoppingRepo) ListByStatus(context.Context, household.HouseholdID, tracking.ItemStatus) ([]*tracking.ShoppingListItem, error) {
	return nil, nil
}

func mustGroceryService(t *testing.T, plans domain.MealPlanRepository, recipes domain.RecipeRepository, shopping tracking.ShoppingListRepository) *app.GroceryFromPlanService {
	t.Helper()
	svc, err := app.NewGroceryFromPlanService(plans, recipes, shopping)
	if err != nil {
		t.Fatalf("NewGroceryFromPlanService: %v", err)
	}
	return svc
}

func recipeWithLine(hh household.HouseholdID, ing tracking.IngredientID, amount float64, unit household.Unit, servings int) *domain.Recipe {
	return &domain.Recipe{
		ID: domain.NewRecipeID(), HouseholdID: &hh, Title: "R", Source: domain.SourceLocal, Servings: servings,
		Ingredients: []domain.RecipeIngredient{{IngredientID: ing, Quantity: household.Quantity{Amount: amount, Unit: unit}}},
	}
}

func TestNewGroceryFromPlanServiceRejectsNilDeps(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo()
	shopping := newFakeMealPlanShoppingRepo()
	if _, err := app.NewGroceryFromPlanService(nil, recipes, shopping); err == nil {
		t.Error("nil plans = nil error, want error")
	}
	if _, err := app.NewGroceryFromPlanService(plans, nil, shopping); err == nil {
		t.Error("nil recipes = nil error, want error")
	}
	if _, err := app.NewGroceryFromPlanService(plans, recipes, nil); err == nil {
		t.Error("nil shopping = nil error, want error")
	}
}

func TestGenerateFromWeekScalesAggregatesAndIsIdempotent(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo()
	shopping := newFakeMealPlanShoppingRepo()
	svc := mustGroceryService(t, plans, recipes, shopping)
	ctx := context.Background()

	hh := household.NewHouseholdID()
	flour := tracking.NewIngredientID()
	// One recipe (yields 2) calling for 100 g of flour.
	recipe := recipeWithLine(hh, flour, 100, household.UnitGram, 2)
	if err := recipes.Create(ctx, recipe); err != nil {
		t.Fatalf("seed recipe: %v", err)
	}
	// Plan it twice in the week: 4 servings (scale 2x -> 200 g) and 2 servings (1x -> 100 g).
	mustUpsert(t, plans, &domain.MealPlanEntry{ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: planDate(22), Meal: domain.MealDinner, RecipeID: recipe.ID, Servings: 4})
	mustUpsert(t, plans, &domain.MealPlanEntry{ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: planDate(23), Meal: domain.MealDinner, RecipeID: recipe.ID, Servings: 2})

	added, err := svc.GenerateFromWeek(ctx, hh, planDate(21))
	if err != nil {
		t.Fatalf("GenerateFromWeek: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1 (one aggregated flour line)", added)
	}
	item := shopping.added[hh.String()+"|"+flour.String()+"|"+household.UnitGram.String()]
	if item == nil {
		t.Fatal("flour was not added to the shopping list")
	}
	// 200 g + 100 g aggregated to 300 g.
	if item.Quantity.Amount != 300 || item.Quantity.Unit != household.UnitGram {
		t.Errorf("aggregated quantity = %v %s, want 300 g", item.Quantity.Amount, item.Quantity.Unit)
	}
	if item.Source != tracking.SourceMealPlan || item.Status != tracking.StatusNeeded {
		t.Errorf("item source/status = %s/%s, want meal_plan/needed", item.Source, item.Status)
	}

	// Re-running the same plan generation adds nothing.
	added, err = svc.GenerateFromWeek(ctx, hh, planDate(21))
	if err != nil {
		t.Fatalf("GenerateFromWeek re-run: %v", err)
	}
	if added != 0 {
		t.Errorf("re-run added = %d, want 0 (idempotent)", added)
	}
}

func TestGenerateFromWeekKeepsSameIngredientDifferentUnitsSeparate(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo()
	shopping := newFakeMealPlanShoppingRepo()
	svc := mustGroceryService(t, plans, recipes, shopping)
	ctx := context.Background()

	hh := household.NewHouseholdID()
	milk := tracking.NewIngredientID()
	// Two recipes both calling for milk, but in different units (ml vs l). The two
	// aggregates must become two separate lines, not collapse into one.
	recipeML := recipeWithLine(hh, milk, 250, household.UnitMilliliter, 1)
	recipeL := recipeWithLine(hh, milk, 1, household.UnitLiter, 1)
	if err := recipes.Create(ctx, recipeML); err != nil {
		t.Fatalf("seed recipe ml: %v", err)
	}
	if err := recipes.Create(ctx, recipeL); err != nil {
		t.Fatalf("seed recipe l: %v", err)
	}
	mustUpsert(t, plans, &domain.MealPlanEntry{ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: planDate(22), Meal: domain.MealLunch, RecipeID: recipeML.ID, Servings: 1})
	mustUpsert(t, plans, &domain.MealPlanEntry{ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: planDate(23), Meal: domain.MealLunch, RecipeID: recipeL.ID, Servings: 1})

	added, err := svc.GenerateFromWeek(ctx, hh, planDate(21))
	if err != nil {
		t.Fatalf("GenerateFromWeek: %v", err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2 (milk in ml + milk in l, distinct lines)", added)
	}
	if len(shopping.added) != 2 {
		t.Errorf("stored lines = %d, want 2 (the second unit must not be lost)", len(shopping.added))
	}
}

func TestGenerateFromWeekPropagatesRecipeError(t *testing.T) {
	plans := newFakeMealPlanRepo()
	recipes := newFakeRecipeRepo() // empty: the planned recipe is unknown
	shopping := newFakeMealPlanShoppingRepo()
	svc := mustGroceryService(t, plans, recipes, shopping)
	ctx := context.Background()
	hh := household.NewHouseholdID()

	// Plan an entry whose recipe was never created, so the recipe fetch fails.
	mustUpsert(t, plans, &domain.MealPlanEntry{ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: planDate(22), Meal: domain.MealDinner, RecipeID: domain.NewRecipeID(), Servings: 2})

	if _, err := svc.GenerateFromWeek(ctx, hh, planDate(21)); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("GenerateFromWeek = %v, want ErrRecipeNotFound propagated", err)
	}
	if len(shopping.added) != 0 {
		t.Errorf("shopping items added = %d, want 0 on a failed generation", len(shopping.added))
	}
}

func mustUpsert(t *testing.T, repo *fakeMealPlanRepo, entry *domain.MealPlanEntry) {
	t.Helper()
	if err := repo.Upsert(context.Background(), entry); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}
