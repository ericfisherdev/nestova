package domain_test

import (
	"errors"
	"math"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	subscriptions "github.com/ericfisherdev/nestova/internal/subscriptions/domain"
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

func TestCycleValidAndParse(t *testing.T) {
	for _, c := range subscriptions.Cycles() {
		if !c.Valid() {
			t.Errorf("Cycles() returned invalid cycle %q", c)
		}
		parsed, err := subscriptions.ParseCycle(c.String())
		if err != nil {
			t.Errorf("ParseCycle(%q) error = %v", c, err)
		}
		if parsed != c {
			t.Errorf("ParseCycle(%q) = %q, want %q", c, parsed, c)
		}
	}
}

func TestParseCycleInvalid(t *testing.T) {
	if _, err := subscriptions.ParseCycle("biweekly"); err == nil {
		t.Fatal("ParseCycle(\"biweekly\") error = nil, want non-nil")
	}
}

func TestNormalizeMonthly(t *testing.T) {
	cases := []struct {
		name      string
		cents     int64
		cycle     subscriptions.Cycle
		wantCents int64
	}{
		{"monthly unchanged", 1000, subscriptions.CycleMonthly, 1000},
		{"yearly divided by 12", 12000, subscriptions.CycleYearly, 1000},
		{"yearly rounds", 10000, subscriptions.CycleYearly, 833},       // 10000/12 = 833.33 -> 833
		{"weekly scaled 52/12", 1000, subscriptions.CycleWeekly, 4333}, // 1000*52/12 = 4333.33 -> 4333
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount := mustMoney(t, tc.cents, "USD")
			got, err := subscriptions.NormalizeMonthly(amount, tc.cycle)
			if err != nil {
				t.Fatalf("NormalizeMonthly() error = %v", err)
			}
			if got.Cents != tc.wantCents {
				t.Errorf("NormalizeMonthly(%d, %s) cents = %d, want %d", tc.cents, tc.cycle, got.Cents, tc.wantCents)
			}
			if got.Currency != "USD" {
				t.Errorf("NormalizeMonthly() currency = %q, want USD", got.Currency)
			}
		})
	}
}

func TestNormalizeMonthlyCustomUnsupported(t *testing.T) {
	amount := mustMoney(t, 1000, "USD")
	if _, err := subscriptions.NormalizeMonthly(amount, subscriptions.CycleCustom); !errors.Is(err, subscriptions.ErrUnsupportedCycle) {
		t.Fatalf("NormalizeMonthly(custom) error = %v, want ErrUnsupportedCycle", err)
	}
}

func TestNormalizeMonthlyUnknownUnsupported(t *testing.T) {
	amount := mustMoney(t, 1000, "USD")
	if _, err := subscriptions.NormalizeMonthly(amount, subscriptions.Cycle("daily")); !errors.Is(err, subscriptions.ErrUnsupportedCycle) {
		t.Fatalf("NormalizeMonthly(unknown) error = %v, want ErrUnsupportedCycle", err)
	}
}

func TestNormalizeMonthlyWeeklyOverflow(t *testing.T) {
	amount := mustMoney(t, math.MaxInt64, "USD")
	if _, err := subscriptions.NormalizeMonthly(amount, subscriptions.CycleWeekly); !errors.Is(err, household.ErrInvalidMoney) {
		t.Fatalf("NormalizeMonthly(weekly overflow) error = %v, want ErrInvalidMoney", err)
	}
}

func TestNormalizeMonthlyInvalidAmount(t *testing.T) {
	// A zero-value Money has an empty currency and fails validation.
	if _, err := subscriptions.NormalizeMonthly(household.Money{}, subscriptions.CycleMonthly); !errors.Is(err, household.ErrInvalidMoney) {
		t.Fatalf("NormalizeMonthly(invalid amount) error = %v, want ErrInvalidMoney", err)
	}
}
