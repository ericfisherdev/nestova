// Package db provides Postgres connectivity for the application: a pooled
// connection built from configuration and a health check used for readiness.
package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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
	dsn := cfg.DSN
	if cfg.SSLRootCert != "" {
		// Let pgx own TLS construction: it reads sslrootcert from the connection
		// string and, when present, upgrades to sslmode=verify-full and loads the
		// CA bundle. Injecting the path is safer than hand-building a tls.Config.
		var err error
		dsn, err = applySSLRootCert(dsn, cfg.SSLRootCert)
		if err != nil {
			return nil, err
		}
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
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

	// Supabase-specific connectivity. The Postgres path is deliberately left
	// untouched so DB_PROVIDER=postgres remains byte-for-byte identical.
	if cfg.Provider == config.DBProviderSupabase {
		if err := applySupabasePooling(poolCfg, cfg.PoolMode); err != nil {
			return nil, err
		}
	}

	return poolCfg, nil
}

// applySupabasePooling enforces TLS for Supabase and, for the transaction
// pooler, switches the pool off cached server-side prepared statements — which
// Supavisor cannot keep across multiplexed transactions — while keeping the
// extended protocol via QueryExecModeExec.
func applySupabasePooling(poolCfg *pgxpool.Config, mode config.DBPoolMode) error {
	// pgx leaves ConnConfig.TLSConfig nil only for sslmode=disable, so a nil here
	// is precisely the "TLS turned off" case Supabase must reject.
	if poolCfg.ConnConfig.TLSConfig == nil {
		return errors.New("DB_PROVIDER=supabase requires TLS: remove sslmode=disable from DATABASE_URL " +
			"(use sslmode=require, or verify-full with DB_SSL_ROOT_CERT)")
	}

	if mode == config.DBPoolModeTransaction {
		// QueryExecModeExec keeps the binary extended protocol in a single round
		// trip without creating named server-side prepared statements (the thing
		// the transaction pooler breaks). The caches are disabled to match — and
		// because the CacheStatement/CacheDescribe modes refuse to run once their
		// cache is off.
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
		poolCfg.ConnConfig.StatementCacheCapacity = 0
		poolCfg.ConnConfig.DescriptionCacheCapacity = 0
	}
	return nil
}

// applySSLRootCert injects the sslrootcert parameter into the connection string.
// pgx reads it during ParseConfig and upgrades the connection to verify-full,
// loading the CA bundle itself. URL-form DSNs (the project default and what
// Supabase hands out) are parsed and re-encoded; keyword/value DSNs get the
// parameter appended.
func applySSLRootCert(dsn, certPath string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse database dsn: %w", err)
		}
		q := u.Query()
		q.Set("sslrootcert", certPath)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	// libpq keyword/value format requires single-quoted values for paths that
	// contain spaces or quotes; escape backslashes and single quotes per its
	// rules so a path like "/var/certs/my app/ca.pem" is not truncated.
	escaped := strings.ReplaceAll(certPath, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return dsn + " sslrootcert='" + escaped + "'", nil
}
