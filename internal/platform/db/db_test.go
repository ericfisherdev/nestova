package db

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// TestPoolConfig verifies the pool configuration is derived from DBConfig
// without needing a live database.
func TestPoolConfig(t *testing.T) {
	const dsn = "postgres://u:p@localhost:5432/db?sslmode=disable"

	t.Run("explicit MaxConns is used", func(t *testing.T) {
		got, err := poolConfig(config.DBConfig{DSN: dsn, MaxConns: 7})
		if err != nil {
			t.Fatalf("poolConfig() error: %v", err)
		}
		if got.MaxConns != 7 {
			t.Errorf("MaxConns = %d, want 7", got.MaxConns)
		}
		if got.MaxConnLifetime != maxConnLifetime {
			t.Errorf("MaxConnLifetime = %v, want %v", got.MaxConnLifetime, maxConnLifetime)
		}
		if got.HealthCheckPeriod != healthCheckPeriod {
			t.Errorf("HealthCheckPeriod = %v, want %v", got.HealthCheckPeriod, healthCheckPeriod)
		}
	})

	t.Run("zero MaxConns defers to the pgx default", func(t *testing.T) {
		got, err := poolConfig(config.DBConfig{DSN: dsn, MaxConns: 0})
		if err != nil {
			t.Fatalf("poolConfig() error: %v", err)
		}
		// pgx's ParseConfig already applies a sane positive default; we must not
		// override it when MaxConns is unset (the DBConfig contract).
		if got.MaxConns <= 0 {
			t.Errorf("MaxConns = %d, want pgx's positive default", got.MaxConns)
		}
	})

	t.Run("invalid DSN returns an error", func(t *testing.T) {
		if _, err := poolConfig(config.DBConfig{DSN: "://not-a-dsn"}); err == nil {
			t.Error("poolConfig() = nil error, want error for invalid DSN")
		}
	})
}

// TestPoolConfigSupabase verifies the Supabase-specific connectivity tuning is
// applied only for the supabase provider, without needing a live database.
func TestPoolConfigSupabase(t *testing.T) {
	const tlsDSN = "postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=require"

	t.Run("transaction mode disables statement caching and uses exec mode", func(t *testing.T) {
		got, err := poolConfig(config.DBConfig{
			DSN:      tlsDSN,
			Provider: config.DBProviderSupabase,
			PoolMode: config.DBPoolModeTransaction,
		})
		if err != nil {
			t.Fatalf("poolConfig() error: %v", err)
		}
		if got.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
			t.Errorf("DefaultQueryExecMode = %v, want QueryExecModeExec", got.ConnConfig.DefaultQueryExecMode)
		}
		if got.ConnConfig.StatementCacheCapacity != 0 {
			t.Errorf("StatementCacheCapacity = %d, want 0", got.ConnConfig.StatementCacheCapacity)
		}
		if got.ConnConfig.DescriptionCacheCapacity != 0 {
			t.Errorf("DescriptionCacheCapacity = %d, want 0", got.ConnConfig.DescriptionCacheCapacity)
		}
	})

	t.Run("session mode keeps pgx's cached-statement defaults", func(t *testing.T) {
		got, err := poolConfig(config.DBConfig{
			DSN:      tlsDSN,
			Provider: config.DBProviderSupabase,
			PoolMode: config.DBPoolModeSession,
		})
		if err != nil {
			t.Fatalf("poolConfig() error: %v", err)
		}
		// Compare against a plain parse so the assertion does not hard-code pgx's
		// default exec mode / cache sizes.
		base, err := pgxpool.ParseConfig(tlsDSN)
		if err != nil {
			t.Fatalf("ParseConfig() error: %v", err)
		}
		if got.ConnConfig.DefaultQueryExecMode != base.ConnConfig.DefaultQueryExecMode {
			t.Errorf("DefaultQueryExecMode = %v, want pgx default %v",
				got.ConnConfig.DefaultQueryExecMode, base.ConnConfig.DefaultQueryExecMode)
		}
		if got.ConnConfig.StatementCacheCapacity != base.ConnConfig.StatementCacheCapacity {
			t.Errorf("StatementCacheCapacity = %d, want pgx default %d",
				got.ConnConfig.StatementCacheCapacity, base.ConnConfig.StatementCacheCapacity)
		}
	})

	t.Run("TLS is required: sslmode=disable is rejected", func(t *testing.T) {
		_, err := poolConfig(config.DBConfig{
			DSN:      "postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=disable",
			Provider: config.DBProviderSupabase,
			PoolMode: config.DBPoolModeTransaction,
		})
		if err == nil {
			t.Fatal("poolConfig() = nil error, want TLS-required error for sslmode=disable")
		}
		if !strings.Contains(err.Error(), "TLS") {
			t.Errorf("error = %q, want it to mention TLS", err.Error())
		}
	})

	t.Run("postgres provider is unaffected by sslmode=disable", func(t *testing.T) {
		const disabledDSN = "postgres://u:p@localhost:5432/db?sslmode=disable"
		got, err := poolConfig(config.DBConfig{
			DSN:      disabledDSN,
			Provider: config.DBProviderPostgres,
			PoolMode: config.DBPoolModeSession,
		})
		if err != nil {
			t.Fatalf("poolConfig() error: %v", err)
		}
		base, err := pgxpool.ParseConfig(disabledDSN)
		if err != nil {
			t.Fatalf("ParseConfig() error: %v", err)
		}
		// The postgres path must not touch the exec mode at all.
		if got.ConnConfig.DefaultQueryExecMode != base.ConnConfig.DefaultQueryExecMode {
			t.Errorf("postgres DefaultQueryExecMode = %v, want unchanged %v",
				got.ConnConfig.DefaultQueryExecMode, base.ConnConfig.DefaultQueryExecMode)
		}
	})

	t.Run("unreadable root cert fails fast", func(t *testing.T) {
		// pgx reads sslrootcert during ParseConfig, so a missing CA file errors
		// before any connection attempt.
		_, err := poolConfig(config.DBConfig{
			DSN:         tlsDSN,
			Provider:    config.DBProviderSupabase,
			PoolMode:    config.DBPoolModeSession,
			SSLRootCert: filepath.Join(t.TempDir(), "does-not-exist.crt"),
		})
		if err == nil {
			t.Fatal("poolConfig() = nil error, want error for unreadable sslrootcert")
		}
	})
}

