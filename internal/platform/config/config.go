// Package config loads and validates runtime configuration from the
// environment. Configuration is read exclusively from environment variables so
// secrets are never committed; an optional .env file is honored in development
// only. Load fails fast, reporting every problem at once rather than one at a
// time.
//
// Most sub-configs (server, database, session, crypto, HSTS, TLS, cache, SMS,
// email) are generic across every application built on this platform and now
// live in github.com/ericfisherdev/nestcore/config; this package re-exports
// them (type aliases + thin wrappers) so every existing consumer keeps
// compiling unchanged, and composes them alongside the config that stays
// specific to Nestova (OAuth, Recipes, Media, Peer).
package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	ncconfig "github.com/ericfisherdev/nestcore/config"
)

// Deployment environments. Env is constrained to these values.
const (
	EnvDev  = ncconfig.EnvDev
	EnvTest = ncconfig.EnvTest
	EnvProd = ncconfig.EnvProd
)

// DefaultHSTSMaxAge is the HSTS max-age applied when HSTS is enabled without an
// explicit HSTS_MAX_AGE.
const DefaultHSTSMaxAge = ncconfig.DefaultHSTSMaxAge

// ServerConfig configures the HTTP listener. Generic across every application
// on this platform; it now lives in nestcore/config and is aliased here so
// every existing consumer keeps referencing config.ServerConfig unchanged.
type ServerConfig = ncconfig.ServerConfig

// HSTSConfig configures the HTTP Strict-Transport-Security response header.
// Generic across every application on this platform; it now lives in
// nestcore/config and is aliased here so every existing consumer keeps
// referencing config.HSTSConfig unchanged.
type HSTSConfig = ncconfig.HSTSConfig

// TLSConfig configures optional app-terminated TLS. Generic across every
// application on this platform; it now lives in nestcore/config and is
// aliased here so every existing consumer keeps referencing config.TLSConfig
// unchanged.
type TLSConfig = ncconfig.TLSConfig

// DBConfig configures Postgres connectivity. Generic across every application
// on this platform; it now lives in nestcore/config and is aliased here so
// every existing consumer keeps referencing config.DBConfig unchanged.
type DBConfig = ncconfig.DBConfig

// SessionConfig configures sessions. Generic across every application on this
// platform; it now lives in nestcore/config and is aliased here so every
// existing consumer keeps referencing config.SessionConfig unchanged.
type SessionConfig = ncconfig.SessionConfig

// CryptoConfig holds an at-rest encryption key used to protect stored
// secrets. Generic across every application on this platform; it now lives in
// nestcore/config and is aliased here so every existing consumer keeps
// referencing config.CryptoConfig unchanged.
type CryptoConfig = ncconfig.CryptoConfig

// CacheConfig configures the on-disk cache. Generic across every application
// on this platform; it now lives in nestcore/config and is aliased here so
// every existing consumer keeps referencing config.CacheConfig unchanged.
type CacheConfig = ncconfig.CacheConfig

// S3Config configures an optional S3-compatible object storage backend.
// Generic across every application on this platform; it now lives in
// nestcore/config and is aliased here so every existing consumer keeps
// referencing config.S3Config unchanged.
type S3Config = ncconfig.S3Config

// SMSConfig configures the optional SMS notification channel. Generic across
// every application on this platform; it now lives in nestcore/config and is
// aliased here so every existing consumer keeps referencing config.SMSConfig
// unchanged.
type SMSConfig = ncconfig.SMSConfig

// EmailConfig configures the optional email notification channel. Generic
// across every application on this platform; it now lives in nestcore/config
// and is aliased here so every existing consumer keeps referencing
// config.EmailConfig unchanged.
type EmailConfig = ncconfig.EmailConfig

// DBProvider selects the database backend. Both are Postgres; the provider
// only changes connectivity (TLS and pooler-safe statement handling), never
// the schema or queries.
type DBProvider = ncconfig.DBProvider

// DBPoolMode declares which Supabase pooler endpoint the DSN targets. It is
// consulted only when Provider is DBProviderSupabase.
type DBPoolMode = ncconfig.DBPoolMode

// DBProvider values.
const (
	DBProviderPostgres DBProvider = ncconfig.DBProviderPostgres
	DBProviderSupabase DBProvider = ncconfig.DBProviderSupabase
)

