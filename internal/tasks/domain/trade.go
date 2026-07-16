package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ---------------------------------------------------------------------------
// ChoreTradeID
// ---------------------------------------------------------------------------

// ChoreTradeID uniquely identifies a chore trade proposal (NES-121).
type ChoreTradeID uuid.UUID

// NewChoreTradeID returns a new time-ordered (UUIDv7) chore trade id, which
// gives better B-tree index locality than random v4 ids. uuid.NewV7 only
// errors if the crypto random source is unavailable — the same failure under
// which uuid.New itself panics — so Must is appropriate here.
func NewChoreTradeID() ChoreTradeID { return ChoreTradeID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id ChoreTradeID) String() string { return uuid.UUID(id).String() }

// ParseChoreTradeID parses a canonical UUID string into a ChoreTradeID.
func ParseChoreTradeID(s string) (ChoreTradeID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ChoreTradeID{}, fmt.Errorf("parse chore trade id: %w", err)
	}
	return ChoreTradeID(u), nil
}

// ---------------------------------------------------------------------------
// TradeStatus
// ---------------------------------------------------------------------------

// TradeStatus is the lifecycle state of a chore trade proposal. Stored as
// text, validated here. The values match the chore_trade.status CHECK
// constraint (NES-121).
type TradeStatus string

// Chore trade lifecycle statuses.
const (
	// TradeProposed marks a live proposal awaiting the responder's decision.
	// It is the only non-terminal status — see [TradeStatus.CanTransitionTo].
	TradeProposed TradeStatus = "proposed"
	// TradeAccepted marks a proposal the responder accepted; the offered and
	// requested instances' assignees have been swapped.
	TradeAccepted TradeStatus = "accepted"
	// TradeDeclined marks a proposal the responder explicitly declined.
	TradeDeclined TradeStatus = "declined"
	// TradeCancelled marks a proposal the proposer withdrew before the
	// responder acted on it.
	TradeCancelled TradeStatus = "cancelled"
	// TradeExpired marks a proposal the background sweep resolved because
	// neither party acted before [ChoreTrade.ExpiresAt].
	TradeExpired TradeStatus = "expired"
)

// Valid reports whether s is a known trade status.
func (s TradeStatus) Valid() bool {
	switch s {
	case TradeProposed, TradeAccepted, TradeDeclined, TradeCancelled, TradeExpired:
		return true
	default:
		return false
	}
}

// String returns the status's stored value.
func (s TradeStatus) String() string { return string(s) }

// ParseTradeStatus validates and returns a TradeStatus, or an error for an
// unknown value.
func ParseTradeStatus(s string) (TradeStatus, error) {
	st := TradeStatus(s)
	if !st.Valid() {
		return "", fmt.Errorf("invalid trade status %q", s)
	}
	return st, nil
}

