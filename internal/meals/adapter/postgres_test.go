package adapter_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// newTestPool connects to NESTOVA_TEST_DATABASE_URL and applies migrations, or
// skips when the env var is unset (keeping the default test run hermetic).
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the meals adapter tests")
	}

	setupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := migrate.Reset(setupCtx, dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := migrate.Up(setupCtx, dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		if err := migrate.Reset(cleanupCtx, dsn); err != nil {
			t.Logf("cleanup reset failed: %v", err)
		}
	})

	pool, err := db.New(setupCtx, config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
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

func seedIngredient(t *testing.T, pool *pgxpool.Pool, name string) tracking.IngredientID {
	t.Helper()
	id := tracking.NewIngredientID()
	if _, err := pool.Exec(testCtx(t),
		`INSERT INTO ingredient (id, canonical_name) VALUES ($1, $2)`, id.String(), name); err != nil {
		t.Fatalf("seed ingredient %q: %v", name, err)
	}
	return id
}

func line(ing tracking.IngredientID, amount float64, unit household.Unit, optional bool) domain.RecipeIngredient {
	return domain.RecipeIngredient{
		IngredientID: ing,
		Quantity:     household.Quantity{Amount: amount, Unit: unit},
		Optional:     optional,
	}
}

func newLocalRecipe(hh household.HouseholdID, title string, lines ...domain.RecipeIngredient) *domain.Recipe {
	return &domain.Recipe{
		ID:           domain.NewRecipeID(),
		HouseholdID:  &hh,
		Title:        title,
		Source:       domain.SourceLocal,
		Servings:     4,
		Instructions: "Combine and cook.",
		Ingredients:  lines,
	}
}

func TestRecipeCreateWithIngredientsRoundTrips(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecipeRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	flour := seedIngredient(t, pool, "flour")
	salt := seedIngredient(t, pool, "salt")

	recipe := newLocalRecipe(hh, "Bread",
		line(flour, 500, household.UnitGram, false),
		line(salt, 2, household.UnitGram, true),
	)
	if err := repo.Create(ctx, recipe); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, hh, recipe.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Bread" || got.Servings != 4 || got.Source != domain.SourceLocal {
		t.Errorf("recipe = %+v, want Bread/4/local", got)
	}
	if got.HouseholdID == nil || *got.HouseholdID != hh {
		t.Errorf("HouseholdID = %v, want %v", got.HouseholdID, hh)
	}
	if len(got.Ingredients) != 2 {
		t.Fatalf("ingredients = %d, want 2", len(got.Ingredients))
	}
	byID := map[tracking.IngredientID]domain.RecipeIngredient{}
	for _, l := range got.Ingredients {
		byID[l.IngredientID] = l
	}
	if l := byID[flour]; l.Quantity.Amount != 500 || l.Quantity.Unit != household.UnitGram || l.Optional {
		t.Errorf("flour line = %+v, want 500g required", l)
	}
	if l := byID[salt]; l.Quantity.Amount != 2 || !l.Optional {
		t.Errorf("salt line = %+v, want 2g optional", l)
	}
}

func TestRecipeBoxListIsHouseholdScoped(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecipeRepository(pool)
	ctx := testCtx(t)
	ours := seedHousehold(t, pool)
	theirs := seedHousehold(t, pool)
	flour := seedIngredient(t, pool, "flour")

	if err := repo.Create(ctx, newLocalRecipe(ours, "Soup", line(flour, 1, household.UnitCount, false))); err != nil {
		t.Fatalf("Create ours: %v", err)
	}
	if err := repo.Create(ctx, newLocalRecipe(ours, "Apple Pie", line(flour, 1, household.UnitCount, false))); err != nil {
		t.Fatalf("Create ours 2: %v", err)
	}
	if err := repo.Create(ctx, newLocalRecipe(theirs, "Their Stew", line(flour, 1, household.UnitCount, false))); err != nil {
		t.Fatalf("Create theirs: %v", err)
	}

	list, err := repo.ListByHousehold(ctx, ours)
	if err != nil {
		t.Fatalf("ListByHousehold: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d recipes, want 2", len(list))
	}
	// Ordered by title: "Apple Pie" before "Soup".
	if list[0].Title != "Apple Pie" || list[1].Title != "Soup" {
		t.Errorf("titles = [%q, %q], want [Apple Pie, Soup]", list[0].Title, list[1].Title)
	}
	if len(list[0].Ingredients) != 1 {
		t.Errorf("list[0] ingredients = %d, want 1 (populated)", len(list[0].Ingredients))
	}
}

