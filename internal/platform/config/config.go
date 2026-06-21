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
	// Env is the deployment environment: one of EnvDev, EnvTest, EnvProd.
	Env string
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	// Addr is the TCP address the HTTP server listens on, e.g. ":8080".
	Addr string
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
}

// SessionConfig configures sessions (consumed by NES-23).
type SessionConfig struct {
	// Secret is a high-entropy key reserved for cryptographic signing; it must
	// be at least minSecretLen bytes. The session store is server-side
	// (Postgres via scs), so the session cookie itself carries only an opaque
	// random token and is not signed with Secret. Secret is validated/available
	// for future signing needs (e.g. signed tokens).
	Secret string
	// Secure marks cookies Secure (HTTPS-only); derived as Env == EnvProd.
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

// defaultMaxUploadBytes is the default per-upload size cap (10 MiB).
const defaultMaxUploadBytes int64 = 10 << 20

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
	sessionLifetime, err := getduration("SESSION_LIFETIME", 12*time.Hour)
	collect(err)
	recipesExternalEnabled, err := getbool("RECIPES_EXTERNAL_ENABLED", false)
	collect(err)
	maxUploadBytes, err := getint64("MEDIA_MAX_UPLOAD_BYTES", defaultMaxUploadBytes)
	collect(err)

	// PORT is conventionally a bare port number; tolerate a leading colon
	// (e.g. PORT=":8080") so it does not produce a malformed "::8080" address.
	port := strings.TrimPrefix(getenv("PORT", "8080"), ":")

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

	// Supabase connects through a shared pooler, so default to a modest pool cap
	// when the operator has not set one. Postgres keeps deferring to pgx (zero).
	if dbProvider == DBProviderSupabase && maxConns == 0 {
		maxConns = supabaseDefaultMaxConns
	}

	cfg := Config{
		Env:    env,
		Server: ServerConfig{Addr: ":" + port},
		DB: DBConfig{
			DSN:         dsn,
			MaxConns:    maxConns,
			ConnTimeout: connTimeout,
			Provider:    dbProvider,
			PoolMode:    dbPoolMode,
			SSLRootCert: dbSSLRootCert,
		},
		Session: SessionConfig{
			Secret:   getenv("SESSION_SECRET", devSessionSecret),
			Secure:   env == EnvProd,
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
