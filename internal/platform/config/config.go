// Package config loads and validates runtime configuration from the
// environment. Configuration is read exclusively from environment variables so
// secrets are never committed; an optional .env file is honored in development
// only. Load fails fast, reporting every problem at once rather than one at a
// time.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Deployment environments. Env is constrained to these values.
const (
	EnvDev  = "dev"
	EnvTest = "test"
	EnvProd = "prod"
)

const (
	// minSecretLen is the minimum acceptable SESSION_SECRET length in bytes.
	minSecretLen = 32

	// devDSN is the default database DSN, matching the docker-compose service
	// (NES-16) so the dev happy-path boots without any environment setup.
	devDSN = "postgres://nestova:nestova@localhost:5432/nestova?sslmode=disable"

	// devSessionSecret is a known, insecure default used only in development.
	// It satisfies the length check in dev but is rejected in prod, forcing a
	// real secret in production.
	devSessionSecret = "dev-only-insecure-session-secret-change-me"

	// devEncryptionKey is a known, insecure 32-byte (64-hex) default used only in
	// development so the app starts without configuration. It is rejected in prod,
	// forcing a real key (generated with `openssl rand -hex 32`) there.
	devEncryptionKey = "00000000000000000000000000000000000000000000000000000000deadbeef"

	// defaultTrustedProxies is the TRUSTED_PROXIES default: loopback only, since a
	// same-host reverse proxy (Caddy / tailscale serve) connects over loopback.
	// It applies only when TRUSTED_PROXIES is unset; an explicit empty value
	// trusts no proxy and ignores forwarded headers entirely.
	defaultTrustedProxies = "127.0.0.0/8,::1/128"

	// SESSION_COOKIE_SECURE accepted values (NES-51). auto is the default and
	// preserves the legacy "Secure only in prod" behavior; true/false force it.
	cookieSecureAuto  = "auto"
	cookieSecureTrue  = "true"
	cookieSecureFalse = "false"

	// defaultServerRequestTimeout is SERVER_REQUEST_TIMEOUT's default: generous
	// enough that a phone on weak Wi-Fi can finish uploading a photo near
	// MediaConfig.MaxUploadBytes without the connection or the per-request
	// context deadline cutting it off mid-upload.
	defaultServerRequestTimeout = 120 * time.Second
	// minServerRequestTimeout is the floor Load enforces for
	// SERVER_REQUEST_TIMEOUT: below this, ordinary requests (not just large
	// uploads) would risk spurious timeouts, and httpserver's derived
	// per-request context deadline (RequestTimeout minus its margin) could be
	// squeezed uncomfortably thin.
	minServerRequestTimeout = 15 * time.Second
)

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
	TLS     TLSConfig
	HSTS    HSTSConfig
	// Env is the deployment environment: one of EnvDev, EnvTest, EnvProd.
	Env string
}

// HSTSConfig configures the HTTP Strict-Transport-Security response header
// (NES-52). HSTS is opt-in because it is sticky and hard to undo, so it must
// only be enabled on a stable HTTPS hostname. It is emitted only over HTTPS.
type HSTSConfig struct {
	// Enabled turns the Strict-Transport-Security header on.
	Enabled bool
	// MaxAge is the max-age directive (emitted as whole seconds). It is only
	// meaningful when MaxAgeSet is true; see EffectiveMaxAge.
	MaxAge time.Duration
	// MaxAgeSet records whether HSTS_MAX_AGE was explicitly provided. It lets an
	// explicit max-age=0 (which clears a previously-sent HSTS policy in browsers)
	// be distinguished from "unset" (apply DefaultHSTSMaxAge). A negative explicit
	// value is invalid.
	MaxAgeSet bool
	// IncludeSubdomains adds the includeSubDomains directive.
	IncludeSubdomains bool
	// Preload adds the preload directive (requires includeSubDomains + max-age >= 1y).
	Preload bool
}

// EffectiveMaxAge returns the max-age the header should carry: the explicit value
// when HSTS_MAX_AGE was set (including 0 to clear HSTS), otherwise the built-in
// default.
func (h HSTSConfig) EffectiveMaxAge() time.Duration {
	if !h.MaxAgeSet {
		return DefaultHSTSMaxAge
	}
	return h.MaxAge
}

