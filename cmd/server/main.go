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
	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
)

// shutdownTimeout bounds how long in-flight requests have to drain on shutdown.
const shutdownTimeout = 10 * time.Second

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
	cfg := config.Load()
	srv := httpserver.New(cfg.Addr)

	// Surface listen errors from the background goroutine to the main flow.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("starting http server", "addr", cfg.Addr, "env", cfg.Env)
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