// DBPoolMode values.
const (
	DBPoolModeSession     DBPoolMode = ncconfig.DBPoolModeSession
	DBPoolModeTransaction DBPoolMode = ncconfig.DBPoolModeTransaction
)

// ServerAddrFromEnv returns the HTTP listen address derived from PORT using the
// same parsing as Load (a leading colon is tolerated), without requiring a full,
// validated configuration. It backs first-run setup mode, which must serve the
// HTTP wizard before a complete configuration (notably DATABASE_URL) exists and
// so cannot call Load.
func ServerAddrFromEnv() string {
	return ncconfig.ServerAddrFromEnv()
}

const (
	// devDSN is the default database DSN, matching the docker-compose service
	// (NES-16) so the dev happy-path boots without any environment setup.
	// "nest" is the shared database Nestova, Nestorage, and identity live in
	// (NSTR-118); options carries search_path=nestova,public so Nestova's own
	// connections resolve into its dedicated schema without any query being
	// schema-qualified.
	devDSN = "postgres://nestova:nestova@localhost:5432/nest?sslmode=disable&options=-csearch_path%3Dnestova%2Cpublic" // NOSONAR: fake dev-only credential, matches the docker-compose service (compose.yaml) and is never reachable outside a developer's own machine

	// devMediaRoot is the default photo-storage directory when MEDIA_ROOT is unset.
	devMediaRoot = "./.localdata/media"

	// defaultMaxUploadBytes is the default per-upload size cap (25 MiB) —
	// sized for bulk album uploads of modern phone camera originals (NES-123).
	defaultMaxUploadBytes int64 = 25 << 20

	// defaultChoreProofFreshnessWindow is MEDIA_CHORE_PROOF_FRESHNESS_WINDOW's
	// default (NES-119): generous enough to cover the walk from finishing a
	// chore to opening the upload form on a shared household device, tight
	// enough to reject a photo pulled from an earlier day's camera roll.
	defaultChoreProofFreshnessWindow = 60 * time.Minute

	// defaultS3PresignTTL is S3_PRESIGN_TTL's default (NES-132): long enough for
	// a slow phone connection to actually fetch the photo after the redirect,
	// short enough that a leaked/cached URL stops working soon after.
	defaultS3PresignTTL = 15 * time.Minute

	// defaultChoreProofRetentionDays is MEDIA_CHORE_PROOF_RETENTION_DAYS' default
	// (NES-132): 0 means "keep forever" — retention is opt-in, not a surprise
	// data-loss default.
	defaultChoreProofRetentionDays = 0
)

// maxChoreProofRetentionDays bounds MEDIA_CHORE_PROOF_RETENTION_DAYS so
// choreProofRetentionDuration's days*24*time.Hour conversion can never
// silently overflow time.Duration's underlying int64 nanoseconds — a value
// above this both fails to represent a meaningful retention window and,
// left unchecked, would wrap into a nonsensical (or even negative)
// time.Duration. ~292 years is comfortably beyond any real retention
// policy this app would ever configure.
const maxChoreProofRetentionDays = math.MaxInt64 / int64(24*time.Hour)

