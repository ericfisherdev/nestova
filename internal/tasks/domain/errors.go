package domain

import "errors"

// Domain errors for the tasks bounded context.
var (
	// ErrTaskNotFound is returned by RecurringTaskRepository.Get when the
	// requested RecurringTaskID does not exist.
	ErrTaskNotFound = errors.New("tasks: recurring task not found")

	// ErrInstanceNotFound is returned by TaskInstanceRepository.Get when the
	// requested TaskInstanceID does not exist.
	ErrInstanceNotFound = errors.New("tasks: task instance not found")

	// ErrNoRotationMembers is returned by the generator when a non-claimable
	// recurring task has an empty rotation pool (i.e. SetRotationMembers was
	// never called, or the pool was cleared). A task with RotationFixed or
	// RotationRoundRobin must have at least one member in its pool before
	// instances can be materialised.
	ErrNoRotationMembers = errors.New("tasks: rotation pool is empty")

	// ErrInstanceInTerminalState is returned by TaskInstanceRepository.Complete,
	// TaskInstanceRepository.Skip, and TaskInstanceRepository.Claim when the
	// target instance is already in a terminal state (done or skipped). As of
	// NES-32, overdue is NOT terminal: an overdue chore is still actionable (it
	// can be completed, skipped, or claimed late). Callers should check this
	// sentinel and treat it as a no-op or surface it to the user accordingly.
	ErrInstanceInTerminalState = errors.New("tasks: instance is already in a terminal state")

	// ErrInstanceAlreadyClaimed is returned by TaskInstanceRepository.Claim when
	// the target instance is already assigned to a member. Claiming is
	// first-come, not reassignment: an already-assigned instance cannot be
	// re-claimed by another member.
	ErrInstanceAlreadyClaimed = errors.New("tasks: instance is already claimed")

	// ErrDuplicateInstance is returned by TaskInstanceRepository.Insert when an
	// instance already exists for the (recurring_task_id, due_on) pair. The
	// generator uses this sentinel to implement idempotent materialisation:
	// re-running the generator for an already-materialised window is safe.
	ErrDuplicateInstance = errors.New("tasks: instance already exists for this task and due date")

	// ErrAsNeededRequiresClaimable is returned by TaskService.CreateRecurringTask
	// when task.Cadence.Freq is household.FreqAsNeeded and task.RotationPolicy is
	// anything other than RotationClaimable. An as-needed task's single standing
	// instance is always unassigned until claimed, so a fixed or round-robin
	// rotation policy would never be honoured (NES-116).
	ErrAsNeededRequiresClaimable = errors.New("tasks: as-needed cadence requires the claimable rotation policy")

	// ErrTradeNotPending is returned by ChoreTradeRepository.Accept, Decline,
	// and Cancel (NES-121) when the target trade is unknown, belongs to
	// another household, is not addressed to/from the calling member (wrong
	// responder for Accept/Decline, or wrong proposer for Cancel), has
	// already resolved (accepted, declined, cancelled, or expired), OR — for
	// Accept specifically — its expiry has already passed even though the
	// background sweep has not yet flipped its status to expired. Accept
	// enforces the deadline synchronously (expires_at > the accept instant)
	// rather than waiting for the sweep, so a trade can never be accepted
	// exactly the moment SweepExpiredTrades would otherwise claim it. These
	// causes are deliberately not distinguished: the caller only needs to
	// know whether the action succeeded within an open, caller-addressed,
	// still-live proposal, which after any of these it no longer is —
	// mirroring how ErrInstanceAlreadyClaimed does not distinguish "unknown"
	// from "assigned to someone else" beyond what disambiguateClaim needs.
	ErrTradeNotPending = errors.New("tasks: trade is not in the proposed state")

	// ErrTradeSelf is returned by TradeService.Propose (NES-121) when the
	// proposer and responder are the same member — a member cannot propose a
	// trade with themselves.
	ErrTradeSelf = errors.New("tasks: cannot propose a trade with yourself")

	// ErrInstanceNotTradeable is returned by ChoreTradeRepository.Propose and
	// Accept (NES-121). It covers every case in IsInstanceTradeable's
	// contract (the instance is not pending, not a scheduled instance, or
	// currently claimed) AND the case where the instance already carries a
	// live (status = 'proposed') trade proposal — enforced by the
	// chore_trade_offered_live_uniq / chore_trade_requested_live_uniq
	// partial unique indexes and re-checked defensively before insert. Also
	// returned by Accept when re-validation at accept time finds either
	// instance no longer in the state (or held by the party) it was in at
	// propose time.
	ErrInstanceNotTradeable = errors.New("tasks: instance is not tradeable")

	// ErrNotYourChore is returned by ChoreTradeRepository.Propose (NES-121)
	// when the offered instance is not assigned to the proposer, or the
	// requested instance is not assigned to the responder — a trade can only
	// be proposed over chores the two named parties actually hold.
	ErrNotYourChore = errors.New("tasks: instance is not assigned to the expected member")

	// ErrTradeNotFound is returned by ChoreTradeRepository.Get (NES-121) when
	// the requested ChoreTradeID does not exist or belongs to another
	// household. Accept, Decline, and Cancel use ErrTradeNotPending instead
	// (see its doc) — this sentinel is scoped to the read-only Get lookup.
	ErrTradeNotFound = errors.New("tasks: trade not found")

	// ErrBeforePhotoRequired is returned by TaskService.CompleteInstance
	// (NES-120) when the instance's recurring task's PhotoPolicy is
	// PhotoPolicyBeforeAfter and no "before" chore-proof photo (NES-119) has
	// been captured for the instance yet. Checked before
	// ErrAfterPhotoRequired, so a before_after task missing both photos
	// reports the "before" gap first — the order a member would naturally
	// resolve it in, since an "after" photo taken before any "before" photo
	// exists is itself rejected by media's own ErrAfterPrecedesBefore
	// ordering rule.
	ErrBeforePhotoRequired = errors.New("tasks: a before photo is required to complete this instance")

	// ErrAfterPhotoRequired is returned by TaskService.CompleteInstance
	// (NES-120) when the instance's recurring task's PhotoPolicy is
	// PhotoPolicyAfterOnly or PhotoPolicyBeforeAfter and no "after"
	// chore-proof photo (NES-119) has been captured for the instance yet.
	ErrAfterPhotoRequired = errors.New("tasks: an after photo is required to complete this instance")
)
