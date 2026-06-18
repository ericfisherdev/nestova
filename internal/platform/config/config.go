// Package config loads and validates runtime configuration from the
// environment. Configuration is read exclusively from environment variables so
// secrets are never committed; an optional .env file is honored in development
// only. Load fails fast, reporting every problem at once rather than one at a
// time.
package config

import (
	"errors"
	"fmt"
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
)

// Config holds the validated runtime configuration, grouped by concern so each
// consumer depends only on the section it needs.
type Config struct {
	Server  ServerConfig
	DB      DBConfig
	Session SessionConfig
	OAuth   OAuthConfig
	// Env is the deployment environment: one of EnvDev, EnvTest, EnvProd.
	Env string
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	// Addr is the TCP address the HTTP server listens on, e.g. ":8080".
	Addr string
}

// DBConfig configures Postgres connectivity (consumed by NES-16/17).
type DBConfig struct {
	// DSN is the Postgres connection string.
	DSN string
	// MaxConns caps the connection pool. Zero means "let the pool decide".
	MaxConns int32
	// ConnTimeout bounds the initial connectivity check at startup.
	ConnTimeout time.Duration
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

	cfg := Config{
		Env:    env,
		Server: ServerConfig{Addr: ":" + port},
		DB: DBConfig{
			DSN:         dsn,
			MaxConns:    maxConns,
			ConnTimeout: connTimeout,
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
