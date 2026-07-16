package domain

import (
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// DateOf returns t's calendar date at midnight UTC. It is the canonical
// normalized form for [TaskInstance.DueOn]: persisting and re-reading a value
// produced by DateOf through a DATE column never shifts the calendar day,
// regardless of the input's clock time or location.
func DateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// DueOnPtr returns a pointer to t normalized with [DateOf], for constructing a
// [TaskInstance.DueOn] value inline (e.g. in a struct literal, where a
// function result cannot be addressed directly). Only scheduled instances use
// it; a standing instance's DueOn stays nil.
func DueOnPtr(t time.Time) *time.Time {
	d := DateOf(t)
	return &d
}

// RecurringTask is a template that defines a repeating household chore or
// maintenance item. It is the aggregate root of the tasks bounded context. The
// generator (NES-30) reads active recurring tasks and materialises
// [TaskInstance] rows ahead of time according to the embedded [Cadence].
//
// The Cadence field is marshalled to/from the cadence jsonb column by the
// NES-29 adapter using encoding/json; no custom pgx codec is required.
type RecurringTask struct {
	ID             RecurringTaskID
	HouseholdID    household.HouseholdID
	Title          string
	Category       Category
	Cadence        household.Cadence
	RotationPolicy RotationPolicy
	// Points awarded to the member who completes an instance of this task.
	Points int
	// LeadTimeDays is the number of days before due_on that an instance is
	// made visible (e.g. 2 means the instance appears two days early).
	LeadTimeDays int
	Active       bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TaskInstance is a materialised occurrence of a [RecurringTask] on a specific
// date, or the single standing occurrence of an as-needed task. Its lifecycle
// moves through the [InstanceStatus] states: pending → done/skipped, or
// pending → overdue (via the scheduler sweep). A standing instance ([Kind] =
// [KindStanding]) never becomes overdue because it has no DueOn.
//
// AssigneeID is nil for [RotationClaimable] tasks or when the instance has not
// yet been claimed. CompletedAt and CompletedBy are populated when Status
// transitions to [StatusDone].
//
// NES-117: ClaimedBy/ClaimedAt/ClaimExpiresAt track claim risk metadata
// independently of AssigneeID. AssigneeID keeps its pre-NES-117 meaning (the
// member responsible for the instance, whether via rotation or a claim); the
// new fields exist only to know whether that responsibility carries an
// expiry penalty and, if so, when it lapses.
type TaskInstance struct {
	ID              TaskInstanceID
	RecurringTaskID RecurringTaskID
	HouseholdID     household.HouseholdID
	AssigneeID      *household.MemberID
	// DueOn is a calendar date mapping to the task_instance.due_on DATE column.
	// Non-nil and normalized with [DateOf] (midnight UTC) for a [KindScheduled]
	// instance, so the calendar day is stable across a DATE round-trip; the
	// NES-29 adapter applies DateOf on both write and read. Nil for a
	// [KindStanding] instance (NES-116), which has no due date.
	DueOn       *time.Time
	Status      InstanceStatus
	CompletedAt *time.Time
	CompletedBy *household.MemberID
	// Kind distinguishes an ahead-of-time materialised occurrence
	// ([KindScheduled], the default for every instance created before NES-116)
	// from the always-open standing instance of an as-needed task
	// ([KindStanding]).
	Kind InstanceKind
	// ClaimedBy is the member who currently holds a claim recorded via
	// [TaskInstanceRepository.Claim], or nil when the instance has never been
	// claimed (or its claim already expired/completed). Set together with
	// ClaimedAt (NES-117).
	ClaimedBy *household.MemberID
	// ClaimedAt is when ClaimedBy claimed the instance, or nil to match
	// ClaimedBy.
	ClaimedAt *time.Time
	// ClaimExpiresAt is when an at-risk claim penalizes ClaimedBy if the
	// instance is not completed by then. Non-nil only for a claim made on a
	// previously-unassigned instance; nil for an unclaimed instance and for a
	// self-claim on an already-assigned instance (no risk).
	ClaimExpiresAt *time.Time
	CreatedAt      time.Time
	// UpdatedAt is refreshed on every status transition (claim, complete, skip,
	// overdue sweep, claim expiry); the NES-29 adapter maintains it.
	UpdatedAt time.Time
}
