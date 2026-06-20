package app_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
	trackingadapter "github.com/ericfisherdev/nestova/internal/tracking/adapter"
)

// newTestPool connects to NESTOVA_TEST_DATABASE_URL and applies migrations, or
// skips when the env var is unset (keeping the default test run hermetic).
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the meals app DB-gated tests")
	}
	// migrate.Reset drops the schema, so refuse to run against anything but a
	// dedicated test database — a guard against a misconfigured URL wiping real
	// data. Check the parsed database name (not a substring of the whole DSN, which
	// a host or password could satisfy by accident).
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("invalid NESTOVA_TEST_DATABASE_URL: %v", err)
	}
	if name := strings.ToLower(cfg.ConnConfig.Database); name != "test" && !strings.HasSuffix(name, "_test") {
		t.Fatalf("NESTOVA_TEST_DATABASE_URL must target a dedicated test database (name %q is neither \"test\" nor *_test) — refusing to reset it", cfg.ConnConfig.Database)
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

	poolCtx, poolCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer poolCancel()
	pool, err := db.New(poolCtx, config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedHousehold(t *testing.T, pool *pgxpool.Pool) household.HouseholdID {
	t.Helper()
	id := household.NewHouseholdID()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `INSERT INTO household (id, name) VALUES ($1, $2)`, id.String(), "The Fishers"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	return id
}

// TestRecipeServiceCreateGetRoundTrip exercises the full stack: the service
// normalizes free-text ingredient names to the real catalogue and persists the
// recipe, which then reads back with the same canonical ingredient ids.
func TestRecipeServiceCreateGetRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hh := seedHousehold(t, pool)

	recipes := mealsadapter.NewRecipeRepository(pool)
	ingredients := trackingadapter.NewIngredientRepository(pool)
	svc, err := app.NewRecipeService(recipes, ingredients)
	if err != nil {
		t.Fatalf("NewRecipeService: %v", err)
	}

	created, err := svc.CreateRecipe(ctx, hh, app.RecipeInput{
		Title: "Pancakes", Servings: 4, Instructions: "Mix and griddle.",
		Ingredients: []app.IngredientLine{
			line("  Flour ", 300, household.UnitGram, false),
			line("Eggs", 2, household.UnitCount, false),
		},
	})
	if err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}

	got, err := recipes.Get(ctx, hh, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Pancakes" || got.Servings != 4 || len(got.Ingredients) != 2 {
		t.Errorf("recipe = %q/%d with %d lines, want Pancakes/4/2", got.Title, got.Servings, len(got.Ingredients))
	}
	// The persisted line ids match the catalogue ids the service resolved.
	want := map[string]bool{}
	for _, l := range created.Ingredients {
		want[l.IngredientID.String()] = true
	}
	for _, l := range got.Ingredients {
		if !want[l.IngredientID.String()] {
			t.Errorf("persisted ingredient id %v not among the created ids", l.IngredientID)
		}
	}

	// The normalized names resolve in the catalogue.
	flour, err := ingredients.EnsureIngredient(ctx, "flour")
	if err != nil {
		t.Fatalf("EnsureIngredient: %v", err)
	}
	if flour.CanonicalName != "flour" {
		t.Errorf("canonical name = %q, want flour", flour.CanonicalName)
	}
}
