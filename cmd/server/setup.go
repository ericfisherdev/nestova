package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/ericfisherdev/nestova/internal/platform/bootstrap"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	dbmigrate "github.com/ericfisherdev/nestova/internal/platform/db/migrate"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
	"github.com/ericfisherdev/nestova/internal/platform/setup"
)

// outcome reports how run() ended so main() can decide whether to restart.
type outcome int

const (
	// outcomeShutdown means the process should exit after run() returns.
	outcomeShutdown outcome = iota
	// outcomeRestart means first-run setup completed; run() should be invoked
	// again so the app boots normally with the now-persisted configuration.
	outcomeRestart
)

const (
	// setupConnectTimeout bounds the connectivity check the wizard performs
	// against the operator-supplied DSN.
	setupConnectTimeout = 5 * time.Second
	// setupSessionLifetime bounds the in-memory setup session, which exists only
	// to back the wizard's CSRF token.
	setupSessionLifetime = time.Hour
)

// runSetup serves the first-run setup wizard until the operator completes it
// (returning outcomeRestart so main() re-runs in normal mode) or an interrupt
// arrives (outcomeShutdown). The wizard runs without a database: sessions are
// in-memory and back only CSRF, and the only routes mounted are the wizard plus
// a catch-all that funnels every other path to /setup.
func runSetup(logger *slog.Logger, statePath string) (outcome, error) {
	sm := scs.New() // default in-memory store; no database required.
	sm.Lifetime = setupSessionLifetime
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Path = "/"

	service := setup.NewService(
		// Ping by building the pool db.New would build at boot (with the provider
		// applied), then closing it, so the wizard validates exactly the
		// connectivity the server will use.
		pingerFunc(func(ctx context.Context, conn setup.Conn) error {
			pool, err := db.New(ctx, dbConfigForConn(conn))
			if err != nil {
				return err
			}
			pool.Close()
			return nil
		}),
		migratorFunc(func(ctx context.Context, conn setup.Conn) error {
			dsn, err := migrationDSN(conn)
			if err != nil {
				return err
			}
			return dbmigrate.Up(ctx, dsn)
		}),
		stateStoreFunc(func(state *bootstrap.State) error {
			return bootstrap.SaveState(statePath, state)
		}),
	)

	// done is closed exactly once when setup succeeds; the select below then
	// shuts the server down and returns outcomeRestart.
	done := make(chan struct{})
	var once sync.Once
	onComplete := func() { once.Do(func() { close(done) }) }

	setupToken := os.Getenv(setup.SetupTokenEnv)
	if setupToken == "" {
		logger.Warn("first-run setup screen is unauthenticated; set NESTOVA_SETUP_TOKEN to require a token")
	}
	handlers := setup.NewHandlers(service, sm, logger, onComplete, setupToken)

	// A minimal config is enough for the HTTP layer: setup mode serves plain HTTP
	// (TLS terminates at a proxy if any), with no readiness check or HSTS.
	cfg := config.Config{
		Env:    config.EnvProd,
		Server: config.ServerConfig{Addr: config.ServerAddrFromEnv(), TrustedProxies: "127.0.0.0/8,::1/128"},
	}
	srv := httpserver.New(cfg, httpserver.Deps{
		Logger:     logger,
		Middleware: []middleware.Middleware{sm.LoadAndSave},
		Routes:     handlers.Register,
	})

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("starting first-run setup server", "addr", cfg.Server.Addr, "state_file", statePath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	result := outcomeShutdown
	select {
	case err := <-serverErr:
		return outcomeShutdown, err
	case <-done:
		result = outcomeRestart
	case <-ctx.Done():
		logger.Info("shutdown signal received during setup")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return result, err
	}
	return result, nil
}

// dbConfigForConn maps a setup.Conn to the DBConfig the server builds at boot,
// applying the Supabase provider/pooler/TLS settings when present so the wizard's
// connectivity check exercises exactly the path the running server will use.
func dbConfigForConn(conn setup.Conn) config.DBConfig {
	cfg := config.DBConfig{DSN: conn.DSN, ConnTimeout: setupConnectTimeout}
	if conn.Provider == string(config.DBProviderSupabase) {
		cfg.Provider = config.DBProviderSupabase
		cfg.PoolMode = config.DBPoolMode(conn.PoolMode)
		cfg.SSLRootCert = conn.SSLRootCert
	}
	return cfg
}

// migrationDSN returns the DSN to run migrations against for conn. For the
// Supabase transaction pooler (port 6543) it routes migrations to the session
// endpoint (port 5432): goose serializes migrations with a session-level
// advisory lock that a transaction pooler cannot hold across multiplexed
// transactions, so the pooler must not be used for migrations. It also applies
// the SSL root cert so migrations use the same verify-full path as the pool.
func migrationDSN(conn setup.Conn) (string, error) {
	dsn := conn.DSN
	if conn.Provider == string(config.DBProviderSupabase) && conn.PoolMode == string(config.DBPoolModeTransaction) {
		rewritten, err := sessionEndpoint(dsn)
		if err != nil {
			return "", err
		}
		dsn = rewritten
	}
	if conn.SSLRootCert != "" {
		normalized, err := db.ApplySSLRootCert(dsn, conn.SSLRootCert)
		if err != nil {
			return "", err
		}
		dsn = normalized
	}
	return dsn, nil
}

// transactionPortRe matches the keyword/value "port=6543" token (the Supabase
// transaction pooler) so sessionEndpoint can rewrite it for non-URL DSNs.
var transactionPortRe = regexp.MustCompile(`(\bport=)6543\b`)

// sessionEndpoint rewrites a Supabase transaction-pooler DSN (port 6543) to the
// session endpoint (port 5432) on the same host, handling both URL and
// keyword/value forms. Other ports are left unchanged.
func sessionEndpoint(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse database dsn: %w", err)
		}
		if u.Port() == "6543" {
			u.Host = net.JoinHostPort(u.Hostname(), "5432")
		}
		return u.String(), nil
	}
	return transactionPortRe.ReplaceAllString(dsn, "${1}5432"), nil
}

// pingerFunc, migratorFunc, and stateStoreFunc adapt plain functions to the
// setup ports, wiring db.New / migrate.Up / bootstrap.SaveState in the
// composition root without standalone adapter types.
type pingerFunc func(ctx context.Context, conn setup.Conn) error

func (f pingerFunc) Ping(ctx context.Context, conn setup.Conn) error { return f(ctx, conn) }

type migratorFunc func(ctx context.Context, conn setup.Conn) error

func (f migratorFunc) MigrateUp(ctx context.Context, conn setup.Conn) error { return f(ctx, conn) }

type stateStoreFunc func(state *bootstrap.State) error

func (f stateStoreFunc) Save(state *bootstrap.State) error { return f(state) }
