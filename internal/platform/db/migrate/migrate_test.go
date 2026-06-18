package migrate

import (
	"context"
	"database/sql"
	"io/fs"
	"math"
	"os"
	"strings"
	"testing"

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
