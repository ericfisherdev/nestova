package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func sampleGroceriesView() components.GroceriesView {
	return components.GroceriesView{
		TrackedItems: []components.TrackedItemView{
			{
				ID:                      "item-1",
				Name:                    "Coffee beans",
				Category:                "pantry",
				HasPrediction:           true,
				PredictedDepletionLabel: "Jul 1, 2026",
				ConfidenceLabel:         "40%",
			},
			{ID: "item-2", Name: "Dish soap"},
		},
		Pantry: []components.PantryItemView{
			{ID: "pantry-1", Name: "Olive oil", QuantityLabel: "2 l", Unit: "l", ExpiresLabel: "Jun 25, 2026", Expiring: true},
			{ID: "pantry-2", Name: "Flour", QuantityLabel: "1 kg", Unit: "kg"},
		},
		Needed: []components.ShoppingItemView{
			{ID: "shop-1", Name: "Paper towels", QuantityLabel: "1 count", SourceLabel: "Manual", Status: "needed"},
		},
		InCart: []components.ShoppingItemView{
			{ID: "shop-2", Name: "Milk", QuantityLabel: "2 l", SourceLabel: "Restock", Status: "in_cart"},
		},
		Purchased: nil,
		Units: []components.UnitOption{
			{Value: "count", Label: "count"},
			{Value: "l", Label: "l"},
		},
		CSRFToken: "csrf-token-abc",
	}
}

func TestGroceriesPage_RendersKeyElements(t *testing.T) {
	out := renderString(t, components.GroceriesPage(sampleGroceriesView()))

	for _, want := range []string{
		"Groceries",              // page heading
		"Usage tracker",          // section heading
		"Pantry",                 // section heading
		"Shopping list",          // section heading
		"Coffee beans",           // tracked item name
		"Jul 1, 2026",            // predicted depletion date
		"40% confidence",         // prediction confidence label
		"No prediction yet",      // tracked item without a prediction
		"Olive oil",              // pantry item name
		"2 l",                    // pantry quantity label
		"Expiring Jun 25, 2026",  // expiring-soon highlight text
		"Paper towels",           // needed shopping item
		"Manual",                 // source badge
		"Restock",                // source badge (in cart item)
		"Needed",                 // status group heading
		"In cart",                // status group heading
		"Purchased",              // status group heading
		`value="csrf-token-abc"`, // CSRF token field
	} {
		if !strings.Contains(out, want) {
			t.Errorf("GroceriesPage missing %q", want)
		}
	}
}

func TestGroceriesPage_PostFormsCarryHTMXAndCSRF(t *testing.T) {
	out := renderString(t, components.GroceriesPage(sampleGroceriesView()))

	for _, want := range []string{
		`hx-post="/groceries/items"`,                   // register item
		`hx-post="/groceries/items/item-1/usage"`,      // usage action
		`hx-post="/groceries/pantry"`,                  // pantry add
		`hx-post="/groceries/pantry/pantry-1/consume"`, // pantry consume
		`hx-post="/groceries/pantry/pantry-1/adjust"`,  // pantry adjust
		`hx-post="/groceries/shopping"`,                // shopping add
		`hx-post="/groceries/shopping/shop-1/status"`,  // status toggle
		`name="csrf_token"`,                            // CSRF field present
		`name="type"`,                                  // usage type hidden field
		`name="status"`,                                // status hidden field
	} {
		if !strings.Contains(out, want) {
			t.Errorf("GroceriesPage missing %q", want)
		}
	}
}

func TestGroceriesPage_ExpiringHighlight(t *testing.T) {
	out := renderString(t, components.GroceriesPage(sampleGroceriesView()))
	if !strings.Contains(out, "bg-member-ochre-tint") {
		t.Errorf("GroceriesPage missing expiring-soon highlight class: %q", out)
	}
}

func TestGroceriesPage_EmptyStates(t *testing.T) {
	out := renderString(t, components.GroceriesPage(components.GroceriesView{
		Units:     []components.UnitOption{{Value: "count", Label: "count"}},
		CSRFToken: "tok",
	}))

	for _, want := range []string{
		"No tracked items yet",
		"Your pantry is empty",
		"Nothing here.", // empty shopping status groups
	} {
		if !strings.Contains(out, want) {
			t.Errorf("GroceriesPage empty state missing %q", want)
		}
	}
}
