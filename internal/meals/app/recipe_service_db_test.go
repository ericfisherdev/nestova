package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/platform/db/dbtest"
	trackingadapter "github.com/ericfisherdev/nestova/internal/tracking/adapter"
)

// newTestPool returns a pool against this package's own derived database
// (NES-149), freshly reset and migrated. dbtest.NewIsolatedPool owns the
// safety rail, the on-demand CREATE DATABASE, and the reset/migrate
// lifecycle; the per-package database is what lets gated packages run
// concurrently without resetting each other's schema mid-test.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.NewIsolatedPool(t, "mealsapp")
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
