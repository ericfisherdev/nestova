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

// RedemptionFulfiller is the persistence port for the parent-facing
// fulfillment inbox and member-facing self-cancel (NES-127): resolving a
// pending redemption one of three ways. Kept separate from [RewardRedeemer]
// and [RewardCatalogManager] (ISP) — neither of those callers needs these
// methods, and RedemptionService needs none of theirs. Reads
// (ListPendingRedemptions/ListMemberRedemptions) are not part of this
// interface: like every other read in this package (see
// GamificationWebHandlers' direct use of domain.RewardRepository/
// PointLedgerRepository for its own reads), the web handler queries
// domain.RewardRepository directly rather than routing through a service —
// only mutations that need post-commit notification go through
// RedemptionService.
type RedemptionFulfiller interface {
	// Fulfill transitions a pending redemption to fulfilled. See
	// [domain.RewardRepository.Fulfill] for the full contract.
	Fulfill(ctx context.Context, householdID household.HouseholdID, id domain.RewardRedemptionID) (domain.ResolvedRedemption, error)

	// Deny transitions a pending redemption to denied and refunds the
	// debited points. See [domain.RewardRepository.Deny] for the full
	// contract.
	Deny(
		ctx context.Context,
		householdID household.HouseholdID,
		id domain.RewardRedemptionID,
		reason string,
	) (domain.ResolvedRedemption, error)

	// Cancel transitions a pending redemption belonging to memberID to
	// cancelled and refunds the debited points. See
	// [domain.RewardRepository.Cancel] for the full contract.
	Cancel(
		ctx context.Context,
		householdID household.HouseholdID,
		id domain.RewardRedemptionID,
		memberID household.MemberID,
	) (domain.ResolvedRedemption, error)
}

// RedemptionService orchestrates the redemption-resolution use cases
// (NES-127): a parent fulfilling or denying a pending redemption, and a
// member cancelling their own. Fulfill and Deny notify the redeeming member
// of the outcome; Cancel does not — the member already knows, since they
// just performed the action themselves, mirroring TradeService.Cancel's
// identical no-notification precedent.
//
// Dependencies are injected via the constructor so the service is testable
// with fakes (hermetic tests) and wired to Postgres at the composition root.
type RedemptionService struct {
	repo     RedemptionFulfiller
	enqueuer notifydomain.Enqueuer
	logger   *slog.Logger
}

// NewRedemptionService constructs a RedemptionService with injected
// dependencies. Returns an error when any argument is nil, mirroring
// [NewTradeService]'s error-return precedent for a notification-emitting
// service (as opposed to RewardService/RewardAdminService's panic-on-nil
// precedent for pure CRUD services).
func NewRedemptionService(
	repo RedemptionFulfiller,
	enqueuer notifydomain.Enqueuer,
	logger *slog.Logger,
) (*RedemptionService, error) {
	if repo == nil {
		return nil, errors.New("app: NewRedemptionService requires a non-nil RedemptionFulfiller")
	}
	if enqueuer == nil {
		return nil, errors.New("app: NewRedemptionService requires a non-nil enqueuer")
	}
	if logger == nil {
		return nil, errors.New("app: NewRedemptionService requires a non-nil logger")
	}
	return &RedemptionService{repo: repo, enqueuer: enqueuer, logger: logger}, nil
}

// Fulfill approves a pending redemption. On success it enqueues a best-effort
// "reward fulfilled" notification to the redeeming member.
//
// Error contracts:
//   - Returns [domain.ErrRedemptionNotFound] when id is unknown or belongs to
//     another household.
//   - Returns [domain.ErrRedemptionNotPending] when the redemption is not
//     currently pending.
func (s *RedemptionService) Fulfill(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
) error {
	resolved, err := s.repo.Fulfill(ctx, householdID, id)
	if err != nil {
		return fmt.Errorf("fulfill redemption: %w", err)
	}
	s.notifyResolved(ctx, time.Now(), resolved)
	return nil
}

// Deny rejects a pending redemption, refunding the debited points via a
// compensating ledger entry. On success it enqueues a best-effort
// "redemption denied" notification to the redeeming member, including reason
// when non-empty.
//
// Error contracts: same as Fulfill.
func (s *RedemptionService) Deny(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
	reason string,
) error {
	resolved, err := s.repo.Deny(ctx, householdID, id, reason)
	if err != nil {
		return fmt.Errorf("deny redemption: %w", err)
	}
	s.notifyResolved(ctx, time.Now(), resolved)
	return nil
}

// Cancel withdraws a member's own pending redemption, refunding the debited
// points via a compensating ledger entry. No notification is enqueued — see
// this type's doc for why.
//
// Error contracts:
//   - Returns [domain.ErrRedemptionNotPending] when the redemption is
//     unknown, belongs to another household, does not belong to memberID, or
//     is not currently pending.
func (s *RedemptionService) Cancel(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.RewardRedemptionID,
	memberID household.MemberID,
) error {
	if _, err := s.repo.Cancel(ctx, householdID, id, memberID); err != nil {
		return fmt.Errorf("cancel redemption: %w", err)
	}
	return nil
}

// notifyResolved enqueues a single notification to the redeeming member
// describing a fulfillment or denial outcome (NES-127). The redemption has
// already committed by the time this runs, so an enqueue failure is logged
// only, never surfaced as the caller's error — mirroring TradeService.
// notifyAccepted's identical post-commit, best-effort contract.
func (s *RedemptionService) notifyResolved(ctx context.Context, at time.Time, resolved domain.ResolvedRedemption) {
	var title, body string
	switch resolved.Status {
	case domain.RedemptionFulfilled:
		title = "Reward fulfilled"
		body = fmt.Sprintf("Your redemption of %q has been fulfilled!", resolved.RewardName)
	case domain.RedemptionDenied:
		title = "Redemption denied"
		if resolved.DeniedReason != nil && *resolved.DeniedReason != "" {
			body = fmt.Sprintf("Your redemption of %q was denied: %s. Your points have been refunded.",
				resolved.RewardName, *resolved.DeniedReason)
		} else {
			body = fmt.Sprintf("Your redemption of %q was denied. Your points have been refunded.",
				resolved.RewardName)
		}
	default:
		// Cancel never reaches here — RedemptionService.Cancel does not call
		// notifyResolved (see this type's doc). Defensive no-op for any other
		// status this switch does not yet know about.
		return
	}

	memberID := resolved.MemberID
	redemptionUUID := uuid.UUID(resolved.RedemptionID)
	n := &notifydomain.Notification{
		ID:           notifydomain.NewNotificationID(),
		HouseholdID:  resolved.HouseholdID,
		MemberID:     &memberID,
		Channel:      notifydomain.ChannelInApp,
		Title:        title,
		Body:         body,
		ScheduledFor: at,
		Status:       notifydomain.StatusPending,
		SourceType:   "reward_redemption",
		SourceID:     &redemptionUUID,
		EventType:    notifydomain.EventTypeRewardRedemptionResolved,
	}
	if err := s.enqueuer.Enqueue(ctx, n); err != nil {
		s.logger.Error("redemptions: resolution notification enqueue failed",
			"redemption_id", resolved.RedemptionID.String(),
			"error", err,
		)
	}
}
