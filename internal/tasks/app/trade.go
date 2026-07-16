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

// TradeService orchestrates the chore-trade use cases (NES-121): propose,
// accept, decline, cancel, and the background expiry sweep. Unlike the
// TaskService/Reminders split used elsewhere in this package, trade
// notifications (on accept and on expiry) are emitted by the SAME service
// that performs the mutation, since every trade-related notification shares
// the same tradeRepo + enqueuer dependency pair and operates on the same
// aggregate — there is no separate system-process/user-facing split the way
// TaskInstanceRepository serves both TaskService and Reminders.
type TradeService struct {
	tradeRepo domain.ChoreTradeRepository
	enqueuer  notifydomain.Enqueuer
	logger    *slog.Logger
}

// NewTradeService constructs a TradeService with injected dependencies.
// Returns an error when any argument is nil.
func NewTradeService(
	tradeRepo domain.ChoreTradeRepository,
	enqueuer notifydomain.Enqueuer,
	logger *slog.Logger,
) (*TradeService, error) {
	if tradeRepo == nil {
		return nil, errors.New("app: NewTradeService requires a non-nil trade repository")
	}
	if enqueuer == nil {
		return nil, errors.New("app: NewTradeService requires a non-nil enqueuer")
	}
	if logger == nil {
		return nil, errors.New("app: NewTradeService requires a non-nil logger")
	}
	return &TradeService{tradeRepo: tradeRepo, enqueuer: enqueuer, logger: logger}, nil
}

// Propose validates and persists a new trade proposal: proposerID is
// offering offeredInstanceID (which must currently be assigned to them) in
// exchange for responderID's requestedInstanceID.
//
// Error contracts:
//   - Returns domain.ErrTradeSelf when proposerID equals responderID.
//   - Returns domain.ErrInstanceNotTradeable when offeredInstanceID equals
//     requestedInstanceID, when either instance fails
//     domain.IsInstanceTradeable, or when either instance already carries a
//     live proposal.
//   - Returns domain.ErrNotYourChore when the offered instance is not
//     assigned to proposerID, or the requested instance is not assigned to
//     responderID.
//   - Returns domain.ErrInstanceNotFound when either instance id is unknown
//     or belongs to another household.
func (s *TradeService) Propose(
	ctx context.Context,
	householdID household.HouseholdID,
	proposerID, responderID household.MemberID,
	offeredInstanceID, requestedInstanceID domain.TaskInstanceID,
) (*domain.ChoreTrade, error) {
	if proposerID == responderID {
		return nil, fmt.Errorf("propose trade: %w", domain.ErrTradeSelf)
	}
	if offeredInstanceID == requestedInstanceID {
		return nil, fmt.Errorf("propose trade: %w", domain.ErrInstanceNotTradeable)
	}

	trade := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          proposerID,
		ResponderID:         responderID,
		OfferedInstanceID:   offeredInstanceID,
		RequestedInstanceID: requestedInstanceID,
	}
	if err := s.tradeRepo.Propose(ctx, householdID, trade); err != nil {
		return nil, fmt.Errorf("propose trade: %w", err)
	}
	return trade, nil
}

// Accept resolves the trade in responderID's favor: both instances' assignees
// are atomically swapped and the trade is marked accepted. at serves two
// purposes: it is the deadline instant the repository checks trade.ExpiresAt
// against (an accept at or after the deadline fails, even if the background
// sweep hasn't run yet), and it is the notification's ScheduledFor — so
// callers can drive both deterministically in tests rather than relying on
// the wall clock.
//
// Notification enqueue failures are logged but never surface as this
// method's error: the swap has already committed by the time notification
// building starts (matching the scheduler's EmitClaimExpiry/EmitTradeExpiry
// precedent, where the mutation is unrecoverable and the notification is a
// best-effort side channel), so returning a non-nil error here would
// incorrectly suggest to the caller that the trade was not accepted.
//
// Error contracts:
//   - Returns domain.ErrTradeNotPending when the trade is unknown, belongs to
//     another household, responderID is not the trade's responder, the trade
//     has already resolved, or its expiry has already passed as of at.
//   - Returns domain.ErrInstanceNotTradeable when either instance is no
//     longer in the state (or held by the party) it was in at propose time.
func (s *TradeService) Accept(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	responderID household.MemberID,
	at time.Time,
) error {
	resolved, err := s.tradeRepo.Accept(ctx, householdID, id, responderID, at)
	if err != nil {
		return fmt.Errorf("accept trade: %w", err)
	}
	s.notifyAccepted(ctx, at, resolved)
	return nil
}

// Decline resolves the trade against acceptance: no instance assignment
// changes.
//
// Error contracts:
//   - Returns domain.ErrTradeNotPending when the trade is unknown, belongs to
//     another household, responderID is not the trade's responder, or the
//     trade has already resolved.
func (s *TradeService) Decline(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	responderID household.MemberID,
) error {
	if err := s.tradeRepo.Decline(ctx, householdID, id, responderID); err != nil {
		return fmt.Errorf("decline trade: %w", err)
	}
	return nil
}

