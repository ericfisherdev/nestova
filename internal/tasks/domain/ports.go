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
// NES-37 addition:
//   - CompletionDays returns the distinct calendar days (midnight UTC) on which
//     the given member completed at least one task within the household, filtered
//     to rows with completed_at >= since. The result is ordered ascending and is
//     used by the streak calculation ([CurrentStreak]).
//
// Persistence contracts:
//   - Insert expects inst.ID, inst.RecurringTaskID, inst.HouseholdID, inst.Kind,
//     inst.Status, and optionally inst.AssigneeID set. inst.DueOn must be
//     non-nil for [KindScheduled] and nil for [KindStanding] (NES-116); the
//     zero value of Kind is treated as [KindScheduled] for callers that predate
//     NES-116. The store sets CreatedAt and UpdatedAt.
//   - Complete transitions status from pending OR overdue to done and records
//     completed_at and completed_by. Skip transitions pending OR overdue to
//     skipped. Both refresh updated_at. An overdue chore is still actionable: it
//     can be completed or skipped late. NES-116: both (and CompleteAndAward)
//     respawn a fresh standing instance in the same transaction when the row
//     transitioned was kind=standing — the invariant holds on every terminal
//     path, not just completion.
//   - MarkPendingOverdue bulk-transitions pending, [KindScheduled] instances
//     whose due_on < asOf to overdue, scoped to the household, refreshing
//     updated_at. [KindStanding] instances are never affected: they have no due
//     date and so can never be overdue.
//
// Error contracts:
//   - Insert returns [ErrDuplicateInstance] on (recurring_task_id, due_on)
//     conflict (constraint task_instance_task_due_uniq). A NULL due_on
//     (standing instances) is never considered a duplicate of another NULL
//     due_on, so a task's completed standing instances accumulate as distinct
//     history rows, matching scheduled instances.
//   - Get returns [ErrInstanceNotFound] when id is unknown or belongs to another
//     household.
//   - Claim, Complete, and Skip act on a pending or overdue instance.
//   - Claim, Complete, and Skip return [ErrInstanceNotFound] when id is unknown
//     or belongs to another household.
//   - Claim returns [ErrInstanceAlreadyClaimed] when the instance is already
//     assigned to a DIFFERENT member than the one claiming. Claiming an
//     instance already assigned to the calling member (a self-claim) succeeds
//     idempotently instead (NES-117): see Claim's own doc for the full
//     assignee/claim-expiry contract.
//   - Complete, Skip, and Claim return [ErrInstanceInTerminalState] when the
//     instance is already in a terminal state. As of NES-32, terminal means done
//     or skipped only — overdue is no longer terminal for these transitions.
//     NES-116 reuses this same sentinel for a lost double-completion race on a
//     standing instance: the loser's UPDATE matches zero rows because the
//     winner's commit already flipped status to done, which is indistinguishable
//     from (and handled identically to) any other already-terminal instance.
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

	// ListByHousehold returns [KindScheduled] instances for the household
	// filtered by status and due date range [from, to] (inclusive). Standing
	// instances are never returned (they have no due date to filter by).
	// Returns an empty slice when none match.
	ListByHousehold(ctx context.Context, householdID household.HouseholdID, status InstanceStatus, from, to time.Time) ([]*TaskInstance, error)

	// ListStanding returns every pending [KindStanding] instance for the
	// household — each is the single open occurrence of an as-needed recurring
	// task (NES-116). Ordered by created_at for a stable display order. Returns
	// an empty slice (not an error) when none exist.
	ListStanding(ctx context.Context, householdID household.HouseholdID) ([]*TaskInstance, error)

	// LatestDueOn returns the most recent due_on materialised for the task within
	// the household and ok=true, or the zero time and ok=false when no instances
	// exist yet.
	LatestDueOn(ctx context.Context, householdID household.HouseholdID, id RecurringTaskID) (time.Time, bool, error)

	// Claim assigns the instance to assignee when it is pending or overdue and
	// either currently unassigned or already assigned to assignee. Claiming is
	// first-come for anyone else: an instance assigned to a DIFFERENT member
	// cannot be taken over.
	//
	// NES-117 claim-expiry semantics (evaluated against the instance's
	// pre-update state):
	//   - assignee already holds an active claim (ClaimedBy already equals
	//     assignee) → this call only re-asserts an existing claim. ClaimedAt
	//     and ClaimExpiresAt are left UNCHANGED — re-claiming your own claim
	//     must never reset or clear its expiry, or a member could evade the
	//     penalty indefinitely by calling Claim repeatedly.
	//   - Otherwise, previously unassigned (claimable, or a NES-116 standing
	//     instance) → this is a NEW claim on a chore not originally assigned
	//     to anyone, so ClaimedBy/ClaimedAt are set and ClaimExpiresAt is set
	//     to claimed_at + [ClaimWindow]. AssigneeID becomes assignee.
	//   - Otherwise, already assigned to assignee but never claimed (a
	//     fixed/round-robin instance's own assignee claiming it for the first
	//     time) → ClaimedBy/ClaimedAt are set but ClaimExpiresAt is left nil:
	//     no risk, since the chore was already assignee's responsibility.
	//     AssigneeID is unchanged.
	//
	// Returns [ErrInstanceNotFound] when id is unknown or belongs to another household.
	// Returns [ErrInstanceInTerminalState] when the instance is done or skipped.
	// Returns [ErrInstanceAlreadyClaimed] when a pending/overdue instance is
	// already assigned to a different member than assignee.
	Claim(ctx context.Context, householdID household.HouseholdID, id TaskInstanceID, assignee household.MemberID) error

	// Complete transitions the instance from pending or overdue to done, recording
	// by and at. It does not award points (see CompleteAndAward for the
	// user-facing completion flow). NES-116: like every terminal transition on a
	// [KindStanding] instance, a fresh pending standing instance for the same
	// recurring task is materialised in the same transaction — the "always
	// exactly one open standing instance" invariant does not depend on which
	// method performed the transition.
	//
	// NES-117: ClaimedBy, ClaimedAt, and ClaimExpiresAt are cleared in the same
	// transition — they describe a CURRENT claim, and a done instance has none.
	// This applies to every terminal transition ([Complete], [CompleteAndAward],
	// and [Skip] alike).
	//
	// Returns [ErrInstanceNotFound] when id is unknown or belongs to another household.
	// Returns [ErrInstanceInTerminalState] when the instance is already done or skipped.
	Complete(ctx context.Context, householdID household.HouseholdID, id TaskInstanceID, by household.MemberID, at time.Time) error

	// CompleteAndAward atomically transitions the instance from pending or
	// overdue to done and appends a point_ledger credit for the completing
	// member. Both writes run inside a single database transaction so the award
	// is never separated from the state change. The ledger INSERT uses ON
	// CONFLICT DO NOTHING so a duplicate award for the same instance is silently
	// skipped (belt-and-suspenders: the status guard already prevents
	// re-completion). When the associated recurring task has points = 0, no
	// ledger row is produced.
	//
	// NES-116: when the completed instance is [KindStanding], a fresh pending
	// standing instance for the same recurring task is materialised in the same
	// transaction, so an as-needed task always has exactly one open standing
	// instance both before and immediately after completion. The same holds for
	// [Complete] and [Skip]: the invariant is enforced on every terminal
	// transition of a standing instance, not just this one.
	//
	// Returns [ErrInstanceNotFound] when id is unknown or belongs to another
	// household.
	// Returns [ErrInstanceInTerminalState] when the instance is already done or
	// skipped (no award is made, no replacement instance is materialised). Two
	// concurrent completions of the same standing instance race on this guard:
	// exactly one succeeds and awards points; the other observes the row already
	// done and returns this sentinel. The same race resolution applies when a
	// completion and a skip target the same standing instance concurrently —
	// exactly one transition wins and exactly one respawn happens.
	//
	// NES-117: ClaimedBy/ClaimedAt/ClaimExpiresAt are cleared in the same
	// transition as [Complete] — see its doc.
	CompleteAndAward(ctx context.Context, householdID household.HouseholdID, id TaskInstanceID, by household.MemberID, at time.Time) error

	// Skip transitions the instance from pending or overdue to skipped. NES-116:
	// skipping a [KindStanding] instance releases it back to the pool — a fresh,
	// unassigned standing instance for the same recurring task is materialised
	// in the same transaction, exactly as [Complete] and [CompleteAndAward] do
	// on their own terminal transitions.
	//
	// NES-117: ClaimedBy/ClaimedAt/ClaimExpiresAt are cleared in the same
	// transition as [Complete] — see its doc. AssigneeID, in contrast, is left
	// untouched: Skip does not release the instance's assignee back to the
	// pool the way an expiry-driven revert does.
	//
	// Returns [ErrInstanceNotFound] when id is unknown or belongs to another household.
	// Returns [ErrInstanceInTerminalState] when the instance is already done or skipped.
	Skip(ctx context.Context, householdID household.HouseholdID, id TaskInstanceID) error

	// MarkPendingOverdue bulk-transitions all pending, [KindScheduled] instances
	// for the household whose due_on < asOf to overdue. [KindStanding] instances
	// are never selected — their due_on is NULL, which never satisfies the
	// comparison. Returns the number of rows updated.
	MarkPendingOverdue(ctx context.Context, householdID household.HouseholdID, asOf time.Time) (int, error)

	// MarkPendingOverdueAll bulk-transitions all pending, [KindScheduled]
	// instances across ALL households whose due_on < asOf to overdue.
	// [KindStanding] instances are never selected: they have no due date and so
	// can never become overdue. Returns the newly-overdue rows as
	// [ReminderTarget] values (Kind=[ReminderOverdue]) so the caller can
	// enqueue overdue notifications without an additional query. Callers that
	// only want the count use len() on the returned slice.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process method reserved for the background scheduler (NES-31) and
	// must not be called from user-facing request handlers, which must use the
	// household-scoped [MarkPendingOverdue] instead. The same precedent applies
	// here as for [RecurringTaskRepository.ListAllActive].
	MarkPendingOverdueAll(ctx context.Context, asOf time.Time) ([]ReminderTarget, error)

	// ClaimDueSoonReminders atomically selects pending, [KindScheduled] instances
	// inside the closed due-soon window (asOf <= due_on <= asOf + lead_time_days)
	// that have not yet been reminded (reminded_at IS NULL), marks reminded_at =
	// now() on each, and returns them as [ReminderTarget] values
	// (Kind=[ReminderDueSoon]). [KindStanding] instances never receive a
	// due-soon reminder: they have no due date to enter the window with.
	// Because reminded_at is set atomically, a row is returned at most once
	// across concurrent or repeated calls — the idempotency guarantee.
	//
	// The lower bound (due_on >= asOf) excludes past-due pending rows: those are
	// overdue and belong to the [MarkPendingOverdueAll] path, so an overdue-sweep
	// failure in the same tick cannot leak a past-due row into the due-soon
	// stream.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process method reserved for the background scheduler (NES-34) and
	// must not be called from user-facing request handlers. The same precedent
	// applies here as for [RecurringTaskRepository.ListAllActive].
	ClaimDueSoonReminders(ctx context.Context, asOf time.Time) ([]ReminderTarget, error)

	// ClearDueSoonReminder resets reminded_at to NULL for the instance, making it
	// eligible for re-claim by [ClaimDueSoonReminders] on a later tick. It is the
	// recovery counterpart to ClaimDueSoonReminders: when the caller fails to
	// enqueue a due-soon notification after the row was claimed (reminded_at
	// stamped), clearing reminded_at prevents the reminder from being lost
	// permanently.
	//
	// It is a no-op (nil error) when id is unknown — recovery must be idempotent
	// and tolerant of a row deleted between claim and clear.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process recovery method reserved for the background scheduler
	// (NES-34) and must not be called from user-facing request handlers.
	ClearDueSoonReminder(ctx context.Context, id TaskInstanceID) error

	// CompletionDays returns the distinct calendar days (midnight UTC) on which
	// member completed at least one task within the household, restricted to rows
	// whose completed_at is at or after since.  Results are ordered ascending.
	// Returns an empty slice (not an error) when no completions match.
	// Used by the NES-37 streak calculation.
	CompletionDays(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, since time.Time) ([]time.Time, error)

	// SweepExpiredClaims atomically reverts every claim whose ClaimExpiresAt is
	// at or before asOf and whose instance is still pending or overdue: the
	// claim fields (AssigneeID, ClaimedBy, ClaimedAt, ClaimExpiresAt) are
	// cleared back to their unclaimed state, and a negative point_ledger entry
	// (source_type [SourceTypeClaimExpiry]) of -[ClaimExpiryPenalty] is
	// appended for each claimant in the SAME transaction as the revert, so a
	// claimant's balance always reflects an actually-reverted claim. The
	// penalty is applied unconditionally — it is never clamped or skipped
	// because a member's balance is already zero or negative.
	//
	// NES-116: reverting a standing instance's claim is not a terminal
	// transition. The row keeps its id, stays pending, and is not respawned —
	// it simply becomes an unclaimed standing instance again, preserving the
	// "exactly one open standing instance per as-needed task" invariant
	// without special-casing.
	//
	// Idempotency: implementations must guarantee that the same claim window
	// (an instance's specific claimed-then-expired episode) is never
	// penalized twice, while a later, independent claim on the same instance
	// that also expires IS penalized again on its own.
	//
	// Orphaned claims: a claim whose claimant's member row was deleted before
	// expiry has ClaimedBy nil but ClaimedAt/ClaimExpiresAt still set (the
	// member deletion nulls only ClaimedBy — see Claim's doc). Such a row is
	// still reverted so it does not accumulate as a dangling claimed instance,
	// but no penalty entry is appended and it is excluded from the returned
	// slice: there is no one left to penalize or notify.
	//
	// Returns the reverted claims as [ExpiredClaim] values so the caller can
	// enqueue a notification per claimant without an additional query. Returns
	// an empty slice (not an error) when no claims are expired as of asOf.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process method reserved for the background scheduler and must
	// not be called from user-facing request handlers, matching the
	// precedent set by [MarkPendingOverdueAll] and [ClaimDueSoonReminders].
	SweepExpiredClaims(ctx context.Context, asOf time.Time) ([]ExpiredClaim, error)
}
