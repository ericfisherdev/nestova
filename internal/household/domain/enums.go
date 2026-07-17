package domain

import "fmt"

// Role is a member's role within a household. Stored as text, validated here.
type Role string

// Member roles.
const (
	RoleOwner Role = "owner"
	RoleAdult Role = "adult"
	RoleChild Role = "child"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdult, RoleChild:
		return true
	default:
		return false
	}
}

// IsParent reports whether r carries household-parent privileges (owner or
// adult) — the role gate shared by chore-trade history (NES-122), reward
// admin (NES-126), and redemption fulfillment (NES-127) across every bounded
// context that needs to distinguish a parent from a child member.
func (r Role) IsParent() bool {
	return r == RoleOwner || r == RoleAdult
}

// String returns the role's stored value.
func (r Role) String() string { return string(r) }

// ParseRole validates and returns a Role, or an error for an unknown value.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if !r.Valid() {
		return "", fmt.Errorf("invalid role %q", s)
	}
	return r, nil
}

// MemberColor is one of the five A · Hearth palette keys. The value matches the
// Tailwind member-color token infix (see web/static/css/input.css), so a member
// renders as bg-member-<color>-tint etc.
type MemberColor string

// The five A · Hearth palette colors, in canonical assignment order.
const (
	ColorSage  MemberColor = "sage"
	ColorClay  MemberColor = "clay"
	ColorOchre MemberColor = "ochre"
	ColorBlue  MemberColor = "blue"
	ColorPlum  MemberColor = "plum"
)

// MemberColors returns the palette in canonical assignment order.
func MemberColors() []MemberColor {
	return []MemberColor{ColorSage, ColorClay, ColorOchre, ColorBlue, ColorPlum}
}

// Valid reports whether c is a known palette color.
func (c MemberColor) Valid() bool {
	switch c {
	case ColorSage, ColorClay, ColorOchre, ColorBlue, ColorPlum:
		return true
	default:
		return false
	}
}

// String returns the color's stored value.
func (c MemberColor) String() string { return string(c) }

// ParseMemberColor validates and returns a MemberColor, or an error for an
// unknown value.
func ParseMemberColor(s string) (MemberColor, error) {
	c := MemberColor(s)
	if !c.Valid() {
		return "", fmt.Errorf("invalid member color %q", s)
	}
	return c, nil
}
