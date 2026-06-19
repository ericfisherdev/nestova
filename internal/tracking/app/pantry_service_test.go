package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/app"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// fakePantryRepo is an in-memory domain.PantryRepository for hermetic tests. Its
// Adjust/Consume mirror the real adapter by routing through Quantity arithmetic,
// so unit-mismatch and below-zero errors surface through the service.
type fakePantryRepo struct {
	items map[domain.PantryItemID]*domain.PantryItem
}

func newFakePantryRepo() *fakePantryRepo {
	return &fakePantryRepo{items: map[domain.PantryItemID]*domain.PantryItem{}}
}

func clonePantryItem(item *domain.PantryItem) *domain.PantryItem {
	cp := *item
	if item.ExpiresOn != nil {
		exp := *item.ExpiresOn
		cp.ExpiresOn = &exp
	}
	return &cp
}

func (f *fakePantryRepo) Create(_ context.Context, item *domain.PantryItem) error {
	f.items[item.ID] = clonePantryItem(item)
	return nil
}

func (f *fakePantryRepo) Get(_ context.Context, id domain.PantryItemID) (*domain.PantryItem, error) {
	item, ok := f.items[id]
	if !ok {
		return nil, domain.ErrPantryItemNotFound
	}
	return clonePantryItem(item), nil
}

func (f *fakePantryRepo) Adjust(_ context.Context, _ household.HouseholdID, id domain.PantryItemID, delta household.Quantity) (*domain.PantryItem, error) {
	return f.mutate(id, func(current household.Quantity) (household.Quantity, error) { return current.Add(delta) })
}

func (f *fakePantryRepo) Consume(_ context.Context, _ household.HouseholdID, id domain.PantryItemID, amount household.Quantity) (*domain.PantryItem, error) {
	return f.mutate(id, func(current household.Quantity) (household.Quantity, error) { return current.Subtract(amount) })
}

func (f *fakePantryRepo) mutate(id domain.PantryItemID, op func(household.Quantity) (household.Quantity, error)) (*domain.PantryItem, error) {
	item, ok := f.items[id]
	if !ok {
		return nil, domain.ErrPantryItemNotFound
	}
	updated, err := op(item.Quantity)
	if err != nil {
		return nil, err
	}
	item.Quantity = updated
	return clonePantryItem(item), nil
}

func (f *fakePantryRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*domain.PantryItem, error) {
	return f.all(), nil
}

func (f *fakePantryRepo) ListExpiringWithin(context.Context, household.HouseholdID, time.Time, int) ([]*domain.PantryItem, error) {
	return f.all(), nil
}

func (f *fakePantryRepo) all() []*domain.PantryItem {
	out := make([]*domain.PantryItem, 0, len(f.items))
	for _, item := range f.items {
		out = append(out, clonePantryItem(item))
	}
	return out
}

func mustQty(t *testing.T, amount float64, unit household.Unit) household.Quantity {
	t.Helper()
	q, err := household.NewQuantity(amount, unit)
	if err != nil {
		t.Fatalf("NewQuantity(%v, %q): %v", amount, unit, err)
	}
	return q
}

func mustService(t *testing.T, repo domain.PantryRepository) *app.PantryService {
	t.Helper()
	s, err := app.NewPantryService(repo)
	if err != nil {
		t.Fatalf("NewPantryService: %v", err)
	}
	return s
}

func TestNewPantryServiceRejectsNilRepo(t *testing.T) {
	if _, err := app.NewPantryService(nil); err == nil {
		t.Error("NewPantryService(nil) = nil error, want error")
	}
}

