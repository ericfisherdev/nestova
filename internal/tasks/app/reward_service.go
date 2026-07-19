package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
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
	// returned as-is; rejecting a retired reward is RedeemWithDebit's
	// responsibility, not GetReward's — see [RewardService.Redeem]'s doc for why
	// this method's result is only ever used as an optimistic fast-fail, never as
	// the authoritative source for whether a redemption may proceed.
	GetReward(ctx context.Context, householdID household.HouseholdID, id domain.RewardID) (*domain.Reward, error)

	// RedeemWithDebit atomically inserts a reward_redemption row and a negative
	// point_ledger entry, serialised by an advisory lock so concurrent redeems
	// cannot race on the balance check. It reads and locks the reward's active
	// flag and cost_points ITSELF, inside the same transaction as the debit —
	// it never trusts a caller-supplied price or active flag, which could have
	// gone stale between GetReward's read and this call (NES-127: closes a
	// TOCTOU window where a reward could be archived, or repriced, in that
	// gap). The returned int is the cost actually debited, straight from the
	// row this method locked.
	//
	// Returns [domain.ErrRewardNotFound] when redemption.RewardID is unknown,
	// belongs to another household, or is archived (active = false).
	// Returns [domain.ErrInsufficientPoints] when the member's balance is below
	// the reward's current (locked) cost. Returns [domain.ErrRewardOutOfStock]
	// when the reward has a finite quantity_available and it has already been
	// reached (NES-127). Returns [domain.ErrDeepLinkAlreadyRedeemed] (NES-129)
	// when redemption.DeepLinkSignatureHash is non-nil and a redemption with
	// the same (household, hash) pair has already committed — enforced by a
	// database constraint, not an in-process guard, so it holds durably
	// across process restarts and multiple server instances.
	RedeemWithDebit(ctx context.Context, redemption *domain.RewardRedemption) (int, error)
}

// HouseholdMemberLister is the minimal capability RewardService needs to
// notify parents of a new redemption (NES-127): listing a household's
// members so they can be filtered to owner/adult roles. Narrower than
// [household.HouseholdRepository] (ISP) — Redeem needs no other household
// capability. [household.HouseholdRepository] satisfies this interface
// structurally, so the composition root passes it in directly with no
// adapter needed.
type HouseholdMemberLister interface {
	ListMembers(ctx context.Context, householdID household.HouseholdID) ([]*household.Member, error)
}

// RewardService orchestrates the reward-redemption use case.  It validates
// that the reward exists and is active, builds the [domain.RewardRedemption]
// entity, and delegates the atomic debit + insert to [RewardRedeemer]. On
// success it enqueues a best-effort notification to every parent (owner or
// adult) in the household (NES-127), mirroring TradeService's post-commit
// notification contract.
//
// Dependencies are injected via the constructor so the service is testable with
// fakes (hermetic tests) and wired to Postgres at the composition root.
type RewardService struct {
	repo     RewardRedeemer
	members  HouseholdMemberLister
	enqueuer notifydomain.Enqueuer
	logger   *slog.Logger
}

// NewRewardService constructs a RewardService with the injected dependencies.
// Panics if any dependency is nil so misconfigured composition roots fail at
// startup rather than at the first HTTP request.
func NewRewardService(
	repo RewardRedeemer,
	members HouseholdMemberLister,
	enqueuer notifydomain.Enqueuer,
	logger *slog.Logger,
) *RewardService {
	if repo == nil {
		panic("app: NewRewardService requires a non-nil RewardRedeemer")
	}
	if members == nil {
		panic("app: NewRewardService requires a non-nil HouseholdMemberLister")
	}
	if enqueuer == nil {
		panic("app: NewRewardService requires a non-nil Enqueuer")
	}
	if logger == nil {
		panic("app: NewRewardService requires a non-nil logger")
	}
	return &RewardService{repo: repo, members: members, enqueuer: enqueuer, logger: logger}
}

