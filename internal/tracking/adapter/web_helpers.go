package adapter

import (
	"strconv"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// parseLeadDays parses the restock lead-days form value, defaulting to 0 for an
// empty or malformed value (the brief specifies a default of 0). A negative value
// is clamped to 0 since lead days cannot be negative.
func parseLeadDays(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseQuantity builds a validated household.Quantity from the amount and unit
// form values, returning household.ErrInvalidQuantity for a malformed amount or
// unknown unit so the handler can map it to a 400.
func parseQuantity(amountRaw, unitRaw string) (household.Quantity, error) {
	amount, err := strconv.ParseFloat(strings.TrimSpace(amountRaw), 64)
	if err != nil {
		return household.Quantity{}, household.ErrInvalidQuantity
	}
	unit, err := household.ParseUnit(strings.TrimSpace(unitRaw))
	if err != nil {
		// Normalize an unknown unit to the domain's invalid-quantity sentinel so
		// callers can treat every parseQuantity failure as ErrInvalidQuantity.
		return household.Quantity{}, household.ErrInvalidQuantity
	}
	return household.NewQuantity(amount, unit)
}

// parseOptionalDate parses an optional YYYY-MM-DD form value. An empty value
// yields (nil, nil) — the field is optional. A malformed value is an error.
func parseOptionalDate(raw string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := time.Parse(dateLayout, trimmed)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// formatConfidence renders a prediction confidence in [0,1] as a whole-number
// percentage string (e.g. 0.4 → "40%").
func formatConfidence(confidence float64) string {
	return strconv.Itoa(int(confidence*100+0.5)) + "%"
}

// formatQuantity renders a Quantity as a trimmed amount plus its unit (e.g.
// "2 l", "1.5 kg") so integral amounts do not show a spurious ".0".
func formatQuantity(q household.Quantity) string {
	amount := strconv.FormatFloat(q.Amount, 'f', -1, 64)
	return amount + " " + q.Unit.String()
}

// unitOptions returns the measurement-unit dropdown options in canonical order.
func unitOptions() []components.UnitOption {
	units := household.Units()
	opts := make([]components.UnitOption, 0, len(units))
	for _, u := range units {
		opts = append(opts, components.UnitOption{Value: u.String(), Label: u.String()})
	}
	return opts
}

// sourceLabel maps a shopping-item source to its human-readable badge text.
func sourceLabel(source domain.ItemSource) string {
	switch source {
	case domain.SourceManual:
		return "Manual"
	case domain.SourceRestock:
		return "Restock"
	case domain.SourceMealPlan:
		return "Meal plan"
	case domain.SourcePantryLow:
		return "Pantry low"
	default:
		return source.String()
	}
}

// pantryItemName resolves a pantry item's display name from the batch ingredient
// name map, falling back to a stable placeholder when the ingredient is unknown.
func pantryItemName(item *domain.PantryItem, names map[domain.IngredientID]string) string {
	if name, ok := names[item.IngredientID]; ok && name != "" {
		return name
	}
	return "Unknown ingredient"
}

// shoppingItemName resolves a shopping item's display name: its free-text Name
// when set, otherwise the resolved ingredient name, falling back to a stable
// placeholder.
func shoppingItemName(item *domain.ShoppingListItem, names map[domain.IngredientID]string) string {
	if strings.TrimSpace(item.Name) != "" {
		return item.Name
	}
	if item.IngredientID != nil {
		if name, ok := names[*item.IngredientID]; ok && name != "" {
			return name
		}
	}
	return "Unknown ingredient"
}

// toPantryViews maps pantry items to view models, flagging the expiring-soon ones
// via expiringIDs and resolving ingredient names via the batch name map.
func toPantryViews(
	items []*domain.PantryItem,
	expiringIDs map[domain.PantryItemID]bool,
	names map[domain.IngredientID]string,
) []components.PantryItemView {
	views := make([]components.PantryItemView, 0, len(items))
	for _, item := range items {
		view := components.PantryItemView{
			ID:            item.ID.String(),
			Name:          pantryItemName(item, names),
			QuantityLabel: formatQuantity(item.Quantity),
			Unit:          item.Quantity.Unit.String(),
			Expiring:      expiringIDs[item.ID],
		}
		if item.ExpiresOn != nil {
			view.ExpiresLabel = item.ExpiresOn.Format(displayDateLayout)
		}
		views = append(views, view)
	}
	return views
}

// toShoppingViews maps shopping items in one status to view models, resolving
// ingredient names via the batch name map.
func toShoppingViews(items []*domain.ShoppingListItem, names map[domain.IngredientID]string) []components.ShoppingItemView {
	views := make([]components.ShoppingItemView, 0, len(items))
	for _, item := range items {
		views = append(views, components.ShoppingItemView{
			ID:            item.ID.String(),
			Name:          shoppingItemName(item, names),
			QuantityLabel: formatQuantity(item.Quantity),
			SourceLabel:   sourceLabel(item.Source),
			Status:        item.Status.String(),
		})
	}
	return views
}
