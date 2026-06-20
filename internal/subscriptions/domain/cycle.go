package domain

import (
	"errors"
	"fmt"
	"math"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Cycle is a subscription's billing cadence. Stored as text, validated here (not
// iota), so a matching Postgres CHECK can mirror the allowed set.
type Cycle string

// Billing cycles.
const (
	// CycleWeekly bills every seven days.
	CycleWeekly Cycle = "weekly"
	// CycleMonthly bills once per calendar month.
	CycleMonthly Cycle = "monthly"
	// CycleYearly bills once per calendar year.
	CycleYearly Cycle = "yearly"
	// CycleCustom marks a non-standard cadence the app neither normalizes nor
	// auto-renews. Such subscriptions are stored and displayed but excluded from
	// the monthly cost rollup (NormalizeMonthly returns ErrUnsupportedCycle) and
	// from the renewal scheduler.
	CycleCustom Cycle = "custom"
)

// Cycles returns the supported billing cycles in canonical order. Callers (e.g.
// a CHECK-constraint generator or a UI dropdown) can range over this rather than
// hard-coding the set.
func Cycles() []Cycle {
	return []Cycle{CycleWeekly, CycleMonthly, CycleYearly, CycleCustom}
}

// Valid reports whether c is a known billing cycle.
func (c Cycle) Valid() bool {
	switch c {
	case CycleWeekly, CycleMonthly, CycleYearly, CycleCustom:
		return true
	default:
		return false
	}
}

// String returns the cycle's stored value.
func (c Cycle) String() string { return string(c) }

// ParseCycle validates and returns a Cycle, or an error for an unknown value.
func ParseCycle(s string) (Cycle, error) {
	c := Cycle(s)
	if !c.Valid() {
		return "", fmt.Errorf("invalid cycle %q", s)
	}
	return c, nil
}

// ErrUnsupportedCycle is returned by NormalizeMonthly for a cycle that has no
// defined monthly-normalized figure (the custom cadence, or an unknown value).
// Callers treat such subscriptions as a zero contribution to a cost rollup.
var ErrUnsupportedCycle = errors.New("subscriptions: unsupported cycle for monthly normalization")

// Normalization constants: a year is treated as exactly 52 weeks and 12 months.
const (
	weeksPerYear  = 52
	monthsPerYear = 12
)

// NormalizeMonthly converts a subscription's per-cycle amount to an equivalent
// monthly figure: weekly is scaled by 52/12, monthly is unchanged, and yearly
// is divided by 12. The result is rounded to the nearest minor unit (half up;
// equivalently half away from zero, as Money.Cents is non-negative). The custom
// cycle (and any unknown value) has no defined monthly figure and returns
// ErrUnsupportedCycle. amount must be valid.
//
// The arithmetic is integer-only: a float64 round-trip would lose precision for
// large Cents values, and an out-of-range float64->int64 conversion is
// implementation-dependent per the Go spec. The weekly multiplication is
// guarded against int64 overflow and returns ErrInvalidMoney if it would wrap.
//
// This lives in the subscriptions domain rather than on Money so the shared
// kernel stays free of the Cycle concept (and the import stays one-directional:
// subscriptions depends on household, never the reverse).
func NormalizeMonthly(amount household.Money, cycle Cycle) (household.Money, error) {
	if err := amount.Validate(); err != nil {
		return household.Money{}, err
	}
	var cents int64
	switch cycle {
	case CycleWeekly:
		// round(cents * 52 / 12); add half the divisor before the integer
		// division to round half up. Guard the multiplication against overflow.
		if amount.Cents > (math.MaxInt64-monthsPerYear/2)/weeksPerYear {
			return household.Money{}, fmt.Errorf("%w: weekly amount too large to normalize", household.ErrInvalidMoney)
		}
		cents = (amount.Cents*weeksPerYear + monthsPerYear/2) / monthsPerYear
	case CycleMonthly:
		cents = amount.Cents
	case CycleYearly:
		// round(cents / 12); amount.Cents + 6 cannot overflow because a valid
		// Money's Cents is well below MaxInt64-6, but the check is cheap insurance.
		if amount.Cents > math.MaxInt64-monthsPerYear/2 {
			return household.Money{}, fmt.Errorf("%w: yearly amount too large to normalize", household.ErrInvalidMoney)
		}
		cents = (amount.Cents + monthsPerYear/2) / monthsPerYear
	case CycleCustom:
		return household.Money{}, ErrUnsupportedCycle
	default:
		return household.Money{}, fmt.Errorf("%w: %q", ErrUnsupportedCycle, cycle)
	}
	return household.NewMoney(cents, amount.Currency)
}
