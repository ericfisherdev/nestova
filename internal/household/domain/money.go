package domain

import (
	"errors"
	"fmt"
	"math"
)

// Money errors.
var (
	// ErrInvalidMoney is returned for malformed money: a negative amount or a
	// currency code that is not three uppercase ASCII letters.
	ErrInvalidMoney = errors.New("household: invalid money")
	// ErrCurrencyMismatch is returned by Add when operands carry different
	// currencies. Money performs no currency conversion.
	ErrCurrencyMismatch = errors.New("household: money currency mismatch")
)

// Money is the shared-kernel value object for a monetary amount, stored as an
// integer number of minor units (e.g. US cents) in an ISO-4217-style currency.
// It is co-located with Quantity as a shared-kernel value type reused by any
// cost-bearing context (subscriptions today). It is a value type used by copy:
// Add returns a new Money and never mutates the receiver. Construct one with
// NewMoney to enforce the invariants — a valid Money has a non-negative Cents
// amount and a three-letter uppercase Currency. The fields are exported for
// adapter scanning and serialization; assigning to them directly bypasses
// validation, so treat a Money as read-only after construction and rebuild via
// NewMoney if you must change it.
type Money struct {
	// Cents is the amount in the currency's minor unit (e.g. US cents). It is
	// non-negative; a meaningful zero is permitted (callers that require a
	// strictly positive amount, such as a subscription cost, enforce that
	// themselves).
	Cents int64
	// Currency is the ISO-4217 alphabetic code (three uppercase ASCII letters,
	// e.g. "USD").
	Currency string
}

// NewMoney constructs a validated Money, returning ErrInvalidMoney for a
// negative amount or a malformed currency code.
func NewMoney(cents int64, currency string) (Money, error) {
	m := Money{Cents: cents, Currency: currency}
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	return m, nil
}

// Validate reports whether the money is well-formed, wrapping ErrInvalidMoney
// with detail otherwise.
func (m Money) Validate() error {
	if m.Cents < 0 {
		return fmt.Errorf("%w: amount must be non-negative, got %d", ErrInvalidMoney, m.Cents)
	}
	if !validCurrency(m.Currency) {
		return fmt.Errorf("%w: currency must be three uppercase letters, got %q", ErrInvalidMoney, m.Currency)
	}
	return nil
}

// Add returns the sum of m and other. Both operands must be valid and share the
// same currency; otherwise it returns ErrCurrencyMismatch or ErrInvalidMoney.
func (m Money) Add(other Money) (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	if err := other.Validate(); err != nil {
		return Money{}, err
	}
	if m.Currency != other.Currency {
		return Money{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.Currency, other.Currency)
	}
	// Both Cents are non-negative (Validate guarantees it), so guard the sum
	// against int64 overflow explicitly rather than relying on NewMoney to
	// reject the wrapped-around negative result with a misleading message.
	if m.Cents > math.MaxInt64-other.Cents {
		return Money{}, fmt.Errorf("%w: sum would overflow", ErrInvalidMoney)
	}
	return NewMoney(m.Cents+other.Cents, m.Currency)
}

// String formats the amount as major units with two minor digits followed by
// the currency code, e.g. "12.34 USD". It assumes a two-decimal minor unit,
// which holds for the currencies this app supports.
func (m Money) String() string {
	return fmt.Sprintf("%d.%02d %s", m.Cents/100, m.Cents%100, m.Currency)
}

// validCurrency reports whether s is exactly three uppercase ASCII letters.
func validCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
