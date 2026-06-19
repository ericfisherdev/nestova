package adapter_test

import (
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/adapter"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func adHocItem(hh household.HouseholdID, name string, addedBy *household.MemberID) *domain.ShoppingListItem {
	return &domain.ShoppingListItem{
		ID: domain.NewShoppingListItemID(), HouseholdID: hh, Name: name,
		Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount},
		Source:   domain.SourceManual, Status: domain.StatusNeeded, AddedBy: addedBy,
	}
}

func restockItem(hh household.HouseholdID, ing domain.IngredientID) *domain.ShoppingListItem {
	return &domain.ShoppingListItem{
		ID: domain.NewShoppingListItemID(), HouseholdID: hh, IngredientID: &ing,
		Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount},
		Source:   domain.SourceRestock, Status: domain.StatusNeeded,
	}
}

func TestShoppingAddAndListByStatus(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Maya")
	ing := seedIngredient(t, pool, "milk")

	adhoc := adHocItem(hh, "Paper Towels", &member)
	if err := repo.Add(ctx, adhoc); err != nil {
		t.Fatalf("Add ad-hoc: %v", err)
	}
	catalogue := &domain.ShoppingListItem{
		ID: domain.NewShoppingListItemID(), HouseholdID: hh, IngredientID: &ing,
		Quantity: qty(t, 2, household.UnitLiter), Source: domain.SourceManual, Status: domain.StatusNeeded,
	}
	if err := repo.Add(ctx, catalogue); err != nil {
		t.Fatalf("Add catalogue: %v", err)
	}

	needed, err := repo.ListByStatus(ctx, hh, domain.StatusNeeded)
	if err != nil {
		t.Fatalf("ListByStatus(needed): %v", err)
	}
	if len(needed) != 2 {
		t.Fatalf("needed list = %d items, want 2", len(needed))
	}

	if _, err := repo.UpdateStatus(ctx, adhoc.ID, domain.StatusInCart); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	needed, _ = repo.ListByStatus(ctx, hh, domain.StatusNeeded)
	inCart, _ := repo.ListByStatus(ctx, hh, domain.StatusInCart)
	if len(needed) != 1 || len(inCart) != 1 {
		t.Errorf("after transition: needed=%d in_cart=%d, want 1 and 1", len(needed), len(inCart))
	}
	if inCart[0].AddedBy == nil || *inCart[0].AddedBy != member {
		t.Errorf("ad-hoc AddedBy = %v, want %v", inCart[0].AddedBy, member)
	}
}

func TestShoppingTransitionLifecycle(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	item := adHocItem(hh, "Bananas", nil)
	if err := repo.Add(ctx, item); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, status := range []domain.ItemStatus{domain.StatusInCart, domain.StatusPurchased} {
		got, err := repo.UpdateStatus(ctx, item.ID, status)
		if err != nil {
			t.Fatalf("UpdateStatus(%s): %v", status, err)
		}
		if got.Status != status {
			t.Errorf("status = %q, want %q", got.Status, status)
		}
	}

	if _, err := repo.UpdateStatus(ctx, domain.NewShoppingListItemID(), domain.StatusInCart); !errors.Is(err, domain.ErrShoppingListItemNotFound) {
		t.Errorf("UpdateStatus(unknown) = %v, want ErrShoppingListItemNotFound", err)
	}
}

func TestShoppingRestockDedup(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "coffee")

	first := restockItem(hh, ing)
	inserted, err := repo.AddRestockIfAbsent(ctx, first)
	if err != nil || !inserted {
		t.Fatalf("first AddRestockIfAbsent = (%v, %v), want (true, nil)", inserted, err)
	}

	// A second open restock entry for the same ingredient is deduped.
	inserted, err = repo.AddRestockIfAbsent(ctx, restockItem(hh, ing))
	if err != nil {
		t.Fatalf("second AddRestockIfAbsent: %v", err)
	}
	if inserted {
		t.Error("second AddRestockIfAbsent inserted a duplicate open restock entry")
	}

	// Once the first is purchased, a fresh restock can be raised again.
	if _, err := repo.UpdateStatus(ctx, first.ID, domain.StatusPurchased); err != nil {
		t.Fatalf("purchase first: %v", err)
	}
	inserted, err = repo.AddRestockIfAbsent(ctx, restockItem(hh, ing))
	if err != nil || !inserted {
		t.Fatalf("post-purchase AddRestockIfAbsent = (%v, %v), want (true, nil)", inserted, err)
	}

	open, _ := repo.ListByStatus(ctx, hh, domain.StatusNeeded)
	if len(open) != 1 {
		t.Errorf("open restock entries = %d, want exactly 1", len(open))
	}
}

func TestShoppingAddRestockGuards(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)

	// Wrong source.
	manual := adHocItem(hh, "x", nil)
	if _, err := repo.AddRestockIfAbsent(ctx, manual); err == nil {
		t.Error("AddRestockIfAbsent(manual source) = nil error, want error")
	}
	// Missing ingredient id.
	noIng := &domain.ShoppingListItem{
		ID: domain.NewShoppingListItemID(), HouseholdID: hh, Name: "x",
		Quantity: qty(t, 1, household.UnitCount), Source: domain.SourceRestock, Status: domain.StatusNeeded,
	}
	if _, err := repo.AddRestockIfAbsent(ctx, noIng); err == nil {
		t.Error("AddRestockIfAbsent(no ingredient) = nil error, want error")
	}
}

func TestShoppingAddFKSentinels(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewShoppingListRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	ing := seedIngredient(t, pool, "flour")

	badHousehold := adHocItem(household.NewHouseholdID(), "ghost", nil)
	if err := repo.Add(ctx, badHousehold); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Errorf("Add(bad household) = %v, want ErrHouseholdNotFound", err)
	}

	badIngredient := &domain.ShoppingListItem{
		ID: domain.NewShoppingListItemID(), HouseholdID: hh, IngredientID: ptr(domain.NewIngredientID()),
		Quantity: qty(t, 1, household.UnitCount), Source: domain.SourceManual, Status: domain.StatusNeeded,
	}
	if err := repo.Add(ctx, badIngredient); !errors.Is(err, domain.ErrIngredientNotFound) {
		t.Errorf("Add(bad ingredient) = %v, want ErrIngredientNotFound", err)
	}
	_ = ing

	badMember := household.NewMemberID()
	badAdder := adHocItem(hh, "note", &badMember)
	if err := repo.Add(ctx, badAdder); !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("Add(bad added_by) = %v, want ErrMemberNotFound", err)
	}
}

func ptr[T any](v T) *T { return &v }
