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

	// ClaimWarnings atomically selects every claim whose ClaimExpiresAt falls
	// within [ClaimWarningWindow] of asOf and has not yet been warned
	// (claim_warned_at IS NULL), marks claim_warned_at = now() on each, and
	// returns them as [ClaimWarning] values (NES-118). Because
	// claim_warned_at is set in the same statement as the selection, the same
	// claim window is warned at most once — mirroring [ClaimDueSoonReminders]'s
	// reminded_at idempotency guarantee.
	//
	// A claim whose window has already expired as of asOf (ClaimExpiresAt <=
	// asOf) is excluded: that claim belongs to [SweepExpiredClaims] instead,
	// not this warning path. Only pending or overdue instances are
	// considered, matching SweepExpiredClaims' status predicate.
	//
	// Orphaned claims (the claimant's member row was deleted before expiry,
	// leaving ClaimedBy nil per [TaskInstanceRepository.Claim]'s doc) are
	// excluded — there is no one to warn.
	//
	// Returns the warned claims as [ClaimWarning] values so the caller can
	// enqueue a notification per claimant without an additional query.
	// Returns an empty slice (not an error) when no claims enter the warning
	// window as of asOf.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process method reserved for the background scheduler and must
	// not be called from user-facing request handlers, matching the
	// precedent set by [SweepExpiredClaims] and [ClaimDueSoonReminders].
	ClaimWarnings(ctx context.Context, asOf time.Time) ([]ClaimWarning, error)

	// ClearClaimWarning resets claim_warned_at to NULL for id's claim, scoped
	// to the specific claim window identified by expiresAt. It is the
	// recovery counterpart to [ClaimWarnings], invoked when a warning
	// notification fails to enqueue after the row was marked warned, so the
	// warning is retried on a later tick instead of being lost.
	//
	// The expiresAt guard scopes the reset to the SAME claim window the
	// warning was generated for: if the instance has since moved on to a
	// different claim window (completed, skipped, or swept-and-reclaimed), a
	// blind reset by id alone would clear a later, unrelated window's
	// claim_warned_at and cause a spurious duplicate warning.
	//
	// It is a no-op (nil error) when id is unknown or its claim_expires_at no
	// longer matches expiresAt — recovery must be idempotent and tolerant of
	// the row having moved on before the clear runs.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process recovery method reserved for the background scheduler
	// and must not be called from user-facing request handlers.
	ClearClaimWarning(ctx context.Context, id TaskInstanceID, expiresAt time.Time) error

	// ListTradeableAssignedToOthers returns every instance that satisfies
	// [IsInstanceTradeable] AND is assigned to a member OTHER than
	// excludeMemberID (NES-122). It is the picker query behind "propose a
	// trade": given the member proposing a trade, it lists every chore a
	// sibling currently holds that could be requested in exchange. The
	// predicate mirrors IsInstanceTradeable exactly (pending, kind=scheduled,
	// unclaimed, has a due date) plus the assignee exclusion — it does not
	// duplicate IsInstanceTradeable's Go-level check, which callers still run
	// against the OFFERED side once a candidate is chosen.
	// Ordered by due_on so the nearest-due chores surface first. Returns an
	// empty slice (not an error) when none match.
	ListTradeableAssignedToOthers(ctx context.Context, householdID household.HouseholdID, excludeMemberID household.MemberID) ([]*TaskInstance, error)
}

