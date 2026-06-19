package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/adapter"
	"github.com/ericfisherdev/nestova/internal/tracking/app"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- fakes for the restock scheduler ---

type fakeTrackedItemRepo struct {
	active []*domain.TrackedItem
}

func (f *fakeTrackedItemRepo) Create(context.Context, *domain.TrackedItem) error { return nil }
func (f *fakeTrackedItemRepo) Get(context.Context, domain.TrackedItemID) (*domain.TrackedItem, error) {
	return nil, domain.ErrTrackedItemNotFound
}
func (f *fakeTrackedItemRepo) Update(context.Context, *domain.TrackedItem) error { return nil }
func (f *fakeTrackedItemRepo) ListActiveByHousehold(context.Context, household.HouseholdID) ([]*domain.TrackedItem, error) {
	return nil, nil
}

func (f *fakeTrackedItemRepo) ListAllActive(context.Context) ([]*domain.TrackedItem, error) {
	return f.active, nil
}

func (f *fakeTrackedItemRepo) ListDueForRestock(context.Context, household.HouseholdID, time.Time) ([]*domain.TrackedItem, error) {
	return nil, nil
}

type fakeIngredientEnsurer struct{ id domain.IngredientID }

func (f *fakeIngredientEnsurer) EnsureIngredient(_ context.Context, name string) (*domain.Ingredient, error) {
	return &domain.Ingredient{ID: f.id, CanonicalName: name}, nil
}

type fakeRestockShoppingRepo struct {
	inserted bool
	addCalls int
}

func (f *fakeRestockShoppingRepo) Add(context.Context, *domain.ShoppingListItem) error { return nil }
func (f *fakeRestockShoppingRepo) AddRestockIfAbsent(context.Context, *domain.ShoppingListItem) (bool, error) {
	f.addCalls++
	return f.inserted, nil
}

func (f *fakeRestockShoppingRepo) UpdateStatus(context.Context, domain.ShoppingListItemID, domain.ItemStatus) (*domain.ShoppingListItem, error) {
	return nil, domain.ErrShoppingListItemNotFound
}

func (f *fakeRestockShoppingRepo) ListByStatus(context.Context, household.HouseholdID, domain.ItemStatus) ([]*domain.ShoppingListItem, error) {
	return nil, nil
}

type fakeEnqueuer struct {
	enqueued []*notifydomain.Notification
	err      error
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, n *notifydomain.Notification) error {
	if f.err != nil {
		return f.err
	}
	f.enqueued = append(f.enqueued, n)
	return nil
}

// dueItem builds an active tracked item plus a predictor whose depletion history
// (days 0 and 10 from baseDay → predicted depletion 2026-01-21) makes it due.
func dueSetup(t *testing.T, leadDays int) (*domain.TrackedItem, *app.Predictor) {
	t.Helper()
	item := &domain.TrackedItem{
		ID: domain.NewTrackedItemID(), HouseholdID: household.NewHouseholdID(),
		Name: "Coffee", RestockLeadDays: leadDays, Active: true,
	}
	events := &fakeEventRepo{depletions: depletionsAtDays(0, 10)}
	predictor, err := app.NewPredictor(events, &fakePredictionRepo{})
	if err != nil {
		t.Fatalf("NewPredictor: %v", err)
	}
	return item, predictor
}

func mustRestockScheduler(t *testing.T, items domain.TrackedItemRepository, predictor *app.Predictor, ing domain.IngredientEnsurer, shop domain.ShoppingListRepository, enq notifydomain.Enqueuer) *app.RestockScheduler {
	t.Helper()
	s, err := app.NewRestockScheduler(items, predictor, ing, shop, enq, discardLogger(), time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("NewRestockScheduler: %v", err)
	}
	return s
}

func TestRestockRunOnceRaisesEntryAndNotifiesWhenDue(t *testing.T) {
	item, predictor := dueSetup(t, 0)
	items := &fakeTrackedItemRepo{active: []*domain.TrackedItem{item}}
	shop := &fakeRestockShoppingRepo{inserted: true}
	enq := &fakeEnqueuer{}
	sched := mustRestockScheduler(t, items, predictor, &fakeIngredientEnsurer{id: domain.NewIngredientID()}, shop, enq)

	// asOf well past the predicted depletion (2026-01-21) so the item is due.
	asOf := baseDay.AddDate(0, 0, 25)
	generated, err := sched.RunOnce(context.Background(), asOf)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if generated != 1 || shop.addCalls != 1 {
		t.Errorf("generated=%d addCalls=%d, want 1 and 1", generated, shop.addCalls)
	}
	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued %d notifications, want 1", len(enq.enqueued))
	}
	n := enq.enqueued[0]
	if n.SourceType != "restock" || n.SourceID == nil {
		t.Errorf("notification source = (%q, %v), want restock with a source id", n.SourceType, n.SourceID)
	}
}

