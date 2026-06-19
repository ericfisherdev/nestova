package domain_test

import (
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// today is a fixed reference date used across all streak table tests so no test
// depends on the wall clock.
var streakToday = time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)

// day returns midnight UTC for an offset from streakToday.
func day(offset int) time.Time {
	return streakToday.AddDate(0, 0, offset)
}

func TestCurrentStreak(t *testing.T) {
	cases := []struct {
		name           string
		completionDays []time.Time
		want           int
	}{
		{
			name:           "empty slice returns 0",
			completionDays: nil,
			want:           0,
		},
		{
			name:           "today only returns 1",
			completionDays: []time.Time{day(0)},
			want:           1,
		},
		{
			name:           "today and yesterday returns 2",
			completionDays: []time.Time{day(0), day(-1)},
			want:           2,
		},
		{
			name:           "three consecutive days ending today returns 3",
			completionDays: []time.Time{day(0), day(-1), day(-2)},
			want:           3,
		},
		{
			name:           "gap one day back breaks streak to 1",
			completionDays: []time.Time{day(0), day(-2)},
			want:           1,
		},
		{
			name:           "yesterday but not today still counts as 1",
			completionDays: []time.Time{day(-1)},
			want:           1,
		},
		{
			name:           "yesterday and two days ago returns 2",
			completionDays: []time.Time{day(-1), day(-2)},
			want:           2,
		},
		{
			name:           "two days ago but not yesterday returns 0",
			completionDays: []time.Time{day(-2)},
			want:           0,
		},
		{
			name:           "gap between yesterday and two days ago still yields 1 (anchor at yesterday, gap before)",
			completionDays: []time.Time{day(-1), day(-3)},
			want:           1,
		},
		{
			name: "duplicate entries for same day deduplicated",
			completionDays: []time.Time{
				day(0), day(0),
				day(-1), day(-1), day(-1),
			},
			want: 2,
		},
		{
			name: "unsorted input still produces correct streak",
			completionDays: []time.Time{
				day(-2), day(0), day(-1),
			},
			want: 3,
		},
		{
			name: "non-midnight timestamps normalised by DateOf",
			completionDays: []time.Time{
				streakToday.Add(14*time.Hour + 30*time.Minute),   // today at 14:30
				streakToday.AddDate(0, 0, -1).Add(9 * time.Hour), // yesterday at 09:00
			},
			want: 2,
		},
		{
			name:           "future completions beyond today do not extend streak backward",
			completionDays: []time.Time{day(1), day(0), day(-1)},
			want:           2, // future day irrelevant; anchor at today, today + yesterday = 2
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.CurrentStreak(tc.completionDays, streakToday)
			if got != tc.want {
				t.Errorf("CurrentStreak(...) = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCurrentStreak_BucketsByUTCDayNotLocal locks the NES-37 rule that streaks
// are counted by UTC calendar day, not by the location of the input timestamp.
// A completion at 01:00 in a +03:00 zone is 22:00 the *previous* UTC day, and a
// completion at 23:00 in a -05:00 zone is 04:00 the *next* UTC day; both must be
// bucketed by their UTC day so the streak boundary is timezone-independent.
func TestCurrentStreak_BucketsByUTCDayNotLocal(t *testing.T) {
	plus3 := time.FixedZone("UTC+3", 3*60*60)
	minus5 := time.FixedZone("UTC-5", -5*60*60)

	// Reference "today" is 2025-06-15 UTC.
	today := time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)

	t.Run("local-day-next is really previous UTC day", func(t *testing.T) {
		// 2025-06-15 01:00 +03:00 == 2025-06-14 22:00 UTC -> UTC day = June 14.
		// 2025-06-14 01:00 +03:00 == 2025-06-13 22:00 UTC -> UTC day = June 13.
		// By UTC day these are June 14 and June 13 (yesterday + day-before),
		// anchored at yesterday: streak = 2. If the function (wrongly) bucketed by
		// local day it would see June 15 and June 14 (today + yesterday) = also 2,
		// so to make the test discriminating we add a real today-UTC completion
		// and assert the chain is today, yesterday, day-before = 3 only when UTC
		// bucketing places the +03:00 values on June 14 and June 13.
		completions := []time.Time{
			time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC), // today UTC (June 15)
			time.Date(2025, 6, 15, 1, 0, 0, 0, plus3),     // June 14 UTC
			time.Date(2025, 6, 14, 1, 0, 0, 0, plus3),     // June 13 UTC
		}
		got := domain.CurrentStreak(completions, today)
		if got != 3 {
			t.Errorf("CurrentStreak(+03:00 inputs) = %d, want 3 (UTC days June 15,14,13)", got)
		}
	})

	t.Run("local-day-prev is really next UTC day", func(t *testing.T) {
		// 2025-06-14 23:00 -05:00 == 2025-06-15 04:00 UTC -> UTC day = June 15 (today).
		// 2025-06-13 23:00 -05:00 == 2025-06-14 04:00 UTC -> UTC day = June 14 (yesterday).
		// By UTC day: today + yesterday = 2. If bucketed by local day they would be
		// June 14 and June 13 (yesterday + day-before, anchor yesterday) = also 2,
		// so add a hard UTC day-before so the discriminating answer is 3 under UTC
		// bucketing (June 15, 14, 13) and only 2 under local bucketing.
		completions := []time.Time{
			time.Date(2025, 6, 14, 23, 0, 0, 0, minus5),   // June 15 UTC (today)
			time.Date(2025, 6, 13, 23, 0, 0, 0, minus5),   // June 14 UTC (yesterday)
			time.Date(2025, 6, 13, 12, 0, 0, 0, time.UTC), // June 13 UTC (day-before)
		}
		got := domain.CurrentStreak(completions, today)
		if got != 3 {
			t.Errorf("CurrentStreak(-05:00 inputs) = %d, want 3 (UTC days June 15,14,13)", got)
		}
	})

	t.Run("today supplied in non-UTC zone still anchors on UTC day", func(t *testing.T) {
		// today passed as 2025-06-15 02:00 +03:00 == 2025-06-14 23:00 UTC.
		// The anchor must be the UTC day June 14, so a completion on June 14 UTC
		// plus June 13 UTC yields a streak of 2.
		todayPlus3 := time.Date(2025, 6, 15, 2, 0, 0, 0, plus3) // June 14 UTC
		completions := []time.Time{
			time.Date(2025, 6, 14, 10, 0, 0, 0, time.UTC), // June 14 UTC
			time.Date(2025, 6, 13, 10, 0, 0, 0, time.UTC), // June 13 UTC
		}
		got := domain.CurrentStreak(completions, todayPlus3)
		if got != 2 {
			t.Errorf("CurrentStreak(today +03:00) = %d, want 2 (anchor UTC June 14)", got)
		}
	})
}
