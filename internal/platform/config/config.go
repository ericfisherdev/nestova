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
	"math"
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
	SMS     SMSConfig
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
	// PublicBaseURL is the externally-reachable origin (scheme + host, no
	// trailing slash, e.g. "https://nestova.tailxxxx.ts.net") the kiosk's QR
	// deep links are built against (NES-129), so a phone scanning a code from
	// off the kiosk's own LAN segment still reaches a working URL. Empty (the
	// default) means "derive it from the incoming request" instead: the
	// resolved scheme (RequestScheme, honoring X-Forwarded-Proto from a
	// trusted proxy) plus the request's Host header. That default already
	// produces the correct externally-reachable URL for the documented
	// Tailscale deployment (README "HTTPS deployment") — `tailscale serve`
	// terminates TLS and forwards to Nestova with Host already set to the
	// tailnet MagicDNS name — so PublicBaseURL only needs to be set to
	// override that (e.g. a reverse proxy that changes the Host header, or a
	// deployment reachable at a name the kiosk's own request Host does not
	// carry).
	//
	// WebAuthn passkey registration (NES-136) additionally REQUIRES this to
	// be set: unlike a deep link, a WebAuthn Relying Party ID must be a
	// single fixed value pinned once at server startup — a per-request
	// derived origin cannot work, since the RP ID is baked into every
	// credential an authenticator ever registers against it. A deployment
	// with PublicBaseURL empty simply does not offer passkey registration at
	// all (cmd/server/main.go only constructs the WebAuthn Relying Party when
	// this is set). Changing PublicBaseURL's HOST after passkeys have been
	// registered orphans every one of them — the browser will never present
	// a stored passkey to a Relying Party ID it was not registered under; see
	// docs/webauthn.md.
	PublicBaseURL string
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

// S3Config configures the optional S3-compatible PhotoStore backend
// (NES-132). It is only consulted when MediaConfig.Backend is
// MediaStorageBackendS3; every field is otherwise ignored (and unvalidated)
// so a local-backend deployment never has to set any of it.
type S3Config struct {
	// Endpoint is the S3-compatible API's base URL. Blank targets real AWS
	// S3 (the SDK's regional default endpoint); a custom endpoint — MinIO or
	// Garage on the LAN, or Cloudflare R2 — is a first-class target for this
	// app's local-appliance deployment story, not an afterthought, so this is
	// deliberately supported from day one rather than added later.
	Endpoint string
	// Region is passed to every S3 request. AWS S3 requires a real region;
	// most S3-compatible servers (MinIO, Garage) accept any non-empty value
	// (e.g. "us-east-1") since they do not partition by region.
	Region string
	// Bucket is the single bucket every photo (both classes — see
	// PhotoClass) is stored under, namespaced by the same
	// households/<household>/<class-prefix>/... key layout LocalPhotoStore
	// uses (see classKeyPrefix).
	Bucket string
	// AccessKeyID / SecretAccessKey are optional static credentials. When
	// BOTH are blank, the AWS SDK's default credential chain (environment,
	// shared config/credentials file, EC2/ECS instance role, etc.) supplies
	// credentials instead — so a deployment that already provisions
	// credentials another way (e.g. an IAM role) never needs to duplicate
	// them here. Config.validate enforces both-or-neither, mirroring
	// TLSConfig's CertFile/KeyFile pairing.
	AccessKeyID     string
	SecretAccessKey string
	// UsePathStyle forces path-style bucket addressing
	// (https://endpoint/bucket/key instead of https://bucket.endpoint/key).
	// MinIO and most self-hosted S3-compatible servers require this; real
	// AWS S3 does not.
	UsePathStyle bool
	// PresignTTL is how long a presigned GET URL (PhotoStore.URL) stays
	// valid when the caller passes a non-positive ttl — S3PhotoStore's own
	// applied default. Kept short: a presigned URL is a bearer credential
	// for as long as it is valid, so the default favors a tight window over
	// convenience.
	PresignTTL time.Duration
}

// MediaConfig configures photo storage for the rotating album (NES-72): where
// the local PhotoStore writes photo bytes and the per-upload size cap. The root
// has a safe default in every environment (no secret), so it is never required.
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

