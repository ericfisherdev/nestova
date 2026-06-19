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

// PantryRepository is the pgx-backed domain.PantryRepository. The Quantity value
// object maps to the numeric quantity + text unit columns; numerics round-trip
// via ::float8. UUIDs are passed and scanned as text.
type PantryRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.PantryRepository = (*PantryRepository)(nil)

// NewPantryRepository constructs the repository with an injected query executor
// (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewPantryRepository(dbtx db.TX) *PantryRepository {
	if dbtx == nil {
		panic("adapter: NewPantryRepository requires a non-nil db.TX")
	}
	return &PantryRepository{dbtx: dbtx}
}

// Create inserts a pantry item and populates its timestamps. It maps FK
// violations to household.ErrHouseholdNotFound and domain.ErrIngredientNotFound.
func (r *PantryRepository) Create(ctx context.Context, item *domain.PantryItem) error {
	if item == nil {
		return errors.New("adapter: create pantry item: nil item")
	}
	const q = `
		INSERT INTO pantry_item (id, household_id, ingredient_id, quantity, unit, expires_on)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`
	err := r.dbtx.QueryRow(ctx, q,
		item.ID.String(), item.HouseholdID.String(), item.IngredientID.String(),
		item.Quantity.Amount, item.Quantity.Unit.String(), item.ExpiresOn,
	).Scan(&item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			switch pgErr.ConstraintName {
			case pantryItemHouseholdFK:
				return household.ErrHouseholdNotFound
			case pantryItemIngredientFK:
				return domain.ErrIngredientNotFound
			}
		}
		return fmt.Errorf("create pantry item: %w", err)
	}
	return nil
}

// Get returns the pantry item, or domain.ErrPantryItemNotFound.
func (r *PantryRepository) Get(ctx context.Context, id domain.PantryItemID) (*domain.PantryItem, error) {
	const q = `
		SELECT id, household_id, ingredient_id, quantity::float8, unit, expires_on, created_at, updated_at
		FROM pantry_item WHERE id = $1`
	item, err := scanPantryItem(r.dbtx.QueryRow(ctx, q, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPantryItemNotFound
		}
		return nil, fmt.Errorf("get pantry item: %w", err)
	}
	return item, nil
}

// Adjust increases the item's on-hand quantity by delta (e.g. a restock).
func (r *PantryRepository) Adjust(ctx context.Context, id domain.PantryItemID, delta household.Quantity) (*domain.PantryItem, error) {
	return r.mutateQuantity(ctx, id, func(current household.Quantity) (household.Quantity, error) {
		return current.Add(delta)
	})
}

// Consume decreases the item's on-hand quantity by amount (e.g. using some up).
func (r *PantryRepository) Consume(ctx context.Context, id domain.PantryItemID, amount household.Quantity) (*domain.PantryItem, error) {
	return r.mutateQuantity(ctx, id, func(current household.Quantity) (household.Quantity, error) {
		return current.Subtract(amount)
	})
}

// mutateQuantity loads the item FOR UPDATE, applies op to its quantity, and
// writes the result — all in one transaction so the row lock serializes
// concurrent Adjust/Consume calls and no update is lost. op carries the shared
// Quantity arithmetic, so a unit mismatch or below-zero result is returned
// unchanged (ErrUnitMismatch / ErrInvalidQuantity) and the row is left intact.
func (r *PantryRepository) mutateQuantity(ctx context.Context, id domain.PantryItemID, op func(household.Quantity) (household.Quantity, error)) (*domain.PantryItem, error) {
	beginner, ok := r.dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return nil, errors.New("adapter: pantry mutate: executor does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("mutate pantry quantity: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectQ = `
		SELECT id, household_id, ingredient_id, quantity::float8, unit, expires_on, created_at, updated_at
		FROM pantry_item WHERE id = $1 FOR UPDATE`
	item, err := scanPantryItem(tx.QueryRow(ctx, selectQ, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPantryItemNotFound
		}
		return nil, fmt.Errorf("mutate pantry quantity: load: %w", err)
	}

	updated, err := op(item.Quantity)
	if err != nil {
		return nil, err
	}

	// Only the amount changes: Quantity.Add/Subtract preserve the receiver's unit
	// (and reject a mismatched operand), so the stored unit never needs rewriting.
	const updateQ = `
		UPDATE pantry_item SET quantity = $2, updated_at = now()
		WHERE id = $1 RETURNING updated_at`
	if err := tx.QueryRow(ctx, updateQ, id.String(), updated.Amount).Scan(&item.UpdatedAt); err != nil {
		return nil, fmt.Errorf("mutate pantry quantity: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("mutate pantry quantity: commit: %w", err)
	}
	item.Quantity = updated
	return item, nil
}

// ListByHousehold returns the household's pantry items ordered by creation.
func (r *PantryRepository) ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.PantryItem, error) {
	const q = `
		SELECT id, household_id, ingredient_id, quantity::float8, unit, expires_on, created_at, updated_at
		FROM pantry_item
		WHERE household_id = $1
		ORDER BY created_at, id`
	return r.queryItems(ctx, "list pantry items", q, householdID.String())
}

// ListExpiringWithin returns items whose expiry is on or before asOf + days,
// ordered by expiry ascending.
func (r *PantryRepository) ListExpiringWithin(ctx context.Context, householdID household.HouseholdID, asOf time.Time, days int) ([]*domain.PantryItem, error) {
	const q = `
		SELECT id, household_id, ingredient_id, quantity::float8, unit, expires_on, created_at, updated_at
		FROM pantry_item
		WHERE household_id = $1
		  AND expires_on IS NOT NULL
		  AND expires_on >= $2::date
		  AND expires_on <= ($2::date + $3::int)
		ORDER BY expires_on, id`
	return r.queryItems(ctx, "list expiring pantry items", q, householdID.String(), asOf, days)
}

func (r *PantryRepository) queryItems(ctx context.Context, label, q string, args ...any) ([]*domain.PantryItem, error) {
	rows, err := r.dbtx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer rows.Close()

	items := make([]*domain.PantryItem, 0)
	for rows.Next() {
		item, err := scanPantryItem(rows)
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

func scanPantryItem(r row) (*domain.PantryItem, error) {
	var (
		item                       domain.PantryItem
		idStr, hhStr, ingStr, unit string
		amount                     float64
		expiresOn                  *time.Time
	)
	if err := r.Scan(&idStr, &hhStr, &ingStr, &amount, &unit, &expiresOn,
		&item.CreatedAt, &item.UpdatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParsePantryItemID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan pantry item: %w", err)
	}
	hhID, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("scan pantry item: %w", err)
	}
	ingID, err := domain.ParseIngredientID(ingStr)
	if err != nil {
		return nil, fmt.Errorf("scan pantry item: %w", err)
	}
	parsedUnit, err := household.ParseUnit(unit)
	if err != nil {
		return nil, fmt.Errorf("scan pantry item: %w", err)
	}
	item.ID, item.HouseholdID, item.IngredientID = id, hhID, ingID
	item.Quantity = household.Quantity{Amount: amount, Unit: parsedUnit}
	item.ExpiresOn = expiresOn
	return &item, nil
}
