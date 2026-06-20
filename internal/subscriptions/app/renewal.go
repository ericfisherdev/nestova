package app

import (
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

// NextRenewal returns the renewal date one cycle after from: weekly adds 7 days,
// monthly adds one calendar month (clamping a short month, e.g. Jan 31 -> Feb
// 28/29), and yearly adds twelve calendar months (so Feb 29 -> Feb 28 in a
// non-leap year). The custom cycle has no defined advance and returns
// domain.ErrUnsupportedCycle. It is deterministic: the reference date is the
// caller's from, never the wall clock.
func NextRenewal(cycle domain.Cycle, from time.Time) (time.Time, error) {
	switch cycle {
	case domain.CycleWeekly:
		return from.AddDate(0, 0, 7), nil
	case domain.CycleMonthly:
		return household.AddMonthsClamped(from, 1), nil
	case domain.CycleYearly:
		return household.AddMonthsClamped(from, 12), nil
	case domain.CycleCustom:
		return time.Time{}, domain.ErrUnsupportedCycle
	default:
		return time.Time{}, fmt.Errorf("%w: %q", domain.ErrUnsupportedCycle, cycle)
	}
}

// AdvancePastDue rolls next forward by whole cycles until it is on or after asOf
// (compared by UTC date), returning the first such occurrence. A subscription
// whose next_renewal_on has passed (the charge happened) is advanced to its next
// future renewal; multiple missed cycles are skipped in one call. If next is
// already on or after asOf it is returned unchanged. The custom cycle returns
// domain.ErrUnsupportedCycle.
func AdvancePastDue(cycle domain.Cycle, next, asOf time.Time) (time.Time, error) {
	target := dateOf(asOf)
	cur := next
	// Bound the loop defensively so a degenerate cycle can never spin forever;
	// the weekly cycle advances ~52 occurrences per year, so a decade of arrears
	// is well under this cap.
	for range maxAdvanceSteps {
		if !cur.Before(target) {
			return cur, nil
		}
		advanced, err := NextRenewal(cycle, cur)
		if err != nil {
			return time.Time{}, err
		}
		cur = advanced
	}
	return time.Time{}, fmt.Errorf("subscriptions: renewal did not reach %s within %d steps", target.Format(time.DateOnly), maxAdvanceSteps)
}

// maxAdvanceSteps caps AdvancePastDue's loop. A weekly subscription advances ~52
// times per year, so this covers more than a century of missed renewals.
const maxAdvanceSteps = 10000

// dateOf truncates a timestamp to its UTC calendar date, matching the date-typed
// next_renewal_on column (stored as UTC midnight).
func dateOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
