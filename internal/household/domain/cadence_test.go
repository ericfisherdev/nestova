package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/household/domain"
)

// dt is a terse UTC time constructor for the tests (2026-01-01 is a Thursday).
func dt(y, m, d, h int) time.Time {
	return time.Date(y, time.Month(m), d, h, 0, 0, 0, time.UTC)
}

func TestFreqParseValidString(t *testing.T) {
	t.Parallel()
	for _, f := range []domain.Freq{domain.FreqDaily, domain.FreqWeekly, domain.FreqMonthly, domain.FreqAsNeeded} {
		got, err := domain.ParseFreq(f.String())
		if err != nil || got != f {
			t.Errorf("ParseFreq(%q) = (%v, %v), want (%v, nil)", f, got, err, f)
		}
		if !f.Valid() {
			t.Errorf("%q should be valid", f)
		}
	}
	if _, err := domain.ParseFreq("yearly"); err == nil {
		t.Error("ParseFreq(yearly) = nil error, want error")
	}
	if domain.Freq("yearly").Valid() {
		t.Error("yearly should be invalid")
	}
}

// TestFreqAsNeededCadenceNeverProducesOccurrences is the NES-116 regression
// test for the recurrence-engine guard: a FreqAsNeeded cadence must never hang
// or produce occurrences via NextAfter/OccurrencesBetween, since as-needed
// tasks are never scheduled by the recurrence engine (they have a single
// standing instance instead).
func TestFreqAsNeededCadenceNeverProducesOccurrences(t *testing.T) {
	t.Parallel()
	c := domain.Cadence{Freq: domain.FreqAsNeeded, Interval: 1, Anchor: dt(2026, 1, 1, 10)}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil (as-needed is a valid cadence)", err)
	}

	if got := c.NextAfter(dt(2026, 1, 1, 9)); !got.IsZero() {
		t.Errorf("NextAfter() = %v, want the zero time", got)
	}
	// Also probe with t at/after the anchor, where the unguarded correction
	// loop would otherwise spin forever on a degenerate (non-advancing) step.
	if got := c.NextAfter(dt(2026, 6, 1, 0)); !got.IsZero() {
		t.Errorf("NextAfter() = %v, want the zero time", got)
	}

	if occs := c.OccurrencesBetween(dt(2026, 1, 1, 0), dt(2027, 1, 1, 0)); occs != nil {
		t.Errorf("OccurrencesBetween() = %v, want nil", occs)
	}
}

func TestCadenceValidate(t *testing.T) {
	t.Parallel()
	anchor := dt(2026, 1, 1, 10)
	cases := []struct {
		name string
		c    domain.Cadence
		ok   bool
	}{
		{"valid daily", domain.Cadence{Freq: domain.FreqDaily, Interval: 1, Anchor: anchor}, true},
		{"valid weekly+weekday", domain.Cadence{Freq: domain.FreqWeekly, Interval: 2, ByWeekday: []time.Weekday{time.Monday}, Anchor: anchor}, true},
		{"valid monthly", domain.Cadence{Freq: domain.FreqMonthly, Interval: 1, Anchor: anchor}, true},
		{"zero interval", domain.Cadence{Freq: domain.FreqDaily, Interval: 0, Anchor: anchor}, false},
		{"negative interval", domain.Cadence{Freq: domain.FreqDaily, Interval: -1, Anchor: anchor}, false},
		{"unknown freq", domain.Cadence{Freq: domain.Freq("hourly"), Interval: 1, Anchor: anchor}, false},
		{"weekday on daily", domain.Cadence{Freq: domain.FreqDaily, Interval: 1, ByWeekday: []time.Weekday{time.Monday}, Anchor: anchor}, false},
		{"zero anchor", domain.Cadence{Freq: domain.FreqDaily, Interval: 1}, false},
		{"bad weekday", domain.Cadence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Weekday(9)}, Anchor: anchor}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tc.ok {
				if err == nil {
					t.Error("Validate() = nil, want error")
				} else if !errors.Is(err, domain.ErrInvalidCadence) {
					t.Errorf("Validate() = %v, want ErrInvalidCadence", err)
				}
			}
		})
	}
}

