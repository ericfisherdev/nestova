package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ---------------------------------------------------------------------------
// IDs
// ---------------------------------------------------------------------------

// PointEntryID uniquely identifies a point ledger entry.
type PointEntryID uuid.UUID

// RewardID uniquely identifies a reward in the catalogue.
type RewardID uuid.UUID

// RewardRedemptionID uniquely identifies a reward redemption.
type RewardRedemptionID uuid.UUID

// NewPointEntryID returns a new time-ordered (UUIDv7) point entry id, which
// gives better B-tree index locality than random v4 ids. uuid.NewV7 only
// errors if the crypto random source is unavailable — the same failure under
// which uuid.New itself panics — so Must is appropriate here.
func NewPointEntryID() PointEntryID { return PointEntryID(uuid.Must(uuid.NewV7())) }

// NewRewardID returns a new time-ordered (UUIDv7) reward id.
func NewRewardID() RewardID { return RewardID(uuid.Must(uuid.NewV7())) }

// NewRewardRedemptionID returns a new time-ordered (UUIDv7) reward redemption id.
func NewRewardRedemptionID() RewardRedemptionID {
	return RewardRedemptionID(uuid.Must(uuid.NewV7()))
}

// String returns the canonical UUID string.
func (id PointEntryID) String() string { return uuid.UUID(id).String() }

// String returns the canonical UUID string.
func (id RewardID) String() string { return uuid.UUID(id).String() }

// String returns the canonical UUID string.
func (id RewardRedemptionID) String() string { return uuid.UUID(id).String() }

// ParsePointEntryID parses a canonical UUID string into a PointEntryID.
func ParsePointEntryID(s string) (PointEntryID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return PointEntryID{}, fmt.Errorf("parse point entry id: %w", err)
	}
	return PointEntryID(u), nil
}

// ParseRewardID parses a canonical UUID string into a RewardID.
func ParseRewardID(s string) (RewardID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return RewardID{}, fmt.Errorf("parse reward id: %w", err)
	}
	return RewardID(u), nil
}

// ParseRewardRedemptionID parses a canonical UUID string into a RewardRedemptionID.
func ParseRewardRedemptionID(s string) (RewardRedemptionID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return RewardRedemptionID{}, fmt.Errorf("parse reward redemption id: %w", err)
	}
	return RewardRedemptionID(u), nil
}

// ---------------------------------------------------------------------------
// Enum: RedemptionStatus
// ---------------------------------------------------------------------------

// RedemptionStatus is the lifecycle state of a reward redemption. Stored as
// text, validated here. The values match the reward_redemption.status CHECK
// constraint.
type RedemptionStatus string

// Reward redemption lifecycle statuses.
const (
	// RedemptionRequested marks a redemption that has been submitted by a member
	// but not yet acted on by a household admin.
	RedemptionRequested RedemptionStatus = "requested"
	// RedemptionFulfilled marks a redemption that has been approved and delivered
	// by a household admin.
	RedemptionFulfilled RedemptionStatus = "fulfilled"
	// RedemptionCancelled marks a redemption that was cancelled before fulfilment,
	// either by the member or an admin.
	RedemptionCancelled RedemptionStatus = "cancelled"
)

// Valid reports whether s is a known redemption status.
func (s RedemptionStatus) Valid() bool {
	switch s {
	case RedemptionRequested, RedemptionFulfilled, RedemptionCancelled:
		return true
	default:
		return false
	}
}

// String returns the redemption status's stored value.
func (s RedemptionStatus) String() string { return string(s) }