// SMSConfig configures the optional SMS notification channel (NES-138), an
// AWS End User Messaging-backed domain.SMSSender. It is only consulted
// when Enabled is true; every other field is otherwise ignored (and
// unvalidated), mirroring S3Config's own
// enabled-gates-required-fields pattern — a deployment with SMS disabled
// (the default) never has to set any of it, and runs with the Noop sender
// and zero AWS dependency (NES-138 AC).
type SMSConfig struct {
	// Enabled turns the SMS channel on.
	Enabled bool
	// OriginationIdentity is the verified toll-free number (or its ARN, or
	// a pool id/ARN) SendTextMessage sends from. Required when Enabled.
	OriginationIdentity string
	// Region is passed to every SMS API request. Required when Enabled.
	Region string
	// AccessKeyID / SecretAccessKey are optional static credentials. When
	// BOTH are blank, the AWS SDK's default credential chain (environment,
	// shared config/credentials file, EC2/ECS instance role, etc.)
	// supplies credentials instead — mirrors S3Config's identical field
	// pair and its own doc.
	AccessKeyID     string
	SecretAccessKey string
	// RetryMaxAttempts caps the AWS SDK's own built-in retryer. SMS is
	// billed per attempt handed to the carrier, so this is kept tight by
	// default — see AWSEndUserMessagingSMSParams.RetryMaxAttempts's own
	// doc for why no backoff is hand-rolled on top of it.
	RetryMaxAttempts int
}

// devMediaRoot is the default photo-storage directory when MEDIA_ROOT is unset.
const devMediaRoot = "./.localdata/media"

// defaultMaxUploadBytes is the default per-upload size cap (25 MiB) —
// sized for bulk album uploads of modern phone camera originals (NES-123).
const defaultMaxUploadBytes int64 = 25 << 20

// defaultChoreProofFreshnessWindow is MEDIA_CHORE_PROOF_FRESHNESS_WINDOW's
// default (NES-119): generous enough to cover the walk from finishing a
// chore to opening the upload form on a shared household device, tight
// enough to reject a photo pulled from an earlier day's camera roll.
const defaultChoreProofFreshnessWindow = 60 * time.Minute

// defaultS3PresignTTL is S3_PRESIGN_TTL's default (NES-132): long enough for
// a slow phone connection to actually fetch the photo after the redirect,
// short enough that a leaked/cached URL stops working soon after.
const defaultS3PresignTTL = 15 * time.Minute

// defaultChoreProofRetentionDays is MEDIA_CHORE_PROOF_RETENTION_DAYS' default
// (NES-132): 0 means "keep forever" — retention is opt-in, not a surprise
// data-loss default.
const defaultChoreProofRetentionDays = 0

