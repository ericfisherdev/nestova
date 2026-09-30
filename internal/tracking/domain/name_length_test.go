package domain_test

import (
	"errors"
	"strings"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func TestShoppingListItemValidate_NameLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label string
		name  string
		want  error
	}{
		{"at the limit", strings.Repeat("a", domain.MaxShoppingListItemNameLength), nil},
		{"multibyte at the limit", strings.Repeat("家", domain.MaxShoppingListItemNameLength), nil},
		{"one over", strings.Repeat("a", domain.MaxShoppingListItemNameLength+1), domain.ErrShoppingListItemNameTooLong},
		{"multibyte one over", strings.Repeat("家", domain.MaxShoppingListItemNameLength+1), domain.ErrShoppingListItemNameTooLong},
		{"far over", strings.Repeat("a", 10_000), domain.ErrShoppingListItemNameTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			item := &domain.ShoppingListItem{
				ID: domain.NewShoppingListItemID(), HouseholdID: household.NewHouseholdID(), Name: tc.name,
				Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount},
				Source:   domain.SourceManual, Status: domain.StatusNeeded,
			}
			if err := item.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestValidateNormalizedName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label string
		name  string
		want  error
	}{
		{"plain", "olive oil", nil},
		{"at the limit", strings.Repeat("a", domain.MaxIngredientNameLength), nil},
		{"multibyte at the limit", strings.Repeat("家", domain.MaxIngredientNameLength), nil},
		{"empty", "", domain.ErrInvalidIngredient},
		{"one over", strings.Repeat("a", domain.MaxIngredientNameLength+1), domain.ErrIngredientNameTooLong},
		{"far over", strings.Repeat("a", 10_000), domain.ErrIngredientNameTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			if err := domain.ValidateNormalizedName(tc.name); !errors.Is(err, tc.want) {
				t.Errorf("ValidateNormalizedName() = %v, want %v", err, tc.want)
			}
		})
	}
	// An over-length name must also read as an invalid ingredient, so callers
	// that already skip an invalid line skip this one too.
	if err := domain.ValidateNormalizedName(strings.Repeat("a", 10_000)); !errors.Is(err, domain.ErrInvalidIngredient) {
		t.Errorf("over-length name = %v, want it to wrap ErrInvalidIngredient", err)
	}
}
