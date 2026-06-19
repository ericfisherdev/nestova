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
