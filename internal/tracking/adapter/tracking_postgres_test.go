package adapter_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tracking/adapter"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// TestDepletionLiteralMatchesDomainConstant guards the literal 'depleted' SQL
// predicate in ListDepletionEvents (kept literal so the planner matches the
// partial index) against drift from the domain constant. Hermetic — no DB.
func TestDepletionLiteralMatchesDomainConstant(t *testing.T) {
	if domain.UsageDepleted.String() != "depleted" {
		t.Fatalf("domain.UsageDepleted = %q, but ListDepletionEvents and the partial index hard-code 'depleted'",
			domain.UsageDepleted)
	}
}

func seedHousehold(t *testing.T, pool *pgxpool.Pool) household.HouseholdID {
	t.Helper()
	id := household.NewHouseholdID()
	if _, err := pool.Exec(testCtx(t), `INSERT INTO household (id, name) VALUES ($1, $2)`,
		id.String(), "The Fishers"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	return id
}

func seedMember(t *testing.T, pool *pgxpool.Pool, hh household.HouseholdID, name string) household.MemberID {
	t.Helper()
	id := household.NewMemberID()
	if _, err := pool.Exec(testCtx(t),
		`INSERT INTO member (id, household_id, display_name, role, color_key) VALUES ($1, $2, $3, 'owner', 'sage')`,
		id.String(), hh.String(), name); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func seedTrackedItem(t *testing.T, repo *adapter.TrackedItemRepository, hh household.HouseholdID, name string, leadDays int) *domain.TrackedItem {
	t.Helper()
	item := &domain.TrackedItem{
		ID:              domain.NewTrackedItemID(),
		HouseholdID:     hh,
		Name:            name,
		Category:        "pantry",
		RestockLeadDays: leadDays,
		Active:          true,
	}
	if err := repo.Create(testCtx(t), item); err != nil {
		t.Fatalf("seed tracked item %q: %v", name, err)
	}
	return item
}

func TestTrackedItemCreateGetUpdate(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTrackedItemRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)

	item := seedTrackedItem(t, repo, hh, "Olive Oil", 5)
	if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		t.Error("Create did not populate timestamps")
	}

	got, err := repo.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Olive Oil" || got.RestockLeadDays != 5 || !got.Active {
		t.Errorf("Get = %+v, want name 'Olive Oil' lead 5 active", got)
	}

	got.Name = "Olive Oil (large)"
	got.RestockLeadDays = 7
	got.Active = false
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reloaded, err := repo.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if reloaded.Name != "Olive Oil (large)" || reloaded.RestockLeadDays != 7 || reloaded.Active {
		t.Errorf("after Update = %+v, want renamed lead 7 inactive", reloaded)
	}
}

func TestTrackedItemNotFoundAndBadHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTrackedItemRepository(pool)
	ctx := testCtx(t)

	if _, err := repo.Get(ctx, domain.NewTrackedItemID()); !errors.Is(err, domain.ErrTrackedItemNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrTrackedItemNotFound", err)
	}
	bad := &domain.TrackedItem{
		ID: domain.NewTrackedItemID(), HouseholdID: household.NewHouseholdID(),
		Name: "Ghost", Active: true,
	}
	if err := repo.Create(ctx, bad); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Errorf("Create(bad household) = %v, want ErrHouseholdNotFound", err)
	}
}

func TestTrackedItemListActiveByHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewTrackedItemRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)

	seedTrackedItem(t, repo, hh, "Apples", 0)
	inactive := seedTrackedItem(t, repo, hh, "Zucchini", 0)
	inactive.Active = false
	if err := repo.Update(ctx, inactive); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	active, err := repo.ListActiveByHousehold(ctx, hh)
	if err != nil {
		t.Fatalf("ListActiveByHousehold: %v", err)
	}
	if len(active) != 1 || active[0].Name != "Apples" {
		t.Errorf("ListActiveByHousehold = %+v, want only Apples", active)
	}
}

func TestUsageEventAppendAndListDepletion(t *testing.T) {
	pool := newTestPool(t)
	itemRepo := adapter.NewTrackedItemRepository(pool)
	eventRepo := adapter.NewUsageEventRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Maya")
	item := seedTrackedItem(t, itemRepo, hh, "Paper Towels", 2)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Two depletions (member-attributed and system) plus a non-depletion event.
	appendEvent(t, eventRepo, hh, item.ID, domain.UsageDepleted, base, &member)
	appendEvent(t, eventRepo, hh, item.ID, domain.UsageOpened, base.AddDate(0, 0, 1), nil)
	appendEvent(t, eventRepo, hh, item.ID, domain.UsageDepleted, base.AddDate(0, 0, 10), nil)

	depletions, err := eventRepo.ListDepletionEvents(ctx, item.ID)
	if err != nil {
		t.Fatalf("ListDepletionEvents: %v", err)
	}
	if len(depletions) != 2 {
		t.Fatalf("ListDepletionEvents returned %d events, want 2 (depleted only)", len(depletions))
	}
	if !depletions[0].OccurredAt.Before(depletions[1].OccurredAt) {
		t.Error("ListDepletionEvents not ordered ascending by occurred_at")
	}
	if depletions[0].MemberID == nil || *depletions[0].MemberID != member {
		t.Errorf("first depletion member = %v, want %v", depletions[0].MemberID, member)
	}
	if depletions[1].MemberID != nil {
		t.Errorf("system depletion member = %v, want nil", depletions[1].MemberID)
	}
}

