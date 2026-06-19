package domain

import (
	"context"
	"errors"
	"time"
)

// ErrPredictionNotFound is returned when no cached prediction exists for an item.
var ErrPredictionNotFound = errors.New("tracking: restock prediction not found")

// RestockPrediction is the cached output of the prediction engine for one
// tracked item (one row per item). AvgIntervalDays is the mean number of days
// between observed depletion events; PredictedDepletionOn is LastEventAt plus
// that mean, as a calendar date. Confidence is in [0,1] and is 0 until at least
// two depletion intervals exist (see the NES-41 engine).
type RestockPrediction struct {
	TrackedItemID        TrackedItemID
	AvgIntervalDays      float64
	LastEventAt          time.Time
	PredictedDepletionOn time.Time
	Confidence           float64
	UpdatedAt            time.Time
}

// RestockPredictionRepository caches per-item predictions.
//
// Contracts:
//   - Upsert inserts or replaces the prediction for TrackedItemID and refreshes
//     UpdatedAt; re-running it overwrites the prior row.
//   - Get returns ErrPredictionNotFound when no prediction is cached for the item.
type RestockPredictionRepository interface {
	Upsert(ctx context.Context, prediction *RestockPrediction) error
	Get(ctx context.Context, trackedItemID TrackedItemID) (*RestockPrediction, error)
}
