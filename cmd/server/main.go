// Command server is the Nestova application entrypoint. It wires runtime
// configuration to the HTTP server and runs it with graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	calendaradapter "github.com/ericfisherdev/nestova/internal/calendar/adapter"
	calendarapp "github.com/ericfisherdev/nestova/internal/calendar/app"
	deeplinkadapter "github.com/ericfisherdev/nestova/internal/deeplink/adapter"
	deeplinkapp "github.com/ericfisherdev/nestova/internal/deeplink/app"
	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	kioskadapter "github.com/ericfisherdev/nestova/internal/kiosk/adapter"
	kioskapp "github.com/ericfisherdev/nestova/internal/kiosk/app"
	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	mealsapp "github.com/ericfisherdev/nestova/internal/meals/app"
	mealsdomain "github.com/ericfisherdev/nestova/internal/meals/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediaapp "github.com/ericfisherdev/nestova/internal/media/app"
	mediabootstrap "github.com/ericfisherdev/nestova/internal/media/bootstrap"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	notifyapp "github.com/ericfisherdev/nestova/internal/notify/app"
	notifybootstrap "github.com/ericfisherdev/nestova/internal/notify/bootstrap"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/bootstrap"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
	"github.com/ericfisherdev/nestova/internal/platform/totp"
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

// photoStoreInitTimeout bounds mediabootstrap.NewPhotoStoreResolver's S3 startup
// reachability check (HeadBucket, see mediaadapter.NewS3PhotoStore's doc):
// a network-unreachable S3 endpoint must fail the boot promptly, not hang
// it indefinitely. Generous enough for a slow LAN MinIO/Garage instance or
// real AWS S3 over a home internet connection, short enough that a genuinely
// unreachable endpoint is reported quickly. Unused when backend=local (the
// local store has no network call to bound).
const photoStoreInitTimeout = 10 * time.Second

// smsSenderInitTimeout bounds notifybootstrap.NewSMSSender's AWS config
// loading (LoadDefaultConfig may reach the EC2/ECS instance metadata
// service to resolve credentials — see NewSMSSender's own doc). Unused
// when NOTIFY_SMS_ENABLED is false (the Noop sender has no network call to
// bound).
const smsSenderInitTimeout = 10 * time.Second

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

// deepLinkSignerPurpose is the derivation label passed to
// deeplinkapp.NewSignerFromSecret (NES-129), so the QR deep-link signing key
// is cryptographically distinct from every other consumer of
// cfg.Session.Secret (e.g. calendarapp.OAuthStateSigner, which signs with the
// raw secret directly) even though both trace back to the same root secret.
const deepLinkSignerPurpose = "nestova:deeplink:v1"

// rememberDeviceSignerPurpose is the derivation label passed to
// authapp.NewRememberDeviceSignerFromSecret (NES-135), keeping the "remember
// this device" cookie's signing key cryptographically distinct from every
// other consumer of cfg.Session.Secret, per the same reasoning as
// deepLinkSignerPurpose above.
const rememberDeviceSignerPurpose = "nestova:auth:remember-device:v1"

// webauthnUserHandleSignerPurpose is the derivation label passed to
// authapp.NewWebAuthnUserHandleDeriverFromSecret (NES-136), keeping the
// per-member WebAuthn user handle's derivation key cryptographically
// distinct from every other consumer of cfg.Session.Secret, per the same
// reasoning as deepLinkSignerPurpose above.
const webauthnUserHandleSignerPurpose = "nestova:auth:webauthn-user-handle:v1"

// webauthnRegistrationTimeout bounds how long a member has to complete a
// passkey registration ceremony (present the authenticator prompt and
// confirm) before the server-side challenge expires — enforced by
// webauthn.TimeoutConfig.Enforce (NES-136 AC: "challenges ... expire").
// Sixty seconds matches this library's own documented example default and
// is ample for a biometric prompt.
const webauthnRegistrationTimeout = 60 * time.Second

