package domain_test

import (
	"errors"
	"math"
	"testing"

	"github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestParseUnit(t *testing.T) {
	for _, u := range domain.Units() {
		got, err := domain.ParseUnit(u.String())
		if err != nil || got != u {
			t.Errorf("ParseUnit(%q) = (%q, %v), want (%q, nil)", u, got, err, u)
		}
	}
	if _, err := domain.ParseUnit("furlong"); err == nil {
		t.Error("ParseUnit(furlong) = nil error, want error for unknown unit")
	}
}

func TestNewQuantityValidation(t *testing.T) {
	tests := []struct {
		name    string
		amount  float64
		unit    domain.Unit
		wantErr error
	}{
		{"valid", 2.5, domain.UnitLiter, nil},
		{"zero is valid", 0, domain.UnitCount, nil},
		{"unknown unit", 1, domain.Unit("furlong"), domain.ErrInvalidQuantity},
		{"negative amount", -1, domain.UnitGram, domain.ErrInvalidQuantity},
		{"NaN amount", math.NaN(), domain.UnitGram, domain.ErrInvalidQuantity},
		{"inf amount", math.Inf(1), domain.UnitGram, domain.ErrInvalidQuantity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := domain.NewQuantity(tt.amount, tt.unit)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("NewQuantity(%v, %q) error = %v, want %v", tt.amount, tt.unit, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewQuantity(%v, %q) unexpected error %v", tt.amount, tt.unit, err)
			}
			if q.Amount != tt.amount || q.Unit != tt.unit {
				t.Errorf("NewQuantity = %+v, want amount %v unit %q", q, tt.amount, tt.unit)
			}
		})
	}
}

func TestQuantityAdd(t *testing.T) {
	a := mustQty(t, 2, domain.UnitLiter)
	b := mustQty(t, 1.5, domain.UnitLiter)

	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.Amount != 3.5 || sum.Unit != domain.UnitLiter {
		t.Errorf("Add = %+v, want {3.5 l}", sum)
	}

	if _, err := a.Add(mustQty(t, 1, domain.UnitGram)); !errors.Is(err, domain.ErrUnitMismatch) {
		t.Errorf("Add mismatched units error = %v, want ErrUnitMismatch", err)
	}
}

func TestQuantitySubtract(t *testing.T) {
	a := mustQty(t, 3, domain.UnitKilogram)

	diff, err := a.Subtract(mustQty(t, 1.25, domain.UnitKilogram))
	if err != nil {
		t.Fatalf("Subtract: %v", err)
	}
	if diff.Amount != 1.75 || diff.Unit != domain.UnitKilogram {
		t.Errorf("Subtract = %+v, want {1.75 kg}", diff)
	}

	if _, err := a.Subtract(mustQty(t, 5, domain.UnitKilogram)); !errors.Is(err, domain.ErrInvalidQuantity) {
		t.Errorf("Subtract below zero error = %v, want ErrInvalidQuantity", err)
	}
	if _, err := a.Subtract(mustQty(t, 1, domain.UnitGram)); !errors.Is(err, domain.ErrUnitMismatch) {
		t.Errorf("Subtract mismatched units error = %v, want ErrUnitMismatch", err)
	}
}

func mustQty(t *testing.T, amount float64, unit domain.Unit) domain.Quantity {
	t.Helper()
	q, err := domain.NewQuantity(amount, unit)
	if err != nil {
		t.Fatalf("NewQuantity(%v, %q): %v", amount, unit, err)
	}
	return q
}
