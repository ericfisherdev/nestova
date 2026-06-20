package app_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/subscriptions/app"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestNextRenewal(t *testing.T) {
	cases := []struct {
		name  string
		cycle domain.Cycle
		from  time.Time
		want  time.Time
	}{
		{"weekly adds 7 days", domain.CycleWeekly, day(2026, 7, 1), day(2026, 7, 8)},
		{"monthly adds a month", domain.CycleMonthly, day(2026, 1, 15), day(2026, 2, 15)},
		{"monthly clamps short month", domain.CycleMonthly, day(2026, 1, 31), day(2026, 2, 28)},
		{"yearly adds a year", domain.CycleYearly, day(2026, 3, 1), day(2027, 3, 1)},
		{"yearly clamps leap day", domain.CycleYearly, day(2024, 2, 29), day(2025, 2, 28)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := app.NextRenewal(tc.cycle, tc.from)
			if err != nil {
				t.Fatalf("NextRenewal() error = %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("NextRenewal(%s, %s) = %s, want %s", tc.cycle, tc.from.Format(time.DateOnly), got.Format(time.DateOnly), tc.want.Format(time.DateOnly))
			}
		})
	}
}

func TestNextRenewalUnsupported(t *testing.T) {
	for _, cycle := range []domain.Cycle{domain.CycleCustom, domain.Cycle("daily")} {
		if _, err := app.NextRenewal(cycle, day(2026, 7, 1)); !errors.Is(err, domain.ErrUnsupportedCycle) {
			t.Fatalf("NextRenewal(%s) error = %v, want ErrUnsupportedCycle", cycle, err)
		}
	}
}

func TestAdvancePastDue(t *testing.T) {
	cases := []struct {
		name  string
		cycle domain.Cycle
		next  time.Time
		asOf  time.Time
		want  time.Time
	}{
		{"monthly rolls multiple missed cycles", domain.CycleMonthly, day(2026, 1, 15), day(2026, 4, 10), day(2026, 4, 15)},
		{"weekly rolls multiple missed cycles", domain.CycleWeekly, day(2026, 6, 1), day(2026, 6, 20), day(2026, 6, 22)},
		{"already future is unchanged", domain.CycleMonthly, day(2026, 8, 1), day(2026, 6, 20), day(2026, 8, 1)},
		{"exactly asOf is unchanged", domain.CycleMonthly, day(2026, 6, 20), day(2026, 6, 20), day(2026, 6, 20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := app.AdvancePastDue(tc.cycle, tc.next, tc.asOf)
			if err != nil {
				t.Fatalf("AdvancePastDue() error = %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("AdvancePastDue(%s, %s, %s) = %s, want %s", tc.cycle,
					tc.next.Format(time.DateOnly), tc.asOf.Format(time.DateOnly),
					got.Format(time.DateOnly), tc.want.Format(time.DateOnly))
			}
		})
	}
}

func TestAdvancePastDueCustomUnsupported(t *testing.T) {
	if _, err := app.AdvancePastDue(domain.CycleCustom, day(2026, 1, 1), day(2026, 6, 20)); !errors.Is(err, domain.ErrUnsupportedCycle) {
		t.Fatalf("AdvancePastDue(custom) error = %v, want ErrUnsupportedCycle", err)
	}
}

// A non-midnight asOf must not change the date the advance targets.
func TestAdvancePastDueIgnoresTimeOfDay(t *testing.T) {
	asOf := time.Date(2026, 4, 10, 23, 59, 59, 0, time.UTC)
	got, err := app.AdvancePastDue(domain.CycleMonthly, day(2026, 1, 15), asOf)
	if err != nil {
		t.Fatalf("AdvancePastDue() error = %v", err)
	}
	if !got.Equal(day(2026, 4, 15)) {
		t.Fatalf("AdvancePastDue() = %s, want 2026-04-15", got.Format(time.DateOnly))
	}
}
