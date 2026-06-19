// Command server is the Nestova application entrypoint. It wires runtime
// configuration to the HTTP server and runs it with graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	notifyapp "github.com/ericfisherdev/nestova/internal/notify/app"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	trackingadapter "github.com/ericfisherdev/nestova/internal/tracking/adapter"
	trackingapp "github.com/ericfisherdev/nestova/internal/tracking/app"
)

// shutdownTimeout bounds how long in-flight requests have to drain on shutdown.
// It is kept at or above the HTTP layer's per-request timeout (13s) so a request
// running up to its deadline can still finish during a graceful shutdown.
const shutdownTimeout = 15 * time.Second

// Notification dispatcher tuning (NES-24).
const (
	// dispatchBatchSize caps how many due notifications one poll cycle claims.
	dispatchBatchSize = 50
	// dispatchPollInterval is how often the dispatcher polls the outbox.
	dispatchPollInterval = 30 * time.Second
)

// Task scheduler tuning (NES-31).
const (
	// taskGenerationHorizon is how far ahead of now task instances are
	// materialised by the background generator. Two weeks keeps upcoming
	// instances visible without over-generating.
	taskGenerationHorizon = 14 * 24 * time.Hour
	// taskSchedulerPollInterval is how often the scheduler runs a
	// generation+overdue-sweep cycle. Five minutes is sufficient; the
	// overdue sweep is idempotent so occasional double-runs are harmless.
	taskSchedulerPollInterval = 5 * time.Minute
)

