package domain_test

import (
	"errors"
	"math"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// mustMoney constructs a Money for tests, failing the test if the value is not
// valid so an unexpected constructor error never masquerades as the behavior
// under test.
func mustMoney(t *testing.T, cents int64, currency string) household.Money {
	t.Helper()
	m, err := household.NewMoney(cents, currency)
	if err != nil {
		t.Fatalf("NewMoney(%d, %q) error = %v", cents, currency, err)
	}
	return m
}

func TestNewMoneyValid(t *testing.T) {
	m, err := household.NewMoney(1299, "USD")
	if err != nil {
		t.Fatalf("NewMoney() error = %v", err)
	}
	if m.Cents != 1299 || m.Currency != "USD" {
		t.Fatalf("NewMoney() = %+v, want {1299 USD}", m)
	}
}

func TestNewMoneyZeroAllowed(t *testing.T) {
	if _, err := household.NewMoney(0, "USD"); err != nil {
		t.Fatalf("NewMoney(0, USD) error = %v, want nil (zero is a valid Money)", err)
	}
}

func TestNewMoneyInvalid(t *testing.T) {
	cases := []struct {
		name     string
		cents    int64
		currency string
	}{
		{"negative amount", -1, "USD"},
		{"empty currency", 100, ""},
		{"too short currency", 100, "US"},
		{"too long currency", 100, "USDD"},
		{"lowercase currency", 100, "usd"},
		{"mixed case currency", 100, "Usd"},
		{"non-letter currency", 100, "US1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := household.NewMoney(tc.cents, tc.currency); !errors.Is(err, household.ErrInvalidMoney) {
				t.Fatalf("NewMoney(%d, %q) error = %v, want ErrInvalidMoney", tc.cents, tc.currency, err)
			}
		})
	}
}

func TestMoneyAdd(t *testing.T) {
	a := mustMoney(t, 150, "USD")
	b := mustMoney(t, 250, "USD")
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if sum.Cents != 400 {
		t.Fatalf("Add() cents = %d, want 400", sum.Cents)
	}
}

func TestMoneyAddOverflow(t *testing.T) {
	a := mustMoney(t, math.MaxInt64, "USD")
	b := mustMoney(t, 1, "USD")
	if _, err := a.Add(b); !errors.Is(err, household.ErrInvalidMoney) {
		t.Fatalf("Add() overflow error = %v, want ErrInvalidMoney", err)
	}
}

func TestMoneyAddCurrencyMismatch(t *testing.T) {
	a := mustMoney(t, 150, "USD")
	b := mustMoney(t, 250, "EUR")
	if _, err := a.Add(b); !errors.Is(err, household.ErrCurrencyMismatch) {
		t.Fatalf("Add() error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestMoneyString(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{0, "0.00 USD"},
		{5, "0.05 USD"},
		{99, "0.99 USD"},
		{100, "1.00 USD"},
		{1299, "12.99 USD"},
	}
	for _, tc := range cases {
		m := mustMoney(t, tc.cents, "USD")
		if got := m.String(); got != tc.want {
			t.Errorf("Money{%d}.String() = %q, want %q", tc.cents, got, tc.want)
		}
	}
}