// webauthnLoginTimeout is registration's own analogue for a login or
// step-up assertion ceremony (NES-137): the same reasoning and the same
// duration — a biometric prompt takes no longer to confirm at login than
// at registration.
const webauthnLoginTimeout = 60 * time.Second

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

	// NES-23: auth bounded context wiring. authHandlers itself is
	// constructed further below (NES-135), once the MFA service and the
	// remember-device signer it now depends on both exist.
	credRepo := authadapter.NewCredentialRepository(pool)
	authn := authapp.New(credRepo)
	householdRepo := householdadapter.NewPostgresRepository(pool)

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
	smsMetrics := metrics.NewSMSMetrics(registry)

	// NES-24: notification outbox wiring.
	outboxRepo := notifyadapter.NewOutboxRepository(pool)
	inAppSender := notifyadapter.NewInAppSender(logger)

	// NES-138: SMS sender wiring — NoopSMSSender when NOTIFY_SMS_ENABLED is
	// false (the default; zero AWS dependency), else an
	// AWSEndUserMessagingSender instrumented with smsMetrics (see
	// notifybootstrap.NewSMSSender's own doc). ctx bounds AWS config
	// loading (LoadDefaultConfig may reach the EC2/ECS instance metadata
	// service to resolve credentials when no static ones are configured),
	// mirroring photoStoreCtx's identical bounded-startup-call reasoning
	// below.
	smsCtx, cancelSMSCtx := context.WithTimeout(context.Background(), smsSenderInitTimeout)
	smsSender, err := notifybootstrap.NewSMSSender(smsCtx, cfg.SMS, smsMetrics, logger)
	cancelSMSCtx()
	if err != nil {
		return fmt.Errorf("create sms sender: %w", err)
	}
	switch smsSender.(type) {
	case *notifyadapter.NoopSMSSender:
		logger.Info("sms sender configured", "provider", "noop")
	default:
		logger.Info("sms sender configured", "provider", "aws_end_user_messaging", "region", cfg.SMS.Region)
	}

	// NES-139: member SMS contact details (phone_e164/sms_opted_in_at) and
	// per-event-type channel preferences. contactDirectory is shared by
	// the dispatcher's own SMS sender (below, to resolve a member's phone
	// at delivery time) and by routingEnqueuer (to resolve a member's
	// current opt-in state at enqueue time) — see those two consumers'
	// own docs for why each needs its own, separately-timed check.
	contactDirectory := notifyadapter.NewPostgresContactDirectory(pool)
	preferenceRepo := notifyadapter.NewPostgresPreferenceRepository(pool)

	// NES-139: wires the SMS channel into the dispatcher at last —
	// AWSEndUserMessagingSender/NoopSMSSender (NES-138) is the raw
	// send-a-body-to-a-number port; SMSNotificationSender adds the
	// member-to-phone-number resolution a full domain.Sender needs (see
	// that type's own doc).
	smsNotificationSender := notifyadapter.NewSMSNotificationSender(smsSender, contactDirectory)

	dispatcher, err := notifyapp.NewDispatcher(
		outboxRepo,
		[]domain.Sender{inAppSender, smsNotificationSender},
		logger,
		tickRecorder,
		smsMetrics,
		dispatchBatchSize,
		dispatchPollInterval,
	)
	if err != nil {
		return fmt.Errorf("create dispatcher: %w", err)
	}

	// NES-139: routingEnqueuer decorates outboxRepo with per-member,
	// per-event-type channel resolution and household quiet-hours
	// deferral (see that type's own doc) — every scheduler/service below
	// that raises a member-addressed, preference-routable notification is
	// wired against THIS, not outboxRepo directly. householdRepo already
	// satisfies routing's narrow household-quiet-hours read port
	// structurally (see householdReader's own doc), so no separate
	// adapter is needed here.
	//
	// The auth context's own notification producers (webauthnService,
	// loginMFAHandlers, further below) stay wired directly to outboxRepo,
	// unchanged: they are outside NES-139's scope (security/login
	// notifications, not preference-routable event types), and
	// routingEnqueuer would be a safe no-op passthrough for them anyway
	// (their notifications carry no EventType).
	routingEnqueuer := notifyapp.NewRoutingEnqueuer(outboxRepo, preferenceRepo, contactDirectory, householdRepo, logger)

	// NES-31: task scheduler wiring.
	recurringTaskRepo := tasksadapter.NewRecurringTaskRepository(pool)
	taskInstanceRepo := tasksadapter.NewTaskInstanceRepository(pool)
	taskGenerator, err := tasksapp.NewGenerator(recurringTaskRepo, taskInstanceRepo, logger, taskGenerationHorizon)
	if err != nil {
		return fmt.Errorf("create task generator: %w", err)
	}
	// NES-121: chore trade wiring. choreTradeRepo is shared by the
	// scheduler's own internal TradeService (its trade-expiry sweep step,
	// constructed inside NewScheduler) and by the web-facing TradeService
	// constructed below for the propose/accept/decline/cancel HTTP handlers
	// (NES-122) — two TradeService instances over the same repository, one
	// per consumer, mirroring how taskInstanceRepo is shared without a
	// shared service.
	choreTradeRepo := tasksadapter.NewTradeRepository(pool)
	// outboxRepo satisfies notifydomain.Enqueuer (it embeds Enqueue); passing it
	// here lets the scheduler emit due-soon and overdue reminders via the same
	// outbox the dispatcher already consumes (NES-34).
	taskScheduler, err := tasksapp.NewScheduler(taskGenerator, taskInstanceRepo, choreTradeRepo, routingEnqueuer, logger, tickRecorder, taskSchedulerPollInterval)
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
		trackedItemRepo, predictor, ingredientRepo, shoppingListRepo, routingEnqueuer, logger,
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
		subscriptionRepo, routingEnqueuer, logger,
		tickRecorder, renewalSchedulerPollInterval, renewalSchedulerTickTimeout,
	)
	if err != nil {
		return fmt.Errorf("create renewal scheduler: %w", err)
	}

	// NES-120: the ProofPhotoChecker adapter wraps media's own
	// TaskInstancePhotoRepository (NES-119) so TaskService.CompleteInstance
	// can gate completion on a recurring task's photo policy, and so the
	// /tasks row builder can show the capture/review UI, without tasks
	// depending on media's adapter/app layers — see
	// tasksdomain.ProofPhotoChecker's doc. Constructed here, ahead of the
	// rest of NES-119's media wiring further below, purely because
	// TaskService needs it now; both consumers below share this single
	// repository instance.
	proofPhotoRepo := mediaadapter.NewTaskInstancePhotoRepository(pool, mediabootstrap.StorageBackend(cfg.Media.Backend))
	proofPhotoChecker := tasksadapter.NewProofPhotoChecker(proofPhotoRepo)

	// NES-32: task UI wiring — TaskService + HTTP handlers for the tasks list
	// and the three mutation actions (complete, skip, claim).
	taskService, err := tasksapp.NewTaskService(recurringTaskRepo, taskInstanceRepo, proofPhotoChecker)
	if err != nil {
		return fmt.Errorf("create task service: %w", err)
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(taskService, recurringTaskRepo, taskInstanceRepo, householdRepo, sm, logger, proofPhotoChecker)

	// NES-122: chore trade UI wiring — TradeService (web-facing instance, see
	// choreTradeRepo's comment above) + HTTP handlers for the propose-trade
	// picker, the four mutation actions, and the parent-only trade history
	// page. taskWebHandlers satisfies the handlers' taskGroupsBuilder
	// dependency via its BuildGroups method, so an accept can refresh an
	// already-open /tasks page's #task-groups fragment.
	tradeService, err := tasksapp.NewTradeService(choreTradeRepo, routingEnqueuer, logger)
	if err != nil {
		return fmt.Errorf("create trade service: %w", err)
	}
	tradeWebHandlers := tasksadapter.NewTradeWebHandlers(
		tradeService, choreTradeRepo, taskInstanceRepo, recurringTaskRepo, householdRepo, taskWebHandlers, sm, logger,
	)

	// NES-37: gamification UI wiring — scoreboard, streaks, and reward redemption.
	// NES-126: rewardAdminService adds the parent-only catalogue admin
	// (create/edit/archive) over the same rewardRepo.
	// NES-127: redemptionService adds parent fulfillment/denial and member
	// self-cancel, and rewardService gains outboxRepo/householdRepo so a
	// redemption can notify the household's parents.
	pointLedgerRepo := tasksadapter.NewPointLedgerPostgresRepository(pool)
	rewardRepo := tasksadapter.NewRewardPostgresRepository(pool)
	rewardService := tasksapp.NewRewardService(rewardRepo, householdRepo, routingEnqueuer, logger)
	rewardAdminService := tasksapp.NewRewardAdminService(rewardRepo, logger)
	redemptionService, err := tasksapp.NewRedemptionService(rewardRepo, routingEnqueuer, logger)
	if err != nil {
		return fmt.Errorf("create redemption service: %w", err)
	}
	gamificationWebHandlers := tasksadapter.NewGamificationWebHandlers(
		pointLedgerRepo,
		rewardRepo,
		rewardService,
		rewardAdminService,
		redemptionService,
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

	// NES-134: TOTP MFA enrollment. The TOTP secret is protected at rest by
	// the SAME tokenCipher instance that protects calendar OAuth tokens
	// above (one ENCRYPTION_KEY-derived cipher, multiple consumers) rather
	// than a second cipher. credRepo (constructed earlier for password
	// login) also satisfies the owner-reauth password lookup MFAService
	// needs — no separate credential store. householdRepo also satisfies
	// the member-lookup port ResetMemberMFA uses to resolve the acting
	// owner's role/household independently of any caller claim.
	mfaRepo := authadapter.NewMFARepository(pool)
	mfaService, err := authapp.NewMFAService(mfaRepo, tokenCipher, totp.NewProvider(), credRepo, householdRepo, logger)
	if err != nil {
		return fmt.Errorf("create mfa service: %w", err)
	}
	mfaWebHandlers := authadapter.NewMFAWebHandlers(mfaService, householdRepo, sm, logger)

	// NES-136: WebAuthn passkey registration, and (NES-137) passkey login
	// and step-up. wa (the third-party Relying Party instance) is
	// constructed ONLY when Server.PublicBaseURL is configured: WebAuthn
	// requires a fixed, stable origin (the Relying Party ID), which a
	// per-request-derived origin (this codebase's fallback when
	// PublicBaseURL is unset, e.g. for kiosk deep links) cannot provide —
	// the RP ID must be pinned once, at startup, for the entire lifetime of
	// every credential ever registered against it (see docs/webauthn.md:
	// changing it later orphans every existing passkey). A deployment with
	// no PublicBaseURL simply does not offer passkey registration OR
	// USERNAMELESS login at all — webauthnHandlers/webauthnService/
	// loginPasskeyHandlers all stay nil, registerSettingsPage never wires
	// the settings-page routes or renders its section
	// (ShowWebAuthnSection), LoginForm never shows the passkey button
	// (ShowPasskeyLogin), and the /login/passkey/... routes are never
	// registered at all (loginPasskeyHandlers == nil, home.go). The
	// /login/mfa/passkey/... step-up routes are DIFFERENT: they stay
	// registered whenever loginMFAHandlers itself exists (always, in
	// production) regardless of whether WebAuthn is wired — PasskeyBegin/
	// PasskeyFinish defensively 404 on a nil webauthn service instead
	// (see their own doc, login_mfa.go).
	//
	// Constructed here — BEFORE authHandlers/loginMFAHandlers below — since
	// NES-137 makes both depend on webauthnService (possibly nil) too:
	// authHandlers needs to know whether to show the passkey login button
	// at all, and loginMFAHandlers needs the service itself to offer
	// passkey step-up.
	var (
		webauthnHandlers     *authadapter.WebAuthnWebHandlers
		webauthnService      *authapp.WebAuthnService
		loginPasskeyHandlers *authadapter.LoginPasskeyHandlers
	)
	if cfg.Server.PublicBaseURL != "" {
		rpID, err := webauthnRPID(cfg.Server.PublicBaseURL)
		if err != nil {
			return fmt.Errorf("derive webauthn relying party id: %w", err)
		}
		wa, err := webauthn.New(&webauthn.Config{
			RPID:          rpID,
			RPDisplayName: "Nestova",
			RPOrigins:     []string{cfg.Server.PublicBaseURL},
			// Required (not merely preferred) user verification: a
			// registered passkey must always be gated by the device's own
			// biometric/PIN prompt, not just its mere physical presence.
			// This SAME config value is also what a login/step-up
			// assertion's UV requirement defaults from (go-webauthn's
			// beginLogin reads webauthn.Config.AuthenticatorSelection.
			// UserVerification when no LoginOption overrides it — verified
			// via go doc against the installed go-webauthn version), so
			// NES-137 needs no separate, second UV configuration for login.
			AuthenticatorSelection: protocol.AuthenticatorSelection{
				UserVerification: protocol.VerificationRequired,
			},
			Timeouts: webauthn.TimeoutsConfig{
				Registration: webauthn.TimeoutConfig{
					Enforce: true,
					Timeout: webauthnRegistrationTimeout,
				},
				Login: webauthn.TimeoutConfig{
					Enforce: true,
					Timeout: webauthnLoginTimeout,
				},
			},
		})
		if err != nil {
			return fmt.Errorf("create webauthn relying party: %w", err)
		}
		webauthnUserHandles, err := authapp.NewWebAuthnUserHandleDeriverFromSecret([]byte(cfg.Session.Secret), webauthnUserHandleSignerPurpose)
		if err != nil {
			return fmt.Errorf("create webauthn user handle deriver: %w", err)
		}
		webauthnRepo := authadapter.NewWebAuthnCredentialRepository(pool)
		// outboxRepo (constructed above for the NES-24 notification
		// outbox) satisfies WebAuthnService's notify.Enqueuer dependency: a
		// suspicious sign-count-decrease notification (NES-137) rides the
		// same outbox every other Nestova notification does.
		webauthnService, err = authapp.NewWebAuthnService(webauthnRepo, wa, webauthnUserHandles, outboxRepo, logger)
		if err != nil {
			return fmt.Errorf("create webauthn service: %w", err)
		}
		webauthnHandlers = authadapter.NewWebAuthnWebHandlers(webauthnService, sm, logger)
		loginPasskeyHandlers = authadapter.NewLoginPasskeyHandlers(sm, webauthnService, logger)
	}

	// NES-135: login MFA enforcement. rememberDeviceSigner is keyed the
	// same way deepLinkSigner below is (a purpose-scoped derivation from
	// cfg.Session.Secret) so its key stays cryptographically independent of
	// every other consumer despite tracing back to the same root secret.
	// authHandlers is constructed here — rather than alongside credRepo/
	// authn/householdRepo above — because Login now depends on mfaService
	// and rememberDeviceSigner, both of which must exist first. outboxRepo
	// (constructed above for the NES-24 notification outbox) satisfies
	// LoginMFAHandlers' notify.Enqueuer dependency: a lockout notification
	// rides the same outbox every other Nestova notification does.
	rememberDeviceSigner, err := authapp.NewRememberDeviceSignerFromSecret([]byte(cfg.Session.Secret), rememberDeviceSignerPurpose)
	if err != nil {
		return fmt.Errorf("create remember-device signer: %w", err)
	}
	authHandlers := authadapter.NewHandlers(sm, authn, mfaService, rememberDeviceSigner, webauthnService, logger)
	loginMFAHandlers := authadapter.NewLoginMFAHandlers(sm, mfaService, rememberDeviceSigner, webauthnService, outboxRepo, cfg.Session.Secure, logger)

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

	// NES-7/NES-132: media (rotating photo album) — storage, services, and
	// the /photos UI. photoStoreResolver/mediaWriteBackend are selected
	// ONCE, app-wide, from cfg.Media.Backend (see MediaConfig.Backend's doc
	// for why the WRITE target is deliberately all-or-nothing within one
	// running deployment); the resolver itself may hold MORE than one
	// backend's store (the local store is always constructed — see
	// mediabootstrap.NewPhotoStoreResolver's doc) so reads keep working for any
	// historical rows a backend switch would otherwise strand. The S3
	// backend's HeadBucket reachability check is bounded by
	// photoStoreInitTimeout so an unreachable endpoint fails the boot
	// promptly rather than hanging it.
	photoStoreCtx, cancelPhotoStoreCtx := context.WithTimeout(context.Background(), photoStoreInitTimeout)
	photoStoreResolver, mediaWriteBackend, err := mediabootstrap.NewPhotoStoreResolver(photoStoreCtx, cfg.Media)
	cancelPhotoStoreCtx()
	if err != nil {
		return fmt.Errorf("create photo store: %w", err)
	}
	albumRepo := mediaadapter.NewAlbumRepository(pool)
	photoRepo := mediaadapter.NewPhotoRepository(pool, mediaWriteBackend)
	albumPhotoRepo := mediaadapter.NewAlbumPhotoRepository(pool)
	photoService, err := mediaapp.NewPhotoService(photoStoreResolver, mediaWriteBackend, mediaadapter.NewExifReader(), photoRepo)
	if err != nil {
		return fmt.Errorf("create photo service: %w", err)
	}
	albumService, err := mediaapp.NewAlbumService(albumRepo, photoRepo, albumPhotoRepo)
	if err != nil {
		return fmt.Errorf("create album service: %w", err)
	}
	mediaWebHandlers := mediaadapter.NewWebHandlers(albumService, photoService, householdRepo, sm, logger, cfg.Media.MaxUploadBytes)

	// NES-119: chore-proof (before/after) photo upload — a structurally
	// separate table and storage class (domain.PhotoClassChoreProof) from
	// the album path above, reusing the same PhotoStoreResolver/ExifReader.
	// proofPhotoRepo is the same TaskInstancePhotoRepository instance the
	// NES-120 ProofPhotoChecker above already wraps — constructed once and
	// shared, not duplicated.
	choreProofPhotoService, err := mediaapp.NewChoreProofPhotoService(
		photoStoreResolver, mediaWriteBackend, mediaadapter.NewExifReader(), proofPhotoRepo,
		cfg.Media.MaxUploadBytes, cfg.Media.ChoreProofFreshnessWindow,
	)
	if err != nil {
		return fmt.Errorf("create chore proof photo service: %w", err)
	}
	choreProofWebHandlers := mediaadapter.NewChoreProofWebHandlers(choreProofPhotoService, sm, logger, cfg.Media.MaxUploadBytes)

	// NES-129: QR deep-link signing. The signing key is derived from
	// cfg.Session.Secret (its own doc comment already reserves it for "future
	// signing needs") via a purpose-scoped HMAC derivation rather than reused
	// raw, so it stays cryptographically independent of every other secret.Secret
	// consumer (deeplinkapp.NewSignerFromSecret's doc explains why).
	deepLinkSigner, err := deeplinkapp.NewSignerFromSecret([]byte(cfg.Session.Secret), deepLinkSignerPurpose)
	if err != nil {
		return fmt.Errorf("create deep link signer: %w", err)
	}
	deepLinkWebHandlers := deeplinkadapter.NewWebHandlers(
		deepLinkSigner, taskService, recurringTaskRepo, taskInstanceRepo,
		rewardService, rewardRepo, sm, logger, nil,
	)

	// NES-128: kiosk device auth + the touch-first kiosk shell. The kiosk
	// service authenticates the wall-mounted display's bearer-token cookie
	// (a DEVICE identity, never a member session); the web handlers build
	// read-only views directly from each bounded context's application
	// service, reusing exactly the same services already wired above.
	kioskDeviceRepo := kioskadapter.NewKioskDeviceRepository(pool)
	kioskActivationCodeRepo := kioskadapter.NewActivationCodeRepository(pool)
	kioskService, err := kioskapp.NewKioskService(kioskDeviceRepo, kioskActivationCodeRepo, nil)
	if err != nil {
		return fmt.Errorf("create kiosk service: %w", err)
	}
	settingsWebHandlers := kioskadapter.NewSettingsWebHandlers(kioskService, sm, logger)

	// NES-139: SMS notification settings — phone entry/opt-in, per-event-type
	// preferences, and (owner-only) household quiet hours. householdRepo
	// satisfies quietHoursStore structurally (GetHousehold + SetQuietHours),
	// so no separate adapter is needed here, mirroring routingEnqueuer's own
	// reuse of it above.
	notifySettingsService := notifyapp.NewSettingsService(contactDirectory, preferenceRepo, householdRepo)
	notifyWebHandlers := notifyadapter.NewNotifyWebHandlers(notifySettingsService, sm, logger)
	kioskWebHandlers := kioskadapter.NewKioskWebHandlers(
		kioskService, taskInstanceRepo, recurringTaskRepo, unifiedCalendarService,
		plannerService, recipeRepo, shoppingListService, ingredientRepo,
		albumService, photoService, householdRepo, rewardRepo, sm, logger,
		cfg.Session.Secure, deepLinkSigner, cfg.Server.PublicBaseURL, nil,
	)

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
			// NES-128: loads the kiosk device identity (if the request carries a
			// valid, active kiosk cookie) alongside the member identity above;
			// neither middleware rejects a request on its own — RequireMember and
			// kioskadapter.RequireKioskOrMember enforce identity per route group.
			kioskadapter.AuthenticateDevice(kioskService, logger),
		},
		Routes: func(mux *http.ServeMux) {
			registerWebRoutes(mux, logger, sm, authHandlers, loginMFAHandlers, loginPasskeyHandlers, onboardingHandlers, householdRepo, taskWebHandlers, tradeWebHandlers, gamificationWebHandlers, groceryWebHandlers, mealsWebHandlers, calendarWebHandlers)
			registerCalendarSubscriptionPages(mux, logger, sm, householdRepo, calendarViewHandlers, subscriptionWebHandlers)
			registerMediaPages(mux, logger, sm, householdRepo, mediaWebHandlers)
			registerChoreProofPhotoRoutes(mux, sm, choreProofWebHandlers)
			registerSettingsPage(mux, logger, sm, householdRepo, settingsWebHandlers, mfaWebHandlers, mfaService, webauthnHandlers, webauthnService, notifyWebHandlers)
			registerKioskPages(mux, kioskWebHandlers)
			registerDeepLinkPages(mux, sm, deepLinkWebHandlers)
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

// webauthnRPID derives the WebAuthn Relying Party ID from publicBaseURL:
// the effective domain only — no scheme, no port (see
// webauthn.Config.RPID's own doc: "should generally be the origin without a
// scheme and port"). publicBaseURL has already been validated at config
// load (config.go's own validate(), called before runServer ever runs) to
// be an origin-only http(s) URL with a non-empty host, so a failure here
// would mean that validation itself has a gap — this is a defensive check,
// not an expected runtime failure mode.
func webauthnRPID(publicBaseURL string) (string, error) {
	u, err := url.Parse(publicBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse PUBLIC_BASE_URL: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("PUBLIC_BASE_URL %q has no host", publicBaseURL)
	}
	return u.Hostname(), nil
}
