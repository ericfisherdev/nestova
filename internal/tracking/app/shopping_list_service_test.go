package app_test

import (
	"context"
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/app"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// fakeShoppingListRepo is an in-memory domain.ShoppingListRepository whose
// ListByStatus honors the household and status filters so delegation tests catch
// mis-passed parameters.
type fakeShoppingListRepo struct {
	items []*domain.ShoppingListItem
}

func (f *fakeShoppingListRepo) Add(_ context.Context, item *domain.ShoppingListItem) error {
	f.items = append(f.items, item)
	return nil
}

func (f *fakeShoppingListRepo) AddRestockIfAbsent(context.Context, *domain.ShoppingListItem) (bool, error) {
	return true, nil
}

func (f *fakeShoppingListRepo) AddMealPlanIfAbsent(context.Context, *domain.ShoppingListItem) (bool, error) {
	return true, nil
}

func (f *fakeShoppingListRepo) UpdateStatus(_ context.Context, householdID household.HouseholdID, id domain.ShoppingListItemID, status domain.ItemStatus) (*domain.ShoppingListItem, error) {
	for _, item := range f.items {
		// Scope by household like the real adapter: a foreign household sees not-found.
		if item.ID == id && item.HouseholdID == householdID {
			item.Status = status
			return item, nil
		}
	}
	return nil, domain.ErrShoppingListItemNotFound
}

func (f *fakeShoppingListRepo) MarkInCart(_ context.Context, householdID household.HouseholdID, id domain.ShoppingListItemID) (*domain.ShoppingListItem, error) {
	for _, item := range f.items {
		if item.ID == id && item.HouseholdID == householdID {
			switch item.Status {
			case domain.StatusNeeded, domain.StatusInCart:
				item.Status = domain.StatusInCart
				return item, nil
			default:
				return nil, domain.ErrShoppingListItemNotInCartable
			}
		}
	}
	return nil, domain.ErrShoppingListItemNotFound
}

func (f *fakeShoppingListRepo) ListByStatus(_ context.Context, householdID household.HouseholdID, status domain.ItemStatus) ([]*domain.ShoppingListItem, error) {
	var out []*domain.ShoppingListItem
	for _, item := range f.items {
		if item.HouseholdID == householdID && item.Status == status {
			out = append(out, item)
		}
	}
	return out, nil
}

func mustShoppingService(t *testing.T, repo domain.ShoppingListRepository) *app.ShoppingListService {
	t.Helper()
	s, err := app.NewShoppingListService(repo)
	if err != nil {
		t.Fatalf("NewShoppingListService: %v", err)
	}
	return s
}

func TestNewShoppingListServiceRejectsNilRepo(t *testing.T) {
	if _, err := app.NewShoppingListService(nil); err == nil {
		t.Error("NewShoppingListService(nil) = nil error, want error")
	}
}

func TestAddManualItemIdentityRule(t *testing.T) {
	svc := mustShoppingService(t, &fakeShoppingListRepo{})
	ctx := context.Background()
	hh := household.NewHouseholdID()
	ing := domain.NewIngredientID()
	q := mustQty(t, 1, household.UnitCount)

	// Neither ingredient nor name → invalid.
	if _, err := svc.AddManualItem(ctx, hh, nil, "  ", q, nil); !errors.Is(err, domain.ErrInvalidShoppingListItem) {
		t.Errorf("AddManualItem(neither) = %v, want ErrInvalidShoppingListItem", err)
	}
	// Both ingredient and name → invalid.
	if _, err := svc.AddManualItem(ctx, hh, &ing, "milk", q, nil); !errors.Is(err, domain.ErrInvalidShoppingListItem) {
		t.Errorf("AddManualItem(both) = %v, want ErrInvalidShoppingListItem", err)
	}
}

func TestAddManualItemValidVariants(t *testing.T) {
	ctx := context.Background()
	hh := household.NewHouseholdID()
	q := mustQty(t, 2, household.UnitCount)

	t.Run("ad-hoc name", func(t *testing.T) {
		repo := &fakeShoppingListRepo{}
		svc := mustShoppingService(t, repo)
		item, err := svc.AddManualItem(ctx, hh, nil, "  Paper Towels  ", q, nil)
		if err != nil {
			t.Fatalf("AddManualItem: %v", err)
		}
		if item.Name != "Paper Towels" || item.IngredientID != nil {
			t.Errorf("ad-hoc item = %+v, want trimmed name and nil ingredient", item)
		}
		if item.Source != domain.SourceManual || item.Status != domain.StatusNeeded {
			t.Errorf("ad-hoc item source/status = %q/%q, want manual/needed", item.Source, item.Status)
		}
	})

	t.Run("catalogue ingredient", func(t *testing.T) {
		repo := &fakeShoppingListRepo{}
		svc := mustShoppingService(t, repo)
		ing := domain.NewIngredientID()
		item, err := svc.AddManualItem(ctx, hh, &ing, "", q, nil)
		if err != nil {
			t.Fatalf("AddManualItem: %v", err)
		}
		if item.IngredientID == nil || *item.IngredientID != ing || item.Name != "" {
			t.Errorf("catalogue item = %+v, want ingredient set and empty name", item)
		}
	})
}

func TestAddManualItemRejectsInvalidQuantity(t *testing.T) {
	svc := mustShoppingService(t, &fakeShoppingListRepo{})
	bad := household.Quantity{Amount: -1, Unit: household.UnitCount}
	if _, err := svc.AddManualItem(context.Background(), household.NewHouseholdID(), nil, "eggs", bad, nil); !errors.Is(err, household.ErrInvalidQuantity) {
		t.Errorf("AddManualItem(invalid qty) = %v, want ErrInvalidQuantity", err)
	}
}

func TestTransitionAndListRejectInvalidStatus(t *testing.T) {
	svc := mustShoppingService(t, &fakeShoppingListRepo{})
	ctx := context.Background()
	if _, err := svc.TransitionStatus(ctx, household.NewHouseholdID(), domain.NewShoppingListItemID(), domain.ItemStatus("shipped")); err == nil {
		t.Error("TransitionStatus(invalid) = nil error, want error")
	}
	if _, err := svc.ListByStatus(ctx, household.NewHouseholdID(), domain.ItemStatus("shipped")); err == nil {
		t.Error("ListByStatus(invalid) = nil error, want error")
	}
}

func TestListByStatusFiltersByHouseholdAndStatus(t *testing.T) {
	repo := &fakeShoppingListRepo{}
	svc := mustShoppingService(t, repo)
	ctx := context.Background()
	hh := household.NewHouseholdID()
	other := household.NewHouseholdID()

	needed, err := svc.AddManualItem(ctx, hh, nil, "milk", mustQty(t, 1, household.UnitCount), nil)
	if err != nil {
		t.Fatalf("AddManualItem: %v", err)
	}
	inCartItem, err := svc.AddManualItem(ctx, hh, nil, "eggs", mustQty(t, 1, household.UnitCount), nil)
	if err != nil {
		t.Fatalf("AddManualItem: %v", err)
	}
	if _, err := svc.TransitionStatus(ctx, inCartItem.HouseholdID, inCartItem.ID, domain.StatusInCart); err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}
	// Another household's needed item must not leak into hh's list.
	if _, err := svc.AddManualItem(ctx, other, nil, "soap", mustQty(t, 1, household.UnitCount), nil); err != nil {
		t.Fatalf("AddManualItem(other): %v", err)
	}

	got, err := svc.ListByStatus(ctx, hh, domain.StatusNeeded)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	if len(got) != 1 || got[0].ID != needed.ID {
		t.Errorf("ListByStatus(needed) = %d items, want only the needed milk item", len(got))
	}
}

func TestTransitionStatusDelegates(t *testing.T) {
	repo := &fakeShoppingListRepo{}
	svc := mustShoppingService(t, repo)
	ctx := context.Background()
	item, err := svc.AddManualItem(ctx, household.NewHouseholdID(), nil, "bread", mustQty(t, 1, household.UnitCount), nil)
	if err != nil {
		t.Fatalf("AddManualItem: %v", err)
	}
	updated, err := svc.TransitionStatus(ctx, item.HouseholdID, item.ID, domain.StatusInCart)
	if err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}
	if updated.Status != domain.StatusInCart {
		t.Errorf("status = %q, want in_cart", updated.Status)
	}
}
