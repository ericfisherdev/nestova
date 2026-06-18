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
// date. Its lifecycle moves through the [InstanceStatus] states: pending →
// done/skipped, or pending → overdue (via the scheduler sweep).
//
// AssigneeID is nil for [RotationClaimable] tasks or when the instance has not
// yet been claimed. CompletedAt and CompletedBy are populated when Status
// transitions to [StatusDone].
type TaskInstance struct {
	ID              TaskInstanceID
	RecurringTaskID RecurringTaskID
	HouseholdID     household.HouseholdID
	AssigneeID      *household.MemberID
	// DueOn is a calendar date mapping to the task_instance.due_on DATE column.
	// It must be normalized with [DateOf] (midnight UTC) so the calendar day is
	// stable across a DATE round-trip; the NES-29 adapter applies DateOf on both
	// write and read.
	DueOn       time.Time
	Status      InstanceStatus
	CompletedAt *time.Time
	CompletedBy *household.MemberID
	CreatedAt   time.Time
	// UpdatedAt is refreshed on every status transition (claim, complete, skip,
	// overdue sweep); the NES-29 adapter maintains it.
	UpdatedAt time.Time
}
