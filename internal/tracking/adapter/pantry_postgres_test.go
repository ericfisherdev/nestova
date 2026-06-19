package adapter_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/adapter"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func seedIngredient(t *testing.T, pool *pgxpool.Pool, name string) domain.IngredientID {
	t.Helper()
	ing, err := adapter.NewIngredientRepository(pool).EnsureIngredient(testCtx(t), name)
	if err != nil {
		t.Fatalf("seed ingredient %q: %v", name, err)
	}
	return ing.ID
}

func qty(t *testing.T, amount float64, unit household.Unit) household.Quantity {
	t.Helper()
	q, err := household.NewQuantity(amount, unit)
	if err != nil {
		t.Fatalf("NewQuantity: %v", err)
	}
	return q
}

func createPantryItem(t *testing.T, repo *adapter.PantryRepository, hh household.HouseholdID, ing domain.IngredientID, quantity household.Quantity, expires *time.Time) *domain.PantryItem {
	t.Helper()
	item := &domain.PantryItem{
		ID: domain.NewPantryItemID(), HouseholdID: hh, IngredientID: ing,
		Quantity: quantity, ExpiresOn: expires,
	}
	if err := repo.Create(testCtx(t), item); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return item
}

func TestPantryCreateGetAndMutate(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPantryRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "milk")

	expires := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	item := createPantryItem(t, repo, hh, ing, qty(t, 2.5, household.UnitLiter), &expires)

	got, err := repo.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Quantity.Amount != 2.5 || got.Quantity.Unit != household.UnitLiter {
		t.Errorf("Get quantity = %+v, want 2.5 l", got.Quantity)
	}
	if got.ExpiresOn == nil || !got.ExpiresOn.Equal(expires) {
		t.Errorf("Get ExpiresOn = %v, want %v", got.ExpiresOn, expires)
	}

	adjusted, err := repo.Adjust(ctx, item.ID, qty(t, 1.5, household.UnitLiter))
	if err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	if adjusted.Quantity.Amount != 4 {
		t.Errorf("after Adjust = %v, want 4", adjusted.Quantity.Amount)
	}

	consumed, err := repo.Consume(ctx, item.ID, qty(t, 3, household.UnitLiter))
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if consumed.Quantity.Amount != 1 {
		t.Errorf("after Consume = %v, want 1", consumed.Quantity.Amount)
	}

	// Mutation is persisted.
	reloaded, err := repo.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get after mutate: %v", err)
	}
	if reloaded.Quantity.Amount != 1 {
		t.Errorf("persisted quantity = %v, want 1", reloaded.Quantity.Amount)
	}
}

func TestPantryMutateErrors(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPantryRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "oil")
	item := createPantryItem(t, repo, hh, ing, qty(t, 2, household.UnitLiter), nil)

	if _, err := repo.Consume(ctx, item.ID, qty(t, 5, household.UnitLiter)); !errors.Is(err, household.ErrInvalidQuantity) {
		t.Errorf("Consume below zero = %v, want ErrInvalidQuantity", err)
	}
	if _, err := repo.Consume(ctx, item.ID, qty(t, 1, household.UnitGram)); !errors.Is(err, household.ErrUnitMismatch) {
		t.Errorf("Consume unit mismatch = %v, want ErrUnitMismatch", err)
	}
	if _, err := repo.Adjust(ctx, domain.NewPantryItemID(), qty(t, 1, household.UnitLiter)); !errors.Is(err, domain.ErrPantryItemNotFound) {
		t.Errorf("Adjust(unknown) = %v, want ErrPantryItemNotFound", err)
	}
	if _, err := repo.Consume(ctx, domain.NewPantryItemID(), qty(t, 1, household.UnitLiter)); !errors.Is(err, domain.ErrPantryItemNotFound) {
		t.Errorf("Consume(unknown) = %v, want ErrPantryItemNotFound", err)
	}

	// A rejected mutation leaves the stored quantity untouched.
	got, err := repo.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Quantity.Amount != 2 {
		t.Errorf("quantity after rejected mutations = %v, want 2", got.Quantity.Amount)
	}
}

func TestPantryGetNotFoundAndFKSentinels(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPantryRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "flour")

	if _, err := repo.Get(ctx, domain.NewPantryItemID()); !errors.Is(err, domain.ErrPantryItemNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrPantryItemNotFound", err)
	}

	badHousehold := &domain.PantryItem{
		ID: domain.NewPantryItemID(), HouseholdID: household.NewHouseholdID(),
		IngredientID: ing, Quantity: qty(t, 1, household.UnitCount),
	}
	if err := repo.Create(ctx, badHousehold); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Errorf("Create(bad household) = %v, want ErrHouseholdNotFound", err)
	}

	badIngredient := &domain.PantryItem{
		ID: domain.NewPantryItemID(), HouseholdID: hh,
		IngredientID: domain.NewIngredientID(), Quantity: qty(t, 1, household.UnitCount),
	}
	if err := repo.Create(ctx, badIngredient); !errors.Is(err, domain.ErrIngredientNotFound) {
		t.Errorf("Create(bad ingredient) = %v, want ErrIngredientNotFound", err)
	}
}

func TestPantryListByHouseholdIsolatesHouseholds(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPantryRepository(pool)
	ctx := testCtx(t)
	hhA := seedHousehold(t, pool)
	hhB := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "rice")

	createPantryItem(t, repo, hhA, ing, qty(t, 1, household.UnitKilogram), nil)
	createPantryItem(t, repo, hhA, ing, qty(t, 2, household.UnitKilogram), nil)
	createPantryItem(t, repo, hhB, ing, qty(t, 9, household.UnitKilogram), nil)

	itemsA, err := repo.ListByHousehold(ctx, hhA)
	if err != nil {
		t.Fatalf("ListByHousehold(A): %v", err)
	}
	if len(itemsA) != 2 {
		t.Errorf("household A has %d items, want 2 (B's item must be excluded)", len(itemsA))
	}
	for _, it := range itemsA {
		if it.HouseholdID != hhA {
			t.Errorf("ListByHousehold(A) returned item from household %v", it.HouseholdID)
		}
	}
}

func TestPantryListExpiringWithin(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewPantryRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "yogurt")

	asOf := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	soon := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)     // within window
	boundary := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC) // exactly asOf + 7 days (inclusive)
	later := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)    // beyond window

	createPantryItem(t, repo, hh, ing, qty(t, 1, household.UnitCount), &soon)
	createPantryItem(t, repo, hh, ing, qty(t, 1, household.UnitCount), &boundary)
	createPantryItem(t, repo, hh, ing, qty(t, 1, household.UnitCount), &later)
	createPantryItem(t, repo, hh, ing, qty(t, 1, household.UnitCount), nil) // no expiry — excluded

	expiring, err := repo.ListExpiringWithin(ctx, hh, asOf, 7)
	if err != nil {
		t.Fatalf("ListExpiringWithin: %v", err)
	}
	if len(expiring) != 2 {
		t.Fatalf("ListExpiringWithin returned %d items, want 2 (soon + boundary, inclusive)", len(expiring))
	}
	// Ordered ascending by expiry.
	if !expiring[0].ExpiresOn.Equal(soon) || !expiring[1].ExpiresOn.Equal(boundary) {
		t.Errorf("expiring order = [%v, %v], want [%v, %v]",
			expiring[0].ExpiresOn, expiring[1].ExpiresOn, soon, boundary)
	}
}