func TestRestockRunOnceNoDuplicateNotificationOnRerun(t *testing.T) {
	item, predictor := dueSetup(t, 0)
	items := &fakeTrackedItemRepo{active: []*domain.TrackedItem{item}}
	// An open restock entry already exists: AddRestockIfAbsent reports not inserted.
	shop := &fakeRestockShoppingRepo{inserted: false}
	enq := &fakeEnqueuer{}
	sched := mustRestockScheduler(t, items, predictor, &fakeIngredientEnsurer{id: domain.NewIngredientID()}, shop, enq)

	generated, err := sched.RunOnce(context.Background(), baseDay.AddDate(0, 0, 25))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if generated != 0 {
		t.Errorf("generated=%d, want 0 (dedup)", generated)
	}
	if len(enq.enqueued) != 0 {
		t.Errorf("enqueued %d notifications, want 0 (no duplicate notification)", len(enq.enqueued))
	}
}

func TestRestockRunOnceKeepsEntryWhenNotificationFails(t *testing.T) {
	item, predictor := dueSetup(t, 0)
	items := &fakeTrackedItemRepo{active: []*domain.TrackedItem{item}}
	shop := &fakeRestockShoppingRepo{inserted: true}
	// Enqueue fails, but the restock entry was already created.
	enq := &fakeEnqueuer{err: errors.New("outbox down")}
	sched := mustRestockScheduler(t, items, predictor, &fakeIngredientEnsurer{id: domain.NewIngredientID()}, shop, enq)

	generated, err := sched.RunOnce(context.Background(), baseDay.AddDate(0, 0, 25))
	if err != nil {
		t.Fatalf("RunOnce returned error %v, want nil (notification is best-effort)", err)
	}
	if generated != 1 || shop.addCalls != 1 {
		t.Errorf("generated=%d addCalls=%d, want 1 and 1 (entry kept despite notify failure)", generated, shop.addCalls)
	}
}

func TestRestockRunOnceSkipsItemsNotYetDue(t *testing.T) {
	item, predictor := dueSetup(t, 0)
	items := &fakeTrackedItemRepo{active: []*domain.TrackedItem{item}}
	shop := &fakeRestockShoppingRepo{inserted: true}
	enq := &fakeEnqueuer{}
	sched := mustRestockScheduler(t, items, predictor, &fakeIngredientEnsurer{id: domain.NewIngredientID()}, shop, enq)

	// asOf well before the predicted depletion (2026-01-21): not due.
	generated, err := sched.RunOnce(context.Background(), baseDay)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if generated != 0 || shop.addCalls != 0 || len(enq.enqueued) != 0 {
		t.Errorf("not-due item produced generated=%d addCalls=%d notifications=%d, want all 0",
			generated, shop.addCalls, len(enq.enqueued))
	}
}

func TestRestockRunOnceSkipsItemsWithoutPrediction(t *testing.T) {
	item := &domain.TrackedItem{
		ID: domain.NewTrackedItemID(), HouseholdID: household.NewHouseholdID(),
		Name: "Salt", Active: true,
	}
	// Only one depletion → Predictor returns no prediction.
	events := &fakeEventRepo{depletions: depletionsAtDays(0)}
	predictor, err := app.NewPredictor(events, &fakePredictionRepo{})
	if err != nil {
		t.Fatalf("NewPredictor: %v", err)
	}
	shop := &fakeRestockShoppingRepo{inserted: true}
	enq := &fakeEnqueuer{}
	sched := mustRestockScheduler(t, &fakeTrackedItemRepo{active: []*domain.TrackedItem{item}}, predictor, &fakeIngredientEnsurer{id: domain.NewIngredientID()}, shop, enq)

	generated, err := sched.RunOnce(context.Background(), baseDay.AddDate(0, 0, 100))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if generated != 0 || shop.addCalls != 0 {
		t.Errorf("item without prediction produced generated=%d addCalls=%d, want 0", generated, shop.addCalls)
	}
}