// DefaultHSTSMaxAge is the HSTS max-age applied when HSTS is enabled without an
// explicit HSTS_MAX_AGE (~180 days) — long enough to be effective, short of the
// 1-year preload-list minimum so it stays low-risk.
const DefaultHSTSMaxAge = 180 * 24 * time.Hour

// TLSConfig configures optional app-terminated TLS (NES-54). When both files are
// set, the server listens with TLS (ListenAndServeTLS); otherwise it serves plain
// HTTP and relies on a reverse proxy for TLS. Both-or-neither is enforced at Load.
type TLSConfig struct {
	// CertFile is the path to the PEM server certificate (chain).
	CertFile string
	// KeyFile is the path to the PEM private key for CertFile.
	KeyFile string
}

// Enabled reports whether app-terminated TLS is configured (both files present).
func (t TLSConfig) Enabled() bool {
	return t.CertFile != "" && t.KeyFile != ""
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	// Addr is the TCP address the HTTP server listens on, e.g. ":8080".
	Addr string
	// TrustedProxies is the raw, comma-separated CIDR list (from TRUSTED_PROXIES)
	// of reverse-proxy source networks whose X-Forwarded-* headers are trusted.
	// It is validated at Load; call TrustedProxyPrefixes for the parsed form.
	// Forwarded headers are honored only when the immediate peer falls inside one
	// of these networks, so an external client cannot spoof a secure context. An
	// empty value trusts no proxy.
	TrustedProxies string
	// RequestTimeout bounds how long the server allows a single request to take
	// end to end — it backs both the connection-level ReadTimeout/WriteTimeout
	// (httpserver.New) and (minus a small margin) the per-request context
	// deadline applied to every handler. It must be generous enough for the
	// slowest legitimate request the server handles: uploading a photo near
	// MediaConfig.MaxUploadBytes over a weak connection, not just a fast LAN
	// request, since ReadTimeout bounds the whole request body read and
	// WriteTimeout's deadline is set once when headers finish reading and does
	// not reset while the body is still being read (net/http.conn.readRequest).
	RequestTimeout time.Duration
}

// TrustedProxyPrefixes parses TrustedProxies into netip prefixes for the
// ForwardedHeaders middleware. TrustedProxies is validated during Load, so any
// malformed entry would already have failed startup; this drops such entries
// defensively and never returns an error.
func (s ServerConfig) TrustedProxyPrefixes() []netip.Prefix {
	prefixes, _ := parseTrustedProxies(s.TrustedProxies)
	return prefixes
}

// DBProvider selects the database backend. Both are Postgres; the provider only
// changes connectivity (TLS and pooler-safe statement handling), never the
// schema or queries.
type DBProvider string

const (
	// DBProviderPostgres is the default self-hosted Postgres backend (NES-16).
	DBProviderPostgres DBProvider = "postgres"
	// DBProviderSupabase targets Supabase: Postgres reached through the Supavisor
	// connection pooler, requiring TLS and pooler-safe statement handling.
	DBProviderSupabase DBProvider = "supabase"
)

// DBPoolMode declares which Supabase pooler endpoint the DSN targets. It is
// consulted only when Provider is DBProviderSupabase.
type DBPoolMode string

const (
	// DBPoolModeSession targets the session pooler or a direct connection, where
	// a backend connection is not multiplexed mid-transaction, so pgx's default
	// cached server-side prepared statements are safe.
	DBPoolModeSession DBPoolMode = "session"
	// DBPoolModeTransaction targets the transaction pooler (Supavisor port 6543),
	// which multiplexes a backend connection per transaction and is incompatible
	// with cached server-side prepared statements.
	DBPoolModeTransaction DBPoolMode = "transaction"
)

// supabaseDefaultMaxConns is the modest pool cap applied when DB_MAX_CONNS is
// unset and Provider is Supabase, because the pooler is a shared resource and
// pgx's NumCPU-based default can be too aggressive behind it.
const supabaseDefaultMaxConns int32 = 10

