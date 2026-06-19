package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/app"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// recordingTrackedItemRepo is an in-memory domain.TrackedItemRepository that
// records Create calls so registration tests can assert the persisted item.
type recordingTrackedItemRepo struct {
	created []*domain.TrackedItem
	active  []*domain.TrackedItem
}

func (r *recordingTrackedItemRepo) Create(_ context.Context, item *domain.TrackedItem) error {
	r.created = append(r.created, item)
	return nil
}

func (r *recordingTrackedItemRepo) Get(context.Context, domain.TrackedItemID) (*domain.TrackedItem, error) {
	return nil, domain.ErrTrackedItemNotFound
}

func (r *recordingTrackedItemRepo) Update(context.Context, *domain.TrackedItem) error { return nil }

func (r *recordingTrackedItemRepo) ListActiveByHousehold(context.Context, household.HouseholdID) ([]*domain.TrackedItem, error) {
	return r.active, nil
}

func (r *recordingTrackedItemRepo) ListAllActive(context.Context) ([]*domain.TrackedItem, error) {
	return r.active, nil
}

func (r *recordingTrackedItemRepo) ListDueForRestock(context.Context, household.HouseholdID, time.Time) ([]*domain.TrackedItem, error) {
	return nil, nil
}

// Compile-time assertion.
var _ domain.TrackedItemRepository = (*recordingTrackedItemRepo)(nil)

// recordingEventRepo is an in-memory domain.UsageEventRepository that records
// appended events and serves a fixed depletion history so a depletion LogEvent
// drives the predictor through to an Upsert.
type recordingEventRepo struct {
	appended   []*domain.UsageEvent
	depletions []*domain.UsageEvent
}

func (r *recordingEventRepo) Append(_ context.Context, event *domain.UsageEvent) error {
	r.appended = append(r.appended, event)
	return nil
}

func (r *recordingEventRepo) ListDepletionEvents(context.Context, domain.TrackedItemID) ([]*domain.UsageEvent, error) {
	return r.depletions, nil
}

// Compile-time assertion.
var _ domain.UsageEventRepository = (*recordingEventRepo)(nil)

func mustUsageService(t *testing.T, items domain.TrackedItemRepository, events domain.UsageEventRepository, preds domain.RestockPredictionRepository) *app.UsageService {
	t.Helper()
	predictor := mustPredictor(t, events, preds)
	svc, err := app.NewUsageService(items, events, predictor)
	if err != nil {
		t.Fatalf("NewUsageService: %v", err)
	}
	return svc
}

func TestNewUsageServiceRejectsNilDeps(t *testing.T) {
	items := &recordingTrackedItemRepo{}
	events := &recordingEventRepo{}
	predictor := mustPredictor(t, events, &fakePredictionRepo{})

	if _, err := app.NewUsageService(nil, events, predictor); err == nil {
		t.Error("NewUsageService(nil items) = nil error, want error")
	}
	if _, err := app.NewUsageService(items, nil, predictor); err == nil {
		t.Error("NewUsageService(nil events) = nil error, want error")
	}
	if _, err := app.NewUsageService(items, events, nil); err == nil {
		t.Error("NewUsageService(nil predictor) = nil error, want error")
	}
}

func TestRegisterItemTrimsAndPersists(t *testing.T) {
	items := &recordingTrackedItemRepo{}
	svc := mustUsageService(t, items, &recordingEventRepo{}, &fakePredictionRepo{})
	hh := household.NewHouseholdID()

	item, err := svc.RegisterItem(context.Background(), hh, "  Coffee Beans  ", " pantry ", 3)
	if err != nil {
		t.Fatalf("RegisterItem: %v", err)
	}
	if item.Name != "Coffee Beans" {
		t.Errorf("Name = %q, want trimmed %q", item.Name, "Coffee Beans")
	}
	if item.Category != "pantry" {
		t.Errorf("Category = %q, want trimmed %q", item.Category, "pantry")
	}
	if item.RestockLeadDays != 3 {
		t.Errorf("RestockLeadDays = %d, want 3", item.RestockLeadDays)
	}
	if !item.Active {
		t.Error("RegisterItem should mark the item Active")
	}
	if len(items.created) != 1 || items.created[0].ID != item.ID {
		t.Errorf("expected exactly one Create of the returned item, got %d", len(items.created))
	}
}

func TestRegisterItemRejectsEmptyName(t *testing.T) {
	items := &recordingTrackedItemRepo{}
	svc := mustUsageService(t, items, &recordingEventRepo{}, &fakePredictionRepo{})

	if _, err := svc.RegisterItem(context.Background(), household.NewHouseholdID(), "   ", "pantry", 0); err == nil {
		t.Error("RegisterItem(blank name) = nil error, want error")
	}
	if len(items.created) != 0 {
		t.Errorf("RegisterItem(blank name) must not persist, got %d Create calls", len(items.created))
	}
}