// Redeem exchanges a member's points for a reward. It:
//  1. Fetches the reward — returns [domain.ErrRewardNotFound] if missing or
//     belongs to another household. This read is an OPTIMISTIC fast-fail
//     only (a friendly 404 before opening a transaction, and a source for
//     the reward's Name for the notification below) — it is never trusted
//     for the active flag or cost, since [RewardRedeemer.RedeemWithDebit]
//     re-reads and locks both itself. A reward archived or repriced between
//     this read and that call is still handled correctly; only this read's
//     OWN optimistic guard (step 2) could be stale, and RedeemWithDebit's
//     locked re-check is what actually decides the outcome.
//  2. Optimistically guards that the reward is active — inactive rewards are
//     treated as [domain.ErrRewardNotFound] from the caller's perspective (a
//     retired reward can no longer be redeemed). This is a fast-fail only;
//     see step 1.
//  3. Builds a [domain.RewardRedemption] with status = 'pending'.
//  4. Calls [RewardRedeemer.RedeemWithDebit] — the authoritative check.
//     Returns [domain.ErrRewardNotFound] when the reward is unknown or has
//     since been archived, [domain.ErrInsufficientPoints] when the balance
//     is below the reward's current (locked) cost, or
//     [domain.ErrRewardOutOfStock] when a finite-stock cap has been reached
//     (NES-127).
//  5. On success, enqueues a best-effort "new redemption" notification to
//     every parent (owner or adult) in the household (NES-127) — see
//     notifyParentsOfRedemption's doc for why an enqueue failure here never
//     surfaces as this method's error.
//
// Error contracts:
//   - Returns [domain.ErrRewardNotFound] when the reward is unknown, belongs to
//     another household, or is inactive.
//   - Returns [domain.ErrInsufficientPoints] when the member's point balance is
//     less than the reward's current cost.
//   - Returns [domain.ErrRewardOutOfStock] when the reward has a finite
//     quantity_available and it has already been reached.
//   - Propagates unexpected repository errors unchanged.
func (s *RewardService) Redeem(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	rewardID domain.RewardID,
) (*domain.RewardRedemption, error) {
	return s.redeem(ctx, householdID, memberID, rewardID, nil)
}

// RedeemViaDeepLink is Redeem's NES-129 kiosk QR deep-link counterpart: it
// additionally records signatureHash — the SHA-256 hash (hex-encoded) of the
// signed link's canonical decoded signature bytes, e.g.
// deeplinkapp.HashSignature's return value — as the redemption's
// DeepLinkSignatureHash, so [RewardRedeemer.RedeemWithDebit]'s DATABASE-level
// unique constraint rejects a second redemption attempt for the SAME signed
// link. This durably prevents a resubmitted POST (double-tap, browser
// refresh-and-resend, or a request landing within the deep-link rate
// limiter's own burst) from redeeming the reward twice — durably meaning
// across process restarts and multiple server instances, unlike an
// in-process guard.
//
// Error contracts: identical to Redeem, PLUS returns
// [domain.ErrDeepLinkAlreadyRedeemed] when signatureHash was already used
// for a successful redemption in this household.
func (s *RewardService) RedeemViaDeepLink(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	rewardID domain.RewardID,
	signatureHash string,
) (*domain.RewardRedemption, error) {
	if signatureHash == "" {
		return nil, errors.New("redeem via deep link: signatureHash must not be empty")
	}
	return s.redeem(ctx, householdID, memberID, rewardID, &signatureHash)
}

