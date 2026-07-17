package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// shoppingListColumns is the shared SELECT/RETURNING column list (quantity cast
// to float8 so the numeric maps cleanly to the Quantity's float64 amount).
const shoppingListColumns = `id, household_id, ingredient_id, name, quantity::float8, unit, source, status, added_by, created_at`

// ShoppingListRepository is the pgx-backed domain.ShoppingListRepository.
type ShoppingListRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.ShoppingListRepository = (*ShoppingListRepository)(nil)

// NewShoppingListRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewShoppingListRepository(dbtx db.TX) *ShoppingListRepository {
	if dbtx == nil {
		panic("adapter: NewShoppingListRepository requires a non-nil db.TX")
	}
	return &ShoppingListRepository{dbtx: dbtx}
}

// Add inserts a shopping-list item and populates CreatedAt. It maps FK
// violations to household.ErrHouseholdNotFound, domain.ErrIngredientNotFound, and
// household.ErrMemberNotFound.
func (r *ShoppingListRepository) Add(ctx context.Context, item *domain.ShoppingListItem) error {
	if item == nil {
		return errors.New("adapter: add shopping list item: nil item")
	}
	const q = `
		INSERT INTO shopping_list_item
		    (id, household_id, ingredient_id, name, quantity, unit, source, status, added_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at`
	if err := r.dbtx.QueryRow(ctx, q, r.insertArgs(item)...).Scan(&item.CreatedAt); err != nil {
		return r.mapInsertError("add shopping list item", err)
	}
	return nil
}

// AddRestockIfAbsent inserts a system restock item only when no open
// (non-purchased) restock entry already exists for its (household, ingredient),
// reporting whether a new row was inserted. It relies on the partial unique index
// shopping_list_item_open_restock_uniq, so it requires Source == SourceRestock
// and a non-nil IngredientID.
func (r *ShoppingListRepository) AddRestockIfAbsent(ctx context.Context, item *domain.ShoppingListItem) (bool, error) {
	if item == nil {
		return false, errors.New("adapter: add restock item: nil item")
	}
	if item.Source != domain.SourceRestock || item.IngredientID == nil {
		return false, errors.New("adapter: add restock item: requires restock source and an ingredient id")
	}
	const q = `
		INSERT INTO shopping_list_item
		    (id, household_id, ingredient_id, name, quantity, unit, source, status, added_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (household_id, ingredient_id) WHERE source = 'restock' AND status <> 'purchased'
		DO NOTHING
		RETURNING created_at`
	err := r.dbtx.QueryRow(ctx, q, r.insertArgs(item)...).Scan(&item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// An open restock entry already exists; nothing inserted.
		return false, nil
	}
	if err != nil {
		return false, r.mapInsertError("add restock item", err)
	}
	return true, nil
}

// AddMealPlanIfAbsent inserts a meal-plan-sourced item only when no open
// (non-purchased) meal_plan entry already exists for its (household, ingredient,
// unit), reporting whether a new row was inserted. It relies on the partial unique
// index shopping_list_item_open_meal_plan_uniq, so it requires Source ==
// SourceMealPlan and a non-nil IngredientID. The unit is part of the key so the
// same ingredient in different units (kept as separate aggregated lines) can
// coexist while each line still de-duplicates.
func (r *ShoppingListRepository) AddMealPlanIfAbsent(ctx context.Context, item *domain.ShoppingListItem) (bool, error) {
	if item == nil {
		return false, errors.New("adapter: add meal plan item: nil item")
	}
	if item.Source != domain.SourceMealPlan || item.IngredientID == nil {
		return false, errors.New("adapter: add meal plan item: requires meal_plan source and an ingredient id")
	}
	const q = `
		INSERT INTO shopping_list_item
		    (id, household_id, ingredient_id, name, quantity, unit, source, status, added_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (household_id, ingredient_id, unit) WHERE source = 'meal_plan' AND status <> 'purchased'
		DO NOTHING
		RETURNING created_at`
	err := r.dbtx.QueryRow(ctx, q, r.insertArgs(item)...).Scan(&item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// An open meal_plan entry already exists; nothing inserted.
		return false, nil
	}
	if err != nil {
		return false, r.mapInsertError("add meal plan item", err)
	}
	return true, nil
}

// UpdateStatus transitions an item's status and returns the updated item, or
// domain.ErrShoppingListItemNotFound when the id is unknown in the household. The
// household scope stops a member transitioning another household's item by id.
func (r *ShoppingListRepository) UpdateStatus(ctx context.Context, householdID household.HouseholdID, id domain.ShoppingListItemID, status domain.ItemStatus) (*domain.ShoppingListItem, error) {
	const q = `UPDATE shopping_list_item SET status = $2 WHERE id = $1 AND household_id = $3 RETURNING ` + shoppingListColumns
	item, err := scanShoppingListItem(r.dbtx.QueryRow(ctx, q, id.String(), status.String(), householdID.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrShoppingListItemNotFound
		}
		return nil, fmt.Errorf("update shopping list item status: %w", err)
	}
	return item, nil
}