// DBConfig configures Postgres connectivity (consumed by NES-16/17).
type DBConfig struct {
	// DSN is the Postgres connection string.
	DSN string
	// MaxConns caps the connection pool. Zero means "let the pool decide".
	MaxConns int32
	// ConnTimeout bounds the initial connectivity check at startup.
	ConnTimeout time.Duration
	// Provider selects the database backend (default DBProviderPostgres). The
	// Postgres path is byte-for-byte identical to before this field existed.
	Provider DBProvider
	// PoolMode declares the Supabase pooler endpoint the DSN targets; consulted
	// only when Provider is DBProviderSupabase (default DBPoolModeSession).
	PoolMode DBPoolMode
	// SSLRootCert is an optional path to a CA bundle. When set, the connection
	// upgrades to sslmode=verify-full and verifies the server certificate against
	// this CA (pgx reads sslrootcert from the DSN and builds the TLS config).
	SSLRootCert string
	// MigrateDSN is an optional override (MIGRATE_DATABASE_URL) for the connection
	// the migration tool uses; empty means "use DSN". Operators point this at the
	// Supabase direct/session connection (port 5432) so DDL and goose version
	// bookkeeping run on a session connection while the app server uses the
	// transaction pooler (port 6543).
	MigrateDSN string
}

// SessionConfig configures sessions (consumed by NES-23).
type SessionConfig struct {
	// Secret is a high-entropy key reserved for cryptographic signing; it must
	// be at least minSecretLen bytes. The session store is server-side
	// (Postgres via scs), so the session cookie itself carries only an opaque
	// random token and is not signed with Secret. Secret is validated/available
	// for future signing needs (e.g. signed tokens).
	Secret string
	// Secure marks the session cookie Secure (HTTPS-only). It is resolved from
	// SESSION_COOKIE_SECURE: auto (the default) keeps the legacy behavior of
	// Secure only when APP_ENV=prod, while true/false force it — letting a
	// TLS-terminated deployment emit Secure cookies regardless of APP_ENV.
	Secure bool
	// Lifetime is the maximum session duration.
	Lifetime time.Duration
}

// OAuthConfig holds Google OAuth credentials (placeholders until the calendar
// phase). Required only in prod.
type OAuthConfig struct {
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string
}

// CryptoConfig holds the at-rest encryption key used to protect stored secrets
// (e.g. OAuth tokens, NES-67). EncryptionKey is a 64-character hex string
// (32 bytes), produced by `openssl rand -hex 32`. Required in prod; when set in
// any environment it must be valid. Key decodes and validates it.
type CryptoConfig struct {
	EncryptionKey string
}

// Key decodes the configured hex EncryptionKey into its 32 raw bytes, returning
// an error when it is unset or not exactly 32 bytes of hex.
func (c CryptoConfig) Key() ([]byte, error) {
	if c.EncryptionKey == "" {
		return nil, errors.New("ENCRYPTION_KEY is not set")
	}
	key, err := hex.DecodeString(c.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("ENCRYPTION_KEY must be hex: %w", err)
	}
	if len(key) != encryptionKeyLen {
		return nil, fmt.Errorf("ENCRYPTION_KEY must decode to %d bytes, got %d", encryptionKeyLen, len(key))
	}
	return key, nil
}

// encryptionKeyLen is the required decoded key length (AES-256).
const encryptionKeyLen = 32

// RecipesConfig configures the external recipe provider behind the "discover
// more" finder (NES-59). External lookups are off unless ExternalEnabled is set,
// in which case APIKey and BaseURL are required (a swappable provider, so no
// secret lives in code). When disabled, the finder serves recipe-box results only.
type RecipesConfig struct {
	ExternalEnabled bool
	APIKey          string
	BaseURL         string
}

// MediaConfig configures photo storage for the rotating album (NES-72): where
// the local PhotoStore writes photo bytes and the per-upload size cap. The root
// has a safe default in every environment (no secret), so it is never required.
type MediaConfig struct {
	// Root is the directory the local PhotoStore writes photo bytes under.
	Root string
	// MaxUploadBytes caps a single photo upload (bytes).
	MaxUploadBytes int64
}

// devMediaRoot is the default photo-storage directory when MEDIA_ROOT is unset.
const devMediaRoot = "./.localdata/media"

