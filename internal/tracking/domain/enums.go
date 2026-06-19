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
