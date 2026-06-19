package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// RewardRedeemer is the persistence port for atomic reward redemptions.
// It is satisfied by [adapter.RewardPostgresRepository] which adds
// RedeemWithDebit on top of the base [domain.RewardRepository] interface.
//
// Keeping this as a minimal, focused interface (ISP) means the service only
// depends on the one method it actually calls, rather than all of
// [domain.RewardRepository].
type RewardRedeemer interface {
	// GetReward returns the reward identified by id within the household,
	// regardless of its active flag. Returns [domain.ErrRewardNotFound] only when
	// the reward is unknown or belongs to another household. Inactive rewards are
	// returned as-is; rejecting a retired reward is the service's responsibility
	// (see [RewardService.Redeem]'s active guard), not the repository's.
	GetReward(ctx context.Context, householdID household.HouseholdID, id domain.RewardID) (*domain.Reward, error)

	// RedeemWithDebit atomically inserts a reward_redemption row and a negative
	// point_ledger entry, serialised by an advisory lock so concurrent redeems
	// cannot race on the balance check.
	//
	// Returns [domain.ErrInsufficientPoints] when the member's balance is below
	// costPoints.  Returns [domain.ErrRewardNotFound] when the reward FK is
	// violated.
	RedeemWithDebit(ctx context.Context, redemption *domain.RewardRedemption, costPoints int) error
}

// RewardService orchestrates the reward-redemption use case.  It validates
// that the reward exists and is active, builds the [domain.RewardRedemption]
// entity, and delegates the atomic debit + insert to [RewardRedeemer].
//
// Dependencies are injected via the constructor so the service is testable with
// fakes (hermetic tests) and wired to Postgres at the composition root.
type RewardService struct {
	repo   RewardRedeemer
	logger *slog.Logger
}

// NewRewardService constructs a RewardService with the injected dependencies.
// Panics if any dependency is nil so misconfigured composition roots fail at
// startup rather than at the first HTTP request.
func NewRewardService(repo RewardRedeemer, logger *slog.Logger) *RewardService {
	if repo == nil {
		panic("app: NewRewardService requires a non-nil RewardRedeemer")
	}
	if logger == nil {
		panic("app: NewRewardService requires a non-nil logger")
	}
	return &RewardService{repo: repo, logger: logger}
}

// Redeem exchanges a member's points for a reward. It:
//  1. Fetches the reward — returns [domain.ErrRewardNotFound] if missing or
//     belongs to another household.
//  2. Guards that the reward is active — inactive rewards are treated as
//     [domain.ErrRewardNotFound] from the caller's perspective (a retired reward
//     can no longer be redeemed).
//  3. Builds a [domain.RewardRedemption] with status = 'requested'.
//  4. Calls [RewardRedeemer.RedeemWithDebit] — returns
//     [domain.ErrInsufficientPoints] when the balance is too low, or
//     [domain.ErrRewardNotFound] when the reward FK fails (race between fetch and
//     insert is possible in theory; the FK guards the invariant).
//
// Error contracts:
//   - Returns [domain.ErrRewardNotFound] when the reward is unknown, belongs to
//     another household, or is inactive.
//   - Returns [domain.ErrInsufficientPoints] when the member's point balance is
//     less than the reward's CostPoints.
//   - Propagates unexpected repository errors unchanged.
func (s *RewardService) Redeem(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	rewardID domain.RewardID,
) (*domain.RewardRedemption, error) {
	// Step 1: fetch and validate the reward.
	reward, err := s.repo.GetReward(ctx, householdID, rewardID)
	if err != nil {
		if errors.Is(err, domain.ErrRewardNotFound) {
			return nil, domain.ErrRewardNotFound
		}
		return nil, fmt.Errorf("redeem: get reward: %w", err)
	}

	// Step 2: inactive rewards cannot be redeemed.
	if !reward.Active {
		return nil, domain.ErrRewardNotFound
	}

	// Step 3: build the redemption entity.
	now := time.Now().UTC()
	redemption := &domain.RewardRedemption{
		ID:          domain.NewRewardRedemptionID(),
		HouseholdID: householdID,
		RewardID:    rewardID,
		MemberID:    memberID,
		Status:      domain.RedemptionRequested,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Step 4: atomic debit + insert.
	if err := s.repo.RedeemWithDebit(ctx, redemption, reward.CostPoints); err != nil {
		return nil, fmt.Errorf("redeem: %w", err)
	}

	s.logger.InfoContext(ctx, "reward redeemed",
		"household_id", householdID.String(),
		"member_id", memberID.String(),
		"reward_id", rewardID.String(),
		"redemption_id", redemption.ID.String(),
		"cost_points", reward.CostPoints,
	)
	return redemption, nil
}
