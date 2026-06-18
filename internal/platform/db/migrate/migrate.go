// Package migrate runs the database schema migrations. SQL migrations are
// embedded into the binary for reproducibility and applied with goose over the
// pgx stdlib driver (a database/sql handle, distinct from the application's
// pgxpool). Migrations are run explicitly (make targets / cmd/migrate), never
// automatically on server boot.
//
// goose records applied migrations in its goose_db_version table (created
// automatically on first run); the up/down/status/reset operations consult it
// to compute the delta to apply or revert.
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	dialect = "postgres"
	// dir is the migrations directory within the embedded filesystem.
	dir = "migrations"
)

// goose's base FS and dialect are process-global, so configure them once. The
// dialect is a compile-time constant, so SetDialect cannot fail in practice; a
// failure here is a programming error.
func init() {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect(dialect); err != nil {
		panic(fmt.Sprintf("migrate: invalid goose dialect %q: %v", dialect, err))
	}
}

// Up applies all pending migrations.
func Up(ctx context.Context, dsn string) error { return run(ctx, "up", dsn) }

// Down rolls back the most recently applied migration.
func Down(ctx context.Context, dsn string) error { return run(ctx, "down", dsn) }

// Status prints the migration status to stdout.
func Status(ctx context.Context, dsn string) error { return run(ctx, "status", dsn) }

// Reset rolls back every applied migration. Intended for tests and local resets.
func Reset(ctx context.Context, dsn string) error { return run(ctx, "reset", dsn) }

// run opens a database/sql handle via the pgx stdlib driver and executes the
// goose command against the embedded migrations.
func run(ctx context.Context, command, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Verify connectivity up front so an invalid DSN or unreachable database
	// fails with a clear error before goose starts.
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	if err := goose.RunContext(ctx, command, db, dir); err != nil {
		return fmt.Errorf("goose %s: %w", command, err)
	}
	return nil
}
