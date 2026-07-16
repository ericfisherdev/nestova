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
	calendaradapter "github.com/ericfisherdev/nestova/internal/calendar/adapter"
	calendarapp "github.com/ericfisherdev/nestova/internal/calendar/app"
	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	mealsapp "github.com/ericfisherdev/nestova/internal/meals/app"
	mealsdomain "github.com/ericfisherdev/nestova/internal/meals/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediaapp "github.com/ericfisherdev/nestova/internal/media/app"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	notifyapp "github.com/ericfisherdev/nestova/internal/notify/app"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/bootstrap"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
	subscriptionsadapter "github.com/ericfisherdev/nestova/internal/subscriptions/adapter"
	subscriptionsapp "github.com/ericfisherdev/nestova/internal/subscriptions/app"
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

// Renewal scheduler tuning (NES-65).
const (
	// renewalSchedulerPollInterval is how often the renewal scheduler raises
	// subscription reminders and advances past-due renewals. Renewals move on a
	// daily granularity (next_renewal_on is a date), so hourly polling is ample
	// and keeps load low.
	renewalSchedulerPollInterval = time.Hour
	// renewalSchedulerTickTimeout bounds a single reminder+advance cycle (and so
	// how long an in-flight cycle can delay shutdown), decoupled from the long
	// poll interval so graceful shutdown is not held up for an hour.
	renewalSchedulerTickTimeout = 5 * time.Minute
)

// Calendar sync scheduler tuning (NES-68).
const (
	// calendarSyncPollInterval is how often the sync engine pulls each connected
	// account's events. Incremental sync via the sync token keeps each pass cheap,
	// so a 15-minute cadence keeps the cache reasonably fresh without heavy load.
	calendarSyncPollInterval = 15 * time.Minute
	// calendarSyncTickTimeout bounds a single sync cycle across all accounts (and
	// so how long an in-flight cycle can delay shutdown), decoupled from the poll
	// interval.
	calendarSyncTickTimeout = 5 * time.Minute
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// run() returns outcomeRestart once first-run setup completes, so the boot is
	// retried in normal mode with the now-persisted configuration. Any other
	// outcome (clean shutdown) exits the loop.
	for {
		result, err := run(logger)
		if err != nil {
			logger.Error("server exited with error", "error", err)
			os.Exit(1)
		}
		if result != outcomeRestart {
			return
		}
		// Setup just completed and persisted its configuration. Clear any
		// forced-setup override for this process so the restart boots normally
		// instead of re-entering the wizard — NESTOVA_FORCE_SETUP would otherwise
		// make NeedsSetup return true indefinitely, looping forever.
		if err := os.Unsetenv(bootstrap.ForceSetupEnv); err != nil {
			logger.Error("failed to clear force-setup flag", "error", err)
			os.Exit(1)
		}
		logger.Info("first-run setup complete; restarting in normal mode")
	}
}

// run decides between first-run setup mode and normal operation. When nothing is
// configured (no DATABASE_URL and no persisted state file) it serves the setup
// wizard; otherwise it applies any persisted state to the environment and boots
// the full server. Detection lives here, ahead of config.Load, so the wizard can
// run before a complete configuration (notably DATABASE_URL) exists.
func run(logger *slog.Logger) (outcome, error) {
	statePath := bootstrap.StatePath()
	state, err := bootstrap.LoadState(statePath)
	if err != nil {
		return outcomeShutdown, fmt.Errorf("load setup state: %w", err)
	}
	if bootstrap.NeedsSetup(state) {
		logger.Info("no database configured; entering first-run setup", "state_file", statePath)
		return runSetup(logger, statePath)
	}
	// Persisted first-run config feeds the unchanged env-based config.Load; the
	// real environment still wins (ExportToEnv only sets unset variables).
	if err := bootstrap.ExportToEnv(state); err != nil {
		return outcomeShutdown, fmt.Errorf("apply setup state: %w", err)
	}
	return outcomeShutdown, runServer(logger)
}

// listenAndServe starts srv with app-terminated TLS when cert+key are configured
// (NES-54), otherwise plain HTTP. Both paths return http.ErrServerClosed on a
// graceful Shutdown, which the caller treats as a clean exit. The branch is a
// thin wrapper so the cert-configured decision (TLSConfig.Enabled) stays unit
// testable without binding a socket.
func listenAndServe(srv *http.Server, tlsCfg config.TLSConfig) error {
	if tlsCfg.Enabled() {
		return srv.ListenAndServeTLS(tlsCfg.CertFile, tlsCfg.KeyFile)
	}
	return srv.ListenAndServe()
}

