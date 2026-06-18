// Package db provides Postgres connectivity for the application: a pooled
// connection built from configuration and a health check used for readiness.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// TX is the minimal query surface shared by *pgxpool.Pool and pgx.Tx. Adapters
// depend on this interface (instead of the concrete pool) so the same repository
// code runs both directly against the pool and inside a transaction: a
// transactional caller constructs a repository with a pgx.Tx, while the default
// composition uses the pool. Both concrete types satisfy TX unchanged.
//
// It is named TX (not DBTX) to avoid the db.DBTX stutter; "db.TX" reads as
// "a database transaction-capable executor".
type TX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pool tuning. MaxConns is configurable via DBConfig (zero defers to pgx's own
// default); the lifetime and health-check periods are conservative fixed
// defaults appropriate for a long-running server.
const (
	maxConnLifetime   = time.Hour
	maxConnIdleTime   = 30 * time.Minute
	healthCheckPeriod = time.Minute
)

// New builds a pgx connection pool from cfg, verifies connectivity with a
// bounded Ping, and returns the ready-to-use pool. A bad DSN or unreachable
// database fails fast with a descriptive error. The caller owns the pool and
// must Close it.
func New(ctx context.Context, cfg config.DBConfig) (*pgxpool.Pool, error) {
	poolCfg, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	// A non-positive ConnTimeout means "no explicit bound" (inherit ctx) rather
	// than an already-expired deadline that would fail the ping immediately.
	pingCtx, cancel := ctx, func() {}
	if cfg.ConnTimeout > 0 {
		pingCtx, cancel = context.WithTimeout(ctx, cfg.ConnTimeout)
	}
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	return pool, nil
}

// Health verifies live connectivity by acquiring a connection and pinging it.
// It backs the HTTP readiness check (route registered by the HTTP server) and
// returns a non-nil error when the database is unreachable.
func Health(ctx context.Context, pool *pgxpool.Pool) error {
	return pool.Ping(ctx)
}

// poolConfig derives a pgxpool configuration from cfg without connecting, so
// the derivation is unit-testable. It parses the DSN and applies the tuning
// overrides.
func poolConfig(cfg config.DBConfig) (*pgxpool.Config, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}

	// Honor the DBConfig contract: a positive MaxConns overrides the pool size;
	// zero defers to pgx's parsed default (itself max(4, NumCPU)).
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	poolCfg.MaxConnLifetime = maxConnLifetime
	poolCfg.MaxConnIdleTime = maxConnIdleTime
	poolCfg.HealthCheckPeriod = healthCheckPeriod

	return poolCfg, nil
}