// CanTransitionTo reports whether moving from s to next is a legal
// state-machine transition (NES-121). [TradeProposed] is the only
// non-terminal status: it may resolve to accepted, declined, cancelled, or
// expired. Every other status is terminal — once a trade has left proposed,
// no further transition is legal, matching [ChoreTradeRepository.Accept],
// [ChoreTradeRepository.Decline], [ChoreTradeRepository.Cancel], and
// [ChoreTradeRepository.SweepExpiredTrades], each of which guards its
// UPDATE with status = 'proposed' and returns [ErrTradeNotPending] otherwise.
func (s TradeStatus) CanTransitionTo(next TradeStatus) bool {
	if s != TradeProposed {
		return false
	}
	switch next {
	case TradeAccepted, TradeDeclined, TradeCancelled, TradeExpired:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// ChoreTrade
// ---------------------------------------------------------------------------

// ChoreTrade is a 1-for-1 proposal to swap two pending chores between two
// household members: ProposerID offers OfferedInstanceID (a chore currently
// assigned to them) in exchange for ResponderID's RequestedInstanceID. It is
// the aggregate root of the chore-trade sub-domain within the tasks bounded
// context (NES-121).
//
// Points follow the chore: accepting a trade never writes to the point
// ledger, and the rotation pool (rotation_member) is never modified — a trade
// only ever changes task_instance.assignee_id on the two referenced rows.
type ChoreTrade struct {
	ID          ChoreTradeID
	HouseholdID household.HouseholdID
	// ProposerID is the member who initiated the trade, offering
	// OfferedInstanceID.
	ProposerID household.MemberID
	// ResponderID is the member whose acceptance or decline resolves the
	// trade.
	ResponderID household.MemberID
	// OfferedInstanceID is the task instance ProposerID is offering to give
	// up. It must be assigned to ProposerID at propose time.
	OfferedInstanceID TaskInstanceID
	// RequestedInstanceID is the task instance ProposerID wants in exchange.
	// It must be assigned to ResponderID at propose time.
	RequestedInstanceID TaskInstanceID
	Status              TradeStatus
	CreatedAt           time.Time
	// ResolvedAt is set the moment Status leaves [TradeProposed] (accept,
	// decline, cancel, or expiry) and nil while the trade is still live.
	ResolvedAt *time.Time
	// ExpiresAt is the earlier of the two instances' due dates, computed once
	// at propose time. The background sweep transitions a still-[TradeProposed]
	// trade to [TradeExpired] once asOf reaches this instant.
	ExpiresAt time.Time
}

// IsInstanceTradeable reports whether inst is eligible to participate in a
// chore trade, on either the offered or requested side (NES-121). inst == nil
// is not tradeable. A non-nil tradeable instance must be:
//   - status = [StatusPending] — unlike Claim/Complete/Skip, which still act
//     on an overdue instance, a trade cannot outlive either side's due date:
//     ExpiresAt is fixed to the earlier of the two due dates at propose time,
//     so the background sweep always resolves a live trade to expired at or
//     before either instance would otherwise go overdue under normal
//     scheduler operation. Accept therefore never needs to tolerate overdue.
//   - kind = [KindStanding] is NOT tradeable: a standing instance (NES-116)
//     has no fixed assignee to call "your chore" while unclaimed, and no due
//     date to bound a trade's expiry.
//   - ClaimedBy is nil — a claimed instance (NES-117) already carries its own
//     claim-expiry risk window and penalty; trading it would let a member
//     hand off that risk to someone who never agreed to it.
//   - DueOn is non-nil — kind = [KindScheduled] is supposed to guarantee this
//     (see validateInstanceKindDueOn's insert-time invariant in the adapter),
//     but this check is defensive: a caller composing a [TaskInstance] by hand
//     (e.g. a test fixture) could otherwise slip a nil DueOn past the kind
//     check and crash [ChoreTradeRepository.Propose]'s expires_at computation,
//     which dereferences DueOn on both instances once IsInstanceTradeable
//     passes for each.
//
// It does not check assignee ownership; see [ChoreTradeRepository.Propose]'s
// contract for the separate [ErrNotYourChore] check.
func IsInstanceTradeable(inst *TaskInstance) bool {
	return inst != nil &&
		inst.Status == StatusPending &&
		inst.Kind == KindScheduled &&
		inst.ClaimedBy == nil &&
		inst.DueOn != nil
}

// ---------------------------------------------------------------------------
// Scheduler/notification payloads
// ---------------------------------------------------------------------------

// AcceptedTrade carries the fields needed to build and route the pair of
// trade-accepted notifications (one to the proposer, one to the responder)
// after [ChoreTradeRepository.Accept] atomically swaps the two instances'
// assignees and marks the trade accepted (NES-121).
type AcceptedTrade struct {
	TradeID     ChoreTradeID
	HouseholdID household.HouseholdID
	ProposerID  household.MemberID
	ResponderID household.MemberID
	// OfferedTitle is the recurring_task.title of the instance that moved
	// from ProposerID to ResponderID.
	OfferedTitle string
	// RequestedTitle is the recurring_task.title of the instance that moved
	// from ResponderID to ProposerID.
	RequestedTitle string
}

// ExpiredTrade carries the fields needed to build and route a trade-expiry
// notification to the proposer (NES-121). It reflects the trade's state
// immediately before [ChoreTradeRepository.SweepExpiredTrades] transitioned it
// to [TradeExpired]. No assignment change ever accompanies an expiry — the two
// referenced instances keep whatever assignee they already had.
type ExpiredTrade struct {
	TradeID        ChoreTradeID
	HouseholdID    household.HouseholdID
	ProposerID     household.MemberID
	OfferedTitle   string
	RequestedTitle string
}

// ProposedTrade carries the fields needed to build and route the
// trade-proposal-received notification to the responder immediately after
// [ChoreTradeRepository.Propose] persists a new trade (NES-122), mirroring
// [AcceptedTrade] and [ExpiredTrade]'s shape: the adapter resolves both
// instances' titles within the same transaction as the insert, so the app
// layer never needs its own [TaskInstanceRepository]/[RecurringTaskRepository]
// dependency just to describe the proposal in a notification body.
type ProposedTrade struct {
	TradeID        ChoreTradeID
	HouseholdID    household.HouseholdID
	ProposerID     household.MemberID
	ResponderID    household.MemberID
	OfferedTitle   string
	RequestedTitle string
}

// DeclinedTrade carries the fields needed to build and route the
// trade-declined notification to the proposer after
// [ChoreTradeRepository.Decline] resolves a trade against acceptance
// (NES-122), mirroring [AcceptedTrade]'s shape.
type DeclinedTrade struct {
	TradeID        ChoreTradeID
	HouseholdID    household.HouseholdID
	ProposerID     household.MemberID
	OfferedTitle   string
	RequestedTitle string
}

// ---------------------------------------------------------------------------
// Read-model projections
// ---------------------------------------------------------------------------

// TradeHistoryLimit caps how many rows [ChoreTradeRepository.ListHistory]
// returns, newest first (NES-122). The parent-only trade-history page is a
// recent-activity view, not an unbounded audit log: without a cap, a
// household's full trade history would grow the query (and the page) without
// bound for as long as the household keeps trading chores.
const TradeHistoryLimit = 50

// TradeSummary is a read-only, denormalized view of a chore_trade row joined
// with both referenced instances' recurring-task titles and point values
// (NES-122). It is what [ChoreTradeRepository.ListPendingByMember] and
// [ChoreTradeRepository.ListHistory] return, so a caller can render a trade
// card or history row directly, without resolving each side's title/points
// via its own [TaskInstanceRepository.Get] + [RecurringTaskRepository.Get]
// pair per trade — the N+1 shape those two methods originally required of
// their callers before this type replaced it.
//
// OfferedTitle/RequestedTitle render "(archived)" (with the corresponding
// Points left at 0) when the parent recurring task is inactive, mirroring
// WebHandlers.buildInstanceRow's precedent for the live task list — an
// inactive task's title is not shown as if it were still a going concern.
// Member display names are deliberately NOT included here: unlike titles
// (which require the tasks bounded context's own task_instance/
// recurring_task join), a member's display name is already resolved in a
// single household.HouseholdRepository.ListMembers call by every caller of
// this type, so joining it here would duplicate a lookup that is already
// O(1) rather than eliminate a real N+1.
type TradeSummary struct {
	TradeID         ChoreTradeID
	HouseholdID     household.HouseholdID
	ProposerID      household.MemberID
	ResponderID     household.MemberID
	OfferedTitle    string
	OfferedPoints   int
	RequestedTitle  string
	RequestedPoints int
	Status          TradeStatus
	CreatedAt       time.Time
	ResolvedAt      *time.Time
}