// choreProofRetentionDuration converts days into a time.Duration with a
// checked conversion: negative and overflowing values are both rejected
// explicitly, rather than let a raw days*24*time.Hour multiplication wrap
// silently (see maxChoreProofRetentionDays' doc).
func choreProofRetentionDuration(days int64) (time.Duration, error) {
	if days < 0 {
		return 0, fmt.Errorf("MEDIA_CHORE_PROOF_RETENTION_DAYS must be >= 0, got %d", days)
	}
	if days > maxChoreProofRetentionDays {
		return 0, fmt.Errorf("MEDIA_CHORE_PROOF_RETENTION_DAYS must be <= %d, got %d", maxChoreProofRetentionDays, days)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// Config holds the validated runtime configuration, grouped by concern so each
// consumer depends only on the section it needs.
type Config struct {
	Server  ServerConfig
	DB      DBConfig
	Session SessionConfig
	OAuth   OAuthConfig
	Crypto  CryptoConfig
	Recipes RecipesConfig
	Media   MediaConfig
	SMS     SMSConfig
	Email   EmailConfig
	Cache   CacheConfig
	TLS     TLSConfig
	HSTS    HSTSConfig
	// Peer configures the sidebar's cross-app nav control (NSTR-124) — the
	// sibling Nestorage install, if any.
	Peer PeerConfig
	// Env is the deployment environment: one of EnvDev, EnvTest, EnvProd.
	Env string
}

// PeerConfig configures the sidebar's cross-app nav control (NSTR-124): the
// origin of the sibling app (Nestorage) it links to. It has no nestcore
// equivalent and stays hand-rolled here.
type PeerConfig struct {
	// NestorageURL is PEER_NESTORAGE_URL, optional. When unset, Nestorage is
	// not installed on this appliance and no cross-app nav control renders
	// at all (see components.PeerLink's own doc) — a bare "not configured"
	// state, not an error.
	NestorageURL string
}

// OAuthConfig holds Google OAuth credentials (placeholders until the calendar
// phase). Required only in prod. It has no nestcore equivalent and stays
// hand-rolled here.
type OAuthConfig struct {
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string
}

// RecipesConfig configures the external recipe provider behind the "discover
// more" finder (NES-59). External lookups are off unless ExternalEnabled is set,
// in which case APIKey and BaseURL are required (a swappable provider, so no
// secret lives in code). When disabled, the finder serves recipe-box results only.
// It has no nestcore equivalent and stays hand-rolled here.
type RecipesConfig struct {
	ExternalEnabled bool
	APIKey          string
	BaseURL         string
}

// MediaStorageBackend selects which domain.PhotoStore implementation photo
// bytes are persisted through, app-wide: the composition root (cmd/server)
// selects a single backend once at startup from this value — see the 00028
// migration's doc for why storage_backend is a per-deployment choice, not a
// per-photo one, and NES-132's ticket for why that makes local/S3
// deliberately all-or-nothing within one running deployment (switching
// backends does not retroactively move already-stored bytes; that is
// NES-133's planned migrate/verify tooling's job).
type MediaStorageBackend string

// MediaStorageBackend values. Local (LocalPhotoStore, the pre-NES-132
// default) is always available with no configuration; S3 (S3PhotoStore,
// NES-132) is opt-in and requires MediaConfig.S3 to be populated.
const (
	MediaStorageBackendLocal MediaStorageBackend = "local"
	MediaStorageBackendS3    MediaStorageBackend = "s3"
)

// MediaConfig configures photo storage for the rotating album (NES-72): where
// the local PhotoStore writes photo bytes and the per-upload size cap. The root
// has a safe default in every environment (no secret), so it is never required.
// It has no nestcore equivalent (beyond the nested S3Config) and stays
// hand-rolled here.
type MediaConfig struct {
	// Root is the directory the local PhotoStore writes photo bytes under.
	// Consulted only when Backend is MediaStorageBackendLocal.
	Root string
	// MaxUploadBytes caps a single photo upload (bytes), enforced by
	// whichever backend is active.
	MaxUploadBytes int64
	// ChoreProofFreshnessWindow bounds how far a chore-proof photo's EXIF
	// capture time may fall from the upload instant, in either direction,
	// before ChoreProofPhotoService.Upload rejects it with
	// domain.ErrPhotoStale (NES-119).
	ChoreProofFreshnessWindow time.Duration
	// Backend selects the domain.PhotoStore implementation (NES-132).
	// Defaults to MediaStorageBackendLocal.
	Backend MediaStorageBackend
	// S3 configures the S3-compatible backend; consulted only when Backend
	// is MediaStorageBackendS3.
	S3 S3Config
	// ChoreProofRetention is how old a chore-proof (before/after) photo must
	// be, by UploadedAt, before the storage reaper deletes its row and lets
	// its object age out — zero (the default) means keep forever. Album
	// photos have no such retention knob: only chore-proof photos are
	// transient documentation, not the family's photo library.
	ChoreProofRetention time.Duration
}

// Load reads configuration from the environment and validates it. In
// development it first loads an optional .env file (real environment variables
// always take precedence). It returns an aggregated error enumerating every
// missing or invalid value so the operator can fix them all in one pass.
func Load() (Config, error) {
	env := ncconfig.String("APP_ENV", EnvDev)

	// Collect problems instead of returning early so a single typo does not
	// mask other issues; parsing and validation below append to this slice.
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	collectAll := func(es []error) {
		errs = append(errs, es...)
	}

	// Optional dev-only .env. LoadDotenv does not overwrite variables that are
	// already set, so the real environment always wins. Skipped outside dev
	// and when no .env file is present. A malformed .env is surfaced (not
	// swallowed) to keep startup fail-fast.
	if env == EnvDev {
		collectAll(ncconfig.LoadDotenv())
	}

	// Re-read APP_ENV after .env is loaded: if APP_ENV is defined only in .env,
	// the initial read above returned the default, and every other field would
	// pick up .env values while Env did not. Re-reading keeps Env consistent
	// with the rest of the configuration.
	env = ncconfig.String("APP_ENV", EnvDev)

	serverCfg, serverErrs := ncconfig.LoadServer()
	collectAll(serverErrs)

	dbCfg, dbErrs := ncconfig.LoadDB()
	collectAll(dbErrs)
	// The dev DSN convenience default applies only in dev. test and prod
	// require an explicit DATABASE_URL: an empty value is left empty so
	// validation rejects it, rather than silently connecting a non-dev run to
	// the dev database.
	if dbCfg.DSN == "" && env == EnvDev {
		dbCfg.DSN = devDSN
	}

	sessionCfg, sessionErrs := ncconfig.LoadSession(env)
	collectAll(sessionErrs)

	cryptoCfg := ncconfig.LoadCrypto()

	hstsCfg, hstsErrs := ncconfig.LoadHSTS()
	collectAll(hstsErrs)

	cacheCfg := ncconfig.LoadCache()

	tlsCfg := ncconfig.LoadTLS()

	smsCfg, smsErrs := ncconfig.LoadSMS()
	collectAll(smsErrs)

	emailCfg, emailErrs := ncconfig.LoadEmail()
	collectAll(emailErrs)

	recipesExternalEnabled, err := ncconfig.Bool("RECIPES_EXTERNAL_ENABLED", false)
	collect(err)

	maxUploadBytes, err := ncconfig.Int64("MEDIA_MAX_UPLOAD_BYTES", defaultMaxUploadBytes)
	collect(err)
	choreProofFreshnessWindow, err := ncconfig.Duration("MEDIA_CHORE_PROOF_FRESHNESS_WINDOW", defaultChoreProofFreshnessWindow)
	collect(err)

	// Resolve the media storage backend BEFORE any S3-specific parsing —
	// every S3_* setting below is parsed/validated ONLY when this
	// deployment actually selected the s3 backend (NES-132 review): a
	// local-backend deployment (the default) must never fail startup on a
	// malformed or partial S3_* value it will never use, e.g. a stray
	// S3_PRESIGN_TTL left over from a copy-pasted .env. Normalized so
	// casing/whitespace in the environment does not defeat the enum
	// validation below, mirroring DB.Provider's identical pattern.
	mediaBackend := MediaStorageBackend(strings.ToLower(strings.TrimSpace(
		ncconfig.String("MEDIA_STORAGE_BACKEND", string(MediaStorageBackendLocal)))))

	// S3_ENDPOINT/REGION/BUCKET/credentials are always read (they are plain
	// strings that cannot fail to parse); S3_PRESIGN_TTL/S3_USE_PATH_STYLE are
	// only PARSED (and their parse errors only collected) when mediaBackend is
	// s3 — see nestcore's S3Config.LoadS3 doc: it always parses every field,
	// so it is only called when this deployment actually opted into S3.
	// Otherwise a local deployment's Config always ends up with the same
	// S3Config it would if S3_* were unset entirely.
	s3Cfg := S3Config{
		Endpoint:        strings.TrimSpace(os.Getenv("S3_ENDPOINT")),
		Region:          strings.TrimSpace(os.Getenv("S3_REGION")),
		Bucket:          strings.TrimSpace(os.Getenv("S3_BUCKET")),
		AccessKeyID:     strings.TrimSpace(os.Getenv("S3_ACCESS_KEY_ID")),
		SecretAccessKey: strings.TrimSpace(os.Getenv("S3_SECRET_ACCESS_KEY")),
		PresignTTL:      defaultS3PresignTTL,
	}
	if mediaBackend == MediaStorageBackendS3 {
		var s3Errs []error
		s3Cfg, s3Errs = ncconfig.LoadS3()
		collectAll(s3Errs)
	}

	choreProofRetentionDays, err := ncconfig.Int64("MEDIA_CHORE_PROOF_RETENTION_DAYS", defaultChoreProofRetentionDays)
	collect(err)
	choreProofRetention, err := choreProofRetentionDuration(choreProofRetentionDays)
	collect(err)

	// PEER_NESTORAGE_URL (NSTR-124): the sibling Nestorage install's origin,
	// consulted only when set (see PeerConfig.NestorageURL's own doc).
	// TrimRight of a trailing slash mirrors PUBLIC_BASE_URL's own trim in
	// LoadServer: left untrimmed, a trailing slash would survive into probe's
	// "{baseURL}/healthz" concatenation as "//healthz", which only resolves
	// today by accident of net/http.ServeMux's path cleaning.
	peerNestorageURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PEER_NESTORAGE_URL")), "/")

	cfg := Config{
		Env:     env,
		Server:  serverCfg,
		DB:      dbCfg,
		Session: sessionCfg,
		OAuth: OAuthConfig{
			GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			GoogleRedirectURL:  os.Getenv("GOOGLE_REDIRECT_URL"),
		},
		Crypto: cryptoCfg,
		Recipes: RecipesConfig{
			ExternalEnabled: recipesExternalEnabled,
			// Trim at read: BaseURL is consumed directly by the HTTP client, so a
			// stray-whitespace value must not survive past validation.
			APIKey:  strings.TrimSpace(os.Getenv("RECIPES_API_KEY")),
			BaseURL: strings.TrimSpace(os.Getenv("RECIPES_API_BASE_URL")),
		},
		Media: MediaConfig{
			Root:                      strings.TrimSpace(ncconfig.String("MEDIA_ROOT", devMediaRoot)),
			MaxUploadBytes:            maxUploadBytes,
			ChoreProofFreshnessWindow: choreProofFreshnessWindow,
			Backend:                   mediaBackend,
			S3:                        s3Cfg,
			ChoreProofRetention:       choreProofRetention,
		},
		SMS:   smsCfg,
		Email: emailCfg,
		Cache: cacheCfg,
		TLS:   tlsCfg,
		HSTS:  hstsCfg,
		Peer: PeerConfig{
			NestorageURL: peerNestorageURL,
		},
	}

	errs = append(errs, cfg.validate()...)
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n%w", errors.Join(errs...))
	}
	return cfg, nil
}

// validate returns every configuration problem found, so callers can surface
// them together.
func (c Config) validate() []error {
	var errs []error

	switch c.Env {
	case EnvDev, EnvTest, EnvProd:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be one of %s|%s|%s, got %q",
			EnvDev, EnvTest, EnvProd, c.Env))
	}

	errs = append(errs, c.DB.Validate()...)
	errs = append(errs, c.Server.Validate()...)
	errs = append(errs, c.Session.Validate(c.Env)...)
	errs = append(errs, c.Media.validate()...)
	errs = append(errs, c.Cache.Validate()...)
	errs = append(errs, c.TLS.Validate()...)
	errs = append(errs, c.HSTS.Validate()...)
	errs = append(errs, c.SMS.Validate()...)
	errs = append(errs, c.Email.Validate()...)

	// PEER_NESTORAGE_URL (NSTR-124): must be an absolute http(s) URL so it
	// can be used directly as a full-page navigation target and as the
	// probe's own {baseURL}/healthz base — a looser check than
	// PUBLIC_BASE_URL's (path allowed, no user/query/fragment) since this
	// value is never used as a WebAuthn origin, only navigated to and
	// concatenated with "/healthz". The path allowance is deliberate (a
	// peer reachable only behind a path-stripping reverse proxy), unlike
	// userinfo: a credential embedded here would render verbatim into the
	// sidebar anchor's href on every authenticated page, so u.User is
	// rejected the same way PUBLIC_BASE_URL's own check rejects it.
	// u may be nil when err != nil, so the second case is only ever
	// reached once the first has already ruled that out (a switch, not
	// independent ifs, so u is never dereferenced unparsed).
	if c.Peer.NestorageURL != "" {
		u, err := url.Parse(c.Peer.NestorageURL)
		switch {
		case err != nil, u.Scheme != "http" && u.Scheme != "https", u.Host == "":
			errs = append(errs, fmt.Errorf("PEER_NESTORAGE_URL must be an absolute http(s) URL, got %q", c.Peer.NestorageURL))
		case u.User != nil, u.RawQuery != "", u.Fragment != "":
			errs = append(errs, fmt.Errorf("PEER_NESTORAGE_URL must be an origin only (no user, query, or fragment), got %q", c.Peer.NestorageURL))
		}
	}

	// External recipe lookups must not be enabled without the credentials to make
	// them, in any environment (enabling them with no key is a config mistake).
	if c.Recipes.ExternalEnabled {
		// APIKey and BaseURL are trimmed at read, so an empty check suffices here.
		if c.Recipes.APIKey == "" {
			errs = append(errs, errors.New("RECIPES_API_KEY is required when RECIPES_EXTERNAL_ENABLED is true"))
		}
		if c.Recipes.BaseURL == "" {
			errs = append(errs, errors.New("RECIPES_API_BASE_URL is required when RECIPES_EXTERNAL_ENABLED is true"))
		} else if u, err := url.Parse(c.Recipes.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			// Fail fast on a malformed base URL rather than surfacing it as an opaque
			// request error at the first lookup; require an absolute http(s) URL.
			errs = append(errs, fmt.Errorf("RECIPES_API_BASE_URL must be an absolute http(s) URL, got %q", c.Recipes.BaseURL))
		}
	}

	// When an encryption key is provided, it must be valid in any environment so
	// a malformed key fails fast at startup rather than at the first encrypt;
	// CryptoConfig.Validate additionally requires (and rejects the default) it
	// in prod.
	errs = append(errs, c.Crypto.Validate(c.Env)...)

	if c.Env == EnvProd {
		if strings.TrimSpace(c.OAuth.GoogleClientID) == "" {
			errs = append(errs, errors.New("GOOGLE_CLIENT_ID is required in prod"))
		}
		if strings.TrimSpace(c.OAuth.GoogleClientSecret) == "" {
			errs = append(errs, errors.New("GOOGLE_CLIENT_SECRET is required in prod"))
		}
		if strings.TrimSpace(c.OAuth.GoogleRedirectURL) == "" {
			errs = append(errs, errors.New("GOOGLE_REDIRECT_URL is required in prod"))
		}
	}

	return errs
}

