package app_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db/dbtest"
	"github.com/ericfisherdev/nestova/internal/tracking/adapter"
	"github.com/ericfisherdev/nestova/internal/tracking/app"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

var baseDay = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeEventRepo is an in-memory domain.UsageEventRepository for hermetic tests.
type fakeEventRepo struct {
	depletions []*domain.UsageEvent
}

func (f *fakeEventRepo) Append(context.Context, *domain.UsageEvent) error { return nil }

func (f *fakeEventRepo) ListDepletionEvents(context.Context, domain.TrackedItemID) ([]*domain.UsageEvent, error) {
	return f.depletions, nil
}

// fakePredictionRepo is an in-memory domain.RestockPredictionRepository.
type fakePredictionRepo struct {
	upserts  int
	upserted *domain.RestockPrediction
}

func (f *fakePredictionRepo) Upsert(_ context.Context, p *domain.RestockPrediction) error {
	f.upserts++
	f.upserted = p
	return nil
}

func (f *fakePredictionRepo) Get(context.Context, domain.TrackedItemID) (*domain.RestockPrediction, error) {
	if f.upserted == nil {
		return nil, domain.ErrPredictionNotFound
	}
	return f.upserted, nil
}

// depletionsAtDays builds depletion events at the given day offsets from baseDay.
func depletionsAtDays(days ...int) []*domain.UsageEvent {
	events := make([]*domain.UsageEvent, len(days))
	for i, d := range days {
		events[i] = &domain.UsageEvent{
			ID:         domain.NewUsageEventID(),
			Type:       domain.UsageDepleted,
			OccurredAt: baseDay.AddDate(0, 0, d),
		}
	}
	return events
}

func TestRecomputeFewerThanTwoEventsWritesNothing(t *testing.T) {
	for _, days := range [][]int{{}, {3}} {
		events := &fakeEventRepo{depletions: depletionsAtDays(days...)}
		preds := &fakePredictionRepo{}
		predictor := mustPredictor(t, events, preds)

		got, err := predictor.Recompute(context.Background(), domain.NewTrackedItemID())
		if err != nil {
			t.Fatalf("Recompute(%d events): %v", len(days), err)
		}
		if got != nil {
			t.Errorf("Recompute(%d events) = %+v, want nil (no false prediction)", len(days), got)
		}
		if preds.upserts != 0 {
			t.Errorf("Recompute(%d events) upserted %d predictions, want 0", len(days), preds.upserts)
		}
	}
}

func TestRecomputeMeanIntervalAndConfidence(t *testing.T) {
	tests := []struct {
		name           string
		days           []int
		wantMeanDays   float64
		wantDepletion  time.Time
		wantConfidence float64
	}{
		{
			name: "evenly spaced", days: []int{0, 10, 20},
			wantMeanDays: 10, wantDepletion: baseDay.AddDate(0, 0, 30),
			wantConfidence: 2.0 / app.ConfidenceThreshold, // 2 intervals
		},
		{
			name: "two events", days: []int{0, 7},
			wantMeanDays: 7, wantDepletion: baseDay.AddDate(0, 0, 14),
			wantConfidence: 1.0 / app.ConfidenceThreshold, // 1 interval
		},
		{
			name: "uneven intervals", days: []int{0, 10, 40},
			wantMeanDays: 20, wantDepletion: baseDay.AddDate(0, 0, 60),
			wantConfidence: 2.0 / app.ConfidenceThreshold,
		},
		{
			name: "confidence caps at one", days: []int{0, 5, 10, 15, 20, 25, 30}, // 6 intervals > threshold
			wantMeanDays: 5, wantDepletion: baseDay.AddDate(0, 0, 35),
			wantConfidence: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := &fakeEventRepo{depletions: depletionsAtDays(tt.days...)}
			preds := &fakePredictionRepo{}
			predictor := mustPredictor(t, events, preds)
			itemID := domain.NewTrackedItemID()

			got, err := predictor.Recompute(context.Background(), itemID)
			if err != nil {
				t.Fatalf("Recompute: %v", err)
			}
			if got == nil {
				t.Fatal("Recompute returned nil prediction")
			}
			if got.TrackedItemID != itemID {
				t.Errorf("TrackedItemID = %v, want %v", got.TrackedItemID, itemID)
			}
			if !approxEqual(got.AvgIntervalDays, tt.wantMeanDays) {
				t.Errorf("AvgIntervalDays = %v, want %v", got.AvgIntervalDays, tt.wantMeanDays)
			}
			if !got.PredictedDepletionOn.Equal(tt.wantDepletion) {
				t.Errorf("PredictedDepletionOn = %v, want %v", got.PredictedDepletionOn, tt.wantDepletion)
			}
			if !approxEqual(got.Confidence, tt.wantConfidence) {
				t.Errorf("Confidence = %v, want %v", got.Confidence, tt.wantConfidence)
			}
			if preds.upserted == nil || preds.upserts != 1 {
				t.Errorf("expected exactly one upsert, got %d", preds.upserts)
			}
		})
	}
}

