package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// UsageService is the usage-tracking use-case boundary: registering tracked
// items and logging usage events against them. Logging a depletion event
// recomputes the item's restock prediction (NES-41: predictions are recomputed
// on each new depletion event), so the cached prediction stays current as the
// member records usage.
type UsageService struct {
	items     domain.TrackedItemRepository
	events    domain.UsageEventRepository
	predictor *Predictor
	logger    *slog.Logger
}

// NewUsageService constructs the service with injected repositories, the restock
// predictor, and a logger (used to record best-effort prediction-recompute
// failures). It returns an error when any dependency is nil so a misconfigured
// composition root fails at startup rather than at first use.
func NewUsageService(
	items domain.TrackedItemRepository,
	events domain.UsageEventRepository,
	predictor *Predictor,
	logger *slog.Logger,
) (*UsageService, error) {
	if items == nil {
		return nil, errors.New("app: NewUsageService requires a non-nil tracked item repository")
	}
	if events == nil {
		return nil, errors.New("app: NewUsageService requires a non-nil usage event repository")
	}
	if predictor == nil {
		return nil, errors.New("app: NewUsageService requires a non-nil predictor")
	}
	if logger == nil {
		return nil, errors.New("app: NewUsageService requires a non-nil logger")
	}
	return &UsageService{items: items, events: events, predictor: predictor, logger: logger}, nil
}

// RegisterItem creates a new active tracked item. It trims and rejects an empty
// name, and stamps Active true so the item is immediately listed and eligible
// for restock prediction. restockLeadDays is how many days before predicted
// depletion the item should surface on the shopping list.
func (s *UsageService) RegisterItem(
	ctx context.Context,
	householdID household.HouseholdID,
	name, category string,
	restockLeadDays int,
) (*domain.TrackedItem, error) {
	trimmedName := strings.TrimSpace(name)
	if trimmedName == "" {
		return nil, fmt.Errorf("register item: name must not be empty")
	}
	if restockLeadDays < 0 {
		return nil, fmt.Errorf("register item: restock lead days must not be negative")
	}
	item := &domain.TrackedItem{
		ID:              domain.NewTrackedItemID(),
		HouseholdID:     householdID,
		Name:            trimmedName,
		Category:        strings.TrimSpace(category),
		RestockLeadDays: restockLeadDays,
		Active:          true,
	}
	if err := s.items.Create(ctx, item); err != nil {
		return nil, err
	}
	return item, nil
}

// LogEvent appends a usage event for the tracked item. When the event is a
// depletion it recomputes the item's restock prediction (NES-41) and propagates
// any recompute error; the recomputed prediction itself is discarded here since
// callers read it back via the prediction repository when rendering.
func (s *UsageService) LogEvent(
	ctx context.Context,
	householdID household.HouseholdID,
	itemID domain.TrackedItemID,
	usageType domain.UsageType,
	memberID *household.MemberID,
	occurredAt time.Time,
) (*domain.UsageEvent, error) {
	if !usageType.Valid() {
		return nil, fmt.Errorf("log event: invalid usage type %q", usageType)
	}
	event := &domain.UsageEvent{
		ID:            domain.NewUsageEventID(),
		HouseholdID:   householdID,
		TrackedItemID: itemID,
		Type:          usageType,
		OccurredAt:    occurredAt,
		MemberID:      memberID,
	}
	if err := s.events.Append(ctx, event); err != nil {
		return nil, err
	}
	// Recompute is best-effort: the event is already committed, so returning a
	// recompute error would make the caller retry and append a duplicate event,
	// skewing the prediction. Log the failure instead — the next depletion event
	// or the hourly restock scheduler (NES-44) recomputes the prediction anyway.
	if usageType == domain.UsageDepleted {
		if _, err := s.predictor.Recompute(ctx, itemID); err != nil {
			s.logger.Error("usage: recompute prediction after depletion failed (event still logged)",
				"tracked_item_id", itemID.String(), "error", err)
		}
	}
	return event, nil
}

// ListItems returns the household's active tracked items.
func (s *UsageService) ListItems(ctx context.Context, householdID household.HouseholdID) ([]*domain.TrackedItem, error) {
	return s.items.ListActiveByHousehold(ctx, householdID)
}
