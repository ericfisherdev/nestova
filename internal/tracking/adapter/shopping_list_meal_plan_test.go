package adapter_test

import (
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/adapter"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func mealPlanItem(hh household.HouseholdID, ing domain.IngredientID) *domain.ShoppingListItem {
	return &domain.ShoppingListItem{
		ID: domain.NewShoppingListItemID(), HouseholdID: hh, IngredientID: &ing,
		Quantity: household.Quantity{Amount: 2, Unit: household.UnitCount},
		Source:   domain.SourceMealPlan, Status: domain.StatusNeeded,
	}
}

func TestShoppingMealPlanDedup(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "flour")

	inserted, err := repo.AddMealPlanIfAbsent(ctx, mealPlanItem(hh, ing))
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if !inserted {
		t.Fatal("first add reported not inserted, want inserted")
	}

	// A second add for the same open (household, ingredient) is a no-op.
	inserted, err = repo.AddMealPlanIfAbsent(ctx, mealPlanItem(hh, ing))
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if inserted {
		t.Error("second add inserted a duplicate, want no-op")
	}

	items, err := repo.ListByStatus(ctx, hh, domain.StatusNeeded)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	count := 0
	for _, item := range items {
		if item.Source == domain.SourceMealPlan {
			count++
		}
	}
	if count != 1 {
		t.Errorf("open meal_plan items = %d, want 1", count)
	}
}

func TestAddMealPlanIfAbsentAllowsNewAfterPurchased(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "flour")

	// Add a meal-plan item, then mark it purchased: the partial unique index only
	// covers non-purchased rows, so a fresh item for the same ingredient must insert.
	first := mealPlanItem(hh, ing)
	inserted, err := repo.AddMealPlanIfAbsent(ctx, first)
	if err != nil || !inserted {
		t.Fatalf("first add: inserted=%v err=%v", inserted, err)
	}
	if _, err := repo.UpdateStatus(ctx, hh, first.ID, domain.StatusPurchased); err != nil {
		t.Fatalf("mark purchased: %v", err)
	}

	second := mealPlanItem(hh, ing)
	inserted, err = repo.AddMealPlanIfAbsent(ctx, second)
	if err != nil {
		t.Fatalf("second add after purchase: %v", err)
	}
	if !inserted {
		t.Error("add after purchase reported not inserted, want a fresh insert")
	}
}

func TestAddMealPlanIfAbsentAllowsSameIngredientDifferentUnits(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "milk")

	// The same ingredient in two different units must coexist (the index keys on
	// unit too), so both inserts succeed.
	ml := mealPlanItem(hh, ing)
	ml.Quantity = household.Quantity{Amount: 250, Unit: household.UnitMilliliter}
	if inserted, err := repo.AddMealPlanIfAbsent(ctx, ml); err != nil || !inserted {
		t.Fatalf("ml add: inserted=%v err=%v", inserted, err)
	}
	liters := mealPlanItem(hh, ing)
	liters.Quantity = household.Quantity{Amount: 1, Unit: household.UnitLiter}
	if inserted, err := repo.AddMealPlanIfAbsent(ctx, liters); err != nil || !inserted {
		t.Errorf("l add (different unit): inserted=%v err=%v, want a fresh insert", inserted, err)
	}
}

func TestAddMealPlanIfAbsentRequiresMealPlanIdentity(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "flour")

	wrongSource := mealPlanItem(hh, ing)
	wrongSource.Source = domain.SourceManual
	if _, err := repo.AddMealPlanIfAbsent(ctx, wrongSource); err == nil {
		t.Error("non-meal_plan source = nil error, want error")
	}

	noIngredient := &domain.ShoppingListItem{
		ID: domain.NewShoppingListItemID(), HouseholdID: hh, Name: "free text",
		Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount},
		Source:   domain.SourceMealPlan, Status: domain.StatusNeeded,
	}
	if _, err := repo.AddMealPlanIfAbsent(ctx, noIngredient); err == nil {
		t.Error("meal_plan without ingredient id = nil error, want error")
	}
}