// Cancel withdraws a trade the proposer no longer wants resolved: no instance
// assignment changes.
//
// Error contracts:
//   - Returns domain.ErrTradeNotPending when the trade is unknown, belongs to
//     another household, proposerID is not the trade's proposer, or the
//     trade has already resolved.
func (s *TradeService) Cancel(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	proposerID household.MemberID,
) error {
	if err := s.tradeRepo.Cancel(ctx, householdID, id, proposerID); err != nil {
		return fmt.Errorf("cancel trade: %w", err)
	}
	return nil
}

// ExpireTrades calls domain.ChoreTradeRepository.SweepExpiredTrades to
// atomically resolve every trade whose expiry has passed, then enqueues one
// notification per expired trade to its proposer. It is called by
// Scheduler.RunOnce on the same cadence as the claim-expiry/claim-warning
// sweeps, mirroring Reminders.EmitClaimExpiry's shape:
//
// Like EmitClaimExpiry, an expired trade cannot be "un-swept" — the status
// transition has already committed by the time this method enqueues, so a
// failed enqueue has nothing to recover. The returned error exists only to
// make the failure observable to the scheduler.
func (s *TradeService) ExpireTrades(ctx context.Context, asOf time.Time) error {
	expired, err := s.tradeRepo.SweepExpiredTrades(ctx, asOf)
	if err != nil {
		return fmt.Errorf("expire trades: %w", err)
	}

	var failures int
	for _, trade := range expired {
		if err := s.enqueueTradeExpiry(ctx, asOf, trade); err != nil {
			failures++
		}
	}

	if len(expired) > 0 {
		s.logger.Info("trades: expiry emitted", "count", len(expired), "failures", failures)
	}
	if failures > 0 {
		return fmt.Errorf("expire trades: %d of %d expiry enqueues failed", failures, len(expired))
	}
	return nil
}

// notifyAccepted enqueues one notification to the proposer and one to the
// responder describing the swap. Each enqueue is independent: a failure on
// one does not skip the other. Failures are logged only — see Accept's doc
// for why they are not surfaced as an error.
func (s *TradeService) notifyAccepted(ctx context.Context, at time.Time, resolved domain.AcceptedTrade) {
	tradeUUID := uuid.UUID(resolved.TradeID)
	targets := []struct {
		memberID household.MemberID
		body     string
	}{
		{
			resolved.ProposerID,
			fmt.Sprintf("Your trade was accepted: you now have %q instead of %q.",
				resolved.RequestedTitle, resolved.OfferedTitle),
		},
		{
			resolved.ResponderID,
			fmt.Sprintf("You accepted a trade: you now have %q instead of %q.",
				resolved.OfferedTitle, resolved.RequestedTitle),
		},
	}
	for _, target := range targets {
		memberID := target.memberID
		n := &notifydomain.Notification{
			ID:           notifydomain.NewNotificationID(),
			HouseholdID:  resolved.HouseholdID,
			MemberID:     &memberID,
			Channel:      notifydomain.ChannelInApp,
			Title:        "Chore trade accepted",
			Body:         target.body,
			ScheduledFor: at,
			Status:       notifydomain.StatusPending,
			SourceType:   "chore_trade",
			SourceID:     &tradeUUID,
		}
		if err := s.enqueuer.Enqueue(ctx, n); err != nil {
			s.logger.Error("trades: accept notification enqueue failed",
				"trade_id", resolved.TradeID.String(),
				"error", err,
			)
		}
	}
}

// enqueueTradeExpiry builds and enqueues a single expiry notification for
// trade, addressed to the proposer. The enqueue error (nil on success) is
// returned so ExpireTrades can count failures; it is also logged here.
func (s *TradeService) enqueueTradeExpiry(ctx context.Context, asOf time.Time, trade domain.ExpiredTrade) error {
	tradeUUID := uuid.UUID(trade.TradeID)
	proposerID := trade.ProposerID
	n := &notifydomain.Notification{
		ID:          notifydomain.NewNotificationID(),
		HouseholdID: trade.HouseholdID,
		MemberID:    &proposerID,
		Channel:     notifydomain.ChannelInApp,
		Title:       "Chore trade proposal expired",
		Body: fmt.Sprintf("Your trade proposal (%q for %q) expired without a response.",
			trade.OfferedTitle, trade.RequestedTitle),
		ScheduledFor: asOf,
		Status:       notifydomain.StatusPending,
		SourceType:   "chore_trade",
		SourceID:     &tradeUUID,
	}
	if err := s.enqueuer.Enqueue(ctx, n); err != nil {
		s.logger.Error("trades: expiry enqueue failed",
			"trade_id", trade.TradeID.String(),
			"error", err,
		)
		return fmt.Errorf("enqueue trade expiry %s: %w", trade.TradeID.String(), err)
	}
	return nil
}
