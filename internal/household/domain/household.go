package domain

import (
	"context"
	"errors"
	"time"
)

// Household is the aggregate root for the household bounded context.
type Household struct {
	ID   HouseholdID
	Name string
	// QuietHoursStart and QuietHoursEnd bound the household's SMS quiet
	// window (NES-139) as a duration since local midnight (e.g. 22h for
	// 22:00). Both nil means quiet hours are disabled — the default for
	// every household. A window may cross midnight (Start > End, e.g.
	// 22:00-07:00); InQuietHours and QuietHoursEndAfter both handle that
	// case — see InQuietHours' own doc.
	QuietHoursStart *time.Duration
	QuietHoursEnd   *time.Duration
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// InQuietHours reports whether t's local clock time falls inside the
// household's quiet-hours window. Always false when quiet hours are
// disabled (either bound nil).
//
// A window that crosses midnight (Start > End, e.g. 22:00-07:00) is
// handled by treating "inside" as "at or after Start, OR before End"
// rather than the non-wrapping "at or after Start AND before End" a
// same-day window uses.
func (h Household) InQuietHours(t time.Time) bool {
	if h.QuietHoursStart == nil || h.QuietHoursEnd == nil {
		return false
	}
	start, end := *h.QuietHoursStart, *h.QuietHoursEnd
	since := sinceMidnight(t)
	if start <= end {
		return since >= start && since < end
	}
	// Crosses midnight: the window is everything from Start through
	// midnight, plus everything from midnight through End.
	return since >= start || since < end
}

// QuietHoursEndAfter returns the timestamp at which the quiet-hours
// window containing t ends. The caller is expected to have already
// confirmed InQuietHours(t) is true — the result is only meaningful for a
// t that actually falls inside the window; for one that does not, it
// still returns SOME end-of-window timestamp (computed as if t were in
// the window), which the caller must not rely on.
//
// For a same-day window the end boundary falls on t's own calendar date.
// For a window that crosses midnight, a t in the window's EVENING portion
// (since >= Start) has its end boundary on the FOLLOWING calendar date; a
// t in the window's EARLY-MORNING portion (since < End) has its end
// boundary on t's own date, since that portion's window already started
// the previous evening.
func (h Household) QuietHoursEndAfter(t time.Time) time.Time {
	if h.QuietHoursStart == nil || h.QuietHoursEnd == nil {
		return t
	}
	start, end := *h.QuietHoursStart, *h.QuietHoursEnd
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
// hours (NES-139) — separate from HouseholdRepository (ISP) so the many
// existing HouseholdRepository test fakes across the codebase never need
// a matching method: only the quiet-hours settings handler depends on
// this interface, and PostgresRepository satisfies it structurally
// alongside HouseholdRepository without either interface embedding the
// other.
type QuietHoursWriter interface {
	// SetQuietHours updates householdID's quiet-hours window. Passing nil
	// for both start and end disables quiet hours; passing exactly one nil
	// is invalid (both nil = disabled is the only meaningful "partial"
	// state — see Household's own doc) and returns a wrapped error, not a
	// sentinel. Returns ErrHouseholdNotFound when householdID is unknown.
	SetQuietHours(ctx context.Context, householdID HouseholdID, start, end *time.Duration) error
}

// Domain errors returned by HouseholdRepository implementations.
var (
	// ErrHouseholdNotFound is returned when a household does not exist.
	ErrHouseholdNotFound = errors.New("household: household not found")
	// ErrMemberNotFound is returned when a member does not exist.
	ErrMemberNotFound = errors.New("household: member not found")
	// ErrDuplicateMember is returned when adding a member whose display name
	// already exists (case-insensitively) within the household.
	ErrDuplicateMember = errors.New("household: duplicate member display name in household")
	// ErrHouseholdExists is returned by first-run provisioning when a household
	// already exists, so the (single-household) onboarding flow is a no-op.
	ErrHouseholdExists = errors.New("household: a household already exists")
)

// HouseholdRepository persists households and their members. Member is a child
// entity of the Household aggregate, so its operations live on this one
// aggregate-scoped repository.
//
// Persistence contracts (the caller sets identity and valid enum values; the
// store sets timestamps):
//   - CreateHousehold expects h.ID and h.Name set; it populates CreatedAt/UpdatedAt.
//     QuietHoursStart/QuietHoursEnd are always nil on a newly created household
//     (quiet hours are opted into later, via QuietHoursWriter) — CreateHousehold
//     does not read them from h even if the caller set them.
//   - AddMember expects m.ID, m.HouseholdID, m.DisplayName, and valid m.Role /
//     m.Color set; it populates CreatedAt/UpdatedAt. The caller is responsible
//     for supplying valid enum values (the store does not re-validate on write).
//
// Error contracts:
//   - GetHousehold returns ErrHouseholdNotFound when id is unknown.
//   - GetMember returns ErrMemberNotFound when id is unknown.
//   - AddMember returns ErrDuplicateMember when the display name collides within
//     the household, and ErrHouseholdNotFound when m.HouseholdID does not exist.
//   - ListMembers returns an empty slice (not an error) for an unknown household.
//   - CreateHousehold/AddMember surface other failures (e.g. an id collision) as
//     a wrapped error, not a sentinel.
//   - HasAnyHousehold returns (false, nil) on an empty database and (true, nil)
//     once at least one household row exists. It never returns a sentinel error.
type HouseholdRepository interface {
	CreateHousehold(ctx context.Context, h *Household) error
	GetHousehold(ctx context.Context, id HouseholdID) (*Household, error)
	AddMember(ctx context.Context, m *Member) error
	GetMember(ctx context.Context, id MemberID) (*Member, error)
	ListMembers(ctx context.Context, householdID HouseholdID) ([]*Member, error)
	// HasAnyHousehold reports whether at least one household row exists. It is
	// used by the onboarding flow to gate the first-run setup page and to block
	// a second call to the public POST /onboarding route (open-registration guard).
	HasAnyHousehold(ctx context.Context) (bool, error)
}