// redeem is Redeem and RedeemViaDeepLink's shared implementation.
// deepLinkSignatureHash is nil for the ordinary storefront path (Redeem) and
// non-nil for the deep-link path (RedeemViaDeepLink) — see
// domain.RewardRedemption.DeepLinkSignatureHash's doc for what it protects.
func (s *RewardService) redeem(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	rewardID domain.RewardID,
	deepLinkSignatureHash *string,
) (*domain.RewardRedemption, error) {
	// Step 1: fetch the reward for an optimistic fast-fail and its Name (for
	// the notification below) — see this method's doc for why its Active/
	// CostPoints fields are never treated as authoritative.
	reward, err := s.repo.GetReward(ctx, householdID, rewardID)
	if err != nil {
		if errors.Is(err, domain.ErrRewardNotFound) {
			return nil, domain.ErrRewardNotFound
		}
		return nil, fmt.Errorf("redeem: get reward: %w", err)
	}

	// Step 2: optimistic fast-fail — RedeemWithDebit re-checks this
	// authoritatively under its row lock, so a race here just means this
	// early exit is skipped and the authoritative check catches it instead.
	if !reward.Active {
		return nil, domain.ErrRewardNotFound
	}

	// Step 3: build the redemption entity.
	now := time.Now().UTC()
	redemption := &domain.RewardRedemption{
		ID:                    domain.NewRewardRedemptionID(),
		HouseholdID:           householdID,
		RewardID:              rewardID,
		MemberID:              memberID,
		Status:                domain.RedemptionPending,
		DeepLinkSignatureHash: deepLinkSignatureHash,
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	// Step 4: atomic debit + insert. costPoints is the amount actually
	// debited, straight from RedeemWithDebit's locked read — not this
	// method's own (possibly stale) reward.CostPoints.
	costPoints, err := s.repo.RedeemWithDebit(ctx, redemption)
	if err != nil {
		return nil, fmt.Errorf("redeem: %w", err)
	}

	s.logger.InfoContext(ctx, "reward redeemed",
		"household_id", householdID.String(),
		"member_id", memberID.String(),
		"reward_id", rewardID.String(),
		"redemption_id", redemption.ID.String(),
		"cost_points", costPoints,
	)

	// Step 5: best-effort parent notification.
	s.notifyParentsOfRedemption(ctx, now, householdID, memberID, reward.Name, redemption.ID)

	return redemption, nil
}

// notifyParentsOfRedemption enqueues one "new redemption" notification per
// parent (owner or adult) in the household, addressed individually so each
// parent's notification list shows it (NES-127), mirroring TradeService.
// notifyAccepted's per-target loop. The redemption has already committed by
// the time this runs, so an enqueue failure — logged, per target — never
// surfaces as Redeem's error; the redemption itself is not at risk.
func (s *RewardService) notifyParentsOfRedemption(
	ctx context.Context,
	at time.Time,
	householdID household.HouseholdID,
	redeemerID household.MemberID,
	rewardName string,
	redemptionID domain.RewardRedemptionID,
) {
	members, err := s.members.ListMembers(ctx, householdID)
	if err != nil {
		s.logger.Error("redeem: list members for parent notification failed",
			"household_id", householdID.String(),
			"error", err,
		)
		return
	}

	redeemerName := "A member"
	for _, m := range members {
		if m.ID == redeemerID {
			redeemerName = m.DisplayName
			break
		}
	}

	redemptionUUID := uuid.UUID(redemptionID)
	for _, m := range members {
		if !m.Role.IsParent() {
			continue
		}
		parentID := m.ID
		n := &notifydomain.Notification{
			ID:          notifydomain.NewNotificationID(),
			HouseholdID: householdID,
			MemberID:    &parentID,
			Channel:     notifydomain.ChannelInApp,
			Title:       "New reward redemption",
			Body: fmt.Sprintf("%s wants to redeem %q — review it in the rewards inbox.",
				redeemerName, rewardName),
			ScheduledFor: at,
			Status:       notifydomain.StatusPending,
			SourceType:   "reward_redemption",
			SourceID:     &redemptionUUID,
			EventType:    notifydomain.EventTypeRewardRedemptionRequested,
		}
		if err := s.enqueuer.Enqueue(ctx, n); err != nil {
			s.logger.Error("redeem: parent notification enqueue failed",
				"redemption_id", redemptionID.String(),
				"member_id", parentID.String(),
				"error", err,
			)
		}
	}
}
