package domain

import (
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ClaimWindow is the duration after which a claim made on a previously-
// unassigned instance (claimable, or standing per NES-116) expires and
// incurs [ClaimExpiryPenalty] if the instance is not completed by then. A
// claim made on an instance already assigned to the claiming member (a
// self-claim) is not subject to this window at all — see
// [TaskInstanceRepository.Claim].
//
// The window is baked into TaskInstance.ClaimExpiresAt at claim time rather
// than re-derived when the sweep runs, so a future per-household override
// (NES-117 follow-up) would only affect claims made after the change, never
// retroactively shift an already-ticking claim.
const ClaimWindow = 12 * time.Hour

// SourceTypeClaimExpiry is the point_ledger source_type recorded for the
// penalty entry appended when a claim's window expires without the instance
// being completed (NES-117).
const SourceTypeClaimExpiry = "claim_expiry"

// ClaimExpiryPenalty returns the point penalty incurred when a claim on a
// task worth points points expires without completion: half of points,
// rounded down, with a floor of 1. The floor applies even to a zero- or
// one-point task, since the risk of claiming and abandoning a chore is not
// proportional to its award value. Penalties are never clamped by a member's
// balance — callers must apply the full, unconditional penalty and let
// balances go negative.
func ClaimExpiryPenalty(points int) int {
	if half := points / 2; half > 1 {
		return half
	}
	return 1
}

// ClaimWarningWindow is how far ahead of a claim's expiry the one-time
// "claim expiring soon" warning is emitted: [TaskInstanceRepository.
// ClaimWarnings] selects claims whose ClaimExpiresAt falls within this
// window of asOf and have not yet been warned, then marks them warned so
// the same claim window is never warned twice (NES-118). A claim entering
// this notice period gives the claimant a chance to complete it — or
// deliberately let it lapse — before [ClaimExpiryPenalty] is applied.
const ClaimWarningWindow = 2 * time.Hour

// ClaimWarning carries the fields needed to build and route a claim
// approaching-expiry warning notification (NES-118). It mirrors
// [ExpiredClaim] but is emitted earlier, while the claim is still active, and
// carries no penalty — the point loss has not happened yet.
type ClaimWarning struct {
	// InstanceID is the task_instance.id whose claim is approaching expiry.
	InstanceID TaskInstanceID
	// HouseholdID scopes the notification to the correct household.
	HouseholdID household.HouseholdID
	// ClaimedBy is the member who holds the claim and should be warned.
	ClaimedBy household.MemberID
	// Title is the recurring_task.title, used in the notification body.
	Title string
	// ExpiresAt is when the claim's window lapses, for the notification body.
	ExpiresAt time.Time
}

// ExpiredClaim carries the fields needed to build and route a claim-expiry
// notification. It reflects the state a task instance had immediately before
// its claim was reverted by [TaskInstanceRepository.SweepExpiredClaims]
// (NES-117).
type ExpiredClaim struct {
	// InstanceID is the task_instance.id whose claim expired.
	InstanceID TaskInstanceID
	// HouseholdID scopes the notification to the correct household.
	HouseholdID household.HouseholdID
	// RecurringTaskID is the parent recurring task, kept for callers that need
	// it beyond Title (e.g. future per-task claim-window overrides).
	RecurringTaskID RecurringTaskID
	// ClaimedBy is the member whose claim expired and who incurs the penalty.
	ClaimedBy household.MemberID
	// Title is the recurring_task.title, used in the notification body.
	Title string
	// PenaltyPoints is the positive point amount deducted from ClaimedBy's
	// balance (see [ClaimExpiryPenalty]). The point_ledger entry itself is
	// negative; this field is the human-readable magnitude for notification
	// text.
	PenaltyPoints int
}