// ChoreTradeRepository is the persistence port for [ChoreTrade] aggregates —
// the 1-for-1 chore trade propose/accept workflow (NES-121). Implementations
// live in the adapter layer and are injected into [TradeService] (application
// layer) and the background scheduler.
//
// Within this repository's own methods, the two task instances a trade
// references are never mutated by anything OTHER than Accept (a swap):
// Propose, Decline, Cancel, and SweepExpiredTrades read instance state but
// only ever write the chore_trade row itself. This guarantee is scoped to
// ChoreTradeRepository alone — [TaskInstanceRepository]'s own methods
// (Complete, Skip, Claim) and the scheduler's other sweeps can and do mutate
// a referenced instance independently at any time; that external mutation is
// exactly the race Accept's re-validation and its ErrInstanceNotTradeable
// outcome exist to catch.
//
// Lock-ordering convention (to avoid a Propose/Accept deadlock): Propose
// acquires row locks on the two referenced task_instance rows only (ordered
// by id); it never takes an explicit lock on any chore_trade row — the
// chore_trade_reservation table's PRIMARY KEY on instance_id (maintained by a
// trigger on chore_trade, see the migration) is the sole, schema-enforced
// mechanism preventing a duplicate live proposal — in either role, offered or
// requested — so no additional locking is needed for correctness, and it
// holds for every writer, not just this repository. Accept, in the opposite
// order, locks its own chore_trade row first (via its own UPDATE) and then
// the two task_instance rows (via the swap). Because Propose never holds a
// task_instance lock while ALSO waiting to acquire a chore_trade lock, and
// Accept never holds a chore_trade lock while waiting on a task_instance lock
// held by a third party other than another Accept, this asymmetric ordering
// cannot form a reverse-order lock cycle between a concurrent Propose and
// Accept — this holds even though Propose's insert and Accept's update each
// drive the reservation trigger, because the trigger's own reservation-row
// change is always committed atomically together with the very
// chore_trade.status change hasLiveTradeProposal already reads (see that
// function's doc for the full argument). Every method that begins a
// transaction and acquires more than one lock must preserve this invariant —
// see TestTrade_ProposeVsAccept_NoDeadlock in the adapter's test suite.
//
// All ID-based methods except SweepExpiredTrades are tenant-scoped: they take
// the household id as a leading argument and enforce household isolation, so
// a ChoreTradeID belonging to a different household is treated as unknown.
//
// Error contracts:
//   - Propose returns [ErrInstanceNotFound] when either instance id is
//     unknown or belongs to another household.
//   - Propose returns [ErrInstanceNotTradeable] when either instance fails
//     [IsInstanceTradeable], or when either instance already carries a live
//     trade proposal.
//   - Propose returns [ErrNotYourChore] when the offered instance is not
//     assigned to trade.ProposerID, or the requested instance is not
//     assigned to trade.ResponderID.
//   - Get returns [ErrTradeNotFound] when id is unknown or belongs to
//     another household.
//   - Accept, Decline, and Cancel return [ErrTradeNotPending] when the trade
//     is unknown, belongs to another household, addresses the wrong member
//     (the caller is not the responder for Accept/Decline, or not the
//     proposer for Cancel), has already resolved, or — Accept only — its
//     expiry has already passed as of the accept instant even though the
//     background sweep has not yet run. These causes are deliberately not
//     distinguished — see [ErrTradeNotPending]'s doc.
//   - Accept additionally returns [ErrInstanceNotTradeable] when, at accept
//     time, either instance is no longer pending/scheduled/unclaimed, or is
//     no longer assigned to the party that held it at propose time (e.g. the
//     instance was completed, skipped, reassigned, or claimed after the
//     trade was proposed). The trade's status transition and the instance
//     swap happen in ONE transaction: if the re-validation fails, the trade
//     row's status is rolled back to proposed too — Accept is all-or-nothing.
type ChoreTradeRepository interface {
	// Propose validates and persists a new trade proposal atomically. Callers
	// (via [TradeService.Propose]) must populate trade.ID, trade.ProposerID,
	// trade.ResponderID, trade.OfferedInstanceID, and
	// trade.RequestedInstanceID; the store populates trade.HouseholdID,
	// trade.Status ([TradeProposed]), trade.CreatedAt, and trade.ExpiresAt
	// (the earlier of the two instances' due dates — both are guaranteed
	// non-nil once tradeability is confirmed, since [IsInstanceTradeable]
	// requires [KindScheduled]). Returns a [ProposedTrade] describing the new
	// proposal (NES-122) so the caller can notify the responder without an
	// additional query, mirroring [Accept]'s [AcceptedTrade] return.
	Propose(ctx context.Context, householdID household.HouseholdID, trade *ChoreTrade) (ProposedTrade, error)

	// Get returns the trade identified by id within the household.
	Get(ctx context.Context, householdID household.HouseholdID, id ChoreTradeID) (*ChoreTrade, error)

	// Accept atomically re-validates both instances, swaps their AssigneeID
	// (offered → responderID, requested → proposerID), and marks the trade
	// accepted with resolved_at = at, all in one transaction. at is also the
	// instant Accept's deadline check is evaluated against: the UPDATE's
	// WHERE clause requires expires_at > at, so an accept attempted at or
	// after the trade's deadline atomically fails with [ErrTradeNotPending]
	// — Accept never has to wait for [SweepExpiredTrades] to run first, and
	// the two predicates (expires_at > at here, expires_at <= asOf there) are
	// exact complements so no instant is handled by neither or both. Returns
	// an [AcceptedTrade] describing the resolution so the caller can notify
	// both parties without an additional query.
	Accept(ctx context.Context, householdID household.HouseholdID, id ChoreTradeID, responderID household.MemberID, at time.Time) (AcceptedTrade, error)

	// Decline transitions the trade from proposed to declined. No instance
	// assignment is touched. Returns a [DeclinedTrade] describing the
	// resolution (NES-122) so the caller can notify the proposer without an
	// additional query, mirroring [Accept]'s [AcceptedTrade] return.
	Decline(ctx context.Context, householdID household.HouseholdID, id ChoreTradeID, responderID household.MemberID) (DeclinedTrade, error)

	// Cancel transitions the trade from proposed to cancelled. No instance
	// assignment is touched.
	Cancel(ctx context.Context, householdID household.HouseholdID, id ChoreTradeID, proposerID household.MemberID) error

	// SweepExpiredTrades atomically transitions every trade whose expires_at
	// is at or before asOf and whose status is still proposed to expired,
	// recording resolved_at. No instance assignment is touched. Returns the
	// expired trades as [ExpiredTrade] values so the caller can enqueue a
	// notification to each proposer without an additional query. Returns an
	// empty slice (not an error) when no trades are expired as of asOf.
	//
	// WARNING: this method is intentionally NOT household-scoped. It is a
	// system-process method reserved for the background scheduler and must
	// not be called from user-facing request handlers, matching the
	// precedent set by [TaskInstanceRepository.SweepExpiredClaims].
	SweepExpiredTrades(ctx context.Context, asOf time.Time) ([]ExpiredTrade, error)

	// ListPendingByMember returns every live (status = [TradeProposed]) trade
	// within the household where memberID is either the proposer or the
	// responder (NES-122), as [TradeSummary] values pre-joined with both
	// referenced chores' titles/points — the read behind the dashboard's
	// trade cards. The caller distinguishes "awaiting your decision" from
	// "your own outgoing proposal" by comparing TradeSummary.ResponderID and
	// TradeSummary.ProposerID against memberID — this method does not split
	// them, since a member could in principle be involved in both roles
	// across different trades and the caller already needs the comparison to
	// render the right actions (Accept/Decline vs Cancel). Ordered by
	// created_at ascending (oldest proposal first). Returns an empty slice
	// (not an error) when memberID has no live trades.
	ListPendingByMember(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID) ([]TradeSummary, error)

	// ListHistory returns the household's most recent trades, regardless of
	// status, as [TradeSummary] values pre-joined the same way
	// ListPendingByMember's are — the read behind the parent-only
	// trade-history page (NES-122). Ordered by created_at descending (most
	// recent first) and capped at [TradeHistoryLimit] rows: this is a
	// recent-activity view, not an unbounded audit log. Access control
	// (parents only) is enforced by the caller, not this method: tenant
	// scoping to householdID is this repository's only concern, matching
	// every other List* method in this package. Returns an empty slice (not
	// an error) when the household has no trades.
	ListHistory(ctx context.Context, householdID household.HouseholdID) ([]TradeSummary, error)
}
