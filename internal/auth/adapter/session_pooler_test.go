package adapter_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

// newExecModePool builds a pool configured exactly as db.poolConfig's
// Supabase+transaction branch does (QueryExecModeExec, no statement/description
// cache), so the session store is exercised without cached server-side prepared
// statements. It skips the TLS requirement because the local test database
// connects without TLS; the exec mode — not the transport — is what this test
// pins.
func newExecModePool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	poolCfg.ConnConfig.StatementCacheCapacity = 0
	poolCfg.ConnConfig.DescriptionCacheCapacity = 0

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestSessionStorePoolerSafe verifies scs/pgxstore create/read/refresh/delete all
// succeed under the transaction-pooler exec mode (QueryExecModeExec) that the
// Supabase transaction pooler requires. It is DB-gated and skipped unless
// NESTOVA_TEST_DATABASE_URL is set, keeping the default test run hermetic.
func TestSessionStorePoolerSafe(t *testing.T) {
	// NESTOVA_TEST_DATABASE_URL should point at a direct (non-pooled) connection;
	// the test simulates the transaction pooler purely by configuring the pool
	// with QueryExecModeExec, so no real pooler is needed in the test environment.
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the session store pooler test")
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
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := migrate.Reset(cleanupCtx, dsn); err != nil {
			t.Logf("cleanup reset failed: %v", err)
		}
	})

	// pool.Close is registered after the reset cleanup, so it runs first (LIFO):
	// the pool is closed before the final schema reset opens its own handle.
	pool := newExecModePool(t, dsn)
	sm := authadapter.NewSessionManager(pool, config.SessionConfig{Lifetime: time.Hour})
	store := sm.Store

	const token = "pooler-safe-token"
	expiry := time.Now().Add(time.Hour)

	// Create.
	if err := store.Commit(token, []byte("payload-1"), expiry); err != nil {
		t.Fatalf("Commit (create): %v", err)
	}

	// Read.
	data, found, err := store.Find(token)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !found || string(data) != "payload-1" {
		t.Fatalf("Find = (%q, %v), want (\"payload-1\", true)", data, found)
	}

	// Refresh (upsert with new payload + expiry).
	if err := store.Commit(token, []byte("payload-2"), expiry.Add(time.Hour)); err != nil {
		t.Fatalf("Commit (refresh): %v", err)
	}
	data, found, err = store.Find(token)
	if err != nil {
		t.Fatalf("Find after refresh: %v", err)
	}
	if !found || string(data) != "payload-2" {
		t.Fatalf("Find after refresh = (%q, %v), want (\"payload-2\", true)", data, found)
	}

	// Delete.
	if err := store.Delete(token); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, err = store.Find(token); err != nil {
		t.Fatalf("Find after delete: %v", err)
	} else if found {
		t.Fatal("Find after delete = found, want not found")
	}
}