// runServer starts the HTTP server and blocks until an interrupt signal triggers
// a graceful shutdown. It is separated from main so the lifecycle has a single
// error return that is straightforward to test.
func runServer(logger *slog.Logger) error {
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

	// NES-114/NES-115: Prometheus instrumentation. One registry (runtime +
	// process + build-info collectors) feeds the per-request middleware metrics,
	// the background scheduler tick recorder, the calendar sync metrics, and the
	// GET /metrics scrape endpoint. It is built ahead of the background workers
	// so their constructors receive the shared tick recorder.
	registry := metrics.NewRegistry()
	httpMetrics := metrics.NewHTTPMetrics(registry)
	tickRecorder := metrics.NewPromTickRecorder(registry)
	syncMetrics := metrics.NewSyncMetrics(registry)

	// NES-24: notification outbox wiring.
	outboxRepo := notifyadapter.NewOutboxRepository(pool)
	inAppSender := notifyadapter.NewInAppSender(logger)
	dispatcher, err := notifyapp.NewDispatcher(
		outboxRepo,
		[]domain.Sender{inAppSender},
		logger,
		tickRecorder,
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
	// NES-121: chore trade wiring. choreTradeRepo is only needed here for the
	// scheduler's trade-expiry sweep step; a separate TradeService for the
	// propose/accept/decline/cancel HTTP handlers is wired by NES-122.
	choreTradeRepo := tasksadapter.NewTradeRepository(pool)
	// outboxRepo satisfies notifydomain.Enqueuer (it embeds Enqueue); passing it
	// here lets the scheduler emit due-soon and overdue reminders via the same
	// outbox the dispatcher already consumes (NES-34).
	taskScheduler, err := tasksapp.NewScheduler(taskGenerator, taskInstanceRepo, choreTradeRepo, outboxRepo, logger, tickRecorder, taskSchedulerPollInterval)
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
	pantryRepo := trackingadapter.NewPantryRepository(pool)
	shoppingListRepo := trackingadapter.NewShoppingListRepository(pool)
	predictor, err := trackingapp.NewPredictor(usageEventRepo, restockPredictionRepo)
	if err != nil {
		return fmt.Errorf("create restock predictor: %w", err)
	}
	restockScheduler, err := trackingapp.NewRestockScheduler(
		trackedItemRepo, predictor, ingredientRepo, shoppingListRepo, outboxRepo, logger,
		tickRecorder, restockSchedulerPollInterval, restockSchedulerTickTimeout,
	)
	if err != nil {
		return fmt.Errorf("create restock scheduler: %w", err)
	}

	// NES-65: subscription renewal automation. The scheduler raises one reminder
	// per renewal occurrence (idempotent) and rolls past-due renewals forward,
	// emitting reminders via the same outbox the dispatcher consumes.
	subscriptionRepo := subscriptionsadapter.NewSubscriptionRepository(pool)
	renewalScheduler, err := subscriptionsapp.NewRenewalScheduler(
		subscriptionRepo, outboxRepo, logger,
		tickRecorder, renewalSchedulerPollInterval, renewalSchedulerTickTimeout,
	)
	if err != nil {
		return fmt.Errorf("create renewal scheduler: %w", err)
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

	// NES-45: groceries UI wiring — usage tracker, pantry, and shopping list.
	usageService, err := trackingapp.NewUsageService(trackedItemRepo, usageEventRepo, predictor, logger)
	if err != nil {
		return fmt.Errorf("create usage service: %w", err)
	}
	pantryService, err := trackingapp.NewPantryService(pantryRepo)
	if err != nil {
		return fmt.Errorf("create pantry service: %w", err)
	}
	shoppingListService, err := trackingapp.NewShoppingListService(shoppingListRepo)
	if err != nil {
		return fmt.Errorf("create shopping list service: %w", err)
	}
	groceryWebHandlers := trackingadapter.NewWebHandlers(
		usageService,
		pantryService,
		shoppingListService,
		trackedItemRepo,
		restockPredictionRepo,
		ingredientRepo,
		ingredientRepo,
		householdRepo,
		sm,
		logger,
	)

	// NES-62: meals UI wiring — recipe box, weekly planner, and ingredient finder.
	recipeRepo := mealsadapter.NewRecipeRepository(pool)
	mealPlanRepo := mealsadapter.NewMealPlanRepository(pool)
	recipeService, err := mealsapp.NewRecipeService(recipeRepo, ingredientRepo)
	if err != nil {
		return fmt.Errorf("create recipe service: %w", err)
	}
	plannerService, err := mealsapp.NewPlannerService(mealPlanRepo, recipeRepo)
	if err != nil {
		return fmt.Errorf("create planner service: %w", err)
	}
	groceryFromPlanService, err := mealsapp.NewGroceryFromPlanService(mealPlanRepo, recipeRepo, shoppingListRepo)
	if err != nil {
		return fmt.Errorf("create grocery-from-plan service: %w", err)
	}
	localRecipeSource, err := mealsadapter.NewLocalRecipeSource(recipeRepo)
	if err != nil {
		return fmt.Errorf("create local recipe source: %w", err)
	}
	// The external "discover more" source is built only when configured; otherwise
	// the finder serves recipe-box results alone (DIP: callers see only the port).
	var externalRecipeSource mealsdomain.RecipeSource
	if cfg.Recipes.ExternalEnabled {
		externalRecipeSource, err = mealsadapter.NewExternalRecipeSource(
			&http.Client{Timeout: mealsadapter.ExternalRequestTimeout},
			cfg.Recipes.BaseURL, cfg.Recipes.APIKey, recipeRepo, ingredientRepo, ingredientRepo,
		)
		if err != nil {
			return fmt.Errorf("create external recipe source: %w", err)
		}
	}
	recipeSource, err := mealsapp.SelectRecipeSource(cfg.Recipes.ExternalEnabled, localRecipeSource, externalRecipeSource)
	if err != nil {
		return fmt.Errorf("select recipe source: %w", err)
	}
	finderService, err := mealsapp.NewFinderService(recipeSource, pantryRepo, ingredientRepo)
	if err != nil {
		return fmt.Errorf("create finder service: %w", err)
	}
	mealsWebHandlers := mealsadapter.NewWebHandlers(
		recipeService, plannerService, finderService, groceryFromPlanService, ingredientRepo, sm, logger,
	)

	// NES-67: Google OAuth calendar connection. The cipher protects stored tokens
	// at rest; the state signer (keyed by the session secret) protects the OAuth
	// round trip. The encryption key is validated at config load.
	encryptionKey, err := cfg.Crypto.Key()
	if err != nil {
		return fmt.Errorf("load encryption key: %w", err)
	}
	tokenCipher, err := crypto.NewCipher(encryptionKey)
	if err != nil {
		return fmt.Errorf("create token cipher: %w", err)
	}
	oauthStateSigner, err := calendarapp.NewOAuthStateSigner([]byte(cfg.Session.Secret))
	if err != nil {
		return fmt.Errorf("create oauth state signer: %w", err)
	}
	calendarAccountRepo := calendaradapter.NewCalendarAccountRepository(pool)
	googleOAuthClient := calendaradapter.NewGoogleOAuthClient(
		cfg.OAuth.GoogleClientID, cfg.OAuth.GoogleClientSecret, cfg.OAuth.GoogleRedirectURL,
	)
	accountService, err := calendarapp.NewAccountService(calendarAccountRepo, tokenCipher, googleOAuthClient, oauthStateSigner, logger)
	if err != nil {
		return fmt.Errorf("create calendar account service: %w", err)
	}
	calendarWebHandlers := calendaradapter.NewWebHandlers(accountService, sm, logger)

	// NES-68: Google Calendar sync. The sync service pulls each connected
	// account's events into the external-event cache, obtaining a valid access
	// token via the account service (which refreshes transparently).
	externalEventRepo := calendaradapter.NewExternalEventRepository(pool)
	googleCalendarClient := calendaradapter.NewGoogleCalendarClient()
	calendarSyncService, err := calendarapp.NewSyncService(calendarAccountRepo, externalEventRepo, googleCalendarClient, accountService, logger, syncMetrics)
	if err != nil {
		return fmt.Errorf("create calendar sync service: %w", err)
	}
	calendarSyncScheduler, err := calendarapp.NewSyncScheduler(calendarSyncService, logger, tickRecorder, calendarSyncPollInterval, calendarSyncTickTimeout)
	if err != nil {
		return fmt.Errorf("create calendar sync scheduler: %w", err)
	}

	// NES-70: subscriptions UI and the unified calendar view.
	subscriptionService, err := subscriptionsapp.NewSubscriptionService(subscriptionRepo)
	if err != nil {
		return fmt.Errorf("create subscription service: %w", err)
	}
	subscriptionCostService := subscriptionsapp.NewCostService(subscriptionRepo)
	subscriptionWebHandlers := subscriptionsadapter.NewWebHandlers(subscriptionService, subscriptionCostService, householdRepo, sm, logger)
	unifiedCalendarService, err := calendarapp.NewUnifiedCalendarService(externalEventRepo, taskInstanceRepo, recurringTaskRepo, subscriptionRepo, householdRepo, logger)
	if err != nil {
		return fmt.Errorf("create unified calendar service: %w", err)
	}
	calendarViewHandlers := calendaradapter.NewViewHandlers(unifiedCalendarService, calendarAccountRepo, householdRepo, sm, logger)

	// NES-7: media (rotating photo album) — storage, services, and the /photos UI.
	photoStore, err := mediaadapter.NewLocalPhotoStore(cfg.Media.Root, cfg.Media.MaxUploadBytes)
	if err != nil {
		return fmt.Errorf("create photo store: %w", err)
	}
	albumRepo := mediaadapter.NewAlbumRepository(pool)
	photoRepo := mediaadapter.NewPhotoRepository(pool)
	albumPhotoRepo := mediaadapter.NewAlbumPhotoRepository(pool)
	photoService, err := mediaapp.NewPhotoService(photoStore, mediaadapter.NewExifReader(), photoRepo)
	if err != nil {
		return fmt.Errorf("create photo service: %w", err)
	}
	albumService, err := mediaapp.NewAlbumService(albumRepo, photoRepo, albumPhotoRepo)
	if err != nil {
		return fmt.Errorf("create album service: %w", err)
	}
	mediaWebHandlers := mediaadapter.NewWebHandlers(albumService, photoService, householdRepo, sm, logger)

	srv := httpserver.New(cfg, httpserver.Deps{
		Logger: logger,
		Ready: func(ctx context.Context) error {
			return db.Health(ctx, pool)
		},
		HTTPMetrics: httpMetrics,
		// metrics.Handler keeps the promhttp dependency inside the metrics
		// package and reports scrape errors as metrics instead of failing
		// silently (NES-114).
		MetricsHandler: metrics.Handler(registry),
		// NES-23: session loading + authentication injected between Recoverer
		// and Timeout (canonical chain order per server.go).
		Middleware: []middleware.Middleware{
			sm.LoadAndSave,
			authadapter.Authenticate(sm, householdRepo),
		},
		Routes: func(mux *http.ServeMux) {
			registerWebRoutes(mux, logger, sm, authHandlers, onboardingHandlers, householdRepo, taskWebHandlers, gamificationWebHandlers, groceryWebHandlers, mealsWebHandlers, calendarWebHandlers)
			registerCalendarSubscriptionPages(mux, logger, sm, householdRepo, calendarViewHandlers, subscriptionWebHandlers)
			registerMediaPages(mux, logger, sm, householdRepo, mediaWebHandlers)
		},
	})

	// Surface listen errors from the background goroutine to the main flow.
	serverErr := make(chan error, 1)
	go func() {
		// NES-54: terminate TLS in-process when cert+key are configured; otherwise
		// serve plain HTTP and rely on a reverse proxy for TLS (the default).
		logger.Info("starting http server", "addr", cfg.Server.Addr, "env", cfg.Env, "tls", cfg.TLS.Enabled())
		if err := listenAndServe(srv, cfg.TLS); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

	// NES-65: the renewal scheduler is a fourth independent background worker on
	// the same signal-cancelled ctx. Each tick runs under its own bounded context,
	// so an in-flight reminder+advance cycle finishes its writes before Run
	// returns; renewalSchedulerDone is awaited below before pool.Close.
	renewalSchedulerDone := make(chan struct{})
	go func() {
		defer close(renewalSchedulerDone)
		renewalScheduler.Run(ctx)
	}()

	// NES-68: the calendar sync scheduler is a fifth background worker on the
	// same signal-cancelled ctx; each tick runs under its own bounded context, so
	// an in-flight sync cycle finishes its writes before Run returns.
	calendarSyncSchedulerDone := make(chan struct{})
	go func() {
		defer close(calendarSyncSchedulerDone)
		calendarSyncScheduler.Run(ctx)
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
	<-renewalSchedulerDone
	<-calendarSyncSchedulerDone

	if shutdownErr != nil {
		return errors.Join(runErr, shutdownErr)
	}
	return runErr
}
