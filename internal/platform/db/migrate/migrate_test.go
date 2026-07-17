package migrate

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
)

// TestEmbeddedMigrations verifies the migration set is embedded and parseable by
// goose without needing a database.
func TestEmbeddedMigrations(t *testing.T) {
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var sqlFiles int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			sqlFiles++
		}
	}
	if sqlFiles == 0 {
		t.Fatal("no embedded .sql migrations found")
	}

	// goose's base FS + dialect are configured in the package init().
	migrations, err := goose.CollectMigrations(dir, 0, math.MaxInt64)
	if err != nil {
		t.Fatalf("collect migrations: %v", err)
	}
	if len(migrations) != sqlFiles {
		t.Errorf("collected %d migrations, want %d (every .sql parsed)", len(migrations), sqlFiles)
	}
}

// TestPoolerSafeConnConfig verifies the pooler-safe path selects the simple
// query protocol (no named prepared statements) without needing a database.
func TestPoolerSafeConnConfig(t *testing.T) {
	t.Run("selects the simple protocol", func(t *testing.T) {
		cfg, err := poolerSafeConnConfig("postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=require")
		if err != nil {
			t.Fatalf("poolerSafeConnConfig() error: %v", err)
		}
		if cfg.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
			t.Errorf("DefaultQueryExecMode = %v, want QueryExecModeSimpleProtocol", cfg.DefaultQueryExecMode)
		}
	})

	t.Run("invalid DSN returns an error", func(t *testing.T) {
		if _, err := poolerSafeConnConfig("://nope"); err == nil {
			t.Error("poolerSafeConnConfig() = nil error, want error for invalid DSN")
		}
	})
}

// TestUpDownRoundTrip applies and rolls back the full migration set against a
// real database. It is skipped unless NESTOVA_TEST_DATABASE_URL is set, keeping
// the default test run hermetic.
func TestUpDownRoundTrip(t *testing.T) {
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the migration round-trip test")
	}
	ctx := context.Background()

	// Start from a known-clean schema.
	if err := Reset(ctx, dsn); err != nil {
		t.Fatalf("initial Reset: %v", err)
	}
	t.Cleanup(func() {
		if err := Reset(ctx, dsn); err != nil {
			t.Logf("cleanup Reset failed: %v", err)
		}
	})

	if err := Up(ctx, dsn); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, table := range []string{"household", "member", "notification"} {
		if !tableExists(t, dsn, table) {
			t.Errorf("after Up, table %q does not exist", table)
		}
	}

	if err := Reset(ctx, dsn); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, table := range []string{"household", "member", "notification"} {
		if tableExists(t, dsn, table) {
			t.Errorf("after Reset, table %q still exists", table)
		}
	}
}

// TestDownAndStatus exercises single-migration rollback and the status command
// against a real database. Skipped unless NESTOVA_TEST_DATABASE_URL is set.
func TestDownAndStatus(t *testing.T) {
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the migration Down/Status test")
	}
	ctx := context.Background()

	if err := Reset(ctx, dsn); err != nil {
		t.Fatalf("initial Reset: %v", err)
	}
	t.Cleanup(func() {
		if err := Reset(ctx, dsn); err != nil {
			t.Logf("cleanup Reset failed: %v", err)
		}
	})

	if err := Up(ctx, dsn); err != nil {
		t.Fatalf("Up: %v", err)
	}
	beforeDown := appliedVersion(t, dsn)
	if err := Status(ctx, dsn); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if err := Down(ctx, dsn); err != nil {
		t.Fatalf("Down: %v", err)
	}
	// Down must actually roll a migration back, not no-op.
	if afterDown := appliedVersion(t, dsn); afterDown >= beforeDown {
		t.Fatalf("Down did not lower the applied version: before=%d after=%d", beforeDown, afterDown)
	}
	// Single-step rollback must be reversible regardless of how many migrations
	// exist (this stays correct as later tickets add migrations).
	if err := Up(ctx, dsn); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

