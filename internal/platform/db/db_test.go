package db

import (
	"context"
	"os"
	"testing"
	"time"

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
