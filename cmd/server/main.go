// Command server is the Nestova application entrypoint. It wires runtime
// configuration to the HTTP server and runs it with graceful shutdown.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
)

// shutdownTimeout bounds how long in-flight requests have to drain on shutdown.
// It is kept at or above the HTTP layer's per-request timeout (13s) so a request
// running up to its deadline can still finish during a graceful shutdown.
const shutdownTimeout = 15 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// run starts the HTTP server and blocks until an interrupt signal triggers a
// graceful shutdown. It is separated from main so the lifecycle has a single
// error return that is straightforward to test.
func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Establish the Postgres pool up front so a bad DSN or unreachable database
	// fails fast at boot (db.New bounds the connectivity check with
	// DB.ConnTimeout). Feature contexts (repositories, sessions) consume the
	// pool as those tickets land.
	pool, err := db.New(context.Background(), cfg.DB)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("connected to postgres", "max_conns", pool.Config().MaxConns)

	// The readiness probe verifies live database connectivity on each call.
	srv := httpserver.New(cfg, httpserver.Deps{
		Logger: logger,
		Ready: func(ctx context.Context) error {
			return db.Health(ctx, pool)
		},
	})

	// Surface listen errors from the background goroutine to the main flow.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("starting http server", "addr", cfg.Server.Addr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