func TestRecomputePersistsViaRepository(t *testing.T) {
	pool := newTestPool(t)
	ctx := testCtx(t)
	eventRepo := adapter.NewUsageEventRepository(pool)
	predRepo := adapter.NewRestockPredictionRepository(pool)
	predictor := mustPredictor(t, eventRepo, predRepo)

	hh := household.NewHouseholdID()
	if _, err := pool.Exec(ctx, `INSERT INTO household (id, name) VALUES ($1, $2)`, hh.String(), "H"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	itemID := domain.NewTrackedItemID()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tracked_item (id, household_id, name) VALUES ($1, $2, $3)`,
		itemID.String(), hh.String(), "Coffee"); err != nil {
		t.Fatalf("seed tracked item: %v", err)
	}

	// Three depletions 10 days apart, plus a non-depletion event the adapter must
	// exclude from the depletion history.
	for _, d := range []int{0, 10, 20} {
		appendDepletion(t, eventRepo, hh, itemID, d)
	}
	opened := &domain.UsageEvent{
		ID: domain.NewUsageEventID(), HouseholdID: hh, TrackedItemID: itemID,
		Type: domain.UsageOpened, OccurredAt: baseDay.AddDate(0, 0, 25),
	}
	if err := eventRepo.Append(ctx, opened); err != nil {
		t.Fatalf("append opened event: %v", err)
	}

	got, err := predictor.Recompute(ctx, itemID)
	if err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if got == nil || !approxEqual(got.AvgIntervalDays, 10) {
		t.Fatalf("Recompute = %+v, want mean 10 days (opened event must be ignored)", got)
	}

	persisted, err := predRepo.Get(ctx, itemID)
	if err != nil {
		t.Fatalf("Get persisted prediction: %v", err)
	}
	if !approxEqual(persisted.AvgIntervalDays, 10) ||
		!persisted.PredictedDepletionOn.Equal(baseDay.AddDate(0, 0, 30)) ||
		!approxEqual(persisted.Confidence, 2.0/app.ConfidenceThreshold) {
		t.Errorf("persisted = %+v, want mean 10, depletion 2026-01-31, confidence %v",
			persisted, 2.0/app.ConfidenceThreshold)
	}
}

func mustPredictor(t *testing.T, events domain.UsageEventRepository, preds domain.RestockPredictionRepository) *app.Predictor {
	t.Helper()
	p, err := app.NewPredictor(events, preds)
	if err != nil {
		t.Fatalf("NewPredictor: %v", err)
	}
	return p
}

func appendDepletion(t *testing.T, repo *adapter.UsageEventRepository, hh household.HouseholdID, itemID domain.TrackedItemID, dayOffset int) {
	t.Helper()
	event := &domain.UsageEvent{
		ID: domain.NewUsageEventID(), HouseholdID: hh, TrackedItemID: itemID,
		Type: domain.UsageDepleted, OccurredAt: baseDay.AddDate(0, 0, dayOffset),
	}
	if err := repo.Append(testCtx(t), event); err != nil {
		t.Fatalf("append depletion: %v", err)
	}
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// newTestPool returns a pool against this package's own derived database
// (NES-149), freshly reset and migrated. dbtest.NewIsolatedPool owns the
// safety rail, the on-demand CREATE DATABASE, and the reset/migrate
// lifecycle; the per-package database is what lets gated packages run
// concurrently without resetting each other's schema mid-test.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.NewIsolatedPool(t, "trackingapp")
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}