func TestPantryListDelegatesToRepository(t *testing.T) {
	repo := newFakePantryRepo()
	svc := mustService(t, repo)
	ctx := context.Background()
	hh := household.NewHouseholdID()
	if _, err := svc.Add(ctx, hh, domain.NewIngredientID(), mustQty(t, 1, household.UnitCount), nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := svc.Add(ctx, hh, domain.NewIngredientID(), mustQty(t, 2, household.UnitCount), nil); err != nil {
		t.Fatalf("Add: %v", err)
	}

	listed, err := svc.List(ctx, hh)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("List returned %d items, want 2", len(listed))
	}
	expiring, err := svc.ListExpiringWithin(ctx, hh, time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC), 7)
	if err != nil {
		t.Fatalf("ListExpiringWithin: %v", err)
	}
	if len(expiring) != 2 {
		t.Errorf("ListExpiringWithin returned %d items, want 2 (delegated)", len(expiring))
	}
}

func TestPantryAddRejectsInvalidQuantity(t *testing.T) {
	svc := mustService(t, newFakePantryRepo())
	bad := household.Quantity{Amount: -1, Unit: household.UnitCount}
	if _, err := svc.Add(context.Background(), household.NewHouseholdID(), domain.NewIngredientID(), bad, nil); !errors.Is(err, household.ErrInvalidQuantity) {
		t.Errorf("Add(invalid qty) = %v, want ErrInvalidQuantity", err)
	}
}

func TestPantryAdjustAndConsume(t *testing.T) {
	repo := newFakePantryRepo()
	svc := mustService(t, repo)
	ctx := context.Background()

	item, err := svc.Add(ctx, household.NewHouseholdID(), domain.NewIngredientID(), mustQty(t, 5, household.UnitCount), nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	adjusted, err := svc.Adjust(ctx, item.HouseholdID, item.ID, mustQty(t, 3, household.UnitCount))
	if err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	if adjusted.Quantity.Amount != 8 {
		t.Errorf("after Adjust = %v, want 8", adjusted.Quantity.Amount)
	}

	consumed, err := svc.Consume(ctx, item.HouseholdID, item.ID, mustQty(t, 2, household.UnitCount))
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if consumed.Quantity.Amount != 6 {
		t.Errorf("after Consume = %v, want 6", consumed.Quantity.Amount)
	}
}

func TestPantryConsumeBelowZeroAndUnitMismatch(t *testing.T) {
	repo := newFakePantryRepo()
	svc := mustService(t, repo)
	ctx := context.Background()
	item, err := svc.Add(ctx, household.NewHouseholdID(), domain.NewIngredientID(), mustQty(t, 2, household.UnitLiter), nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := svc.Consume(ctx, item.HouseholdID, item.ID, mustQty(t, 5, household.UnitLiter)); !errors.Is(err, household.ErrInvalidQuantity) {
		t.Errorf("Consume below zero = %v, want ErrInvalidQuantity", err)
	}
	if _, err := svc.Consume(ctx, item.HouseholdID, item.ID, mustQty(t, 1, household.UnitGram)); !errors.Is(err, household.ErrUnitMismatch) {
		t.Errorf("Consume unit mismatch = %v, want ErrUnitMismatch", err)
	}
	if _, err := svc.Adjust(ctx, item.HouseholdID, item.ID, mustQty(t, 1, household.UnitGram)); !errors.Is(err, household.ErrUnitMismatch) {
		t.Errorf("Adjust unit mismatch = %v, want ErrUnitMismatch", err)
	}
}

func TestPantryAdjustAndConsumeUnknownItem(t *testing.T) {
	svc := mustService(t, newFakePantryRepo())
	ctx := context.Background()
	if _, err := svc.Adjust(ctx, household.NewHouseholdID(), domain.NewPantryItemID(), mustQty(t, 1, household.UnitCount)); !errors.Is(err, domain.ErrPantryItemNotFound) {
		t.Errorf("Adjust(unknown) = %v, want ErrPantryItemNotFound", err)
	}
	if _, err := svc.Consume(ctx, household.NewHouseholdID(), domain.NewPantryItemID(), mustQty(t, 1, household.UnitCount)); !errors.Is(err, domain.ErrPantryItemNotFound) {
		t.Errorf("Consume(unknown) = %v, want ErrPantryItemNotFound", err)
	}
}