// defaultMaxUploadBytes is the default per-upload size cap (25 MiB) —
// sized for bulk album uploads of modern phone camera originals (NES-123).
const defaultMaxUploadBytes int64 = 25 << 20

// Load reads configuration from the environment and validates it. In
// development it first loads an optional .env file (real environment variables
// always take precedence). It returns an aggregated error enumerating every
// missing or invalid value so the operator can fix them all in one pass.
func Load() (Config, error) {
	env := getenv("APP_ENV", EnvDev)

	// Collect problems instead of returning early so a single typo does not
	// mask other issues; parsing and validation below append to this slice.
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Optional dev-only .env. godotenv.Load does not overwrite variables that
	// are already set, so the real environment always wins. Skipped outside
	// dev and when no .env file is present. A malformed .env is surfaced (not
	// swallowed) to keep startup fail-fast.
	if env == EnvDev {
		if _, err := os.Stat(".env"); err == nil {
			if err := godotenv.Load(); err != nil {
				collect(fmt.Errorf(".env: %w", err))
			}
		} else if !os.IsNotExist(err) {
			// A missing .env is expected; surface permission/I/O errors rather
			// than silently skipping a file the operator clearly intended.
			collect(fmt.Errorf("stat .env: %w", err))
		}
	}

	// Re-read APP_ENV after .env is loaded: if APP_ENV is defined only in .env,
	// the initial read above returned the default, and every other field would
	// pick up .env values while Env did not. Re-reading keeps Env consistent
	// with the rest of the configuration.
	env = getenv("APP_ENV", EnvDev)

	maxConns, err := getint32("DB_MAX_CONNS", 0)
	collect(err)
	connTimeout, err := getduration("DB_CONNECT_TIMEOUT", 5*time.Second)
	collect(err)
	serverRequestTimeout, err := getduration("SERVER_REQUEST_TIMEOUT", defaultServerRequestTimeout)
	collect(err)
	sessionLifetime, err := getduration("SESSION_LIFETIME", 12*time.Hour)
	collect(err)
	recipesExternalEnabled, err := getbool("RECIPES_EXTERNAL_ENABLED", false)
	collect(err)
	maxUploadBytes, err := getint64("MEDIA_MAX_UPLOAD_BYTES", defaultMaxUploadBytes)
	collect(err)
	hstsEnabled, err := getbool("HSTS_ENABLED", false)
	collect(err)
	// Track whether HSTS_MAX_AGE was set explicitly so an explicit 0 (clear HSTS)
	// is distinct from "unset" (apply the built-in default). getduration returns 0
	// for both, so LookupEnv is what disambiguates.
	hstsMaxAge, err := getduration("HSTS_MAX_AGE", 0)
	collect(err)
	hstsMaxAgeRaw, hstsMaxAgeOK := os.LookupEnv("HSTS_MAX_AGE")
	hstsMaxAgeSet := hstsMaxAgeOK && hstsMaxAgeRaw != ""
	hstsIncludeSubdomains, err := getbool("HSTS_INCLUDE_SUBDOMAINS", false)
	collect(err)
	hstsPreload, err := getbool("HSTS_PRELOAD", false)
	collect(err)

	// PORT is conventionally a bare port number; tolerate a leading colon
	// (e.g. PORT=":8080") so it does not produce a malformed "::8080" address.
	port := strings.TrimPrefix(getenv("PORT", "8080"), ":")

	// TRUSTED_PROXIES (NES-50): CIDRs whose X-Forwarded-* headers are trusted.
	// LookupEnv distinguishes "unset" (apply the loopback default) from an
	// explicit empty value (trust nothing, ignoring forwarded headers). The raw
	// value is stored on ServerConfig; validate it now so a malformed CIDR fails
	// fast at startup.
	trustedProxies, ok := os.LookupEnv("TRUSTED_PROXIES")
	if !ok {
		trustedProxies = defaultTrustedProxies
	}
	if _, err := parseTrustedProxies(trustedProxies); err != nil {
		collect(err)
	}

	// The dev DSN convenience default applies only in dev. test and prod
	// require an explicit DATABASE_URL: an empty value is left empty so
	// validation rejects it, rather than silently connecting a non-dev run to
	// the dev database.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" && env == EnvDev {
		dsn = devDSN
	}

	// Database backend selection (NES-46). Both default to the existing
	// self-hosted Postgres behavior; values are normalized so casing/whitespace
	// in the environment does not defeat the enum validation below.
	dbProvider := DBProvider(strings.ToLower(strings.TrimSpace(getenv("DB_PROVIDER", string(DBProviderPostgres)))))
	dbPoolMode := DBPoolMode(strings.ToLower(strings.TrimSpace(getenv("DB_POOL_MODE", string(DBPoolModeSession)))))
	dbSSLRootCert := strings.TrimSpace(os.Getenv("DB_SSL_ROOT_CERT"))
	// Optional separate DSN for the migration tool (NES-47). Empty means "reuse
	// DATABASE_URL"; operators set it to the Supabase direct/session connection.
	dbMigrateDSN := strings.TrimSpace(os.Getenv("MIGRATE_DATABASE_URL"))

	// Supabase connects through a shared pooler, so default to a modest pool cap
	// when the operator has not set one. Postgres keeps deferring to pgx (zero).
	if dbProvider == DBProviderSupabase && maxConns == 0 {
		maxConns = supabaseDefaultMaxConns
	}

	// Session cookie Secure policy (NES-51), decoupled from APP_ENV so a
	// TLS-terminated deployment can emit Secure cookies even when not prod.
	sessionSecure, err := resolveCookieSecure(getenv("SESSION_COOKIE_SECURE", cookieSecureAuto), env)
	collect(err)

	cfg := Config{
		Env:    env,
		Server: ServerConfig{Addr: ":" + port, TrustedProxies: trustedProxies, RequestTimeout: serverRequestTimeout},
		DB: DBConfig{
			DSN:         dsn,
			MaxConns:    maxConns,
			ConnTimeout: connTimeout,
			Provider:    dbProvider,
			PoolMode:    dbPoolMode,
			SSLRootCert: dbSSLRootCert,
			MigrateDSN:  dbMigrateDSN,
		},
		Session: SessionConfig{
			Secret:   getenv("SESSION_SECRET", devSessionSecret),
			Secure:   sessionSecure,
			Lifetime: sessionLifetime,
		},
		OAuth: OAuthConfig{
			GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			GoogleRedirectURL:  os.Getenv("GOOGLE_REDIRECT_URL"),
		},
		Crypto: CryptoConfig{
			EncryptionKey: strings.TrimSpace(getenv("ENCRYPTION_KEY", devEncryptionKey)),
		},
		Recipes: RecipesConfig{
			ExternalEnabled: recipesExternalEnabled,
			// Trim at read: BaseURL is consumed directly by the HTTP client, so a
			// stray-whitespace value must not survive past validation.
			APIKey:  strings.TrimSpace(os.Getenv("RECIPES_API_KEY")),
			BaseURL: strings.TrimSpace(os.Getenv("RECIPES_API_BASE_URL")),
		},
		Media: MediaConfig{
			Root:           strings.TrimSpace(getenv("MEDIA_ROOT", devMediaRoot)),
			MaxUploadBytes: maxUploadBytes,
		},
		TLS: TLSConfig{
			CertFile: strings.TrimSpace(os.Getenv("TLS_CERT_FILE")),
			KeyFile:  strings.TrimSpace(os.Getenv("TLS_KEY_FILE")),
		},
		HSTS: HSTSConfig{
			Enabled:           hstsEnabled,
			MaxAge:            hstsMaxAge,
			MaxAgeSet:         hstsMaxAgeSet,
			IncludeSubdomains: hstsIncludeSubdomains,
			Preload:           hstsPreload,
		},
	}

	errs = append(errs, cfg.validate()...)
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n%w", errors.Join(errs...))
	}
	return cfg, nil
}

