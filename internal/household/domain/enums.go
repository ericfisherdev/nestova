package domain

import (
	"fmt"

	identity "github.com/ericfisherdev/nestcore/identity/domain"
)

// Role is an alias for nestcore's shared identity Role (NSTR-115): the
// unified owner/adult/child vocabulary now lives on identity.member,
// shared by both apps — see identity.Role's own doc. Aliasing (rather than
// redeclaring) means Role's method set — Valid, String, and
// CanAdminister (the owner-or-adult parent-privilege check that replaces
// this package's former IsParent) — comes directly from identity.Role with
// no risk of the two vocabularies drifting apart.
type Role = identity.Role

// Member roles, re-exported so existing household.RoleOwner/RoleAdult/
// RoleChild call sites across the codebase keep compiling unchanged.
const (
	RoleOwner = identity.RoleOwner
	RoleAdult = identity.RoleAdult
	RoleChild = identity.RoleChild
)

// ParseRole validates and returns a Role, or an error for an unknown value.
func ParseRole(s string) (Role, error) { return identity.ParseRole(s) }

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