// validate returns every MediaConfig problem found, so Config.validate can
// surface them together. EVERY S3_* setting is validated ONLY when the S3
// backend is actually selected (NES-132 review): a local-backend deployment
// (the default) must never fail startup on a stray or partial S3_* value it
// will never use.
func (m MediaConfig) validate() []error {
	var errs []error

	if strings.TrimSpace(m.Root) == "" {
		errs = append(errs, errors.New("MEDIA_ROOT must not be empty"))
	}
	if m.MaxUploadBytes <= 0 {
		errs = append(errs, fmt.Errorf("MEDIA_MAX_UPLOAD_BYTES must be positive, got %d", m.MaxUploadBytes))
	}
	if m.ChoreProofFreshnessWindow <= 0 {
		errs = append(errs, fmt.Errorf("MEDIA_CHORE_PROOF_FRESHNESS_WINDOW must be positive, got %v", m.ChoreProofFreshnessWindow))
	}
	// No m.ChoreProofRetention range check here: choreProofRetentionDuration
	// (called during Load, before Config is ever built) already rejects a
	// negative or overflowing MEDIA_CHORE_PROOF_RETENTION_DAYS with a
	// checked conversion, so a MediaConfig that reaches validate() always
	// carries an already-valid ChoreProofRetention — see that function's doc.
	switch m.Backend {
	case MediaStorageBackendLocal, MediaStorageBackendS3:
	default:
		errs = append(errs, fmt.Errorf("MEDIA_STORAGE_BACKEND must be one of %s|%s, got %q",
			MediaStorageBackendLocal, MediaStorageBackendS3, m.Backend))
	}
	if m.Backend == MediaStorageBackendS3 {
		errs = append(errs, m.S3.Validate()...)
	}

	return errs
}
