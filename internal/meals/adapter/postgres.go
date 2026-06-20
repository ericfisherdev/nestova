package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// row is the minimal surface shared by pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

// recipeColumns is the shared recipe SELECT column list (household_id and
// external_ref are nullable: external/cached recipes carry no household).
const recipeColumns = `id, household_id, title, source, external_ref, servings, instructions`

// RecipeRepository is the pgx-backed domain.RecipeRepository. A recipe and its
// ingredient lines are written together in one transaction; UUIDs are passed and
// scanned as text and the numeric quantity round-trips via ::float8.
type RecipeRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.RecipeRepository = (*RecipeRepository)(nil)

// NewRecipeRepository constructs the repository with an injected query executor
// (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewRecipeRepository(dbtx db.TX) *RecipeRepository {
	if dbtx == nil {
		panic("adapter: NewRecipeRepository requires a non-nil db.TX")
	}
	return &RecipeRepository{dbtx: dbtx}
}

// Create inserts a box recipe and its ingredient lines atomically. FK violations
// map to household.ErrHouseholdNotFound (unknown household) and
// tracking.ErrIngredientNotFound (unknown ingredient).
func (r *RecipeRepository) Create(ctx context.Context, recipe *domain.Recipe) error {
	if recipe == nil {
		return errors.New("adapter: create recipe: nil recipe")
	}
	return r.inTx(ctx, "create recipe", func(tx pgx.Tx) error {
		if err := insertRecipe(ctx, tx, recipe); err != nil {
			return err
		}
		return insertRecipeIngredients(ctx, tx, recipe)
	})
}

