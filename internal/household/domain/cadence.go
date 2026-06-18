package domain

import (
	"errors"
	"fmt"
	"time"
)

// Freq is the base unit of a recurrence cadence. Stored as text, validated here.
type Freq string

// Recurrence frequencies.
const (
	FreqDaily   Freq = "daily"
	FreqWeekly  Freq = "weekly"
	FreqMonthly Freq = "monthly"
)

// Valid reports whether f is a known frequency.
func (f Freq) Valid() bool {
	switch f {
	case FreqDaily, FreqWeekly, FreqMonthly:
		return true
	default:
		return false
	}
}

// String returns the frequency's stored value.
func (f Freq) String() string { return string(f) }

// ParseFreq validates and returns a Freq, or an error for an unknown value.
func ParseFreq(s string) (Freq, error) {
	f := Freq(s)
	if !f.Valid() {
		return "", fmt.Errorf("invalid frequency %q", s)
	}
	return f, nil
}

// ErrInvalidCadence is returned by Cadence.Validate for a malformed cadence.
var ErrInvalidCadence = errors.New("household: invalid cadence")

// Cadence describes a recurrence rule. It is the shared-kernel value object that
// drives chores, maintenance, and (later) subscription cycles. It is pure and
// deterministic: callers pass the reference time; no method reads the wall clock.
//
// Occurrences carry the Anchor's clock time (hour/minute/second) and location.
// For FreqWeekly with a non-empty ByWeekday, occurrences fall on each listed
// weekday within every Interval-th week (weeks counted from the Anchor's week,
// which starts on Sunday); with an empty ByWeekday they fall every Interval
// weeks on the Anchor's own weekday. FreqMonthly clamps a day past the end of a
// short month to that month's last day (e.g. the 31st becomes Feb 28/29).
type Cadence struct {
	// Freq is the base recurrence unit.
	Freq Freq
	// Interval is the number of Freq units between occurrences (every N); >= 1.
	Interval int
	// ByWeekday optionally restricts FreqWeekly occurrences to these weekdays.
	ByWeekday []time.Weekday
	// Anchor is the reference start; no occurrence falls before it.
	Anchor time.Time
}

// Validate reports whether the cadence is well-formed, returning ErrInvalidCadence
// (wrapped with detail) otherwise. NextAfter and OccurrencesBetween assume a
// cadence that has passed Validate.
func (c Cadence) Validate() error {
	if !c.Freq.Valid() {
		return fmt.Errorf("%w: unknown frequency %q", ErrInvalidCadence, c.Freq)
	}
	if c.Interval < 1 {
		return fmt.Errorf("%w: interval must be >= 1, got %d", ErrInvalidCadence, c.Interval)
	}
	if c.Anchor.IsZero() {
		return fmt.Errorf("%w: anchor is required", ErrInvalidCadence)
	}
	if len(c.ByWeekday) > 0 && c.Freq != FreqWeekly {
		return fmt.Errorf("%w: ByWeekday is only valid for weekly frequency", ErrInvalidCadence)
	}
	for _, wd := range c.ByWeekday {
		if wd < time.Sunday || wd > time.Saturday {
			return fmt.Errorf("%w: invalid weekday %d", ErrInvalidCadence, wd)
		}
	}
	return nil
}

// NextAfter returns the first occurrence strictly after t. When t precedes the
// Anchor, the Anchor (the first occurrence) is returned.
func (c Cadence) NextAfter(t time.Time) time.Time {
	if c.Freq == FreqWeekly && len(c.ByWeekday) > 0 {
		return c.nextWeeklyByWeekday(t)
	}

	// Anchor itself is the first occurrence; if it is already after t, use it.
	first := c.stepFromAnchor(0)
	if first.After(t) {
		return first
	}

	// Estimate the occurrence index, then correct in a few bounded steps.
	n := c.estimateIndex(t)
	if n < 0 {
		n = 0
	}
	for n > 0 && c.stepFromAnchor(n-1).After(t) {
		n--
	}
	for !c.stepFromAnchor(n).After(t) {
		n++
	}
	return c.stepFromAnchor(n)
}