// TestUpTo_BackfillsPreExistingRequestedRows proves migration 00025 handles a
// database that already has reward_redemption rows in the pre-NES-127 shape
// (status = 'requested') — coverage TestUpDownRoundTrip cannot provide, since
// every other gated test here starts from an empty schema where the
// backfill's UPDATE trivially matches zero rows and so cannot violate either
// the old or the new CHECK constraint regardless of statement ordering. See
// 00025_reward_redemption_fulfillment.sql's Up section doc for the exact
// ordering bug this guards against (CodeRabbit finding, NES-127).
func TestUpTo_BackfillsPreExistingRequestedRows(t *testing.T) {
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the stepped-migration backfill test")
	}
	ctx := context.Background()

	if err := Reset(ctx, dsn); err != nil {
		t.Fatalf("initial Reset: %v", err)
	}
	t.Cleanup(func() {
		if err := Reset(ctx, dsn); err != nil {
			t.Logf("cleanup Reset failed: %v", err)
		}
	})

	// Stop at 00024 — the last migration before NES-127 — so the schema still
	// has the OLD status CHECK (requested/fulfilled/cancelled) and no
	// denied_reason column.
	const preMigrationVersion = 24
	if err := UpTo(ctx, dsn, preMigrationVersion); err != nil {
		t.Fatalf("UpTo(%d): %v", preMigrationVersion, err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Seed a redemption row in the OLD schema's shape: status = 'requested',
	// raw SQL throughout since this test exercises the schema directly, not
	// through any application-layer repository.
	var householdID, memberID, rewardID, redemptionID string
	if err := db.QueryRow(`INSERT INTO household (name) VALUES ('Pre-migration household') RETURNING id`).Scan(&householdID); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	if err := db.QueryRow(
		`INSERT INTO member (household_id, display_name, role, color_key) VALUES ($1, 'Alice', 'adult', 'sage') RETURNING id`,
		householdID,
	).Scan(&memberID); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := db.QueryRow(
		`INSERT INTO reward (id, household_id, name, cost_points, active) VALUES (gen_random_uuid(), $1, 'Legacy reward', 10, true) RETURNING id`,
		householdID,
	).Scan(&rewardID); err != nil {
		t.Fatalf("seed reward: %v", err)
	}
	if err := db.QueryRow(
		`INSERT INTO reward_redemption (id, household_id, reward_id, member_id, status)
		 VALUES (gen_random_uuid(), $1, $2, $3, 'requested') RETURNING id`,
		householdID, rewardID, memberID,
	).Scan(&redemptionID); err != nil {
		t.Fatalf("seed pre-migration redemption (status='requested'): %v", err)
	}

	// Apply the rest of the migrations, including 00025.
	if err := Up(ctx, dsn); err != nil {
		t.Fatalf("Up (through 00025): %v", err)
	}

	// The pre-existing row must have been backfilled to 'pending', not left
	// stranded or (had the ordering bug been present) rejected the migration
	// outright.
	var status string
	if err := db.QueryRow(`SELECT status FROM reward_redemption WHERE id = $1`, redemptionID).Scan(&status); err != nil {
		t.Fatalf("read migrated redemption status: %v", err)
	}
	if status != "pending" {
		t.Errorf("status after migration = %q, want %q", status, "pending")
	}

	// The NEW CHECK constraint must be the one actually enforced now: the
	// old, pre-rename spelling 'requested' must be rejected — and specifically
	// by reward_redemption_status_check (SQLSTATE 23514), not by some other,
	// unrelated failure that would make this assertion pass for the wrong
	// reason, mirroring nes116_postgres_test.go's *pgconn.PgError precedent.
	_, err = db.Exec(`UPDATE reward_redemption SET status = 'requested' WHERE id = $1`, redemptionID)
	if err == nil {
		t.Fatal("UPDATE to 'requested' succeeded after migration, want a CHECK constraint violation")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("UPDATE to 'requested' = %v, want a *pgconn.PgError CHECK violation", err)
	}
	if pgErr.Code != "23514" || pgErr.ConstraintName != "reward_redemption_status_check" {
		t.Errorf("UPDATE to 'requested' = code %s constraint %q, want 23514 on reward_redemption_status_check",
			pgErr.Code, pgErr.ConstraintName)
	}

	// The new 'denied' value must be accepted by the new CHECK constraint.
	if _, err := db.Exec(`UPDATE reward_redemption SET status = 'denied' WHERE id = $1`, redemptionID); err != nil {
		t.Errorf("UPDATE to 'denied' after migration failed: %v, want success", err)
	}

	// Denial refunds the redemption's points via a compensating point_ledger
	// entry (RewardRepository.Deny) — a downgrade must NOT make a denied
	// redemption look 'requested' (actionable, points still owed) again, or
	// the pre-NES-127 app could fulfill or re-refund it. Roll back 00025 and
	// confirm it folds to 'cancelled' instead (CodeRabbit finding, NES-127).
	if err := Down(ctx, dsn); err != nil {
		t.Fatalf("Down (roll back 00025): %v", err)
	}
	var postDownStatus string
	if err := db.QueryRow(`SELECT status FROM reward_redemption WHERE id = $1`, redemptionID).Scan(&postDownStatus); err != nil {
		t.Fatalf("read post-down redemption status: %v", err)
	}
	if postDownStatus != "cancelled" {
		t.Errorf("status after Down = %q, want %q (a denied/refunded redemption must not look actionable again)",
			postDownStatus, "cancelled")
	}
}

// appliedVersion returns the current goose migration version recorded in the
// database.
func appliedVersion(t *testing.T, dsn string) int64 {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	v, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatalf("goose.GetDBVersion: %v", err)
	}
	return v
}

func tableExists(t *testing.T, dsn, table string) bool {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var name *string
	if err := db.QueryRow(`SELECT to_regclass('public.' || $1)`, table).Scan(&name); err != nil {
		t.Fatalf("query to_regclass(%q): %v", table, err)
	}
	return name != nil
}