// ServerAddrFromEnv returns the HTTP listen address derived from PORT using the
// same parsing as Load (a leading colon is tolerated), without requiring a full,
// validated configuration. It backs first-run setup mode, which must serve the
// HTTP wizard before a complete configuration (notably DATABASE_URL) exists and
// so cannot call Load.
func ServerAddrFromEnv() string {
	port := strings.TrimPrefix(getenv("PORT", "8080"), ":")
	return ":" + port
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

	if strings.TrimSpace(c.DB.DSN) == "" {
		errs = append(errs, errors.New("DATABASE_URL must not be empty"))
	}
	if c.DB.MaxConns < 0 {
		errs = append(errs, fmt.Errorf("DB_MAX_CONNS must be >= 0, got %d", c.DB.MaxConns))
	}
	switch c.DB.Provider {
	case DBProviderPostgres, DBProviderSupabase:
	default:
		errs = append(errs, fmt.Errorf("DB_PROVIDER must be one of %s|%s, got %q",
			DBProviderPostgres, DBProviderSupabase, c.DB.Provider))
	}
	switch c.DB.PoolMode {
	case DBPoolModeSession, DBPoolModeTransaction:
	default:
		errs = append(errs, fmt.Errorf("DB_POOL_MODE must be one of %s|%s, got %q",
			DBPoolModeSession, DBPoolModeTransaction, c.DB.PoolMode))
	}
	if c.DB.ConnTimeout <= 0 {
		errs = append(errs, fmt.Errorf("DB_CONNECT_TIMEOUT must be positive, got %v", c.DB.ConnTimeout))
	}
	if c.Server.RequestTimeout < minServerRequestTimeout {
		errs = append(errs, fmt.Errorf("SERVER_REQUEST_TIMEOUT must be at least %v, got %v",
			minServerRequestTimeout, c.Server.RequestTimeout))
	}
	if len(c.Session.Secret) < minSecretLen {
		errs = append(errs, fmt.Errorf("SESSION_SECRET must be at least %d bytes, got %d",
			minSecretLen, len(c.Session.Secret)))
	}
	if c.Session.Lifetime <= 0 {
		errs = append(errs, fmt.Errorf("SESSION_LIFETIME must be positive, got %v", c.Session.Lifetime))
	}
	if strings.TrimSpace(c.Media.Root) == "" {
		errs = append(errs, errors.New("MEDIA_ROOT must not be empty"))
	}
	// App-terminated TLS (NES-54): both files or neither, so a half-configured
	// listener can never start.
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		errs = append(errs, errors.New("TLS_CERT_FILE and TLS_KEY_FILE must be set together (or both unset)"))
	}
	// HSTS (NES-52): a negative max-age is invalid; zero is allowed and means
	// "use the built-in default". (max-age=0 to expire HSTS is achieved by simply
	// disabling it via HSTS_ENABLED.)
	if c.HSTS.Enabled && c.HSTS.MaxAgeSet && c.HSTS.MaxAge < 0 {
		errs = append(errs, fmt.Errorf("HSTS_MAX_AGE must not be negative, got %v", c.HSTS.MaxAge))
	}
	// The preload directive is a public commitment with strict requirements: the
	// HSTS preload list requires includeSubDomains and max-age >= 1 year. Reject a
	// preload config that browsers' preload submission would, so it is caught at
	// startup rather than after a hard-to-undo deployment.
	if c.HSTS.Enabled && c.HSTS.Preload {
		const hstsPreloadMinMaxAge = 365 * 24 * time.Hour
		if c.HSTS.EffectiveMaxAge() < hstsPreloadMinMaxAge {
			errs = append(errs, fmt.Errorf("HSTS_PRELOAD requires HSTS_MAX_AGE >= 1 year, got %v", c.HSTS.EffectiveMaxAge()))
		}
		if !c.HSTS.IncludeSubdomains {
			errs = append(errs, errors.New("HSTS_PRELOAD requires HSTS_INCLUDE_SUBDOMAINS=true"))
		}
	}
	if c.Media.MaxUploadBytes <= 0 {
		errs = append(errs, fmt.Errorf("MEDIA_MAX_UPLOAD_BYTES must be positive, got %d", c.Media.MaxUploadBytes))
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
	// a malformed key fails fast at startup rather than at the first encrypt.
	if c.Crypto.EncryptionKey != "" {
		if _, err := c.Crypto.Key(); err != nil {
			errs = append(errs, err)
		}
	}

	if c.Env == EnvProd {
		if c.Session.Secret == devSessionSecret {
			errs = append(errs, errors.New("SESSION_SECRET must be set to a non-default value in prod"))
		}
		if strings.TrimSpace(c.OAuth.GoogleClientID) == "" {
			errs = append(errs, errors.New("GOOGLE_CLIENT_ID is required in prod"))
		}
		if strings.TrimSpace(c.OAuth.GoogleClientSecret) == "" {
			errs = append(errs, errors.New("GOOGLE_CLIENT_SECRET is required in prod"))
		}
		if strings.TrimSpace(c.OAuth.GoogleRedirectURL) == "" {
			errs = append(errs, errors.New("GOOGLE_REDIRECT_URL is required in prod"))
		}
		// Validate the key unconditionally in prod so a whitespace-only or
		// otherwise malformed key fails fast at startup, not at the first encrypt.
		if _, err := c.Crypto.Key(); err != nil {
			errs = append(errs, err)
		}
		if c.Crypto.EncryptionKey == devEncryptionKey {
			errs = append(errs, errors.New("ENCRYPTION_KEY must be set to a non-default value in prod"))
		}
	}

	return errs
}