// Update rewrites a box recipe's fields and replaces its ingredient set
// atomically, returning domain.ErrRecipeNotFound when the id is unknown in the
// household.
func (r *RecipeRepository) Update(ctx context.Context, recipe *domain.Recipe) error {
	if recipe == nil {
		return errors.New("adapter: update recipe: nil recipe")
	}
	return r.inTx(ctx, "update recipe", func(tx pgx.Tx) error {
		const q = `
			UPDATE recipe SET title = $3, servings = $4, instructions = $5, updated_at = now()
			WHERE id = $1 AND household_id = $2`
		tag, err := tx.Exec(ctx, q, recipe.ID.String(), householdIDArg(recipe.HouseholdID),
			recipe.Title, recipe.Servings, recipe.Instructions)
		if err != nil {
			return fmt.Errorf("update recipe: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrRecipeNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recipe_ingredient WHERE recipe_id = $1`, recipe.ID.String()); err != nil {
			return fmt.Errorf("update recipe: clear ingredients: %w", err)
		}
		return insertRecipeIngredients(ctx, tx, recipe)
	})
}

// UpsertExternal inserts or refreshes an external/cached recipe keyed by
// ExternalRef and replaces its ingredient lines, returning the stored recipe (its
// ID is the existing row's on conflict). FK violations on the lines map to
// tracking.ErrIngredientNotFound.
func (r *RecipeRepository) UpsertExternal(ctx context.Context, recipe *domain.Recipe) (*domain.Recipe, error) {
	if recipe == nil {
		return nil, errors.New("adapter: upsert external recipe: nil recipe")
	}
	if recipe.ExternalRef == nil || *recipe.ExternalRef == "" {
		return nil, errors.New("adapter: upsert external recipe: requires a non-empty external_ref (the cache key)")
	}
	err := r.inTx(ctx, "upsert external recipe", func(tx pgx.Tx) error {
		const q = `
			INSERT INTO recipe (id, household_id, title, source, external_ref, servings, instructions)
			VALUES ($1, NULL, $2, 'external', $3, $4, $5)
			ON CONFLICT (external_ref) DO UPDATE
			SET title = excluded.title, servings = excluded.servings,
			    instructions = excluded.instructions, updated_at = now()
			RETURNING id`
		var idStr string
		if err := tx.QueryRow(ctx, q, recipe.ID.String(), recipe.Title, recipe.ExternalRef,
			recipe.Servings, recipe.Instructions).Scan(&idStr); err != nil {
			return mapRecipeWriteError("upsert external recipe", err)
		}
		id, err := domain.ParseRecipeID(idStr)
		if err != nil {
			return fmt.Errorf("upsert external recipe: %w", err)
		}
		recipe.ID = id
		if _, err := tx.Exec(ctx, `DELETE FROM recipe_ingredient WHERE recipe_id = $1`, id.String()); err != nil {
			return fmt.Errorf("upsert external recipe: clear ingredients: %w", err)
		}
		return insertRecipeIngredients(ctx, tx, recipe)
	})
	if err != nil {
		return nil, err
	}
	return recipe, nil
}

// Get returns a household-owned recipe with its ingredient lines, or
// domain.ErrRecipeNotFound when the id is unknown in the household.
func (r *RecipeRepository) Get(ctx context.Context, householdID household.HouseholdID, id domain.RecipeID) (*domain.Recipe, error) {
	const q = `SELECT ` + recipeColumns + ` FROM recipe WHERE id = $1 AND household_id = $2`
	recipe, err := scanRecipe(r.dbtx.QueryRow(ctx, q, id.String(), householdID.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRecipeNotFound
		}
		return nil, fmt.Errorf("get recipe: %w", err)
	}
	lines, err := r.loadIngredients(ctx, []string{id.String()})
	if err != nil {
		return nil, fmt.Errorf("get recipe: %w", err)
	}
	recipe.Ingredients = lines[id.String()]
	return recipe, nil
}

// Delete removes a household-owned recipe (its lines and any planned slots cascade
// away), returning domain.ErrRecipeNotFound when the id is unknown in the household.
func (r *RecipeRepository) Delete(ctx context.Context, householdID household.HouseholdID, id domain.RecipeID) error {
	tag, err := r.dbtx.Exec(ctx, `DELETE FROM recipe WHERE id = $1 AND household_id = $2`,
		id.String(), householdID.String())
	if err != nil {
		return fmt.Errorf("delete recipe: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrRecipeNotFound
	}
	return nil
}

// ListByHousehold returns the household's box recipes (with their ingredient
// lines) ordered by title, or an empty slice when none exist.
func (r *RecipeRepository) ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.Recipe, error) {
	recipes, ids, err := r.queryRecipes(ctx,
		`SELECT `+recipeColumns+` FROM recipe WHERE household_id = $1 ORDER BY title, id`,
		householdID.String())
	if err != nil {
		return nil, err
	}
	lines, err := r.loadIngredients(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list recipes: %w", err)
	}
	for _, rec := range recipes {
		rec.Ingredients = lines[rec.ID.String()]
	}
	return recipes, nil
}

// queryRecipes runs a recipe query and fully drains it (releasing the connection
// before any follow-up ingredient load), returning the recipes and their ids.
func (r *RecipeRepository) queryRecipes(ctx context.Context, q string, args ...any) ([]*domain.Recipe, []string, error) {
	rows, err := r.dbtx.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list recipes: %w", err)
	}
	defer rows.Close()

	recipes := make([]*domain.Recipe, 0)
	ids := make([]string, 0)
	for rows.Next() {
		rec, err := scanRecipe(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("list recipes: scan: %w", err)
		}
		recipes = append(recipes, rec)
		ids = append(ids, rec.ID.String())
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list recipes: %w", err)
	}
	return recipes, ids, nil
}

// loadIngredients batch-loads the ingredient lines for the given recipe ids,
// grouped by recipe-id string.
func (r *RecipeRepository) loadIngredients(ctx context.Context, recipeIDs []string) (map[string][]domain.RecipeIngredient, error) {
	result := make(map[string][]domain.RecipeIngredient, len(recipeIDs))
	if len(recipeIDs) == 0 {
		return result, nil
	}
	const q = `
		SELECT recipe_id, ingredient_id, quantity::float8, unit, optional
		FROM recipe_ingredient
		WHERE recipe_id = ANY($1::uuid[])
		ORDER BY recipe_id, ingredient_id`
	rows, err := r.dbtx.Query(ctx, q, recipeIDs)
	if err != nil {
		return nil, fmt.Errorf("load recipe ingredients: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var recipeIDStr, ingStr, unit string
		var amount float64
		var optional bool
		if err := rows.Scan(&recipeIDStr, &ingStr, &amount, &unit, &optional); err != nil {
			return nil, fmt.Errorf("load recipe ingredients: scan: %w", err)
		}
		ingID, err := tracking.ParseIngredientID(ingStr)
		if err != nil {
			return nil, fmt.Errorf("load recipe ingredients: %w", err)
		}
		parsedUnit, err := household.ParseUnit(unit)
		if err != nil {
			return nil, fmt.Errorf("load recipe ingredients: %w", err)
		}
		result[recipeIDStr] = append(result[recipeIDStr], domain.RecipeIngredient{
			IngredientID: ingID,
			Quantity:     household.Quantity{Amount: amount, Unit: parsedUnit},
			Optional:     optional,
		})
	}
	return result, rows.Err()
}

// inTx runs fn inside a transaction, committing on success and rolling back on
// error (the deferred rollback is a no-op after a successful commit).
func (r *RecipeRepository) inTx(ctx context.Context, label string, fn func(pgx.Tx) error) error {
	beginner, ok := r.dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("adapter: %s: executor does not support transactions", label)
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", label, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: commit: %w", label, err)
	}
	return nil
}

