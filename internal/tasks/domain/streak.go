package domain

import "time"

// CurrentStreak returns the number of consecutive calendar days ending at or
// immediately before today on which the member completed at least one task.
//
// Rules:
//   - All inputs are normalised to midnight UTC (DateOf) before comparison so
//     clock time and timezone never affect the day boundary.
//   - Duplicate completion timestamps on the same calendar day count as a single
//     day (the set of distinct completion days is what matters).
//   - The streak is anchored at today if today has at least one completion, or at
//     yesterday if today has none but yesterday does. The rationale: a streak is
//     not broken until a full calendar day has passed with no completion. A member
//     who finished tasks yesterday and has not yet acted today has not broken their
//     streak — they still have all of today to act.
//   - Two or more full days with no completion constitute a break: the streak is 0.
//   - An empty completionDays slice (or one where no day falls within the
//     anchor-back window) returns 0.
//
// Parameters:
//   - completionDays: the raw timestamps of task completions; may be unsorted and
//     may contain duplicates.  The caller typically fetches these from
//     TaskInstanceRepository.CompletionDays.
//   - today: the reference "now" date supplied by the caller so the function is
//     hermetic and never calls time.Now() itself.
func CurrentStreak(completionDays []time.Time, today time.Time) int {
	if len(completionDays) == 0 {
		return 0
	}

	// Normalise to the UTC calendar day. DateOf reads the calendar components in
	// the value's own location, so each input is first converted to UTC; this
	// guarantees day bucketing is by UTC day on non-UTC servers (NES-37 rule).
	todayNorm := DateOf(today.UTC())

	// Build a deduplication set: map each completion to its UTC calendar day.
	daySet := make(map[time.Time]bool, len(completionDays))
	for _, t := range completionDays {
		daySet[DateOf(t.UTC())] = true
	}

	// Determine the anchor: today if there was a completion today, yesterday
	// otherwise. If the anchor day itself has no completion, the streak is 0.
	anchor := todayNorm
	if !daySet[anchor] {
		anchor = todayNorm.AddDate(0, 0, -1)
		if !daySet[anchor] {
			// Neither today nor yesterday has a completion: streak is 0.
			return 0
		}
	}

	// Walk backward from the anchor, counting every consecutive day that has at
	// least one completion. Stop as soon as a day is missing.
	count := 0
	cursor := anchor
	for daySet[cursor] {
		count++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return count
}
