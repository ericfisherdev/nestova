package domain

import (
	"errors"
	"fmt"
	"math"
)

// Unit is a measurement unit for a Quantity. Stored as text, validated here,
// following the same typed-string convention as Role and Freq (not iota), so a
// matching Postgres CHECK can mirror the allowed set.
type Unit string

// Supported measurement units. The set is deliberately small: a unitless count
// for discrete items (eggs, rolls) plus mass and volume in metric. Add/Subtract
// require identical units — Quantity performs no unit conversion.
const (
	UnitCount      Unit = "count"
	UnitGram       Unit = "g"
	UnitKilogram   Unit = "kg"
	UnitMilliliter Unit = "ml"
	UnitLiter      Unit = "l"
)

// Units returns the supported units in canonical order. Callers (e.g. a CHECK
// constraint generator or a UI dropdown) can range over this rather than
// hard-coding the set.
func Units() []Unit {
	return []Unit{UnitCount, UnitGram, UnitKilogram, UnitMilliliter, UnitLiter}
}

// Valid reports whether u is a known unit.
func (u Unit) Valid() bool {
	switch u {
	case UnitCount, UnitGram, UnitKilogram, UnitMilliliter, UnitLiter:
		return true
	default:
		return false
	}
}

// String returns the unit's stored value.
func (u Unit) String() string { return string(u) }

// ParseUnit validates and returns a Unit, or an error for an unknown value.
func ParseUnit(s string) (Unit, error) {
	u := Unit(s)
	if !u.Valid() {
		return "", fmt.Errorf("invalid unit %q", s)
	}
	return u, nil
}

// Quantity errors.
var (
	// ErrInvalidQuantity is returned for a malformed quantity: an unknown unit,
	// a non-finite amount, or a negative amount (including a Subtract that would
	// drop below zero).
	ErrInvalidQuantity = errors.New("household: invalid quantity")
	// ErrUnitMismatch is returned by Add/Subtract when the operands carry
	// different units. Quantity does not convert between units.
	ErrUnitMismatch = errors.New("household: quantity unit mismatch")
)

// Quantity is the shared-kernel value object for an amount of something in a
// given unit. It is a value type used by copy: Add and Subtract return new
// Quantities and never mutate the receiver. Construct one with NewQuantity to
// enforce the invariants — a valid Quantity has a finite, non-negative Amount
// and a known Unit. The fields are exported for adapter scanning and
// serialization; assigning to them directly bypasses validation, so treat a
// Quantity as read-only after construction and re-validate (or rebuild via
// NewQuantity) if you must mutate it.
type Quantity struct {
	Amount float64
	Unit   Unit
}

// NewQuantity constructs a validated Quantity, returning ErrInvalidQuantity for
// an unknown unit or a non-finite/negative amount.
func NewQuantity(amount float64, unit Unit) (Quantity, error) {
	q := Quantity{Amount: amount, Unit: unit}
	if err := q.Validate(); err != nil {
		return Quantity{}, err
	}
	return q, nil
}

// Validate reports whether the quantity is well-formed, wrapping
// ErrInvalidQuantity with detail otherwise.
func (q Quantity) Validate() error {
	if !q.Unit.Valid() {
		return fmt.Errorf("%w: unknown unit %q", ErrInvalidQuantity, q.Unit)
	}
	if math.IsNaN(q.Amount) || math.IsInf(q.Amount, 0) {
		return fmt.Errorf("%w: amount must be finite", ErrInvalidQuantity)
	}
	if q.Amount < 0 {
		return fmt.Errorf("%w: amount must be non-negative, got %v", ErrInvalidQuantity, q.Amount)
	}
	return nil
}

// Add returns the sum of q and other. Both operands must be valid and share the
// same unit; otherwise it returns ErrUnitMismatch or ErrInvalidQuantity.
func (q Quantity) Add(other Quantity) (Quantity, error) {
	if err := bothValid(q, other); err != nil {
		return Quantity{}, err
	}
	if q.Unit != other.Unit {
		return Quantity{}, fmt.Errorf("%w: %s vs %s", ErrUnitMismatch, q.Unit, other.Unit)
	}
	return NewQuantity(q.Amount+other.Amount, q.Unit)
}

// Subtract returns q minus other. Both operands must be valid and share the same
// unit; a result below zero is rejected with ErrInvalidQuantity (you cannot have
// a negative on-hand amount).
func (q Quantity) Subtract(other Quantity) (Quantity, error) {
	if err := bothValid(q, other); err != nil {
		return Quantity{}, err
	}
	if q.Unit != other.Unit {
		return Quantity{}, fmt.Errorf("%w: %s vs %s", ErrUnitMismatch, q.Unit, other.Unit)
	}
	return NewQuantity(q.Amount-other.Amount, q.Unit)
}

// bothValid validates both operands of a binary Quantity operation.
func bothValid(a, b Quantity) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return b.Validate()
}
