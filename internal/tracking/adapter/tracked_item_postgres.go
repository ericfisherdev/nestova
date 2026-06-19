package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// TrackedItemRepository is the pgx-backed domain.TrackedItemRepository. UUIDs are
// passed and scanned as text, matching the household and notify adapters.
type TrackedItemRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.TrackedItemRepository = (*TrackedItemRepository)(nil)

// NewTrackedItemRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewTrackedItemRepository(dbtx db.TX) *TrackedItemRepository {
	if dbtx == nil {
		panic("adapter: NewTrackedItemRepository requires a non-nil db.TX")
	}
	return &TrackedItemRepository{dbtx: dbtx}
}

// Create inserts a tracked item and populates its timestamps. It returns
// household.ErrHouseholdNotFound when HouseholdID does not exist.
func (r *TrackedItemRepository) Create(ctx context.Context, item *domain.TrackedItem) error {
	if item == nil {
		return errors.New("adapter: create tracked item: nil item")
	}
	const q = `
		INSERT INTO tracked_item (id, household_id, name, category, restock_lead_days, active)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		item.ID.String(), item.HouseholdID.String(), item.Name, item.Category,
		item.RestockLeadDays, item.Active,
	).Scan(&item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == trackedItemHouseholdFK {
			return household.ErrHouseholdNotFound
		}
		return fmt.Errorf("create tracked item: %w", err)
	}
	return nil
}

// Get returns the tracked item, or domain.ErrTrackedItemNotFound.
func (r *TrackedItemRepository) Get(ctx context.Context, id domain.TrackedItemID) (*domain.TrackedItem, error) {
	const q = `
		SELECT id, household_id, name, category, restock_lead_days, active, created_at, updated_at
		FROM tracked_item WHERE id = $1`
	item, err := scanTrackedItem(r.dbtx.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTrackedItemNotFound
		}
		return nil, fmt.Errorf("get tracked item: %w", err)
	}
	return item, nil
}

// Update rewrites the item's mutable fields and refreshes updated_at. It returns
// domain.ErrTrackedItemNotFound when the id is unknown.
func (r *TrackedItemRepository) Update(ctx context.Context, item *domain.TrackedItem) error {
	if item == nil {
		return errors.New("adapter: update tracked item: nil item")
	}
	const q = `
		UPDATE tracked_item
		   SET name = $2, category = $3, restock_lead_days = $4, active = $5, updated_at = now()
		 WHERE id = $1
		RETURNING updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		item.ID.String(), item.Name, item.Category, item.RestockLeadDays, item.Active,
	).Scan(&item.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrTrackedItemNotFound
		}
		return fmt.Errorf("update tracked item: %w", err)
	}
	return nil
}

// ListActiveByHousehold returns the household's active items ordered by name.
func (r *TrackedItemRepository) ListActiveByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.TrackedItem, error) {
	const q = `
		SELECT id, household_id, name, category, restock_lead_days, active, created_at, updated_at
		FROM tracked_item
		WHERE household_id = $1 AND active = true
		ORDER BY name, id`
	return r.queryItems(ctx, "list active tracked items", q, householdID.String())
}

// ListDueForRestock returns active items whose cached prediction puts predicted
// depletion within the item's lead window of asOf
// (predicted_depletion_on <= asOf::date + restock_lead_days).
func (r *TrackedItemRepository) ListDueForRestock(ctx context.Context, householdID household.HouseholdID, asOf time.Time) ([]*domain.TrackedItem, error) {
	const q = `
		SELECT ti.id, ti.household_id, ti.name, ti.category, ti.restock_lead_days,
		       ti.active, ti.created_at, ti.updated_at
		FROM tracked_item ti
		JOIN restock_prediction rp ON rp.tracked_item_id = ti.id
		WHERE ti.household_id = $1
		  AND ti.active = true
		  AND rp.predicted_depletion_on <= ($2::date + ti.restock_lead_days)
		ORDER BY rp.predicted_depletion_on, ti.id`
	return r.queryItems(ctx, "list due for restock", q, householdID.String(), asOf)
}

// queryItems runs a tracked-item SELECT and scans the rows. label prefixes any
// wrapped error.
func (r *TrackedItemRepository) queryItems(ctx context.Context, label, q string, args ...any) ([]*domain.TrackedItem, error) {
	rows, err := r.dbtx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer rows.Close()

	items := make([]*domain.TrackedItem, 0)
	for rows.Next() {
		item, err := scanTrackedItem(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", label, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return items, nil
}

func scanTrackedItem(r row) (*domain.TrackedItem, error) {
	var (
		item         domain.TrackedItem
		idStr, hhStr string
	)
	if err := r.Scan(&idStr, &hhStr, &item.Name, &item.Category,
		&item.RestockLeadDays, &item.Active, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseTrackedItemID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan tracked item: %w", err)
	}
	hhID, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("scan tracked item: %w", err)
	}
	item.ID, item.HouseholdID = id, hhID
	return &item, nil
}