// defaultSMSRetryMaxAttempts is SMS_RETRY_MAX_ATTEMPTS's default (NES-138):
// tight deliberately — SMS is billed per attempt handed to the carrier, so
// a generous retry budget (the AWS SDK's own STANDARD retry mode default
// is much higher) is a real spend risk against a persistently failing
// destination, not just a latency one.
const defaultSMSRetryMaxAttempts = 3

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
	choreProofFreshnessWindow, err := getduration("MEDIA_CHORE_PROOF_FRESHNESS_WINDOW", defaultChoreProofFreshnessWindow)
	collect(err)

	// Resolve the media storage backend BEFORE any S3-specific parsing —
	// every S3_* setting below is parsed/validated ONLY when this
	// deployment actually selected the s3 backend (NES-132 review): a
	// local-backend deployment (the default) must never fail startup on a
	// malformed or partial S3_* value it will never use, e.g. a stray
	// S3_PRESIGN_TTL left over from a copy-pasted .env. Normalized so
	// casing/whitespace in the environment does not defeat the enum
	// validation below, mirroring DB.Provider's identical pattern.
	mediaBackend := MediaStorageBackend(strings.ToLower(strings.TrimSpace(getenv("MEDIA_STORAGE_BACKEND", string(MediaStorageBackendLocal)))))

	// S3_PRESIGN_TTL/S3_USE_PATH_STYLE are only PARSED (and their parse
	// errors only collected) when mediaBackend is s3; otherwise the plain
	// defaults apply unconditionally, without even attempting to read the
	// raw environment value, so a local deployment's Config always ends up
	// with the same S3Config it would if S3_* were unset entirely.
	s3PresignTTL := defaultS3PresignTTL
	s3UsePathStyle := false
	if mediaBackend == MediaStorageBackendS3 {
		s3PresignTTL, err = getduration("S3_PRESIGN_TTL", defaultS3PresignTTL)
		collect(err)
		s3UsePathStyle, err = getbool("S3_USE_PATH_STYLE", false)
		collect(err)
	}

	choreProofRetentionDays, err := getint64("MEDIA_CHORE_PROOF_RETENTION_DAYS", defaultChoreProofRetentionDays)
	collect(err)
	choreProofRetention, err := choreProofRetentionDuration(choreProofRetentionDays)
	collect(err)

	// NOTIFY_SMS_ENABLED gates every SMS_* setting below (NES-138),
	// mirroring MEDIA_STORAGE_BACKEND's own S3-gating pattern above: a
	// deployment with SMS disabled (the default) must never fail startup
	// on a stray or malformed SMS_* value it will never use, and must run
	// with zero AWS dependency (NES-138 AC) — the NoopSMSSender the
	// composition root wires when smsEnabled is false imports nothing
	// from aws-sdk-go-v2 at all.
	smsEnabled, err := getbool("NOTIFY_SMS_ENABLED", false)
	collect(err)
	smsRetryMaxAttempts := defaultSMSRetryMaxAttempts
	if smsEnabled {
		var smsRetryMaxAttempts32 int32
		smsRetryMaxAttempts32, err = getint32("SMS_RETRY_MAX_ATTEMPTS", defaultSMSRetryMaxAttempts)
		collect(err)
		smsRetryMaxAttempts = int(smsRetryMaxAttempts32)
	}
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

	// PUBLIC_BASE_URL (NES-129): the externally-reachable origin QR deep links
	// are built against. TrimRight (not TrimSuffix, which removes only ONE
	// occurrence) strips EVERY trailing slash, so an operator's accidental
	// "https://host//" does not survive as a single leftover slash and
	// double up with the leading slash on every concatenated deep-link path
	// (".../go/..." would otherwise become ".../.../go/...").
	publicBaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")), "/")

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
		Env: env,
		Server: ServerConfig{
			Addr: ":" + port, TrustedProxies: trustedProxies, RequestTimeout: serverRequestTimeout,
			PublicBaseURL: publicBaseURL,
		},
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
			Root:                      strings.TrimSpace(getenv("MEDIA_ROOT", devMediaRoot)),
			MaxUploadBytes:            maxUploadBytes,
			ChoreProofFreshnessWindow: choreProofFreshnessWindow,
			Backend:                   mediaBackend,
			S3: S3Config{
				Endpoint:        strings.TrimSpace(os.Getenv("S3_ENDPOINT")),
				Region:          strings.TrimSpace(os.Getenv("S3_REGION")),
				Bucket:          strings.TrimSpace(os.Getenv("S3_BUCKET")),
				AccessKeyID:     strings.TrimSpace(os.Getenv("S3_ACCESS_KEY_ID")),
				SecretAccessKey: strings.TrimSpace(os.Getenv("S3_SECRET_ACCESS_KEY")),
				UsePathStyle:    s3UsePathStyle,
				PresignTTL:      s3PresignTTL,
			},
			ChoreProofRetention: choreProofRetention,
		},
		SMS: SMSConfig{
			Enabled:             smsEnabled,
			OriginationIdentity: strings.TrimSpace(os.Getenv("SMS_ORIGINATION_IDENTITY")),
			Region:              strings.TrimSpace(os.Getenv("SMS_REGION")),
			AccessKeyID:         strings.TrimSpace(os.Getenv("SMS_ACCESS_KEY_ID")),
			SecretAccessKey:     strings.TrimSpace(os.Getenv("SMS_SECRET_ACCESS_KEY")),
			RetryMaxAttempts:    smsRetryMaxAttempts,
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
	if c.Media.ChoreProofFreshnessWindow <= 0 {
		errs = append(errs, fmt.Errorf("MEDIA_CHORE_PROOF_FRESHNESS_WINDOW must be positive, got %v", c.Media.ChoreProofFreshnessWindow))
	}
	// No c.Media.ChoreProofRetention range check here: choreProofRetentionDuration
	// (called during Load, before cfg is ever built) already rejects a
	// negative or overflowing MEDIA_CHORE_PROOF_RETENTION_DAYS with a
	// checked conversion, so a Config that reaches validate() always
	// carries an already-valid ChoreProofRetention — see that function's doc.
	switch c.Media.Backend {
	case MediaStorageBackendLocal, MediaStorageBackendS3:
	default:
		errs = append(errs, fmt.Errorf("MEDIA_STORAGE_BACKEND must be one of %s|%s, got %q",
			MediaStorageBackendLocal, MediaStorageBackendS3, c.Media.Backend))
	}
	// EVERY S3_* setting below — required fields, PresignTTL's positivity,
	// and the credential both-or-neither pairing — is validated ONLY when
	// the S3 backend is actually selected (NES-132 review, reversing an
	// earlier "check credentials unconditionally" design): a local-backend
	// deployment (the default) must never fail startup on a stray or
	// partial S3_* value it will never use, mirroring
	// RECIPES_EXTERNAL_ENABLED's identical enabled-gates-required-fields
	// pattern below.
	if c.Media.Backend == MediaStorageBackendS3 {
		if c.Media.S3.Bucket == "" {
			errs = append(errs, errors.New("S3_BUCKET is required when MEDIA_STORAGE_BACKEND=s3"))
		}
		if c.Media.S3.Region == "" {
			errs = append(errs, errors.New("S3_REGION is required when MEDIA_STORAGE_BACKEND=s3"))
		}
		if c.Media.S3.PresignTTL <= 0 {
			errs = append(errs, fmt.Errorf("S3_PRESIGN_TTL must be positive, got %v", c.Media.S3.PresignTTL))
		}
		// Static S3 credentials are both-or-neither (mirroring
		// TLS_CERT_FILE/TLS_KEY_FILE above): a lone access key or secret is
		// always a misconfiguration, never a valid partial state.
		if (c.Media.S3.AccessKeyID == "") != (c.Media.S3.SecretAccessKey == "") {
			errs = append(errs, errors.New("S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY must be set together (or both left unset to use the default AWS credential chain)"))
		}
	}

	// EVERY SMS_* setting below is validated ONLY when NOTIFY_SMS_ENABLED is
	// true (NES-138), mirroring S3's identical enabled-gates-required-fields
	// pattern immediately above: a deployment with SMS disabled (the
	// default) must never fail startup on a stray or partial SMS_* value it
	// will never use.
	if c.SMS.Enabled {
		if c.SMS.OriginationIdentity == "" {
			errs = append(errs, errors.New("SMS_ORIGINATION_IDENTITY is required when NOTIFY_SMS_ENABLED=true"))
		}
		if c.SMS.Region == "" {
			errs = append(errs, errors.New("SMS_REGION is required when NOTIFY_SMS_ENABLED=true"))
		}
		if c.SMS.RetryMaxAttempts <= 0 {
			errs = append(errs, fmt.Errorf("SMS_RETRY_MAX_ATTEMPTS must be positive, got %d", c.SMS.RetryMaxAttempts))
		}
		// Static SMS credentials are both-or-neither, mirroring S3's
		// identical pairing check.
		if (c.SMS.AccessKeyID == "") != (c.SMS.SecretAccessKey == "") {
			errs = append(errs, errors.New("SMS_ACCESS_KEY_ID and SMS_SECRET_ACCESS_KEY must be set together (or both left unset to use the default AWS credential chain)"))
		}
	}

	// PUBLIC_BASE_URL is optional (empty means "derive from the request"), but
	// when set it must be an origin ONLY — scheme + host(:port), nothing else,
	// not even a bare trailing slash — so it can be concatenated directly
	// with a deep-link path with no further validation, normalization, or
	// path-joining logic at request time. Userinfo, a path (including "/"),
	// a query, or a fragment would all either be silently discarded
	// (surprising) or double up with the deep-link path (broken); rejecting
	// them at startup catches an operator's copy-paste mistake (e.g. pasting
	// a full activation link) before it ever reaches a kiosk-rendered QR
	// code.
	//
	// The bare-"/" exemption a prior version of this check made was removed
	// (NES-136): PublicBaseURL now also feeds WebAuthn's RPOrigins directly
	// (cmd/server/main.go), which go-webauthn matches against the browser's
	// reported origin via EXACT string comparison — an origin never has a
	// trailing slash, so "https://example.org/" would silently never match
	// "https://example.org" and break every passkey registration, despite
	// looking like a harmless, valid-enough origin.
	if c.Server.PublicBaseURL != "" {
		u, err := url.Parse(c.Server.PublicBaseURL)
		switch {
		case err != nil, u.Scheme != "http" && u.Scheme != "https", u.Host == "":
			errs = append(errs, fmt.Errorf("PUBLIC_BASE_URL must be an absolute http(s) URL, got %q", c.Server.PublicBaseURL))
		case u.User != nil, u.Path != "", u.RawQuery != "", u.Fragment != "":
			errs = append(errs, fmt.Errorf("PUBLIC_BASE_URL must be an origin only (scheme + host, no user/path/query/fragment), got %q", c.Server.PublicBaseURL))
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