// resolveCookieSecure maps SESSION_COOKIE_SECURE to the session cookie's Secure
// flag. auto (the default) preserves the legacy behavior — Secure only when
// APP_ENV=prod — while true/false force it independently of APP_ENV. An unknown
// value falls back to the prod-derived default and is reported.
func resolveCookieSecure(mode, env string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case cookieSecureAuto, "":
		return env == EnvProd, nil
	case cookieSecureTrue:
		return true, nil
	case cookieSecureFalse:
		return false, nil
	default:
		return env == EnvProd, fmt.Errorf("SESSION_COOKIE_SECURE must be one of %s|%s|%s, got %q",
			cookieSecureAuto, cookieSecureTrue, cookieSecureFalse, mode)
	}
}

// parseTrustedProxies parses a comma-separated CIDR list (e.g.
// "127.0.0.0/8,::1/128") into masked netip prefixes. Empty or whitespace-only
// entries are skipped, so an empty value yields no prefixes (trust nothing).
// Every malformed entry is reported so the operator can fix them in one pass.
func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	fields := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(fields))
	var errs []error
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXIES entry %q is not a valid CIDR: %w", f, err))
			continue
		}
		// Mask so host bits are zeroed: it normalizes the value for containment
		// checks and makes the parsed result stable regardless of how it was written.
		prefixes = append(prefixes, p.Masked())
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return prefixes, nil
}

