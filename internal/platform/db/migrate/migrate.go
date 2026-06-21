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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver + OpenDB
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

// options configures how a migration command connects.
type options struct {
	poolerSafe bool
}

// Option customizes a migration run.
type Option func(*options)

// PoolerSafe configures the migration connection to use the simple query
// protocol so goose's version-bookkeeping queries do not rely on named
// server-side prepared statements, which a transaction pooler (PgBouncer /
// Supabase Supavisor) cannot keep across multiplexed transactions. Prefer
// pointing the DSN at a direct/session connection over enabling this.
func PoolerSafe() Option { return func(o *options) { o.poolerSafe = true } }

// Up applies all pending migrations.
func Up(ctx context.Context, dsn string, opts ...Option) error { return run(ctx, "up", dsn, opts...) }

// Down rolls back the most recently applied migration.
func Down(ctx context.Context, dsn string, opts ...Option) error {
	return run(ctx, "down", dsn, opts...)
}

// Status prints the migration status to stdout.
func Status(ctx context.Context, dsn string, opts ...Option) error {
	return run(ctx, "status", dsn, opts...)
}

// Reset rolls back every applied migration. Intended for tests and local resets.
func Reset(ctx context.Context, dsn string, opts ...Option) error {
	return run(ctx, "reset", dsn, opts...)
}

// run opens a database/sql handle via the pgx stdlib driver and executes the
// goose command against the embedded migrations.
func run(ctx context.Context, command, dsn string, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	db, err := openDB(dsn, o.poolerSafe)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// Verify connectivity up front so an invalid DSN or unreachable database
	// fails with a clear error before goose starts.
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	if command == "reset" {
		// On a fresh database the goose_db_version table does not exist yet, so
		// "reset" (down to zero) would fail trying to read applied versions.
		// Ensure the table exists first so a reset against a clean database is a
		// harmless no-op rather than an error.
		if _, err := goose.EnsureDBVersionContext(ctx, db); err != nil {
			return fmt.Errorf("ensure goose version table: %w", err)
		}
	}

	if err := goose.RunContext(ctx, command, db, dir); err != nil {
		return fmt.Errorf("goose %s: %w", command, err)
	}
	return nil
}

// openDB returns a database/sql handle over the pgx driver. The default path
// uses the registered "pgx" driver unchanged. The pooler-safe path opens a
// connection configured for the simple protocol so it works through a
// transaction pooler.
func openDB(dsn string, poolerSafe bool) (*sql.DB, error) {
	if !poolerSafe {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		return db, nil
	}
	connCfg, err := poolerSafeConnConfig(dsn)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*connCfg), nil
}

// poolerSafeConnConfig parses dsn and selects the simple query protocol, which
// carries no named server-side prepared statements and so survives a
// transaction pooler's per-transaction connection multiplexing.
func poolerSafeConnConfig(dsn string) (*pgx.ConnConfig, error) {
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}
	connCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return connCfg, nil
}
