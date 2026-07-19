package domain_test

import (
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/household/domain"
)

func hours(h int) time.Duration { return time.Duration(h) * time.Hour }

func at(hour, minute int) time.Time {
	return time.Date(2026, time.July, 19, hour, minute, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// InQuietHours
// ---------------------------------------------------------------------------

func TestHousehold_InQuietHours_Disabled(t *testing.T) {
	tests := []struct {
		name string
		h    domain.Household
	}{
		{"both bounds nil", domain.Household{}},
		{"only start set", domain.Household{QuietHoursStart: durPtr(hours(22))}},
		{"only end set", domain.Household{QuietHoursEnd: durPtr(hours(7))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.h.InQuietHours(at(23, 0)) {
				t.Error("InQuietHours() = true, want false when quiet hours are disabled")
			}
		})
	}
}

func TestHousehold_InQuietHours_SameDayWindow(t *testing.T) {
	// 13:00-15:00, a same-day (non-wrapping) window.
	h := domain.Household{QuietHoursStart: durPtr(hours(13)), QuietHoursEnd: durPtr(hours(15))}

	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"before the window", at(12, 0), false},
		{"exactly at start (inclusive)", at(13, 0), true},
		{"inside the window", at(14, 0), true},
		{"exactly at end (exclusive)", at(15, 0), false},
		{"after the window", at(16, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.InQuietHours(tt.t); got != tt.want {
				t.Errorf("InQuietHours(%s) = %v, want %v", tt.t.Format("15:04"), got, tt.want)
			}
		})
	}
}

func TestHousehold_InQuietHours_CrossesMidnight(t *testing.T) {
	// 22:00-07:00, a window that wraps past midnight.
	h := domain.Household{QuietHoursStart: durPtr(hours(22)), QuietHoursEnd: durPtr(hours(7))}

	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"exactly at start (inclusive)", at(22, 0), true},
		{"late evening", at(23, 30), true},
		{"just before midnight", at(23, 59), true},
		{"early morning, inside the window", at(3, 0), true},
		{"exactly at end (exclusive)", at(7, 0), false},
		{"just after end", at(7, 1), false},
		{"midday, well outside the window", at(12, 0), false},
		{"just before start", at(21, 59), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.InQuietHours(tt.t); got != tt.want {
				t.Errorf("InQuietHours(%s) = %v, want %v", tt.t.Format("15:04"), got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// QuietHoursEndAfter
// ---------------------------------------------------------------------------

func TestHousehold_QuietHoursEndAfter_SameDayWindow(t *testing.T) {
	h := domain.Household{QuietHoursStart: durPtr(hours(13)), QuietHoursEnd: durPtr(hours(15))}

	got := h.QuietHoursEndAfter(at(14, 0))
	want := time.Date(2026, time.July, 19, 15, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("QuietHoursEndAfter(14:00) = %v, want %v", got, want)
	}
}

func TestHousehold_QuietHoursEndAfter_CrossesMidnight_EveningPortion(t *testing.T) {
	h := domain.Household{QuietHoursStart: durPtr(hours(22)), QuietHoursEnd: durPtr(hours(7))}

	// 23:30 is in the window's EVENING portion (since >= start): the
	// window's end boundary falls on the FOLLOWING calendar date.
	got := h.QuietHoursEndAfter(at(23, 30))
	want := time.Date(2026, time.July, 20, 7, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("QuietHoursEndAfter(23:30) = %v, want %v (next-day 07:00)", got, want)
	}
}

func TestHousehold_QuietHoursEndAfter_CrossesMidnight_EarlyMorningPortion(t *testing.T) {
	h := domain.Household{QuietHoursStart: durPtr(hours(22)), QuietHoursEnd: durPtr(hours(7))}

	// 03:00 is in the window's EARLY-MORNING portion (since < end): the
	// window's end boundary falls on t's OWN calendar date — the window
	// already started the previous evening.
	got := h.QuietHoursEndAfter(at(3, 0))
	want := time.Date(2026, time.July, 19, 7, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("QuietHoursEndAfter(03:00) = %v, want %v (same-day 07:00)", got, want)
	}
}

func TestHousehold_QuietHoursEndAfter_Disabled_ReturnsInputUnchanged(t *testing.T) {
	h := domain.Household{}
	in := at(23, 0)
	if got := h.QuietHoursEndAfter(in); !got.Equal(in) {
		t.Errorf("QuietHoursEndAfter() with disabled quiet hours = %v, want the input unchanged (%v)", got, in)
	}
}

// ---------------------------------------------------------------------------
// DST regression tests (CodeRabbit round 2, MAJOR finding #1): atClockTime
// must preserve wall-clock time across a DST transition, not add d as a
// fixed elapsed duration to midnight. America/New_York's 2026 transitions
// (verified empirically, not assumed, against the real tzdata: spring
// forward 2026-03-08, fall back 2026-11-01) are used deliberately, since a
// fixed-offset zone like UTC can never exercise this bug at all.
// ---------------------------------------------------------------------------

func TestHousehold_QuietHoursEndAfter_SpringForward_PreservesWallClockTime(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2026-03-08 in America/New_York: clocks jump from 01:59:59 EST
	// straight to 03:00:00 EDT.
	h := domain.Household{QuietHoursStart: durPtr(hours(22)), QuietHoursEnd: durPtr(hours(7))}

	// 03:30 EDT is in the window's early-morning portion, already past the
	// 2am transition — an ordinary, unambiguous wall-clock time.
	got := h.QuietHoursEndAfter(time.Date(2026, time.March, 8, 3, 30, 0, 0, loc))
	want := time.Date(2026, time.March, 8, 7, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("QuietHoursEndAfter on spring-forward day = %v, want %v (07:00 EDT — the pre-fix elapsed-duration arithmetic produced 08:00)", got, want)
	}
	if got.Hour() != 7 {
		t.Errorf("QuietHoursEndAfter wall-clock hour = %d, want 7", got.Hour())
	}
}

func TestHousehold_QuietHoursEndAfter_FallBack_PreservesWallClockTime(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2026-11-01 in America/New_York: clocks fall from 01:59:59 EDT back
	// to 01:00:00 EST.
	h := domain.Household{QuietHoursStart: durPtr(hours(22)), QuietHoursEnd: durPtr(hours(7))}

	got := h.QuietHoursEndAfter(time.Date(2026, time.November, 1, 3, 30, 0, 0, loc))
	want := time.Date(2026, time.November, 1, 7, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("QuietHoursEndAfter on fall-back day = %v, want %v (07:00 EST — the pre-fix elapsed-duration arithmetic produced 06:00)", got, want)
	}
	if got.Hour() != 7 {
		t.Errorf("QuietHoursEndAfter wall-clock hour = %d, want 7", got.Hour())
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }
