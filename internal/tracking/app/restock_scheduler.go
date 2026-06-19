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
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// restockQuantity is the placeholder quantity for a generated restock entry: one
// unit. The member adjusts it on the shopping list; the value the automation
// raises is intentionally minimal.
var restockQuantity = household.Quantity{Amount: 1, Unit: household.UnitCount}

// RestockScheduler ties the prediction engine to the shopping list. Each run it
// recomputes every active item's prediction and, for items now due within their
// lead window, raises exactly one open restock shopping entry per ingredient and
// emits a notification. It runs as a background goroutine (see Run) alongside the
// notification dispatcher and task scheduler.
type RestockScheduler struct {
	items        domain.TrackedItemRepository
	predictor    *Predictor
	ingredients  domain.IngredientEnsurer
	shopping     domain.ShoppingListRepository
	enqueuer     notifydomain.Enqueuer
	logger       *slog.Logger
	pollInterval time.Duration
}

// NewRestockScheduler constructs the scheduler with injected dependencies.
// pollInterval must be positive.
func NewRestockScheduler(
	items domain.TrackedItemRepository,
	predictor *Predictor,
	ingredients domain.IngredientEnsurer,
	shopping domain.ShoppingListRepository,
	enqueuer notifydomain.Enqueuer,
	logger *slog.Logger,
	pollInterval time.Duration,
) (*RestockScheduler, error) {
	if items == nil {
		return nil, errors.New("app: NewRestockScheduler requires a non-nil tracked item repository")
	}
	if predictor == nil {
		return nil, errors.New("app: NewRestockScheduler requires a non-nil predictor")
	}
	if ingredients == nil {
		return nil, errors.New("app: NewRestockScheduler requires a non-nil ingredient ensurer")
	}
	if shopping == nil {
		return nil, errors.New("app: NewRestockScheduler requires a non-nil shopping list repository")
	}
	if enqueuer == nil {
		return nil, errors.New("app: NewRestockScheduler requires a non-nil enqueuer")
	}
	if logger == nil {
		return nil, errors.New("app: NewRestockScheduler requires a non-nil logger")
	}
	if pollInterval <= 0 {
		return nil, fmt.Errorf("app: NewRestockScheduler pollInterval must be positive, got %v", pollInterval)
	}
	return &RestockScheduler{
		items:        items,
		predictor:    predictor,
		ingredients:  ingredients,
		shopping:     shopping,
		enqueuer:     enqueuer,
		logger:       logger,
		pollInterval: pollInterval,
	}, nil
}

// RunOnce recomputes predictions for all active tracked items and raises restock
// entries for those now due, as of asOf. It returns the number of new restock
// entries created. A failure on one item is logged and recorded, but the rest of
// the batch still runs; the first error encountered is returned.
func (s *RestockScheduler) RunOnce(ctx context.Context, asOf time.Time) (int, error) {
	items, err := s.items.ListAllActive(ctx)
	if err != nil {
		return 0, fmt.Errorf("restock: list active items: %w", err)
	}

	var (
		generated int
		firstErr  error
	)
	for _, item := range items {
		created, itemErr := s.processItem(ctx, item, asOf)
		if itemErr != nil {
			s.logger.Error("restock: process item failed",
				"tracked_item_id", item.ID.String(), "error", itemErr)
			if firstErr == nil {
				firstErr = itemErr
			}
			continue
		}
		generated += created
	}
	return generated, firstErr
}

// processItem recomputes one item's prediction and, if it is due within its lead
// window of asOf, raises an idempotent restock entry and (only when a new entry
// was created) emits a notification. It returns 1 when a new entry was created.
func (s *RestockScheduler) processItem(ctx context.Context, item *domain.TrackedItem, asOf time.Time) (int, error) {
	prediction, err := s.predictor.Recompute(ctx, item.ID)
	if err != nil {
		return 0, fmt.Errorf("recompute: %w", err)
	}
	if prediction == nil {
		// Too few depletions to predict; nothing to restock yet.
		return 0, nil
	}

	// Due when predicted depletion falls on or before asOf + the item's lead days.
	cutoff := dateOf(asOf).AddDate(0, 0, item.RestockLeadDays)
	if prediction.PredictedDepletionOn.After(cutoff) {
		return 0, nil
	}

	// Resolve the item's name to a canonical ingredient so the restock entry
	// dedupes per ingredient (NES-43's open-restock partial unique).
	ingredient, err := s.ingredients.EnsureIngredient(ctx, item.Name)
	if err != nil {
		return 0, fmt.Errorf("ensure ingredient: %w", err)
	}

	ingredientID := ingredient.ID
	restock := &domain.ShoppingListItem{
		ID:           domain.NewShoppingListItemID(),
		HouseholdID:  item.HouseholdID,
		IngredientID: &ingredientID,
		Quantity:     restockQuantity,
		Source:       domain.SourceRestock,
		Status:       domain.StatusNeeded,
	}
	inserted, err := s.shopping.AddRestockIfAbsent(ctx, restock)
	if err != nil {
		return 0, fmt.Errorf("add restock entry: %w", err)
	}
	if !inserted {
		// An open restock entry already exists — no duplicate, no new notification.
		return 0, nil
	}

	// Notification is best-effort: the restock entry is the actionable artifact
	// and is now committed. If we returned an error here the next tick's dedup
	// would skip the (now-existing) entry and never retry the notification, so a
	// failed enqueue would be permanently lost. Log it and keep the entry instead.
	if err := s.notify(ctx, item, asOf); err != nil {
		s.logger.Error("restock: notification enqueue failed (restock entry still created)",
			"tracked_item_id", item.ID.String(), "error", err)
	}
	return 1, nil
}

// notify enqueues an in-app restock notification for the item, sourced to the
// tracked item (source_type "restock", source_id = the tracked item's UUID).
func (s *RestockScheduler) notify(ctx context.Context, item *domain.TrackedItem, asOf time.Time) error {
	sourceID := uuid.UUID(item.ID)
	n := &notifydomain.Notification{
		ID:           notifydomain.NewNotificationID(),
		HouseholdID:  item.HouseholdID,
		Channel:      notifydomain.ChannelInApp,
		Title:        fmt.Sprintf("Restock soon: %s", item.Name),
		Body:         fmt.Sprintf("%s is predicted to run out soon and was added to your shopping list.", item.Name),
		ScheduledFor: asOf,
		Status:       notifydomain.StatusPending,
		SourceType:   "restock",
		SourceID:     &sourceID,
	}
	return s.enqueuer.Enqueue(ctx, n)
}

// Run polls every pollInterval until ctx is cancelled, logging start and stop.
// Errors from RunOnce are logged but do not stop the loop. Cancelling ctx stops
// the loop but does not abort an in-flight cycle: each tick runs under its own
// context (see runTick), so callers can wait for Run to return to know the
// scheduler has fully drained.
func (s *RestockScheduler) Run(ctx context.Context) {
	s.logger.Info("restock scheduler: starting", "poll_interval", s.pollInterval)
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("restock scheduler: stopped")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				s.logger.Info("restock scheduler: stopped")
				return
			}
			s.runTick()
		}
	}
}

// runTick executes one RunOnce under a fresh bounded context independent of Run's
// lifecycle context, so an in-flight cycle finishes its writes during shutdown
// while the timeout still caps how long a stalled cycle delays shutdown.
func (s *RestockScheduler) runTick() {
	runCtx, cancel := context.WithTimeout(context.Background(), s.pollInterval)
	defer cancel()

	generated, err := s.RunOnce(runCtx, time.Now())
	if err != nil {
		s.logger.Error("restock scheduler: run once failed", "error", err)
	}
	if generated > 0 {
		s.logger.Info("restock scheduler: raised restock entries", "count", generated)
	}
}