// MarkInCart transitions an item from needed to in_cart, or no-ops (returns
// the item unchanged) if it is already in_cart — both are captured by the
// single guarded UPDATE below, which only ever matches a row whose CURRENT
// status is needed or in_cart. If zero rows match, a follow-up existence
// check distinguishes an unknown/cross-household id
// (domain.ErrShoppingListItemNotFound) from an item that exists but is past
// the point where "in cart" still applies, i.e. already purchased
// (domain.ErrShoppingListItemNotInCartable) — the item is never moved
// backward out of purchased.
func (r *ShoppingListRepository) MarkInCart(ctx context.Context, householdID household.HouseholdID, id domain.ShoppingListItemID) (*domain.ShoppingListItem, error) {
	const q = `
		UPDATE shopping_list_item SET status = 'in_cart'
		 WHERE id = $1 AND household_id = $2 AND status IN ('needed', 'in_cart')
		RETURNING ` + shoppingListColumns
	item, err := scanShoppingListItem(r.dbtx.QueryRow(ctx, q, id.String(), householdID.String()))
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("mark shopping list item in cart: %w", err)
	}

	var exists bool
	existsErr := r.dbtx.QueryRow(ctx,
		`SELECT true FROM shopping_list_item WHERE id = $1 AND household_id = $2`,
		id.String(), householdID.String(),
	).Scan(&exists)
	switch {
	case errors.Is(existsErr, pgx.ErrNoRows):
		return nil, domain.ErrShoppingListItemNotFound
	case existsErr != nil:
		return nil, fmt.Errorf("mark shopping list item in cart: check existence: %w", existsErr)
	default:
		return nil, domain.ErrShoppingListItemNotInCartable
	}
}

// ListByStatus returns the household's items in the given status ordered by
// creation.
func (r *ShoppingListRepository) ListByStatus(ctx context.Context, householdID household.HouseholdID, status domain.ItemStatus) ([]*domain.ShoppingListItem, error) {
	const q = `SELECT ` + shoppingListColumns + `
		FROM shopping_list_item
		WHERE household_id = $1 AND status = $2
		ORDER BY created_at, id`
	rows, err := r.dbtx.Query(ctx, q, householdID.String(), status.String())
	if err != nil {
		return nil, fmt.Errorf("list shopping list items: %w", err)
	}
	defer rows.Close()

	items := make([]*domain.ShoppingListItem, 0)
	for rows.Next() {
		item, err := scanShoppingListItem(rows)
		if err != nil {
			return nil, fmt.Errorf("list shopping list items: scan: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list shopping list items: %w", err)
	}
	return items, nil
}

// insertArgs builds the positional arguments shared by Add and AddRestockIfAbsent.
func (r *ShoppingListRepository) insertArgs(item *domain.ShoppingListItem) []any {
	var ingredientID *string
	if item.IngredientID != nil {
		s := item.IngredientID.String()
		ingredientID = &s
	}
	var name *string
	if item.Name != "" {
		name = &item.Name
	}
	var addedBy *string
	if item.AddedBy != nil {
		s := item.AddedBy.String()
		addedBy = &s
	}
	return []any{
		item.ID.String(), item.HouseholdID.String(), ingredientID, name,
		item.Quantity.Amount, item.Quantity.Unit.String(), item.Source.String(),
		item.Status.String(), addedBy,
	}
}

// mapInsertError translates FK violations on insert to domain sentinels.
func (r *ShoppingListRepository) mapInsertError(label string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		switch pgErr.ConstraintName {
		case shoppingListItemHouseholdFK:
			return household.ErrHouseholdNotFound
		case shoppingListItemIngredientFK:
			return domain.ErrIngredientNotFound
		case shoppingListItemAddedByFK:
			return household.ErrMemberNotFound
		}
	}
	return fmt.Errorf("%s: %w", label, err)
}

func scanShoppingListItem(r row) (*domain.ShoppingListItem, error) {
	var (
		item                             domain.ShoppingListItem
		idStr, hhStr, unit, source, stat string
		ingredientID, name, addedBy      *string
	)
	if err := r.Scan(&idStr, &hhStr, &ingredientID, &name, &item.Quantity.Amount,
		&unit, &source, &stat, &addedBy, &item.CreatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseShoppingListItemID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan shopping list item: %w", err)
	}
	hhID, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("scan shopping list item: %w", err)
	}
	parsedUnit, err := household.ParseUnit(unit)
	if err != nil {
		return nil, fmt.Errorf("scan shopping list item: %w", err)
	}
	src, err := domain.ParseItemSource(source)
	if err != nil {
		return nil, fmt.Errorf("scan shopping list item: %w", err)
	}
	status, err := domain.ParseItemStatus(stat)
	if err != nil {
		return nil, fmt.Errorf("scan shopping list item: %w", err)
	}
	item.ID, item.HouseholdID, item.Quantity.Unit, item.Source, item.Status = id, hhID, parsedUnit, src, status

	if ingredientID != nil {
		ingID, err := domain.ParseIngredientID(*ingredientID)
		if err != nil {
			return nil, fmt.Errorf("scan shopping list item: %w", err)
		}
		item.IngredientID = &ingID
	}
	if name != nil {
		item.Name = *name
	}
	if addedBy != nil {
		mid, err := household.ParseMemberID(*addedBy)
		if err != nil {
			return nil, fmt.Errorf("scan shopping list item: %w", err)
		}
		item.AddedBy = &mid
	}
	return &item, nil
}
