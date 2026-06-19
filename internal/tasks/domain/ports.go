package domain

import (
	"context"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// RecurringTaskRepository is the persistence port for [RecurringTask] aggregates
// and their associated rotation pools. Implementations live in the adapter layer
// (NES-29) and are injected into application services (NES-30).
//
// All ID-based methods are tenant-scoped: they take the household id as a
// leading argument and enforce household isolation, so a RecurringTaskID that
// belongs to a different household is treated as unknown (yields
// [ErrTaskNotFound]).
//
// Exception: [RecurringTaskRepository.ListAllActive] is intentionally NOT
// household-scoped — it is a system-process method used by the background
// generator (NES-30) to iterate every active task across all households. It
// must never be exposed to user-facing request handlers.
//
// Persistence contracts:
//   - Create expects rt.ID, rt.HouseholdID, rt.Title, rt.Category, rt.Cadence,
//     rt.RotationPolicy, rt.Points, rt.LeadTimeDays, and rt.Active set. The
//     store sets CreatedAt and UpdatedAt.
//   - SetRotationMembers replaces the entire pool atomically; the slice order
//     determines position (position = slice index). Passing an empty slice
//     clears the pool.
//   - RotationMembers returns members ordered by position ascending.
//
// Error contracts:
//   - Get returns [ErrTaskNotFound] when id is unknown or belongs to another
//     household.
//   - ListActive returns an empty slice (not an error) for an unknown household.
//   - ListAllActive returns an empty slice (not an error) when no active tasks
//     exist in any household.
//   - SetRotationMembers returns [ErrTaskNotFound] when id is unknown or belongs
//     to another household.
//   - RotationMembers returns [ErrTaskNotFound] when id is unknown or belongs to
//     another household, and an empty slice (not [ErrNoRotationMembers]) when
//     the pool is empty — the caller (generator) raises [ErrNoRotationMembers]
//     after inspecting the slice.
type RecurringTaskRepository interface {
	// Create persists a new recurring task.
	Create(ctx context.Context, rt *RecurringTask) error

	// CreateWithRotation atomically persists a new recurring task together with
	// its initial rotation pool in a single transaction. If any insert fails
	// (e.g. a member id that does not exist or belongs to another household),
	// the whole operation rolls back and no recurring_task row is left behind.
	//
	// The pool slice order determines rotation position (position = slice index).
	// Passing an empty pool persists the task with no rotation members; callers
	// that require a non-empty pool (fixed/round_robin policies) must enforce
	// that before calling.
	//
	// task.ID, task.HouseholdID, task.Title, task.Category, task.Cadence,
	// task.RotationPolicy, task.Points, task.LeadTimeDays, and task.Active must be
	// set; the store populates task.CreatedAt and task.UpdatedAt.
	CreateWithRotation(ctx context.Context, task *RecurringTask, pool []household.MemberID) error

	// Get returns the recurring task with the given id within the household.
	// Returns [ErrTaskNotFound] when id is unknown or belongs to another household.
	Get(ctx context.Context, householdID household.HouseholdID, id RecurringTaskID) (*RecurringTask, error)

	// ListActive returns all active recurring tasks for the household.
	// Returns an empty slice (not an error) when householdID is unknown.
	ListActive(ctx context.Context, householdID household.HouseholdID) ([]*RecurringTask, error)

	// ListAllActive returns every active recurring task across ALL households,
	// ordered by household_id then created_at.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is reserved
	// for the background materialisation process (Generator.GenerateDue) and must
	// not be called from user-facing request handlers, which must use the
	// household-scoped [ListActive] instead.
	ListAllActive(ctx context.Context) ([]*RecurringTask, error)

	// SetRotationMembers atomically replaces the rotation pool for the task
	// within the household. Position is determined by the slice order
	// (position = index).
	// Returns [ErrTaskNotFound] when id is unknown or belongs to another household.
	SetRotationMembers(ctx context.Context, householdID household.HouseholdID, id RecurringTaskID, members []household.MemberID) error

	// RotationMembers returns the rotation pool members ordered by position for
	// the task within the household.
	// Returns [ErrTaskNotFound] when id is unknown or belongs to another household.
	// Returns an empty slice (not an error) when the pool is empty.
	RotationMembers(ctx context.Context, householdID household.HouseholdID, id RecurringTaskID) ([]household.MemberID, error)
}

// TaskInstanceRepository is the persistence port for [TaskInstance] records.
// Implementations live in the adapter layer (NES-29) and are injected into
// application services (NES-30).
//
// All ID-based methods are tenant-scoped: they take the household id as a
// leading argument and enforce household isolation, so a TaskInstanceID or
// RecurringTaskID that belongs to a different household is treated as unknown
// (yields [ErrInstanceNotFound]).
//
// Persistence contracts:
//   - Insert expects inst.ID, inst.RecurringTaskID, inst.HouseholdID, inst.DueOn,
//     inst.Status, and optionally inst.AssigneeID set. The store sets CreatedAt
//     and UpdatedAt.
//   - Complete transitions status from pending OR overdue to done and records
//     completed_at and completed_by. Skip transitions pending OR overdue to
//     skipped. Both refresh updated_at. An overdue chore is still actionable: it
//     can be completed or skipped late.
//   - MarkPendingOverdue bulk-transitions pending instances whose due_on < asOf
//     to overdue, scoped to the household, refreshing updated_at.
//
// Error contracts:
//   - Insert returns [ErrDuplicateInstance] on (recurring_task_id, due_on)
//     conflict (constraint task_instance_task_due_uniq).
//   - Get returns [ErrInstanceNotFound] when id is unknown or belongs to another
//     household.
//   - Claim, Complete, and Skip act on a pending or overdue instance.
//   - Claim, Complete, and Skip return [ErrInstanceNotFound] when id is unknown
//     or belongs to another household.
//   - Claim returns [ErrInstanceAlreadyClaimed] when the instance is already
//     assigned to a member.
//   - Complete, Skip, and Claim return [ErrInstanceInTerminalState] when the
//     instance is already in a terminal state. As of NES-32, terminal means done
//     or skipped only — overdue is no longer terminal for these transitions.
//   - LatestDueOn returns (zero, false, nil) when no instances exist for the task.
//   - ListByHousehold returns an empty slice (not an error) when no instances
//     match the filter.
type TaskInstanceRepository interface {
	// Insert persists a new task instance.
	// Returns [ErrDuplicateInstance] on a (recurring_task_id, due_on) conflict.
	Insert(ctx context.Context, inst *TaskInstance) error

	// Get returns the task instance with the given id within the household.
	// Returns [ErrInstanceNotFound] when id is unknown or belongs to another household.
	Get(ctx context.Context, householdID household.HouseholdID, id TaskInstanceID) (*TaskInstance, error)

	// ListByHousehold returns instances for the household filtered by status and
	// due date range [from, to] (inclusive). Returns an empty slice when none match.
	ListByHousehold(ctx context.Context, householdID household.HouseholdID, status InstanceStatus, from, to time.Time) ([]*TaskInstance, error)

	// LatestDueOn returns the most recent due_on materialised for the task within
	// the household and ok=true, or the zero time and ok=false when no instances
	// exist yet.
	LatestDueOn(ctx context.Context, householdID household.HouseholdID, id RecurringTaskID) (time.Time, bool, error)

	// Claim assigns the instance to assignee when it is pending or overdue and
	// currently unassigned. Claiming is first-come, not reassignment.
	// Returns [ErrInstanceNotFound] when id is unknown or belongs to another household.
	// Returns [ErrInstanceInTerminalState] when the instance is done or skipped.
	// Returns [ErrInstanceAlreadyClaimed] when a pending/overdue instance is already assigned.
	Claim(ctx context.Context, householdID household.HouseholdID, id TaskInstanceID, assignee household.MemberID) error

	// Complete transitions the instance from pending or overdue to done, recording
	// by and at.
	// Returns [ErrInstanceNotFound] when id is unknown or belongs to another household.
	// Returns [ErrInstanceInTerminalState] when the instance is already done or skipped.
	Complete(ctx context.Context, householdID household.HouseholdID, id TaskInstanceID, by household.MemberID, at time.Time) error

	// Skip transitions the instance from pending or overdue to skipped.
	// Returns [ErrInstanceNotFound] when id is unknown or belongs to another household.
	// Returns [ErrInstanceInTerminalState] when the instance is already done or skipped.
	Skip(ctx context.Context, householdID household.HouseholdID, id TaskInstanceID) error

	// MarkPendingOverdue bulk-transitions all pending instances for the household
	// whose due_on < asOf to overdue. Returns the number of rows updated.
	MarkPendingOverdue(ctx context.Context, householdID household.HouseholdID, asOf time.Time) (int, error)

	// MarkPendingOverdueAll bulk-transitions all pending instances across ALL
	// households whose due_on < asOf to overdue. Returns the newly-overdue rows
	// as [ReminderTarget] values (Kind=[ReminderOverdue]) so the caller can
	// enqueue overdue notifications without an additional query. Callers that
	// only want the count use len() on the returned slice.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process method reserved for the background scheduler (NES-31) and
	// must not be called from user-facing request handlers, which must use the
	// household-scoped [MarkPendingOverdue] instead. The same precedent applies
	// here as for [RecurringTaskRepository.ListAllActive].
	MarkPendingOverdueAll(ctx context.Context, asOf time.Time) ([]ReminderTarget, error)

	// ClaimDueSoonReminders atomically selects pending instances that have
	// entered their lead-time window (due_on - lead_time_days <= asOf) and have
	// not yet been reminded (reminded_at IS NULL), marks reminded_at = now() on
	// each, and returns them as [ReminderTarget] values (Kind=[ReminderDueSoon]).
	// Because reminded_at is set atomically, a row is returned at most once
	// across concurrent or repeated calls — the idempotency guarantee.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process method reserved for the background scheduler (NES-34) and
	// must not be called from user-facing request handlers. The same precedent
	// applies here as for [RecurringTaskRepository.ListAllActive].
	ClaimDueSoonReminders(ctx context.Context, asOf time.Time) ([]ReminderTarget, error)
}