func TestNextAfter(t *testing.T) {
	t.Parallel()
	anchor := dt(2026, 1, 1, 10) // Thursday
	cases := []struct {
		name string
		c    domain.Cadence
		from time.Time
		want time.Time
	}{
		{"daily before anchor returns anchor", domain.Cadence{Freq: domain.FreqDaily, Interval: 1, Anchor: anchor}, dt(2026, 1, 1, 9), anchor},
		{"daily next day", domain.Cadence{Freq: domain.FreqDaily, Interval: 1, Anchor: anchor}, anchor, dt(2026, 1, 2, 10)},
		{"daily interval 3", domain.Cadence{Freq: domain.FreqDaily, Interval: 3, Anchor: anchor}, dt(2026, 1, 2, 10), dt(2026, 1, 4, 10)},
		{"daily far estimate", domain.Cadence{Freq: domain.FreqDaily, Interval: 2, Anchor: anchor}, dt(2026, 2, 1, 10), dt(2026, 2, 2, 10)},
		{"weekly no weekday", domain.Cadence{Freq: domain.FreqWeekly, Interval: 1, Anchor: anchor}, anchor, dt(2026, 1, 8, 10)},
		{"weekly interval 2", domain.Cadence{Freq: domain.FreqWeekly, Interval: 2, Anchor: anchor}, anchor, dt(2026, 1, 15, 10)},
		// anchor Thu Jan1; allowed Mon/Wed/Fri, interval 1 -> first after is Fri Jan2.
		{"weekly+weekday same week", domain.Cadence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Monday, time.Wednesday, time.Friday}, Anchor: anchor}, anchor, dt(2026, 1, 2, 10)},
		// next after Fri Jan2 -> Mon Jan5.
		{"weekly+weekday wraps to monday", domain.Cadence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Monday, time.Wednesday, time.Friday}, Anchor: anchor}, dt(2026, 1, 2, 10), dt(2026, 1, 5, 10)},
		// interval 2, Mondays only: Jan5 is out of phase, Jan12 in phase.
		{"weekly+weekday interval 2 phase", domain.Cadence{Freq: domain.FreqWeekly, Interval: 2, ByWeekday: []time.Weekday{time.Monday}, Anchor: anchor}, anchor, dt(2026, 1, 12, 10)},
		{"monthly normal", domain.Cadence{Freq: domain.FreqMonthly, Interval: 1, Anchor: dt(2026, 1, 15, 9)}, dt(2026, 1, 15, 9), dt(2026, 2, 15, 9)},
		// anchor Jan31 -> Feb clamps to 28 (2026 not a leap year).
		{"monthly clamps short month", domain.Cadence{Freq: domain.FreqMonthly, Interval: 1, Anchor: dt(2026, 1, 31, 9)}, dt(2026, 1, 31, 9), dt(2026, 2, 28, 9)},
		// next after the clamped Feb28 is Mar31 (computed from the anchor, not Feb28).
		{"monthly clamp does not drift", domain.Cadence{Freq: domain.FreqMonthly, Interval: 1, Anchor: dt(2026, 1, 31, 9)}, dt(2026, 2, 28, 9), dt(2026, 3, 31, 9)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.Validate(); err != nil {
				t.Fatalf("cadence invalid: %v", err)
			}
			got := tc.c.NextAfter(tc.from)
			if !got.Equal(tc.want) {
				t.Errorf("NextAfter(%s) = %s, want %s", tc.from.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if !got.After(tc.from) {
				t.Errorf("NextAfter(%s) = %s is not strictly after the input", tc.from.Format(time.RFC3339), got.Format(time.RFC3339))
			}
		})
	}
}

func TestOccurrencesBetween(t *testing.T) {
	t.Parallel()
	c := domain.Cadence{Freq: domain.FreqDaily, Interval: 1, Anchor: dt(2026, 1, 1, 10)}

	// (start, end] excludes start, includes end; anchor is included when after start.
	got := c.OccurrencesBetween(dt(2025, 12, 1, 0), dt(2026, 1, 3, 10))
	want := []time.Time{dt(2026, 1, 1, 10), dt(2026, 1, 2, 10), dt(2026, 1, 3, 10)}
	if len(got) != len(want) {
		t.Fatalf("OccurrencesBetween len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("occurrence[%d] = %s, want %s", i, got[i], want[i])
		}
	}

	// Empty window (end before first occurrence) yields nothing.
	if n := len(c.OccurrencesBetween(dt(2026, 1, 1, 10), dt(2026, 1, 1, 10))); n != 0 {
		t.Errorf("empty window returned %d occurrences, want 0", n)
	}

	// Weekly+weekday across two in-phase weeks (interval 1, Mon & Fri).
	wc := domain.Cadence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Monday, time.Friday}, Anchor: dt(2026, 1, 1, 8)}
	wgot := wc.OccurrencesBetween(dt(2026, 1, 1, 8), dt(2026, 1, 12, 23))
	// Jan2 Fri, Jan5 Mon, Jan9 Fri, Jan12 Mon.
	wwant := []time.Time{dt(2026, 1, 2, 8), dt(2026, 1, 5, 8), dt(2026, 1, 9, 8), dt(2026, 1, 12, 8)}
	if len(wgot) != len(wwant) {
		t.Fatalf("weekly OccurrencesBetween len = %d, want %d (%v)", len(wgot), len(wwant), wgot)
	}
	for i := range wwant {
		if !wgot[i].Equal(wwant[i]) {
			t.Errorf("weekly occurrence[%d] = %s, want %s", i, wgot[i], wwant[i])
		}
	}
}
