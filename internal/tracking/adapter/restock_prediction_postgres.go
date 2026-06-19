package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// RestockPredictionRepository is the pgx-backed
// domain.RestockPredictionRepository.
type RestockPredictionRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.RestockPredictionRepository = (*RestockPredictionRepository)(nil)

// NewRestockPredictionRepository constructs the repository with an injected
// query executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewRestockPredictionRepository(dbtx db.TX) *RestockPredictionRepository {
	if dbtx == nil {
		panic("adapter: NewRestockPredictionRepository requires a non-nil db.TX")
	}
	return &RestockPredictionRepository{dbtx: dbtx}
}

// Upsert inserts or replaces the prediction for the tracked item and refreshes
// updated_at. It returns domain.ErrTrackedItemNotFound when the item does not
// exist. avg_interval_days and confidence are written from float64 and read back
// via ::float8 so the numeric columns map cleanly to Go floats.
func (r *RestockPredictionRepository) Upsert(ctx context.Context, prediction *domain.RestockPrediction) error {
	if prediction == nil {
		return errors.New("adapter: upsert restock prediction: nil prediction")
	}
	const q = `
		INSERT INTO restock_prediction
		    (tracked_item_id, avg_interval_days, last_event_at, predicted_depletion_on, confidence)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tracked_item_id) DO UPDATE
		    SET avg_interval_days      = EXCLUDED.avg_interval_days,
		        last_event_at          = EXCLUDED.last_event_at,
		        predicted_depletion_on = EXCLUDED.predicted_depletion_on,
		        confidence             = EXCLUDED.confidence,
		        updated_at             = now()
		RETURNING updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		prediction.TrackedItemID.String(), prediction.AvgIntervalDays,
		prediction.LastEventAt, prediction.PredictedDepletionOn, prediction.Confidence,
	).Scan(&prediction.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == restockPredictionTrackedItemFK {
			return domain.ErrTrackedItemNotFound
		}
		return fmt.Errorf("upsert restock prediction: %w", err)
	}
	return nil
}

// Get returns the cached prediction, or domain.ErrPredictionNotFound.
func (r *RestockPredictionRepository) Get(ctx context.Context, trackedItemID domain.TrackedItemID) (*domain.RestockPrediction, error) {
	const q = `
		SELECT tracked_item_id, avg_interval_days::float8, last_event_at,
		       predicted_depletion_on, confidence::float8, updated_at
		FROM restock_prediction WHERE tracked_item_id = $1`
	prediction, err := scanRestockPrediction(r.dbtx.QueryRow(ctx, q, trackedItemID.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPredictionNotFound
		}
		return nil, fmt.Errorf("get restock prediction: %w", err)
	}
	return prediction, nil
}

func scanRestockPrediction(r row) (*domain.RestockPrediction, error) {
	var (
		prediction domain.RestockPrediction
		itemStr    string
	)
	if err := r.Scan(&itemStr, &prediction.AvgIntervalDays, &prediction.LastEventAt,
		&prediction.PredictedDepletionOn, &prediction.Confidence, &prediction.UpdatedAt); err != nil {
		return nil, err
	}
	itemID, err := domain.ParseTrackedItemID(itemStr)
	if err != nil {
		return nil, fmt.Errorf("scan restock prediction: %w", err)
	}
	prediction.TrackedItemID = itemID
	return &prediction, nil
}