func insertRecipe(ctx context.Context, q db.TX, recipe *domain.Recipe) error {
	const sql = `
		INSERT INTO recipe (id, household_id, title, source, external_ref, servings, instructions)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := q.Exec(ctx, sql, recipe.ID.String(), householdIDArg(recipe.HouseholdID),
		recipe.Title, recipe.Source.String(), recipe.ExternalRef, recipe.Servings, recipe.Instructions)
	return mapRecipeWriteError("insert recipe", err)
}

func insertRecipeIngredients(ctx context.Context, q db.TX, recipe *domain.Recipe) error {
	const sql = `
		INSERT INTO recipe_ingredient (recipe_id, ingredient_id, quantity, unit, optional)
		VALUES ($1, $2, $3, $4, $5)`
	for _, line := range recipe.Ingredients {
		_, err := q.Exec(ctx, sql, recipe.ID.String(), line.IngredientID.String(),
			line.Quantity.Amount, line.Quantity.Unit.String(), line.Optional)
		if err != nil {
			return mapRecipeWriteError("insert recipe ingredient", err)
		}
	}
	return nil
}

// mapRecipeWriteError translates FK violations on recipe/recipe_ingredient writes
// to domain sentinels.
func mapRecipeWriteError(label string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		switch pgErr.ConstraintName {
		case recipeHouseholdFK:
			return household.ErrHouseholdNotFound
		case recipeIngredientIngredientFK:
			return tracking.ErrIngredientNotFound
		case recipeIngredientRecipeFK:
			return domain.ErrRecipeNotFound
		}
	}
	return fmt.Errorf("%s: %w", label, err)
}

func scanRecipe(r row) (*domain.Recipe, error) {
	var (
		recipe                      domain.Recipe
		idStr, title, source        string
		instructions                string
		householdIDStr, externalRef *string
	)
	if err := r.Scan(&idStr, &householdIDStr, &title, &source, &externalRef,
		&recipe.Servings, &instructions); err != nil {
		return nil, err
	}
	id, err := domain.ParseRecipeID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan recipe: %w", err)
	}
	src, err := domain.ParseRecipeSourceKind(source)
	if err != nil {
		return nil, fmt.Errorf("scan recipe: %w", err)
	}
	recipe.ID, recipe.Title, recipe.Source, recipe.Instructions = id, title, src, instructions
	recipe.ExternalRef = externalRef
	if householdIDStr != nil {
		hid, err := household.ParseHouseholdID(*householdIDStr)
		if err != nil {
			return nil, fmt.Errorf("scan recipe: %w", err)
		}
		recipe.HouseholdID = &hid
	}
	return &recipe, nil
}

// householdIDArg renders an optional household id as a nullable text argument.
func householdIDArg(id *household.HouseholdID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// MealPlanRepository is the pgx-backed domain.MealPlanRepository. A slot is keyed
// by (household, calendar date, meal); dates are bound as UTC calendar dates.
type MealPlanRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.MealPlanRepository = (*MealPlanRepository)(nil)

// NewMealPlanRepository constructs the repository with an injected query executor.
func NewMealPlanRepository(dbtx db.TX) *MealPlanRepository {
	if dbtx == nil {
		panic("adapter: NewMealPlanRepository requires a non-nil db.TX")
	}
	return &MealPlanRepository{dbtx: dbtx}
}

// Upsert assigns the recipe to the (household, date, meal) slot, replacing any
// existing entry in that slot (its id and created_at are preserved). FK violations
// map to household.ErrHouseholdNotFound and domain.ErrRecipeNotFound (the recipe
// is not a box recipe of this household).
func (r *MealPlanRepository) Upsert(ctx context.Context, entry *domain.MealPlanEntry) error {
	if entry == nil {
		return errors.New("adapter: upsert meal plan entry: nil entry")
	}
	const q = `
		INSERT INTO meal_plan_entry (id, household_id, plan_date, meal, recipe_id, servings)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (household_id, plan_date, meal) DO UPDATE
		SET recipe_id = excluded.recipe_id, servings = excluded.servings`
	_, err := r.dbtx.Exec(ctx, q, entry.ID.String(), entry.HouseholdID.String(),
		dateArg(entry.Date), entry.Meal.String(), entry.RecipeID.String(), entry.Servings)
	if err != nil {
		return mapMealPlanWriteError("upsert meal plan entry", err)
	}
	return nil
}

// Delete removes the entry in the (household, date, meal) slot, returning
// domain.ErrMealPlanEntryNotFound when the slot is empty.
func (r *MealPlanRepository) Delete(ctx context.Context, householdID household.HouseholdID, date time.Time, meal domain.Meal) error {
	tag, err := r.dbtx.Exec(ctx,
		`DELETE FROM meal_plan_entry WHERE household_id = $1 AND plan_date = $2 AND meal = $3`,
		householdID.String(), dateArg(date), meal.String())
	if err != nil {
		return fmt.Errorf("delete meal plan entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrMealPlanEntryNotFound
	}
	return nil
}

// ListByDateRange returns the household's entries whose date falls in the inclusive
// window [start, end] ordered by date then meal (daily order), or an empty slice.
func (r *MealPlanRepository) ListByDateRange(ctx context.Context, householdID household.HouseholdID, start, end time.Time) ([]*domain.MealPlanEntry, error) {
	const q = `
		SELECT id, household_id, plan_date, meal, recipe_id, servings
		FROM meal_plan_entry
		WHERE household_id = $1 AND plan_date BETWEEN $2 AND $3
		ORDER BY plan_date, array_position(ARRAY['breakfast','lunch','dinner','snack']::text[], meal)`
	rows, err := r.dbtx.Query(ctx, q, householdID.String(), dateArg(start), dateArg(end))
	if err != nil {
		return nil, fmt.Errorf("list meal plan entries: %w", err)
	}
	defer rows.Close()

	entries := make([]*domain.MealPlanEntry, 0)
	for rows.Next() {
		entry, err := scanMealPlanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("list meal plan entries: scan: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list meal plan entries: %w", err)
	}
	return entries, nil
}

func mapMealPlanWriteError(label string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		switch pgErr.ConstraintName {
		case mealPlanEntryHouseholdFK:
			return household.ErrHouseholdNotFound
		case mealPlanEntryRecipeFK:
			return domain.ErrRecipeNotFound
		}
	}
	return fmt.Errorf("%s: %w", label, err)
}

func scanMealPlanEntry(r row) (*domain.MealPlanEntry, error) {
	var (
		entry                            domain.MealPlanEntry
		idStr, hhStr, mealStr, recipeStr string
		planDate                         time.Time
	)
	if err := r.Scan(&idStr, &hhStr, &planDate, &mealStr, &recipeStr, &entry.Servings); err != nil {
		return nil, err
	}
	id, err := domain.ParseMealPlanEntryID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan meal plan entry: %w", err)
	}
	hh, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("scan meal plan entry: %w", err)
	}
	meal, err := domain.ParseMeal(mealStr)
	if err != nil {
		return nil, fmt.Errorf("scan meal plan entry: %w", err)
	}
	recipeID, err := domain.ParseRecipeID(recipeStr)
	if err != nil {
		return nil, fmt.Errorf("scan meal plan entry: %w", err)
	}
	entry.ID, entry.HouseholdID, entry.Date, entry.Meal, entry.RecipeID = id, hh, planDate, meal, recipeID
	return &entry, nil
}

// dateArg renders a time as a UTC calendar-date literal (YYYY-MM-DD), so the value
// bound to a DATE column carries no time-of-day or timezone ambiguity.
func dateArg(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
