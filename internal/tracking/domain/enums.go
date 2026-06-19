package domain

import "fmt"

// UsageType classifies a usage event on a tracked item. Stored as text and
// validated here (typed-string convention, not iota), mirrored by a Postgres
// CHECK on usage_event.type.
type UsageType string

// Usage event types. Only Depleted drives restock prediction (see the NES-41
// engine); the others record handling that does not by itself imply depletion.
const (
	UsageReplaced UsageType = "replaced"
	UsageRefilled UsageType = "refilled"
	UsageDepleted UsageType = "depleted"
	UsageOpened   UsageType = "opened"
)

// UsageTypes returns the supported usage types in canonical order.
func UsageTypes() []UsageType {
	return []UsageType{UsageReplaced, UsageRefilled, UsageDepleted, UsageOpened}
}

// Valid reports whether t is a known usage type.
func (t UsageType) Valid() bool {
	switch t {
	case UsageReplaced, UsageRefilled, UsageDepleted, UsageOpened:
		return true
	default:
		return false
	}
}

// String returns the usage type's stored value.
func (t UsageType) String() string { return string(t) }

// ParseUsageType validates and returns a UsageType, or an error for an unknown
// value.
func ParseUsageType(s string) (UsageType, error) {
	t := UsageType(s)
	if !t.Valid() {
		return "", fmt.Errorf("invalid usage type %q", s)
	}
	return t, nil
}

// ItemSource records where a shopping-list item came from. Stored as text,
// validated here, mirrored by a Postgres CHECK on shopping_list_item.source.
type ItemSource string

// Shopping-list item sources. SourceManual is member-entered; the rest are
// system-generated (SourceRestock by the restock automation, the others reserved
// for meal planning and low-pantry suggestions).
const (
	SourceManual    ItemSource = "manual"
	SourceRestock   ItemSource = "restock"
	SourceMealPlan  ItemSource = "meal_plan"
	SourcePantryLow ItemSource = "pantry_low"
)

// ItemSources returns the supported sources in canonical order.
func ItemSources() []ItemSource {
	return []ItemSource{SourceManual, SourceRestock, SourceMealPlan, SourcePantryLow}
}

// Valid reports whether s is a known source.
func (s ItemSource) Valid() bool {
	switch s {
	case SourceManual, SourceRestock, SourceMealPlan, SourcePantryLow:
		return true
	default:
		return false
	}
}

// String returns the source's stored value.
func (s ItemSource) String() string { return string(s) }

// ParseItemSource validates and returns an ItemSource, or an error for an
// unknown value.
func ParseItemSource(s string) (ItemSource, error) {
	src := ItemSource(s)
	if !src.Valid() {
		return "", fmt.Errorf("invalid item source %q", s)
	}
	return src, nil
}

// ItemStatus is a shopping-list item's lifecycle state. Stored as text,
// validated here, mirrored by a Postgres CHECK on shopping_list_item.status.
type ItemStatus string

// Shopping-list item statuses, in lifecycle order.
const (
	StatusNeeded    ItemStatus = "needed"
	StatusInCart    ItemStatus = "in_cart"
	StatusPurchased ItemStatus = "purchased"
)

// ItemStatuses returns the supported statuses in lifecycle order.
func ItemStatuses() []ItemStatus {
	return []ItemStatus{StatusNeeded, StatusInCart, StatusPurchased}
}

// Valid reports whether s is a known status.
func (s ItemStatus) Valid() bool {
	switch s {
	case StatusNeeded, StatusInCart, StatusPurchased:
		return true
	default:
		return false
	}
}

// String returns the status's stored value.
func (s ItemStatus) String() string { return string(s) }

// ParseItemStatus validates and returns an ItemStatus, or an error for an
// unknown value.
func ParseItemStatus(s string) (ItemStatus, error) {
	st := ItemStatus(s)
	if !st.Valid() {
		return "", fmt.Errorf("invalid item status %q", s)
	}
	return st, nil
}
