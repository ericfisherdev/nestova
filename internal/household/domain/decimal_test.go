package domain_test

import (
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestParseMoneyCents(t *testing.T) {
	valid := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"9", 900},
		{"9.5", 950},
		{"9.99", 999},
		{"0007", 700},
		{"999999999999.99", 99999999999999},
	}
	for _, tc := range valid {
		got, err := household.ParseMoneyCents(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseMoneyCents(%q) = %d, %v; want %d, nil", tc.in, got, err, tc.want)
		}
	}
	invalid := []string{"", " ", "1e10", "1E2", "9.999", "+5", "-5", "-0.01", "-", "-.", "1,000", ".5", "5.", "0x10", "NaN", "Inf", "١٢", "1000000000000", "99999999999999999999"}
	for _, in := range invalid {
		if _, err := household.ParseMoneyCents(in); !errors.Is(err, household.ErrInvalidMoney) {
			t.Errorf("ParseMoneyCents(%q) error = %v, want ErrInvalidMoney", in, err)
		}
	}
}

func TestParseQuantityAmount(t *testing.T) {
	valid := map[string]float64{"0": 0, "-0": 0, "2": 2, "1.5": 1.5, "0007": 7, "0.000001": 0.000001}
	for in, want := range valid {
		got, err := household.ParseQuantityAmount(in)
		if err != nil || got != want {
			t.Errorf("ParseQuantityAmount(%q) = %v, %v; want %v, nil", in, got, err, want)
		}
	}
	invalid := []string{"", "1e10", "1e300", "+5", "-1", "1,000", ".5", "5.", "NaN", "Inf", "0.0000001"}
	for _, in := range invalid {
		if _, err := household.ParseQuantityAmount(in); !errors.Is(err, household.ErrInvalidQuantity) {
			t.Errorf("ParseQuantityAmount(%q) error = %v, want ErrInvalidQuantity", in, err)
		}
	}
}

func TestQuantityValidateRejectsOversizedAmount(t *testing.T) {
	if _, err := household.NewQuantity(household.MaxQuantityAmount, household.UnitCount); err != nil {
		t.Errorf("NewQuantity(max) error = %v, want nil", err)
	}
	if _, err := household.NewQuantity(household.MaxQuantityAmount+1, household.UnitCount); !errors.Is(err, household.ErrInvalidQuantity) {
		t.Errorf("NewQuantity(max+1) error = %v, want ErrInvalidQuantity", err)
	}
}