// getenv returns the value of the environment variable named key, or fallback
// when the variable is unset or empty.
func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// getint32 parses an int32 environment variable, returning fallback when unset
// or empty and an error when present but not a valid integer.
func getint32(key string, fallback int32) (int32, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		// Return the fallback alongside the error so the resulting Config still
		// holds a sane value and downstream validation does not double-report
		// this field.
		return fallback, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return int32(n), nil
}

// getint64 parses an int64 environment variable, returning fallback when unset
// or empty and an error when present but not a valid integer.
func getint64(key string, fallback int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// Return the fallback alongside the error so the resulting Config still
		// holds a sane value and downstream validation does not double-report
		// this field.
		return fallback, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

// getduration parses a duration environment variable (e.g. "30s", "5m"),
// returning fallback when unset or empty and an error when present but invalid.
func getduration(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Return the fallback alongside the error so the resulting Config still
		// holds a sane value and downstream validation does not double-report
		// this field.
		return fallback, fmt.Errorf("%s must be a duration (e.g. 30s, 5m): %w", key, err)
	}
	return d, nil
}

// getbool parses a boolean environment variable (strconv.ParseBool: 1/t/true,
// 0/f/false, case-insensitive), returning fallback when unset or empty and an
// error when present but invalid.
func getbool(key string, fallback bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		// Return the fallback alongside the error so the resulting Config still
		// holds a sane value and downstream validation does not double-report
		// this field.
		return fallback, fmt.Errorf("%s must be a boolean (true/false): %w", key, err)
	}
	return b, nil
}