// TestApplySSLRootCert verifies the sslrootcert parameter is injected for both
// DSN forms in a way pgx can parse.
func TestApplySSLRootCert(t *testing.T) {
	const certPath = "/etc/ssl/ca.crt"

	t.Run("url form preserves existing params", func(t *testing.T) {
		got, err := applySSLRootCert("postgres://u:p@host:6543/db?sslmode=verify-full", certPath)
		if err != nil {
			t.Fatalf("applySSLRootCert() error: %v", err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse result: %v", err)
		}
		if u.Query().Get("sslrootcert") != certPath {
			t.Errorf("sslrootcert = %q, want %q", u.Query().Get("sslrootcert"), certPath)
		}
		if u.Query().Get("sslmode") != "verify-full" {
			t.Errorf("sslmode = %q, want verify-full (preserved)", u.Query().Get("sslmode"))
		}
	})

	t.Run("keyword form quotes the parameter", func(t *testing.T) {
		got, err := applySSLRootCert("host=db sslmode=verify-full", certPath)
		if err != nil {
			t.Fatalf("applySSLRootCert() error: %v", err)
		}
		if !strings.Contains(got, "sslrootcert='"+certPath+"'") {
			t.Errorf("result = %q, want it to contain sslrootcert='%s'", got, certPath)
		}
	})

	t.Run("keyword form keeps a spaced path intact", func(t *testing.T) {
		const spaced = "/var/certs/my app/ca.pem"
		got, err := applySSLRootCert("host=db sslmode=require", spaced)
		if err != nil {
			t.Fatalf("applySSLRootCert() error: %v", err)
		}
		// pgx reads the (nonexistent) CA file, so ParseConfig is expected to fail —
		// but its error must name the full path, proving the value was parsed as a
		// single quoted token rather than truncated at the space.
		_, perr := pgxpool.ParseConfig(got)
		if perr == nil {
			t.Fatalf("ParseConfig(%q) = nil error, want a CA-file error referencing the full path", got)
		}
		if !strings.Contains(perr.Error(), spaced) {
			t.Errorf("ParseConfig error = %v, want it to reference the full path %q (value not truncated at the space)", perr, spaced)
		}
	})
}

// TestNewInvalidDSN confirms New fails fast on a malformed DSN without needing
// a live database.
func TestNewInvalidDSN(t *testing.T) {
	if pool, err := New(context.Background(), config.DBConfig{DSN: "://nope", ConnTimeout: time.Second}); err == nil {
		pool.Close()
		t.Error("New() = nil error, want error for invalid DSN")
	}
}

// TestNewAndHealth is an integration test exercising a real connection. It is
// skipped unless NESTOVA_TEST_DATABASE_URL points at a reachable test database,
// keeping the default `make test` run hermetic.
func TestNewAndHealth(t *testing.T) {
	dsn := os.Getenv("NESTOVA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NESTOVA_TEST_DATABASE_URL to run the live Postgres integration test")
	}

	pool, err := New(context.Background(), config.DBConfig{DSN: dsn, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Health(ctx, pool); err != nil {
		t.Errorf("Health() error: %v", err)
	}
}
