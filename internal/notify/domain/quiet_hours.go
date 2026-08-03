package domain

import (
	"context"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// QuietHours is a household's SMS quiet-hours window (NES-139), rehomed
// here from internal/household/domain (NSTR-115): once household stopped
// owning its own row (household now lives in the shared identity schema),
// quiet hours could not follow it there — identity is strictly
// authentication/authorization, and delivery policy does not belong in it.
// Notifications stay a per-app concern (see the shared-identity decision
// record), so quiet hours rehomes into notify's own schema and domain
// instead, keyed by the identity household id.
//
// Start and End are a duration since local midnight (e.g. 22h for 22:00).
// Both nil means quiet hours are disabled — the default for every
// household. A window may cross midnight (Start > End, e.g. 22:00-07:00);
// InQuietHours and EndAfter both handle that case — see InQuietHours' own
// doc.
type QuietHours struct {
	HouseholdID household.HouseholdID
	Start       *time.Duration
	End         *time.Duration
}

// InQuietHours reports whether t's local clock time falls inside q's
// quiet-hours window. Always false when quiet hours are disabled (either
// bound nil).
//
// A window that crosses midnight (Start > End, e.g. 22:00-07:00) is
// handled by treating "inside" as "at or after Start, OR before End"
// rather than the non-wrapping "at or after Start AND before End" a
// same-day window uses.
func (q QuietHours) InQuietHours(t time.Time) bool {
	if q.Start == nil || q.End == nil {
		return false
	}
	start, end := *q.Start, *q.End
	since := sinceMidnight(t)
	if start <= end {
		return since >= start && since < end
	}
	// Crosses midnight: the window is everything from Start through
	// midnight, plus everything from midnight through End.
	return since >= start || since < end
}

// EndAfter returns the timestamp at which the quiet-hours window
// containing t ends. The caller is expected to have already confirmed
// InQuietHours(t) is true — the result is only meaningful for a t that
// actually falls inside the window; for one that does not, it still
// returns SOME end-of-window timestamp (computed as if t were in the
// window), which the caller must not rely on.
//
// For a same-day window the end boundary falls on t's own calendar date.
// For a window that crosses midnight, a t in the window's EVENING portion
// (since >= Start) has its end boundary on the FOLLOWING calendar date; a
// t in the window's EARLY-MORNING portion (since < End) has its end
// boundary on t's own date, since that portion's window already started
// the previous evening.
func (q QuietHours) EndAfter(t time.Time) time.Time {
	if q.Start == nil || q.End == nil {
		return t
	}
	start, end := *q.Start, *q.End
	day := t
	if start > end && sinceMidnight(t) >= start {
		day = t.AddDate(0, 0, 1)
	}
	return atClockTime(day, end)
}

// sinceMidnight returns t's clock time as a duration since the start of
// its own calendar day.
func sinceMidnight(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second +
		time.Duration(t.Nanosecond())
}

// atClockTime returns the timestamp on day's calendar date at clock time d
// since midnight, in day's own location.
//
// d's components are passed to time.Date individually, NOT added as an
// elapsed duration to midnight — the two are not equivalent across a DST
// transition. Adding an elapsed duration to a wall-clock midnight can
// silently drift the result by an hour whenever day's date crosses a
// spring-forward or fall-back boundary (e.g. a 07:00 quiet-hours end
// becoming 08:00 or 06:00), because Add is a fixed-duration shift, not a
// wall-clock-aware one. time.Date, by contrast, always resolves the given
// hour/minute/second to the correct instant for that date and location per
// the IANA tzdata rules, so the quiet-hours end is preserved at its
// intended wall-clock time regardless of any DST shift that day.
func atClockTime(day time.Time, d time.Duration) time.Time {
	hour := d / time.Hour
	d %= time.Hour
	minute := d / time.Minute
	d %= time.Minute
	second := d / time.Second
	d %= time.Second
	return time.Date(day.Year(), day.Month(), day.Day(),
		int(hour), int(minute), int(second), int(d), day.Location())
}

// QuietHoursWriter is the narrow port for updating a household's quiet
// hours (NES-139).
type QuietHoursWriter interface {
	// SetQuietHours updates householdID's quiet-hours window. Passing nil
	// for both start and end disables quiet hours; passing exactly one nil
	// is invalid (both nil = disabled is the only meaningful "partial"
	// state — see QuietHours' own doc) and returns a wrapped error, not a
	// sentinel.
	SetQuietHours(ctx context.Context, householdID household.HouseholdID, start, end *time.Duration) error
}

// QuietHoursReader is the narrow port for reading a household's quiet
// hours. A household with no stored row has quiet hours disabled (both
// nil) — there is no ErrHouseholdNotFound sentinel here, unlike
// household.HouseholdRepository.GetHousehold: quiet hours are opt-in, so
// "never configured" and "explicitly disabled" are the same, ordinary
// state.
type QuietHoursReader interface {
	GetQuietHours(ctx context.Context, householdID household.HouseholdID) (QuietHours, error)
}
