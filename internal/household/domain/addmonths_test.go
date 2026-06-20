package domain_test

import (
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestAddMonthsClamped(t *testing.T) {
	cases := []struct {
		name   string
		base   time.Time
		months int
		want   time.Time
	}{
		{"plain add", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), 1, time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)},
		{"clamps to short month", time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), 1, time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)},
		{"twelve months clamps leap day", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), 12, time.Date(2025, 2, 28, 0, 0, 0, 0, time.UTC)},
		{"crosses year boundary", time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC), 1, time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)},
		{"preserves clock time", time.Date(2026, 1, 15, 9, 30, 15, 0, time.UTC), 1, time.Date(2026, 2, 15, 9, 30, 15, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := household.AddMonthsClamped(tc.base, tc.months); !got.Equal(tc.want) {
				t.Fatalf("AddMonthsClamped(%s, %d) = %s, want %s", tc.base, tc.months, got, tc.want)
			}
		})
	}
}