// OccurrencesBetween returns every occurrence in the half-open interval
// (start, end] in ascending order. The window must be bounded by the caller;
// the slice length is the number of occurrences it contains.
func (c Cadence) OccurrencesBetween(start, end time.Time) []time.Time {
	var out []time.Time
	for o := c.NextAfter(start); !o.After(end); o = c.NextAfter(o) {
		out = append(out, o)
	}
	return out
}

// stepFromAnchor returns the nth occurrence (n >= 0) for the simple
// (non-ByWeekday) frequencies.
func (c Cadence) stepFromAnchor(n int) time.Time {
	switch c.Freq {
	case FreqDaily:
		return c.Anchor.AddDate(0, 0, n*c.Interval)
	case FreqWeekly:
		return c.Anchor.AddDate(0, 0, n*c.Interval*7)
	case FreqMonthly:
		return addMonthsClamped(c.Anchor, n*c.Interval)
	default:
		return c.Anchor
	}
}

// estimateIndex returns an approximate occurrence index near t; the correction
// loops in NextAfter refine it to the exact value.
func (c Cadence) estimateIndex(t time.Time) int {
	switch c.Freq {
	case FreqDaily:
		return daysBetween(c.Anchor, t) / c.Interval
	case FreqWeekly:
		return daysBetween(c.Anchor, t) / (c.Interval * 7)
	case FreqMonthly:
		months := (t.Year()-c.Anchor.Year())*12 + int(t.Month()) - int(c.Anchor.Month())
		return months / c.Interval
	default:
		return 0
	}
}

// nextWeeklyByWeekday returns the first weekly-by-weekday occurrence after t.
func (c Cadence) nextWeeklyByWeekday(t time.Time) time.Time {
	allowed := make(map[time.Weekday]bool, len(c.ByWeekday))
	for _, wd := range c.ByWeekday {
		allowed[wd] = true
	}
	anchorWeekStart := weekStart(c.Anchor)

	// Begin scanning at the calendar day of whichever is later, the Anchor or t.
	day := c.Anchor
	if t.After(c.Anchor) {
		day = t
	}
	day = atClock(day, c.Anchor)

	// Bound the scan: at most Interval weeks plus a full week of candidate days.
	maxDays := c.Interval*7 + 7
	for i := 0; i <= maxDays; i++ {
		cand := day.AddDate(0, 0, i)
		if !cand.After(t) || cand.Before(c.Anchor) {
			continue
		}
		if !allowed[cand.Weekday()] {
			continue
		}
		if daysBetween(anchorWeekStart, weekStart(cand))/7%c.Interval == 0 {
			return cand
		}
	}
	// Unreachable for a validated cadence; return Anchor as a safe fallback.
	return c.Anchor
}

// addMonthsClamped adds m calendar months to base, clamping the day to the last
// day of the target month so e.g. Jan 31 + 1 month yields Feb 28/29, not Mar 3.
func addMonthsClamped(base time.Time, m int) time.Time {
	y, mo, d := base.Date()
	// Normalize the target year/month from the first of the month to avoid the
	// day-overflow that time.Date performs.
	target := time.Date(y, mo+time.Month(m), 1, 0, 0, 0, 0, base.Location())
	if last := daysInMonth(target.Year(), target.Month()); d > last {
		d = last
	}
	return time.Date(target.Year(), target.Month(), d,
		base.Hour(), base.Minute(), base.Second(), base.Nanosecond(), base.Location())
}

// daysInMonth returns the number of days in the given month.
func daysInMonth(year int, month time.Month) int {
	// The zeroth day of the next month is the last day of this month.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// weekStart returns midnight on the Sunday that begins t's week, in t's location.
func weekStart(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return d.AddDate(0, 0, -int(d.Weekday()))
}

// atClock returns day's calendar date carrying ref's clock time and location.
func atClock(day, ref time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(),
		ref.Hour(), ref.Minute(), ref.Second(), ref.Nanosecond(), ref.Location())
}

// daysBetween returns the number of whole calendar days from a to b (b - a),
// computed on calendar dates so daylight-saving transitions do not skew it.
func daysBetween(a, b time.Time) int {
	ad := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	bd := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	// Both are UTC midnights, so the difference is an exact multiple of 24h;
	// integer Duration division avoids float rounding.
	return int(bd.Sub(ad) / (24 * time.Hour))
}