// ParseRedemptionStatus validates and returns a RedemptionStatus, or an error
// for an unknown value.
func ParseRedemptionStatus(s string) (RedemptionStatus, error) {
	st := RedemptionStatus(s)
	if !st.Valid() {
		return "", fmt.Errorf("invalid redemption status %q", s)
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Entities
// ---------------------------------------------------------------------------

// PointEntry is a single row in the append-only point ledger. Points may be
// positive (task-completion award) or negative (redemption debit). SourceID is
// nil for manual point adjustments that have no associated source row.
type PointEntry struct {
	ID          PointEntryID
	HouseholdID household.HouseholdID
	MemberID    household.MemberID
	// SourceType identifies the kind of event that produced this entry, e.g.
	// "task_instance" for a task-completion award or "redemption" for a debit.
	SourceType string
	// SourceID is the id of the originating row. It is nil for manual adjustments.
	SourceID  *uuid.UUID
	Points    int
	CreatedAt time.Time
}

// Reward is a redeemable item in the household's reward catalogue. A reward
// with Active=false is retired: existing redemptions remain, but no new ones
// can be created against it.
type Reward struct {
	ID          RewardID
	HouseholdID household.HouseholdID
	Name        string
	CostPoints  int
	Active      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RewardRedemption records a member's request to exchange points for a reward.
// The corresponding point debit is a separate [PointEntry] appended by the
// NES-36 use-case in the same transaction.
type RewardRedemption struct {
	ID          RewardRedemptionID
	HouseholdID household.HouseholdID
	RewardID    RewardID
	MemberID    household.MemberID
	Status      RedemptionStatus
	// CreatedAt is when the redemption was requested; UpdatedAt records the most
	// recent status transition (fulfilled/cancelled) for the audit trail.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

// Domain errors for the gamification sub-domain (tasks bounded context).
var (
	// ErrRewardNotFound is returned by RewardRepository.GetReward when the
	// requested RewardID does not exist within the household.
	ErrRewardNotFound = errors.New("tasks: reward not found")

	// ErrInsufficientPoints is returned by the redeem use-case (NES-36) when a
	// member's current point balance is less than the reward's CostPoints.
	ErrInsufficientPoints = errors.New("tasks: insufficient points to redeem reward")

	// ErrDuplicatePointEntry is returned by PointLedgerRepository.Append when the
	// INSERT violates the point_ledger_task_completion_uniq partial unique index,
	// i.e. a task-completion award for the same task_instance.id already exists.
	// The NES-36 award-on-completion path uses this sentinel to implement
	// idempotent awarding: a duplicate is treated as a no-op.
	ErrDuplicatePointEntry = errors.New("tasks: point entry already exists for this task instance")
)

// ---------------------------------------------------------------------------
// Port types
// ---------------------------------------------------------------------------

// MemberPoints carries a member's aggregated point total for leaderboard use.
type MemberPoints struct {
	MemberID household.MemberID
	Points   int
}

// PointHistoryEntry is a single row in a member's point ledger history,
// enriched with enough context to build a human-readable reason without a
// second round trip per entry (NES-118):
//   - TaskTitle is populated for a SourceType of "task_instance" (a
//     completion award) or [SourceTypeClaimExpiry] (a claim-expiry penalty) —
//     both key SourceID off task_instance.id.
//   - RewardName is populated for a SourceType of "redemption" — keyed off
//     reward_redemption.id via SourceID.
//
// At most one of TaskTitle/RewardName is ever non-empty for a given entry.
// Both are empty for a manual adjustment (no SourceID) or when the joined
// row could not be resolved (e.g. its parent recurring task was deleted).
type PointHistoryEntry struct {
	ID         PointEntryID
	SourceType string
	Points     int
	CreatedAt  time.Time
	TaskTitle  string
	RewardName string
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

// PointLedgerRepository is the persistence port for [PointEntry] rows.
// Implementations live in the adapter layer (NES-36) and are injected into
// application services.
//
// All methods are tenant-scoped via household_id: a member or entry that
// belongs to a different household is treated as unknown.
//
// Error contracts:
//   - Append returns [ErrDuplicatePointEntry] when the INSERT would violate
//     point_ledger_task_completion_uniq (source_type='task_instance' and
//     source_id already present in the ledger).
//   - Balance returns 0, nil when no entries exist for the member (not an
//     error: a member with no history simply has a zero balance).
//   - Leaderboard returns an empty slice (not an error) when no entries exist
//     for the household since the given time.
type PointLedgerRepository interface {
	// Append inserts a new point entry into the ledger.
	// Returns [ErrDuplicatePointEntry] on the task-completion partial unique
	// index violation (source_type='task_instance', duplicate source_id).
	Append(ctx context.Context, entry *PointEntry) error

	// Balance returns the sum of all points for the member within the household.
	// Returns 0, nil when no entries exist.
	Balance(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID) (int, error)

	// Leaderboard returns per-member point totals for the household, summing
	// only entries created at or after since, ordered by total points descending.
	// Returns an empty slice (not an error) when no entries match.
	Leaderboard(ctx context.Context, householdID household.HouseholdID, since time.Time) ([]MemberPoints, error)

	// History returns the member's most recent point ledger entries within
	// the household, newest first, limited to at most limit rows (NES-118).
	// Each entry is enriched (see [PointHistoryEntry]) so the caller can build
	// a human-readable reason without an additional query per entry.
	// Returns an empty slice (not an error) when the member has no entries.
	History(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, limit int) ([]PointHistoryEntry, error)
}

// RewardRepository is the persistence port for [Reward] catalogue entries and
// [RewardRedemption] records. Implementations live in the adapter layer
// (NES-36) and are injected into application services.
//
// All methods are tenant-scoped via household_id: a reward or redemption that
// belongs to a different household is treated as unknown.
//
// Error contracts:
//   - CreateReward expects r.ID, r.HouseholdID, r.Name, r.CostPoints, and
//     r.Active set; the store populates CreatedAt and UpdatedAt.
//   - GetReward returns [ErrRewardNotFound] when id is unknown or belongs to
//     another household.
//   - ListActiveRewards returns an empty slice (not an error) when the
//     household has no active rewards.
//   - Redeem expects redemption.ID, redemption.HouseholdID, redemption.RewardID,
//     redemption.MemberID, redemption.Status, redemption.CreatedAt, and
//     redemption.UpdatedAt set.
//     The NES-36 use-case appends the corresponding point debit via
//     [PointLedgerRepository.Append] in the same transaction; Redeem itself
//     only persists the redemption row.
//   - Redeem returns [ErrRewardNotFound] when redemption.RewardID is unknown or
//     belongs to another household (the composite FK on reward_redemption will
//     reject it).
type RewardRepository interface {
	// CreateReward persists a new reward in the household's catalogue.
	CreateReward(ctx context.Context, r *Reward) error

	// GetReward returns the reward with the given id within the household.
	// Returns [ErrRewardNotFound] when id is unknown or belongs to another household.
	GetReward(ctx context.Context, householdID household.HouseholdID, id RewardID) (*Reward, error)

	// ListActiveRewards returns all active rewards for the household.
	// Returns an empty slice (not an error) when none exist.
	ListActiveRewards(ctx context.Context, householdID household.HouseholdID) ([]*Reward, error)

	// Redeem persists a reward redemption row. The corresponding point debit is
	// appended separately by the use-case via [PointLedgerRepository.Append].
	// Returns [ErrRewardNotFound] when redemption.RewardID is unknown or belongs
	// to another household.
	Redeem(ctx context.Context, redemption *RewardRedemption) error
}
