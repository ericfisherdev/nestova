package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// ConfidenceThreshold is the number of observed depletion intervals at which a
// prediction's confidence reaches its maximum of 1. With fewer intervals the
// confidence scales linearly (intervals / ConfidenceThreshold).
const ConfidenceThreshold = 5

// hoursPerDay converts a day count to a time.Duration for date projection.
const hoursPerDay = 24 * time.Hour

// Predictor computes and caches a tracked item's restock prediction from its
// observed depletion history. It is deterministic: every input comes from the
// stored depletion events, so no method reads the wall clock.
type Predictor struct {
	events      domain.UsageEventRepository
	predictions domain.RestockPredictionRepository
}

// NewPredictor constructs a Predictor with injected repositories.
func NewPredictor(events domain.UsageEventRepository, predictions domain.RestockPredictionRepository) (*Predictor, error) {
	if events == nil {
		return nil, errors.New("app: NewPredictor requires a non-nil usage event repository")
	}
	if predictions == nil {
		return nil, errors.New("app: NewPredictor requires a non-nil prediction repository")
	}
	return &Predictor{events: events, predictions: predictions}, nil
}

// Recompute derives the restock prediction for trackedItemID from its depletion
// events and upserts it, returning the cached prediction.
//
// The estimate uses depletion events only (the repository returns just those):
// predicted depletion = last depletion + the mean interval between consecutive
// depletions, and confidence = min(1, intervals / ConfidenceThreshold) where
// intervals = (number of depletion events − 1).
//
// With fewer than two depletion events there is no interval to average, so no
// prediction is written and Recompute returns (nil, nil) — deliberately leaving
// no false prediction behind. Call this after each new depletion event.
func (p *Predictor) Recompute(ctx context.Context, trackedItemID domain.TrackedItemID) (*domain.RestockPrediction, error) {
	events, err := p.events.ListDepletionEvents(ctx, trackedItemID)
	if err != nil {
		return nil, fmt.Errorf("recompute prediction: list depletion events: %w", err)
	}
	if len(events) < 2 {
		return nil, nil
	}

	// Events are ordered ascending by occurrence (repository contract), so the
	// sum of consecutive intervals telescopes to (last − first).
	intervals := len(events) - 1
	lastEventAt := events[len(events)-1].OccurredAt
	totalSpan := lastEventAt.Sub(events[0].OccurredAt)
	meanIntervalDays := totalSpan.Hours() / 24 / float64(intervals)

	predictedDepletion := lastEventAt.Add(time.Duration(meanIntervalDays * float64(hoursPerDay)))

	prediction := &domain.RestockPrediction{
		TrackedItemID:        trackedItemID,
		AvgIntervalDays:      meanIntervalDays,
		LastEventAt:          lastEventAt,
		PredictedDepletionOn: dateOf(predictedDepletion),
		Confidence:           confidence(intervals),
	}
	if err := p.predictions.Upsert(ctx, prediction); err != nil {
		return nil, fmt.Errorf("recompute prediction: upsert: %w", err)
	}
	return prediction, nil
}

// confidence scales with the number of observed intervals, capped at 1.
func confidence(intervals int) float64 {
	c := float64(intervals) / float64(ConfidenceThreshold)
	if c > 1 {
		return 1
	}
	return c
}

// dateOf truncates a timestamp to its UTC calendar date, matching the date-typed
// predicted_depletion_on column.
func dateOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
