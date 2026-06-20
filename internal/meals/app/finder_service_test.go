package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// fakeRecipeSource records the arguments of the last FindByIngredients call and
// returns canned matches, so delegation and the assembled "have" set are testable.
type fakeRecipeSource struct {
	gotHousehold household.HouseholdID
	gotHave      []tracking.IngredientID
	matches      []domain.RecipeMatch
}

func (f *fakeRecipeSource) FindByIngredients(_ context.Context, householdID household.HouseholdID, have []tracking.IngredientID) ([]domain.RecipeMatch, error) {
	f.gotHousehold = householdID
	f.gotHave = have
	return f.matches, nil
}

// fakePantryRepo is an in-memory tracking.PantryRepository; only ListByHousehold is
// exercised by the finder, the rest satisfy the interface.
type fakePantryRepo struct {
	items []*tracking.PantryItem
}

func (f *fakePantryRepo) Create(context.Context, *tracking.PantryItem) error { return nil }
func (f *fakePantryRepo) Get(context.Context, tracking.PantryItemID) (*tracking.PantryItem, error) {
	return nil, tracking.ErrPantryItemNotFound
}

func (f *fakePantryRepo) Adjust(context.Context, household.HouseholdID, tracking.PantryItemID, household.Quantity) (*tracking.PantryItem, error) {
	return nil, tracking.ErrPantryItemNotFound
}

func (f *fakePantryRepo) Consume(context.Context, household.HouseholdID, tracking.PantryItemID, household.Quantity) (*tracking.PantryItem, error) {
	return nil, tracking.ErrPantryItemNotFound
}

func (f *fakePantryRepo) ListByHousehold(_ context.Context, _ household.HouseholdID) ([]*tracking.PantryItem, error) {
	return f.items, nil
}

func (f *fakePantryRepo) ListExpiringWithin(context.Context, household.HouseholdID, time.Time, int) ([]*tracking.PantryItem, error) {
	return nil, nil
}

func pantryItem(hh household.HouseholdID, ing tracking.IngredientID) *tracking.PantryItem {
	return &tracking.PantryItem{
		ID: tracking.NewPantryItemID(), HouseholdID: hh, IngredientID: ing,
		Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount},
	}
}

func mustFinder(t *testing.T, source domain.RecipeSource, pantry tracking.PantryRepository, ensurer tracking.IngredientEnsurer) *app.FinderService {
	t.Helper()
	svc, err := app.NewFinderService(source, pantry, ensurer)
	if err != nil {
		t.Fatalf("NewFinderService: %v", err)
	}
	return svc
}

func TestNewFinderServiceRejectsNilDeps(t *testing.T) {
	src := &fakeRecipeSource{}
	pantry := &fakePantryRepo{}
	ensurer := newFakeEnsurer()
	if _, err := app.NewFinderService(nil, pantry, ensurer); err == nil {
		t.Error("nil source = nil error, want error")
	}
	if _, err := app.NewFinderService(src, nil, ensurer); err == nil {
		t.Error("nil pantry = nil error, want error")
	}
	if _, err := app.NewFinderService(src, pantry, nil); err == nil {
		t.Error("nil ensurer = nil error, want error")
	}
}

func TestFindFromPantryDedupsAndDelegates(t *testing.T) {
	hh := household.NewHouseholdID()
	flour, eggs := tracking.NewIngredientID(), tracking.NewIngredientID()
	pantry := &fakePantryRepo{items: []*tracking.PantryItem{
		pantryItem(hh, flour),
		pantryItem(hh, eggs),
		pantryItem(hh, flour), // duplicate ingredient (a second batch)
	}}
	src := &fakeRecipeSource{}
	svc := mustFinder(t, src, pantry, newFakeEnsurer())

	if _, err := svc.FindFromPantry(context.Background(), hh); err != nil {
		t.Fatalf("FindFromPantry: %v", err)
	}
	if src.gotHousehold != hh {
		t.Errorf("delegated household = %v, want %v", src.gotHousehold, hh)
	}
	if len(src.gotHave) != 2 {
		t.Errorf("have = %v, want 2 deduped ids", src.gotHave)
	}
}

func TestFindFromIngredientsNormalizesAndDedups(t *testing.T) {
	hh := household.NewHouseholdID()
	src := &fakeRecipeSource{}
	ensurer := newFakeEnsurer()
	svc := mustFinder(t, src, &fakePantryRepo{}, ensurer)

	// "Flour" and "  flour " normalize to the same catalogue id and collapse to one.
	if _, err := svc.FindFromIngredients(context.Background(), hh, []string{"Flour", "  flour ", "Eggs"}); err != nil {
		t.Fatalf("FindFromIngredients: %v", err)
	}
	if len(src.gotHave) != 2 {
		t.Errorf("have = %v, want 2 (Flour/flour deduped, Eggs distinct)", src.gotHave)
	}
}

func TestFindFromIngredientsRejectsBlankName(t *testing.T) {
	svc := mustFinder(t, &fakeRecipeSource{}, &fakePantryRepo{}, newFakeEnsurer())
	if _, err := svc.FindFromIngredients(context.Background(), household.NewHouseholdID(), []string{"flour", "  "}); !errors.Is(err, tracking.ErrInvalidIngredient) {
		t.Errorf("FindFromIngredients(blank) = %v, want ErrInvalidIngredient", err)
	}
}