func TestRecipeUpdateReplacesIngredients(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecipeRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	flour := seedIngredient(t, pool, "flour")
	sugar := seedIngredient(t, pool, "sugar")

	recipe := newLocalRecipe(hh, "Cake", line(flour, 200, household.UnitGram, false))
	if err := repo.Create(ctx, recipe); err != nil {
		t.Fatalf("Create: %v", err)
	}

	recipe.Title = "Sweet Cake"
	recipe.Servings = 8
	recipe.Ingredients = []domain.RecipeIngredient{line(sugar, 100, household.UnitGram, false)}
	if err := repo.Update(ctx, recipe); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.Get(ctx, hh, recipe.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Sweet Cake" || got.Servings != 8 {
		t.Errorf("recipe = %q/%d, want Sweet Cake/8", got.Title, got.Servings)
	}
	if len(got.Ingredients) != 1 || got.Ingredients[0].IngredientID != sugar {
		t.Errorf("ingredients = %+v, want [sugar] only", got.Ingredients)
	}
}

func TestRecipeUpdateUnknownReturnsNotFound(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecipeRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)

	if err := repo.Update(ctx, newLocalRecipe(hh, "Ghost")); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("Update(unknown) = %v, want ErrRecipeNotFound", err)
	}
}

func TestRecipeDeleteScopedToHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecipeRepository(pool)
	ctx := testCtx(t)
	owner := seedHousehold(t, pool)
	attacker := seedHousehold(t, pool)
	flour := seedIngredient(t, pool, "flour")

	recipe := newLocalRecipe(owner, "Secret", line(flour, 1, household.UnitCount, false))
	if err := repo.Create(ctx, recipe); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A foreign household cannot delete it.
	if err := repo.Delete(ctx, attacker, recipe.ID); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("Delete(foreign) = %v, want ErrRecipeNotFound", err)
	}
	// The owner can.
	if err := repo.Delete(ctx, owner, recipe.ID); err != nil {
		t.Fatalf("Delete(owner): %v", err)
	}
	if _, err := repo.Get(ctx, owner, recipe.ID); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("Get(deleted) = %v, want ErrRecipeNotFound", err)
	}
}

func TestRecipeExternalUpsertReplacesByExternalRef(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecipeRepository(pool)
	ctx := testCtx(t)
	flour := seedIngredient(t, pool, "flour")
	yeast := seedIngredient(t, pool, "yeast")

	ref := "spoonacular-555"
	first := &domain.Recipe{
		ID: domain.NewRecipeID(), Title: "Pizza", Source: domain.SourceExternal,
		ExternalRef: &ref, Servings: 2, Ingredients: []domain.RecipeIngredient{line(flour, 1, household.UnitCount, false)},
	}
	stored, err := repo.UpsertExternal(ctx, first)
	if err != nil {
		t.Fatalf("UpsertExternal first: %v", err)
	}
	firstID := stored.ID

	second := &domain.Recipe{
		ID: domain.NewRecipeID(), Title: "Better Pizza", Source: domain.SourceExternal,
		ExternalRef: &ref, Servings: 3, Ingredients: []domain.RecipeIngredient{line(yeast, 5, household.UnitGram, false)},
	}
	stored2, err := repo.UpsertExternal(ctx, second)
	if err != nil {
		t.Fatalf("UpsertExternal second: %v", err)
	}
	if stored2.ID != firstID {
		t.Errorf("re-cache id = %v, want preserved %v", stored2.ID, firstID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recipe WHERE external_ref = $1`, ref).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("rows for external_ref = %d, want 1", count)
	}
	var title string
	var lineCount int
	if err := pool.QueryRow(ctx, `SELECT title FROM recipe WHERE id = $1`, firstID.String()).Scan(&title); err != nil {
		t.Fatalf("query title: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recipe_ingredient WHERE recipe_id = $1`, firstID.String()).Scan(&lineCount); err != nil {
		t.Fatalf("query line count: %v", err)
	}
	if title != "Better Pizza" || lineCount != 1 {
		t.Errorf("after re-cache: title=%q lines=%d, want Better Pizza/1", title, lineCount)
	}
}

func TestRecipeCreateMapsForeignKeySentinels(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewRecipeRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)

	// Unknown ingredient -> tracking.ErrIngredientNotFound.
	unknownIng := newLocalRecipe(hh, "Mystery", line(tracking.NewIngredientID(), 1, household.UnitCount, false))
	if err := repo.Create(ctx, unknownIng); !errors.Is(err, tracking.ErrIngredientNotFound) {
		t.Errorf("Create(unknown ingredient) = %v, want ErrIngredientNotFound", err)
	}

	// Unknown household -> household.ErrHouseholdNotFound.
	flour := seedIngredient(t, pool, "flour")
	unknownHH := newLocalRecipe(household.NewHouseholdID(), "Orphan", line(flour, 1, household.UnitCount, false))
	if err := repo.Create(ctx, unknownHH); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Errorf("Create(unknown household) = %v, want ErrHouseholdNotFound", err)
	}
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func seedRecipe(t *testing.T, repo *adapter.RecipeRepository, hh household.HouseholdID, ing tracking.IngredientID, title string) domain.RecipeID {
	t.Helper()
	recipe := newLocalRecipe(hh, title, line(ing, 1, household.UnitCount, false))
	if err := repo.Create(testCtx(t), recipe); err != nil {
		t.Fatalf("seed recipe %q: %v", title, err)
	}
	return recipe.ID
}