// Restock scheduler tuning (NES-44).
const (
	// restockSchedulerPollInterval is how often the restock scheduler recomputes
	// predictions and raises restock entries. Restock is a slow-moving,
	// idempotent signal (depletion intervals are measured in days), so hourly is
	// ample and keeps load low.
	restockSchedulerPollInterval = time.Hour
	// restockSchedulerTickTimeout bounds a single recompute+restock cycle (and so
	// how long an in-flight cycle can delay shutdown). It is decoupled from the
	// long poll interval so graceful shutdown is not held up for an hour.
	restockSchedulerTickTimeout = 5 * time.Minute
)

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

	// NES-23: session manager backed by Postgres (scs + pgxstore).
	sm := authadapter.NewSessionManager(pool, cfg.Session)

	// NES-23: auth bounded context wiring.
	credRepo := authadapter.NewCredentialRepository(pool)
	authn := authapp.New(credRepo)
	householdRepo := householdadapter.NewPostgresRepository(pool)
	authHandlers := authadapter.NewHandlers(sm, authn, logger)

	// NES-26: onboarding + member provisioning handlers. The provisioner runs the
	// multi-table writes (household + member + credentials) atomically in one
	// transaction; it lives in the composition root so neither bounded-context
	// adapter imports the other.
	provisioner := newTxProvisioner(pool)
	onboardingHandlers := authadapter.NewOnboardingHandlers(householdRepo, credRepo, provisioner, sm, logger)

	// NES-24: notification outbox wiring.
	outboxRepo := notifyadapter.NewOutboxRepository(pool)
	inAppSender := notifyadapter.NewInAppSender(logger)
	dispatcher, err := notifyapp.NewDispatcher(
		outboxRepo,
		[]domain.Sender{inAppSender},
		logger,
		dispatchBatchSize,
		dispatchPollInterval,
	)
	if err != nil {
		return fmt.Errorf("create dispatcher: %w", err)
	}

	// NES-31: task scheduler wiring.
	recurringTaskRepo := tasksadapter.NewRecurringTaskRepository(pool)
	taskInstanceRepo := tasksadapter.NewTaskInstanceRepository(pool)
	taskGenerator, err := tasksapp.NewGenerator(recurringTaskRepo, taskInstanceRepo, logger, taskGenerationHorizon)
	if err != nil {
		return fmt.Errorf("create task generator: %w", err)
	}
	// outboxRepo satisfies notifydomain.Enqueuer (it embeds Enqueue); passing it
	// here lets the scheduler emit due-soon and overdue reminders via the same
	// outbox the dispatcher already consumes (NES-34).
	taskScheduler, err := tasksapp.NewScheduler(taskGenerator, taskInstanceRepo, outboxRepo, logger, taskSchedulerPollInterval)
	if err != nil {
		return fmt.Errorf("create task scheduler: %w", err)
	}

	// NES-44: restock automation. The scheduler recomputes predictions, raises
	// idempotent restock shopping entries, and emits notifications via the same
	// outbox the dispatcher consumes.
	trackedItemRepo := trackingadapter.NewTrackedItemRepository(pool)
	usageEventRepo := trackingadapter.NewUsageEventRepository(pool)
	restockPredictionRepo := trackingadapter.NewRestockPredictionRepository(pool)
	ingredientRepo := trackingadapter.NewIngredientRepository(pool)
	shoppingListRepo := trackingadapter.NewShoppingListRepository(pool)
	predictor, err := trackingapp.NewPredictor(usageEventRepo, restockPredictionRepo)
	if err != nil {
		return fmt.Errorf("create restock predictor: %w", err)
	}
	restockScheduler, err := trackingapp.NewRestockScheduler(
		trackedItemRepo, predictor, ingredientRepo, shoppingListRepo, outboxRepo, logger,
		restockSchedulerPollInterval, restockSchedulerTickTimeout,
	)
	if err != nil {
		return fmt.Errorf("create restock scheduler: %w", err)
	}

	// NES-32: task UI wiring — TaskService + HTTP handlers for the tasks list
	// and the three mutation actions (complete, skip, claim).
	taskService, err := tasksapp.NewTaskService(recurringTaskRepo, taskInstanceRepo)
	if err != nil {
		return fmt.Errorf("create task service: %w", err)
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(taskService, recurringTaskRepo, taskInstanceRepo, householdRepo, sm, logger)

	// NES-37: gamification UI wiring — scoreboard, streaks, and reward redemption.
	pointLedgerRepo := tasksadapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := tasksadapter.NewRewardPostgresRepository(pool)
	rewardService := tasksapp.NewRewardService(rewardRepo, logger)
	gamificationWebHandlers := tasksadapter.NewGamificationWebHandlers(
		pointLedgerRepo,
		rewardRepo,
		rewardService,
		taskInstanceRepo,
		householdRepo,
		sm,
		logger,
	)

	srv := httpserver.New(cfg, httpserver.Deps{
		Logger: logger,
		Ready: func(ctx context.Context) error {
			return db.Health(ctx, pool)
		},
		// NES-23: session loading + authentication injected between Recoverer
		// and Timeout (canonical chain order per server.go).
		Middleware: []middleware.Middleware{
			sm.LoadAndSave,
			authadapter.Authenticate(sm, householdRepo),
		},
		Routes: func(mux *http.ServeMux) {
			registerWebRoutes(mux, logger, sm, authHandlers, onboardingHandlers, householdRepo, taskWebHandlers, gamificationWebHandlers)
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

	// NES-24: the dispatcher uses the signal-cancelled ctx so it stops cleanly
	// on SIGINT/SIGTERM alongside the HTTP server. dispatcherDone closes once the
	// goroutine has fully exited, so shutdown waits for it before pool.Close runs.
	dispatcherDone := make(chan struct{})
	go func() {
		defer close(dispatcherDone)
		dispatcher.Run(ctx)
	}()

	// NES-31: the task scheduler uses the same signal-cancelled ctx. Each tick
	// runs under its own bounded context (context.Background()), so an in-flight
	// generation+sweep cycle completes its database writes before Run returns.
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		taskScheduler.Run(ctx)
	}()

	// NES-44: the restock scheduler is a third independent background worker on
	// the same signal-cancelled ctx. Each tick runs under its own bounded context,
	// so an in-flight recompute+restock cycle finishes its writes before Run
	// returns; restockSchedulerDone is awaited below before pool.Close.
	restockSchedulerDone := make(chan struct{})
	go func() {
		defer close(restockSchedulerDone)
		restockScheduler.Run(ctx)
	}()

	var runErr error
	select {
	case err := <-serverErr:
		runErr = err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Cancel ctx (even on the serverErr path) so both background workers stop
	// looping.
	stop()

	// Stop HTTP intake first so no new request can enqueue work while the
	// background workers drain.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)

	// Then wait for both Run methods to return. Because each worker's tick runs
	// under its own context (not this one), in-flight cycles finish their database
	// writes before Run returns, so these waits drain them cleanly ahead of
	// pool.Close.
	<-dispatcherDone
	<-schedulerDone
	<-restockSchedulerDone

	if shutdownErr != nil {
		return errors.Join(runErr, shutdownErr)
	}
	return runErr
}
