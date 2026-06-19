package adapter_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
	"github.com/ericfisherdev/nestova/internal/tracking/adapter"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// newTestPool connects to NESTOVA_TEST_DATABASE_URL and applies migrations, or
// skips when the env var is unset (keeping the default test run hermetic).
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the tracking adapter tests")
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

func TestEnsureIngredientIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewIngredientRepository(pool)
	ctx := testCtx(t)

	first, err := repo.EnsureIngredient(ctx, "  Flour ")
	if err != nil {
		t.Fatalf("EnsureIngredient first: %v", err)
	}
	if first.CanonicalName != "flour" {
		t.Errorf("canonical name = %q, want %q", first.CanonicalName, "flour")
	}

	// A second call for an equivalent (normalized) name returns the same row.
	second, err := repo.EnsureIngredient(ctx, "FLOUR")
	if err != nil {
		t.Fatalf("EnsureIngredient second: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second EnsureIngredient id = %v, want %v (idempotent)", second.ID, first.ID)
	}
}

func TestEnsureIngredientRejectsEmpty(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewIngredientRepository(pool)

	if _, err := repo.EnsureIngredient(testCtx(t), "   "); !errors.Is(err, domain.ErrInvalidIngredient) {
		t.Errorf("EnsureIngredient(blank) error = %v, want ErrInvalidIngredient", err)
	}
}

func TestResolveByCanonicalAliasAndPlural(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewIngredientRepository(pool)
	ctx := testCtx(t)

	ing, err := repo.EnsureIngredient(ctx, "tomato")
	if err != nil {
		t.Fatalf("EnsureIngredient: %v", err)
	}
	// Attach an alias directly; EnsureIngredient itself does not manage aliases.
	if _, err := pool.Exec(ctx,
		`UPDATE ingredient SET aliases = $1 WHERE id = $2`,
		[]string{"roma"}, ing.ID.String(),
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	cases := []struct {
		name, query string
	}{
		{"canonical", "Tomato"},
		{"plural of canonical", "Tomatoes"},
		{"alias", "Roma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.Resolve(ctx, tc.query)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.query, err)
			}
			if got.ID != ing.ID {
				t.Errorf("Resolve(%q) id = %v, want %v", tc.query, got.ID, ing.ID)
			}
		})
	}
}

func TestResolvePrefersExactCanonicalOverPluralAndAlias(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewIngredientRepository(pool)
	ctx := testCtx(t)

	// Two distinct canonical rows whose names collide under plural folding:
	// resolving the exact plural input must return the plural row, not the
	// singular one reachable only via a singularized candidate.
	singular, err := repo.EnsureIngredient(ctx, "tomato")
	if err != nil {
		t.Fatalf("EnsureIngredient singular: %v", err)
	}
	plural, err := repo.EnsureIngredient(ctx, "tomatoes")
	if err != nil {
		t.Fatalf("EnsureIngredient plural: %v", err)
	}

	if got, err := repo.Resolve(ctx, "Tomatoes"); err != nil {
		t.Fatalf("Resolve(Tomatoes): %v", err)
	} else if got.ID != plural.ID {
		t.Errorf("Resolve(Tomatoes) id = %v, want plural row %v (exact input beats singular guess)", got.ID, plural.ID)
	}
	if got, err := repo.Resolve(ctx, "Tomato"); err != nil {
		t.Fatalf("Resolve(Tomato): %v", err)
	} else if got.ID != singular.ID {
		t.Errorf("Resolve(Tomato) id = %v, want singular row %v", got.ID, singular.ID)
	}

	// A canonical-name match must beat an alias-only match for the same query.
	if _, err := pool.Exec(ctx,
		`UPDATE ingredient SET aliases = $1 WHERE id = $2`,
		[]string{"basil"}, singular.ID.String(),
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	basil, err := repo.EnsureIngredient(ctx, "basil")
	if err != nil {
		t.Fatalf("EnsureIngredient basil: %v", err)
	}
	if got, err := repo.Resolve(ctx, "basil"); err != nil {
		t.Fatalf("Resolve(basil): %v", err)
	} else if got.ID != basil.ID {
		t.Errorf("Resolve(basil) id = %v, want canonical row %v (canonical beats alias)", got.ID, basil.ID)
	}
}

func TestResolveErrors(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewIngredientRepository(pool)
	ctx := testCtx(t)

	if _, err := repo.Resolve(ctx, "nonexistent"); !errors.Is(err, domain.ErrIngredientNotFound) {
		t.Errorf("Resolve(unknown) error = %v, want ErrIngredientNotFound", err)
	}
	if _, err := repo.Resolve(ctx, "   "); !errors.Is(err, domain.ErrInvalidIngredient) {
		t.Errorf("Resolve(blank) error = %v, want ErrInvalidIngredient", err)
	}
}

func TestNamesByIDs(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewIngredientRepository(pool)
	ctx := testCtx(t)

	flour, err := repo.EnsureIngredient(ctx, "flour")
	if err != nil {
		t.Fatalf("EnsureIngredient flour: %v", err)
	}
	sugar, err := repo.EnsureIngredient(ctx, "sugar")
	if err != nil {
		t.Fatalf("EnsureIngredient sugar: %v", err)
	}
	missing := domain.NewIngredientID()

	names, err := repo.NamesByIDs(ctx, []domain.IngredientID{flour.ID, sugar.ID, missing})
	if err != nil {
		t.Fatalf("NamesByIDs: %v", err)
	}
	if names[flour.ID] != "flour" {
		t.Errorf("names[flour] = %q, want %q", names[flour.ID], "flour")
	}
	if names[sugar.ID] != "sugar" {
		t.Errorf("names[sugar] = %q, want %q", names[sugar.ID], "sugar")
	}
	if _, ok := names[missing]; ok {
		t.Errorf("unknown id should be omitted, got %q", names[missing])
	}

	// Empty input returns an empty (non-nil) map without touching the database.
	empty, err := repo.NamesByIDs(ctx, nil)
	if err != nil {
		t.Fatalf("NamesByIDs(nil): %v", err)
	}
	if empty == nil {
		t.Error("NamesByIDs(nil) = nil map, want a non-nil empty map")
	}
	if len(empty) != 0 {
		t.Errorf("NamesByIDs(nil) = %v, want empty map", empty)
	}
}

func TestEnsureIngredientConcurrentIsRaceSafe(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewIngredientRepository(pool)
	ctx := testCtx(t)

	const goroutines = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  []domain.IngredientID
		errs []error
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ing, err := repo.EnsureIngredient(ctx, "milk")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids = append(ids, ing.ID)
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent EnsureIngredient errors: %v", errs)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent EnsureIngredient returned differing ids: %v", ids)
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ingredient WHERE canonical_name = $1`, "milk",
	).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Errorf("ingredient rows for %q = %d, want 1", "milk", count)
	}
}