func TestLogEventAppendsAndAttributesMember(t *testing.T) {
	events := &recordingEventRepo{}
	svc := mustUsageService(t, &recordingTrackedItemRepo{}, events, &fakePredictionRepo{})
	hh := household.NewHouseholdID()
	itemID := domain.NewTrackedItemID()
	memberID := household.NewMemberID()
	occurredAt := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)

	event, err := svc.LogEvent(context.Background(), hh, itemID, domain.UsageOpened, &memberID, occurredAt)
	if err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if len(events.appended) != 1 {
		t.Fatalf("expected exactly one Append, got %d", len(events.appended))
	}
	got := events.appended[0]
	if got.TrackedItemID != itemID || got.Type != domain.UsageOpened {
		t.Errorf("appended event = %+v, want item %v / type opened", got, itemID)
	}
	if got.MemberID == nil || *got.MemberID != memberID {
		t.Errorf("appended event member = %v, want %v", got.MemberID, memberID)
	}
	if !event.OccurredAt.Equal(occurredAt) {
		t.Errorf("OccurredAt = %v, want %v", event.OccurredAt, occurredAt)
	}
}

func TestLogEventRejectsInvalidType(t *testing.T) {
	events := &recordingEventRepo{}
	svc := mustUsageService(t, &recordingTrackedItemRepo{}, events, &fakePredictionRepo{})

	if _, err := svc.LogEvent(context.Background(), household.NewHouseholdID(), domain.NewTrackedItemID(), domain.UsageType("bogus"), nil, time.Now()); err == nil {
		t.Error("LogEvent(invalid type) = nil error, want error")
	}
	if len(events.appended) != 0 {
		t.Errorf("LogEvent(invalid type) must not append, got %d", len(events.appended))
	}
}

func TestLogEventDepletedTriggersRecompute(t *testing.T) {
	// Two prior depletions so the predictor has an interval to average and writes
	// a prediction; the new depletion LogEvent must drive that recompute.
	events := &recordingEventRepo{depletions: depletionsAtDays(0, 10)}
	preds := &fakePredictionRepo{}
	svc := mustUsageService(t, &recordingTrackedItemRepo{}, events, preds)

	if _, err := svc.LogEvent(context.Background(), household.NewHouseholdID(), domain.NewTrackedItemID(), domain.UsageDepleted, nil, baseDay.AddDate(0, 0, 20)); err != nil {
		t.Fatalf("LogEvent(depleted): %v", err)
	}
	if preds.upserts != 1 {
		t.Errorf("LogEvent(depleted) upserts = %d, want 1 (prediction recomputed)", preds.upserts)
	}
}

func TestLogEventNonDepletedDoesNotRecompute(t *testing.T) {
	events := &recordingEventRepo{depletions: depletionsAtDays(0, 10)}
	preds := &fakePredictionRepo{}
	svc := mustUsageService(t, &recordingTrackedItemRepo{}, events, preds)

	for _, usageType := range []domain.UsageType{domain.UsageReplaced, domain.UsageRefilled, domain.UsageOpened} {
		if _, err := svc.LogEvent(context.Background(), household.NewHouseholdID(), domain.NewTrackedItemID(), usageType, nil, baseDay); err != nil {
			t.Fatalf("LogEvent(%s): %v", usageType, err)
		}
	}
	if preds.upserts != 0 {
		t.Errorf("non-depletion LogEvent upserts = %d, want 0 (no recompute)", preds.upserts)
	}
}

func TestLogEventPropagatesRecomputeError(t *testing.T) {
	events := &recordingEventRepo{depletions: depletionsAtDays(0, 10)}
	svc := mustUsageService(t, &recordingTrackedItemRepo{}, events, &erroringPredictionRepo{})

	if _, err := svc.LogEvent(context.Background(), household.NewHouseholdID(), domain.NewTrackedItemID(), domain.UsageDepleted, nil, baseDay.AddDate(0, 0, 20)); err == nil {
		t.Error("LogEvent(depleted) with failing upsert = nil error, want propagated error")
	}
}

func TestListItemsDelegates(t *testing.T) {
	active := []*domain.TrackedItem{
		{ID: domain.NewTrackedItemID(), Name: "Milk", Active: true},
		{ID: domain.NewTrackedItemID(), Name: "Eggs", Active: true},
	}
	svc := mustUsageService(t, &recordingTrackedItemRepo{active: active}, &recordingEventRepo{}, &fakePredictionRepo{})

	got, err := svc.ListItems(context.Background(), household.NewHouseholdID())
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListItems returned %d items, want 2", len(got))
	}
}

// erroringPredictionRepo fails on Upsert so the recompute error path is covered.
type erroringPredictionRepo struct{}

func (erroringPredictionRepo) Upsert(context.Context, *domain.RestockPrediction) error {
	return errors.New("upsert boom")
}

func (erroringPredictionRepo) Get(context.Context, domain.TrackedItemID) (*domain.RestockPrediction, error) {
	return nil, domain.ErrPredictionNotFound
}