func TestUsageEventAppendUnknownItem(t *testing.T) {
	pool := newTestPool(t)
	eventRepo := adapter.NewUsageEventRepository(pool)
	hh := seedHousehold(t, pool)

	event := &domain.UsageEvent{
		ID: domain.NewUsageEventID(), HouseholdID: hh,
		TrackedItemID: domain.NewTrackedItemID(), Type: domain.UsageDepleted,
		OccurredAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := eventRepo.Append(testCtx(t), event); !errors.Is(err, domain.ErrTrackedItemNotFound) {
		t.Errorf("Append(unknown item) = %v, want ErrTrackedItemNotFound", err)
	}
}

func TestRestockPredictionUpsertAndGet(t *testing.T) {
	pool := newTestPool(t)
	itemRepo := adapter.NewTrackedItemRepository(pool)
	predRepo := adapter.NewRestockPredictionRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	item := seedTrackedItem(t, itemRepo, hh, "Coffee", 3)

	lastEvent := time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)
	pred := &domain.RestockPrediction{
		TrackedItemID:        item.ID,
		AvgIntervalDays:      14.5,
		LastEventAt:          lastEvent,
		PredictedDepletionOn: time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC),
		Confidence:           0.5,
	}
	if err := predRepo.Upsert(ctx, pred); err != nil {
		t.Fatalf("Upsert insert: %v", err)
	}

	got, err := predRepo.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AvgIntervalDays != 14.5 || got.Confidence != 0.5 {
		t.Errorf("Get floats = (avg %v, conf %v), want (14.5, 0.5)", got.AvgIntervalDays, got.Confidence)
	}
	if !got.PredictedDepletionOn.Equal(time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("PredictedDepletionOn = %v, want 2026-06-24", got.PredictedDepletionOn)
	}

	// Upsert again replaces the prior row.
	pred.AvgIntervalDays = 20
	pred.Confidence = 1
	pred.PredictedDepletionOn = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := predRepo.Upsert(ctx, pred); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	got, err = predRepo.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.AvgIntervalDays != 20 || got.Confidence != 1 {
		t.Errorf("after Upsert = (avg %v, conf %v), want (20, 1)", got.AvgIntervalDays, got.Confidence)
	}
}

func TestRestockPredictionGetNotFoundAndBadItem(t *testing.T) {
	pool := newTestPool(t)
	predRepo := adapter.NewRestockPredictionRepository(pool)
	ctx := testCtx(t)

	if _, err := predRepo.Get(ctx, domain.NewTrackedItemID()); !errors.Is(err, domain.ErrPredictionNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrPredictionNotFound", err)
	}
	bad := &domain.RestockPrediction{
		TrackedItemID: domain.NewTrackedItemID(), AvgIntervalDays: 1,
		LastEventAt:          time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PredictedDepletionOn: time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		Confidence:           0,
	}
	if err := predRepo.Upsert(ctx, bad); !errors.Is(err, domain.ErrTrackedItemNotFound) {
		t.Errorf("Upsert(bad item) = %v, want ErrTrackedItemNotFound", err)
	}
}

func TestListDueForRestockRespectsLeadWindow(t *testing.T) {
	pool := newTestPool(t)
	itemRepo := adapter.NewTrackedItemRepository(pool)
	predRepo := adapter.NewRestockPredictionRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)

	asOf := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)

	// Due: predicted depletion 2026-06-22 is within the 3-day lead window of asOf.
	due := seedTrackedItem(t, itemRepo, hh, "Due Soon", 3)
	upsertPred(t, predRepo, due.ID, time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC))
	// Not due: predicted depletion 2026-07-15 is well outside the 3-day window.
	notDue := seedTrackedItem(t, itemRepo, hh, "Later", 3)
	upsertPred(t, predRepo, notDue.ID, time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC))

	dueItems, err := itemRepo.ListDueForRestock(ctx, hh, asOf)
	if err != nil {
		t.Fatalf("ListDueForRestock: %v", err)
	}
	if len(dueItems) != 1 || dueItems[0].ID != due.ID {
		t.Errorf("ListDueForRestock = %+v, want only %q", dueItems, due.Name)
	}
}

func appendEvent(t *testing.T, repo *adapter.UsageEventRepository, hh household.HouseholdID, itemID domain.TrackedItemID, typ domain.UsageType, at time.Time, member *household.MemberID) {
	t.Helper()
	event := &domain.UsageEvent{
		ID: domain.NewUsageEventID(), HouseholdID: hh, TrackedItemID: itemID,
		Type: typ, OccurredAt: at, MemberID: member,
	}
	if err := repo.Append(testCtx(t), event); err != nil {
		t.Fatalf("Append %s event: %v", typ, err)
	}
}

func upsertPred(t *testing.T, repo *adapter.RestockPredictionRepository, itemID domain.TrackedItemID, depletionOn time.Time) {
	t.Helper()
	pred := &domain.RestockPrediction{
		TrackedItemID: itemID, AvgIntervalDays: 7,
		LastEventAt: depletionOn.AddDate(0, 0, -7), PredictedDepletionOn: depletionOn, Confidence: 0.8,
	}
	if err := repo.Upsert(testCtx(t), pred); err != nil {
		t.Fatalf("upsert prediction: %v", err)
	}
}