func TestNewRestockSchedulerValidatesDeps(t *testing.T) {
	_, predictor := dueSetup(t, 0)
	items := &fakeTrackedItemRepo{}
	ing := &fakeIngredientEnsurer{id: domain.NewIngredientID()}
	shop := &fakeRestockShoppingRepo{}
	enq := &fakeEnqueuer{}
	log := discardLogger()
	interval := time.Hour
	tick := time.Minute

	tests := []struct {
		name string
		call func() (*app.RestockScheduler, error)
	}{
		{"nil items", func() (*app.RestockScheduler, error) {
			return app.NewRestockScheduler(nil, predictor, ing, shop, enq, log, interval, tick)
		}},
		{"nil predictor", func() (*app.RestockScheduler, error) {
			return app.NewRestockScheduler(items, nil, ing, shop, enq, log, interval, tick)
		}},
		{"nil ingredient ensurer", func() (*app.RestockScheduler, error) {
			return app.NewRestockScheduler(items, predictor, nil, shop, enq, log, interval, tick)
		}},
		{"nil shopping repo", func() (*app.RestockScheduler, error) {
			return app.NewRestockScheduler(items, predictor, ing, nil, enq, log, interval, tick)
		}},
		{"nil enqueuer", func() (*app.RestockScheduler, error) {
			return app.NewRestockScheduler(items, predictor, ing, shop, nil, log, interval, tick)
		}},
		{"nil logger", func() (*app.RestockScheduler, error) {
			return app.NewRestockScheduler(items, predictor, ing, shop, enq, nil, interval, tick)
		}},
		{"zero interval", func() (*app.RestockScheduler, error) {
			return app.NewRestockScheduler(items, predictor, ing, shop, enq, log, 0, tick)
		}},
		{"zero tick timeout", func() (*app.RestockScheduler, error) {
			return app.NewRestockScheduler(items, predictor, ing, shop, enq, log, interval, 0)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.call(); err == nil {
				t.Errorf("NewRestockScheduler(%s) = nil error, want error", tt.name)
			}
		})
	}
}

func TestRestockRunOnceEndToEnd(t *testing.T) {
	pool := newTestPool(t)
	ctx := testCtx(t)

	trackedRepo := adapter.NewTrackedItemRepository(pool)
	eventRepo := adapter.NewUsageEventRepository(pool)
	predRepo := adapter.NewRestockPredictionRepository(pool)
	ingredientRepo := adapter.NewIngredientRepository(pool)
	shoppingRepo := adapter.NewShoppingListRepository(pool)
	predictor, err := app.NewPredictor(eventRepo, predRepo)
	if err != nil {
		t.Fatalf("NewPredictor: %v", err)
	}
	outbox := notifyadapter.NewOutboxRepository(pool)
	sched, err := app.NewRestockScheduler(trackedRepo, predictor, ingredientRepo, shoppingRepo, outbox, discardLogger(), time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("NewRestockScheduler: %v", err)
	}

	hh := household.NewHouseholdID()
	if _, err := pool.Exec(ctx, `INSERT INTO household (id, name) VALUES ($1, $2)`, hh.String(), "H"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	item := &domain.TrackedItem{
		ID: domain.NewTrackedItemID(), HouseholdID: hh, Name: "Coffee", RestockLeadDays: 3, Active: true,
	}
	if err := trackedRepo.Create(ctx, item); err != nil {
		t.Fatalf("create tracked item: %v", err)
	}
	for _, d := range []int{0, 10} {
		ev := &domain.UsageEvent{
			ID: domain.NewUsageEventID(), HouseholdID: hh, TrackedItemID: item.ID,
			Type: domain.UsageDepleted, OccurredAt: baseDay.AddDate(0, 0, d),
		}
		if err := eventRepo.Append(ctx, ev); err != nil {
			t.Fatalf("append depletion: %v", err)
		}
	}

	// asOf past predicted depletion (2026-01-21) so the item is due.
	asOf := baseDay.AddDate(0, 0, 30)
	generated, err := sched.RunOnce(ctx, asOf)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if generated != 1 {
		t.Fatalf("first RunOnce generated %d, want 1", generated)
	}

	// Re-run is idempotent: no new entry, no new notification.
	generated, err = sched.RunOnce(ctx, asOf)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if generated != 0 {
		t.Errorf("second RunOnce generated %d, want 0 (idempotent)", generated)
	}

	needed, err := shoppingRepo.ListByStatus(ctx, hh, domain.StatusNeeded)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	restockCount := 0
	for _, it := range needed {
		if it.Source == domain.SourceRestock {
			restockCount++
		}
	}
	if restockCount != 1 {
		t.Errorf("open restock entries = %d, want exactly 1", restockCount)
	}

	var notifications int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notification WHERE source_type = 'restock'`).Scan(&notifications); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	if notifications != 1 {
		t.Errorf("restock notifications = %d, want exactly 1", notifications)
	}
}