func TestMealPlanUpsertReplaceListAndDelete(t *testing.T) {
	pool := newTestPool(t)
	recipes := adapter.NewRecipeRepository(pool)
	plans := adapter.NewMealPlanRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	flour := seedIngredient(t, pool, "flour")
	bread := seedRecipe(t, recipes, hh, flour, "Bread")
	soup := seedRecipe(t, recipes, hh, flour, "Soup")

	mon := date(2026, 6, 22)
	entry := &domain.MealPlanEntry{
		ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: mon,
		Meal: domain.MealDinner, RecipeID: bread, Servings: 4,
	}
	if err := plans.Upsert(ctx, entry); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Re-assigning the same slot replaces the recipe and preserves the row id.
	replacement := &domain.MealPlanEntry{
		ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: mon,
		Meal: domain.MealDinner, RecipeID: soup, Servings: 6,
	}
	if err := plans.Upsert(ctx, replacement); err != nil {
		t.Fatalf("Upsert replace: %v", err)
	}

	week, err := plans.ListByDateRange(ctx, hh, date(2026, 6, 21), date(2026, 6, 27))
	if err != nil {
		t.Fatalf("ListByDateRange: %v", err)
	}
	if len(week) != 1 {
		t.Fatalf("week entries = %d, want 1 (slot replaced, not duplicated)", len(week))
	}
	if week[0].RecipeID != soup || week[0].Servings != 6 {
		t.Errorf("slot = recipe %v/servings %d, want soup/6", week[0].RecipeID, week[0].Servings)
	}
	if week[0].ID != entry.ID {
		t.Errorf("slot id = %v, want preserved original %v", week[0].ID, entry.ID)
	}
	if !week[0].Date.Equal(mon) {
		t.Errorf("slot date = %v, want %v", week[0].Date, mon)
	}

	if err := plans.Delete(ctx, hh, mon, domain.MealDinner); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := plans.Delete(ctx, hh, mon, domain.MealDinner); !errors.Is(err, domain.ErrMealPlanEntryNotFound) {
		t.Errorf("Delete(empty slot) = %v, want ErrMealPlanEntryNotFound", err)
	}
}

func TestMealPlanListOrdersByDateThenMeal(t *testing.T) {
	pool := newTestPool(t)
	recipes := adapter.NewRecipeRepository(pool)
	plans := adapter.NewMealPlanRepository(pool)
	ctx := testCtx(t)
	hh := seedHousehold(t, pool)
	flour := seedIngredient(t, pool, "flour")
	r := seedRecipe(t, recipes, hh, flour, "Anything")

	day := date(2026, 6, 22)
	// Insert out of daily order; expect breakfast before dinner on the same day.
	for _, meal := range []domain.Meal{domain.MealDinner, domain.MealBreakfast} {
		e := &domain.MealPlanEntry{
			ID: domain.NewMealPlanEntryID(), HouseholdID: hh, Date: day,
			Meal: meal, RecipeID: r, Servings: 2,
		}
		if err := plans.Upsert(ctx, e); err != nil {
			t.Fatalf("Upsert %s: %v", meal, err)
		}
	}

	week, err := plans.ListByDateRange(ctx, hh, day, day)
	if err != nil {
		t.Fatalf("ListByDateRange: %v", err)
	}
	if len(week) != 2 || week[0].Meal != domain.MealBreakfast || week[1].Meal != domain.MealDinner {
		t.Errorf("order = %v, want [breakfast, dinner]", []domain.Meal{week[0].Meal, week[1].Meal})
	}
}

func TestMealPlanUpsertRejectsUnplannableRecipe(t *testing.T) {
	pool := newTestPool(t)
	recipes := adapter.NewRecipeRepository(pool)
	plans := adapter.NewMealPlanRepository(pool)
	ctx := testCtx(t)
	owner := seedHousehold(t, pool)
	other := seedHousehold(t, pool)
	flour := seedIngredient(t, pool, "flour")
	otherRecipe := seedRecipe(t, recipes, other, flour, "Theirs")

	// Planning another household's recipe -> ErrRecipeNotFound (composite FK).
	entry := &domain.MealPlanEntry{
		ID: domain.NewMealPlanEntryID(), HouseholdID: owner, Date: date(2026, 6, 22),
		Meal: domain.MealLunch, RecipeID: otherRecipe, Servings: 2,
	}
	if err := plans.Upsert(ctx, entry); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("Upsert(foreign recipe) = %v, want ErrRecipeNotFound", err)
	}

	// An external/cached recipe is likewise unplannable.
	ref := "spoonacular-999"
	ext, err := recipes.UpsertExternal(ctx, &domain.Recipe{
		ID: domain.NewRecipeID(), Title: "Ext", Source: domain.SourceExternal,
		ExternalRef: &ref, Servings: 2, Ingredients: []domain.RecipeIngredient{line(flour, 1, household.UnitCount, false)},
	})
	if err != nil {
		t.Fatalf("UpsertExternal: %v", err)
	}
	entry.RecipeID = ext.ID
	if err := plans.Upsert(ctx, entry); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("Upsert(external recipe) = %v, want ErrRecipeNotFound", err)
	}
}
