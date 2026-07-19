package config_test

import (
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// allKeys is every environment variable Load reads. Each test sets all of them
// (defaulting to "") so cases are isolated from the developer's ambient
// environment, not just from each other.
var allKeys = []string{
	"PORT", "APP_ENV", "DATABASE_URL", "DB_MAX_CONNS", "DB_CONNECT_TIMEOUT",
	"DB_PROVIDER", "DB_POOL_MODE", "DB_SSL_ROOT_CERT", "MIGRATE_DATABASE_URL",
	"TRUSTED_PROXIES", "SERVER_REQUEST_TIMEOUT", "PUBLIC_BASE_URL",
	"SESSION_SECRET", "SESSION_LIFETIME", "SESSION_COOKIE_SECURE",
	"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_REDIRECT_URL",
	"ENCRYPTION_KEY",
	"RECIPES_EXTERNAL_ENABLED", "RECIPES_API_KEY", "RECIPES_API_BASE_URL",
	"TLS_CERT_FILE", "TLS_KEY_FILE",
	"HSTS_ENABLED", "HSTS_MAX_AGE", "HSTS_INCLUDE_SUBDOMAINS", "HSTS_PRELOAD",
	"MEDIA_CHORE_PROOF_FRESHNESS_WINDOW",
	"MEDIA_STORAGE_BACKEND", "S3_ENDPOINT", "S3_REGION", "S3_BUCKET",
	"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY", "S3_USE_PATH_STYLE", "S3_PRESIGN_TTL",
	"MEDIA_CHORE_PROOF_RETENTION_DAYS",
	"NOTIFY_SMS_ENABLED", "SMS_ORIGINATION_IDENTITY", "SMS_REGION",
	"SMS_ACCESS_KEY_ID", "SMS_SECRET_ACCESS_KEY", "SMS_RETRY_MAX_ATTEMPTS",
}

// validEncryptionKey is a 64-char hex string (32 bytes) for prod test cases.
const validEncryptionKey = "0101010101010101010101010101010101010101010101010101010101010101"

// devDSN mirrors the package default; declared here so black-box assertions do
// not hard-code the literal in many places.
const devDSN = "postgres://nestova:nestova@localhost:5432/nestova?sslmode=disable"

// devSecret mirrors the package's dev default session secret.
const devSecret = "dev-only-insecure-session-secret-change-me"

// devEncKey mirrors the package's dev default encryption key (64 hex chars).
const devEncKey = "00000000000000000000000000000000000000000000000000000000deadbeef"

// defaultSMSRetryMaxAttempts mirrors the package's own private default
// (SMS_RETRY_MAX_ATTEMPTS's default), applied unconditionally regardless of
// NOTIFY_SMS_ENABLED — see config.go's own comment on why the SMS-disabled
// case still carries this value rather than a zero RetryMaxAttempts.
const defaultSMSRetryMaxAttempts = 3

// validSecret is a 32-byte secret distinct from the dev default, used by the
// prod happy-path case.
var validSecret = strings.Repeat("a", 32)

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	// Isolate from any developer-local .env: Load reads .env from the working
	// directory in dev, so run each case in a fresh temp dir. t.Chdir (like
	// t.Setenv) auto-restores and forbids t.Parallel.
	t.Chdir(t.TempDir())
	for _, k := range allKeys {
		// t.Setenv isolates each case and auto-restores afterwards. Unspecified
		// keys are cleared.
		t.Setenv(k, env[k])
	}
}

// TestLoadValid covers configurations that load successfully, asserting the
// exact derived values.
func TestLoadValid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want config.Config
	}{
		{
			name: "defaults when empty",
			env:  map[string]string{},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			name: "PORT and APP_ENV override (test supplies explicit DSN)",
			env: map[string]string{
				"PORT": "9090", "APP_ENV": "test",
				"DATABASE_URL": "postgres://test:test@localhost:5432/nestova_test",
			},
			want: config.Config{
				Env:     config.EnvTest,
				Server:  config.ServerConfig{Addr: ":9090", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: "postgres://test:test@localhost:5432/nestova_test", MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			name: "colon-prefixed PORT is normalized",
			env:  map[string]string{"PORT": ":3000"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":3000", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			name: "DATABASE_URL override in dev",
			env:  map[string]string{"DATABASE_URL": "postgres://custom:pwd@dbhost:5432/mydb"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: "postgres://custom:pwd@dbhost:5432/mydb", MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			name: "parsed numeric and duration overrides",
			env: map[string]string{
				"DB_MAX_CONNS": "10", "DB_CONNECT_TIMEOUT": "2s", "SESSION_LIFETIME": "48h",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 10, ConnTimeout: 2 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 48 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			name: "prod with full secrets marks cookies secure",
			env: map[string]string{
				"APP_ENV": "prod", "SESSION_SECRET": validSecret,
				"DATABASE_URL":     "postgres://u:p@db:5432/app",
				"GOOGLE_CLIENT_ID": "id", "GOOGLE_CLIENT_SECRET": "secret",
				"GOOGLE_REDIRECT_URL": "https://app/callback",
				"ENCRYPTION_KEY":      validEncryptionKey,
			},
			want: config.Config{
				Env:     config.EnvProd,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: "postgres://u:p@db:5432/app", MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: validSecret, Secure: true, Lifetime: 12 * time.Hour},
				OAuth:   config.OAuthConfig{GoogleClientID: "id", GoogleClientSecret: "secret", GoogleRedirectURL: "https://app/callback"},
				Crypto:  config.CryptoConfig{EncryptionKey: validEncryptionKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			name: "external recipes enabled with credentials",
			env: map[string]string{
				"RECIPES_EXTERNAL_ENABLED": "true",
				"RECIPES_API_KEY":          "spoon-key",
				"RECIPES_API_BASE_URL":     "https://api.spoonacular.com",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
				Recipes: config.RecipesConfig{ExternalEnabled: true, APIKey: "spoon-key", BaseURL: "https://api.spoonacular.com"},
			},
		},
		{
			name: "PUBLIC_BASE_URL is trimmed of a trailing slash",
			env:  map[string]string{"PUBLIC_BASE_URL": "https://nestova.tailnet.ts.net/"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second, PublicBaseURL: "https://nestova.tailnet.ts.net"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// Regression case: TrimSuffix removes only ONE trailing slash
			// occurrence, so "https://host//" would leave one slash behind —
			// TrimRight strips them all, so this must normalize down to a
			// clean origin exactly like the single-trailing-slash case above,
			// not double up with the leading slash on every concatenated
			// deep-link path (".../go/..." would otherwise become
			// ".../..../go/...").
			name: "PUBLIC_BASE_URL with multiple trailing slashes is fully trimmed",
			env:  map[string]string{"PUBLIC_BASE_URL": "https://nestova.tailnet.ts.net//"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second, PublicBaseURL: "https://nestova.tailnet.ts.net"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			name: "supabase provider applies a default pool cap and session mode",
			env: map[string]string{
				"DB_PROVIDER":  "supabase",
				"DATABASE_URL": "postgres://u:p@pooler.supabase.com:5432/postgres?sslmode=require",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: "postgres://u:p@pooler.supabase.com:5432/postgres?sslmode=require", MaxConns: 10, ConnTimeout: 5 * time.Second, Provider: config.DBProviderSupabase, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// Case-insensitive provider, transaction pool mode, an explicit pool
			// cap that overrides the supabase default, and a root cert path.
			name: "supabase transaction mode with explicit cap and root cert",
			env: map[string]string{
				"DB_PROVIDER":      "Supabase",
				"DB_POOL_MODE":     "transaction",
				"DB_MAX_CONNS":     "5",
				"DB_SSL_ROOT_CERT": "/etc/ssl/supabase-ca.crt",
				"DATABASE_URL":     "postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=verify-full",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: "postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=verify-full", MaxConns: 5, ConnTimeout: 5 * time.Second, Provider: config.DBProviderSupabase, PoolMode: config.DBPoolModeTransaction, SSLRootCert: "/etc/ssl/supabase-ca.crt"},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// MIGRATE_DATABASE_URL is captured separately so migrations can target a
			// session/direct connection while the app uses the transaction pooler.
			name: "separate migrate DSN is captured",
			env: map[string]string{
				"DB_PROVIDER":          "supabase",
				"DB_POOL_MODE":         "transaction",
				"DATABASE_URL":         "postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=require",
				"MIGRATE_DATABASE_URL": "postgres://u:p@db.supabase.com:5432/postgres?sslmode=require",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: "postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=require", MaxConns: 10, ConnTimeout: 5 * time.Second, Provider: config.DBProviderSupabase, PoolMode: config.DBPoolModeTransaction, MigrateDSN: "postgres://u:p@db.supabase.com:5432/postgres?sslmode=require"},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// An explicit TRUSTED_PROXIES list is stored raw; TrustedProxyPrefixes
			// parses it (see TestTrustedProxies).
			name: "explicit trusted proxies list",
			env:  map[string]string{"TRUSTED_PROXIES": "10.0.0.0/8, 192.168.0.0/16"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", TrustedProxies: "10.0.0.0/8, 192.168.0.0/16", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			name: "explicit SERVER_REQUEST_TIMEOUT override",
			env:  map[string]string{"SERVER_REQUEST_TIMEOUT": "45s"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 45 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// Explicit auto keeps the legacy behavior (Secure only in prod); in dev
			// that means insecure. The prod side of auto is covered by the
			// "prod with full secrets marks cookies secure" case above (auto is the
			// default there).
			name: "SESSION_COOKIE_SECURE=auto in dev stays insecure",
			env:  map[string]string{"SESSION_COOKIE_SECURE": "auto"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// SESSION_COOKIE_SECURE=true forces Secure cookies even in dev.
			name: "SESSION_COOKIE_SECURE=true forces secure outside prod",
			env:  map[string]string{"SESSION_COOKIE_SECURE": "true"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: true, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// SESSION_COOKIE_SECURE=false disables Secure even in prod (where auto
			// would enable it), for plain-HTTP debugging.
			name: "SESSION_COOKIE_SECURE=false overrides prod auto",
			env: map[string]string{
				"APP_ENV": "prod", "SESSION_SECRET": validSecret,
				"DATABASE_URL":          "postgres://u:p@db:5432/app",
				"GOOGLE_CLIENT_ID":      "id",
				"GOOGLE_CLIENT_SECRET":  "secret",
				"GOOGLE_REDIRECT_URL":   "https://app/callback",
				"ENCRYPTION_KEY":        validEncryptionKey,
				"SESSION_COOKIE_SECURE": "false",
			},
			want: config.Config{
				Env:     config.EnvProd,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: "postgres://u:p@db:5432/app", MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: validSecret, Secure: false, Lifetime: 12 * time.Hour},
				OAuth:   config.OAuthConfig{GoogleClientID: "id", GoogleClientSecret: "secret", GoogleRedirectURL: "https://app/callback"},
				Crypto:  config.CryptoConfig{EncryptionKey: validEncryptionKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// Both TLS files set enables app-terminated TLS.
			name: "TLS cert and key enable app-terminated TLS",
			env: map[string]string{
				"TLS_CERT_FILE": "/etc/nestova/tls/cert.pem",
				"TLS_KEY_FILE":  "/etc/nestova/tls/key.pem",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
				TLS:     config.TLSConfig{CertFile: "/etc/nestova/tls/cert.pem", KeyFile: "/etc/nestova/tls/key.pem"},
			},
		},
		{
			// HSTS parameters are captured for the consumer (see TestHSTSHeaderValue).
			// A preload config is valid only with includeSubDomains and a >= 1y
			// max-age, so this case uses 1 year.
			name: "HSTS parameters are captured",
			env: map[string]string{
				"HSTS_ENABLED":            "true",
				"HSTS_MAX_AGE":            "8760h", // 365 days
				"HSTS_INCLUDE_SUBDOMAINS": "true",
				"HSTS_PRELOAD":            "true",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
				HSTS:    config.HSTSConfig{Enabled: true, MaxAge: 8760 * time.Hour, MaxAgeSet: true, IncludeSubdomains: true, Preload: true},
			},
		},
		{
			// An explicit max-age=0 is captured as set (not "unset"), so the consumer
			// emits max-age=0 to clear a previously-sent HSTS policy.
			name: "explicit HSTS max-age=0 is captured as set",
			env:  map[string]string{"HSTS_ENABLED": "true", "HSTS_MAX_AGE": "0s"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS:     config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
				HSTS:    config.HSTSConfig{Enabled: true, MaxAge: 0, MaxAgeSet: true},
			},
		},
		{
			// The S3 backend with a custom (MinIO-shaped) endpoint, static
			// credentials, path-style addressing, and a non-default presign TTL.
			name: "s3 backend with custom endpoint and static credentials",
			env: map[string]string{
				"MEDIA_STORAGE_BACKEND": "s3",
				"S3_ENDPOINT":           "http://127.0.0.1:9000",
				"S3_REGION":             "us-east-1",
				"S3_BUCKET":             "nestova-photos",
				"S3_ACCESS_KEY_ID":      "minioadmin",
				"S3_SECRET_ACCESS_KEY":  "minioadmin",
				"S3_USE_PATH_STYLE":     "true",
				"S3_PRESIGN_TTL":        "5m",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media: config.MediaConfig{
					Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute,
					Backend: config.MediaStorageBackendS3,
					S3: config.S3Config{
						Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "nestova-photos",
						AccessKeyID: "minioadmin", SecretAccessKey: "minioadmin", UsePathStyle: true,
						PresignTTL: 5 * time.Minute,
					},
				},
				SMS: config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// A case-insensitive backend value and a chore-proof retention window
			// expressed in days.
			name: "s3 backend is case-insensitive and retention days converts to a duration",
			env: map[string]string{
				"MEDIA_STORAGE_BACKEND":            "S3",
				"S3_REGION":                        "us-east-1",
				"S3_BUCKET":                        "nestova-photos",
				"MEDIA_CHORE_PROOF_RETENTION_DAYS": "30",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media: config.MediaConfig{
					Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute,
					Backend:             config.MediaStorageBackendS3,
					S3:                  config.S3Config{Region: "us-east-1", Bucket: "nestova-photos", PresignTTL: 15 * time.Minute},
					ChoreProofRetention: 30 * 24 * time.Hour,
				},
				SMS: config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// Regression test (NES-132 review): a local-backend deployment
			// (the default, MEDIA_STORAGE_BACKEND unset) must load
			// successfully even with a malformed S3_PRESIGN_TTL, an
			// unparseable S3_USE_PATH_STYLE, and a lone S3_ACCESS_KEY_ID
			// with no matching secret — every one of those would fail
			// Load() outright if S3_* parsing/validation were not gated on
			// the s3 backend actually being selected. The resulting
			// S3Config carries only the plain defaults/raw strings, never
			// attempting to interpret the malformed values.
			name: "local backend ignores malformed and partial s3 settings entirely",
			env: map[string]string{
				"S3_PRESIGN_TTL":    "not-a-duration",
				"S3_USE_PATH_STYLE": "not-a-bool",
				"S3_ACCESS_KEY_ID":  "minioadmin",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media: config.MediaConfig{
					Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute,
					Backend: config.MediaStorageBackendLocal,
					S3:      config.S3Config{AccessKeyID: "minioadmin", PresignTTL: 15 * time.Minute},
				},
				SMS: config.SMSConfig{RetryMaxAttempts: defaultSMSRetryMaxAttempts},
			},
		},
		{
			// SMS enabled with static credentials and an explicit retry cap
			// (NES-138) — mirrors the "s3 backend with custom endpoint and
			// static credentials" case's shape for the SMS channel.
			name: "sms enabled with static credentials and an explicit retry cap",
			env: map[string]string{
				"NOTIFY_SMS_ENABLED":       "true",
				"SMS_ORIGINATION_IDENTITY": "+18005550100",
				"SMS_REGION":               "us-east-1",
				"SMS_ACCESS_KEY_ID":        "AKIAEXAMPLE",
				"SMS_SECRET_ACCESS_KEY":    "secret",
				"SMS_RETRY_MAX_ATTEMPTS":   "5",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS: config.SMSConfig{
					Enabled: true, OriginationIdentity: "+18005550100", Region: "us-east-1",
					AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret", RetryMaxAttempts: 5,
				},
			},
		},
		{
			// SMS enabled without static credentials falls back to the AWS
			// SDK's default credential chain (NES-138) — mirrors how a blank
			// S3_ACCESS_KEY_ID/S3_SECRET_ACCESS_KEY pair is likewise valid
			// for the S3 backend.
			name: "sms enabled without static credentials uses the default retry cap",
			env: map[string]string{
				"NOTIFY_SMS_ENABLED":       "true",
				"SMS_ORIGINATION_IDENTITY": "+18005550100",
				"SMS_REGION":               "us-east-1",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", RequestTimeout: 120 * time.Second},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 25 << 20, ChoreProofFreshnessWindow: 60 * time.Minute, Backend: config.MediaStorageBackendLocal, S3: config.S3Config{PresignTTL: 15 * time.Minute}},
				SMS: config.SMSConfig{
					Enabled: true, OriginationIdentity: "+18005550100", Region: "us-east-1",
					RetryMaxAttempts: defaultSMSRetryMaxAttempts,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.env)
			got, err := config.Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestLoadInvalid covers configurations that must fail fast, asserting the
// error names every offending variable so the operator can fix them in one
// pass.
func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		wantContains []string
	}{
		{
			name:         "unknown APP_ENV",
			env:          map[string]string{"APP_ENV": "staging"},
			wantContains: []string{"APP_ENV"},
		},
		{
			name:         "short session secret in dev",
			env:          map[string]string{"SESSION_SECRET": "too-short"},
			wantContains: []string{"SESSION_SECRET", "32"},
		},
		{
			name:         "non-integer DB_MAX_CONNS",
			env:          map[string]string{"DB_MAX_CONNS": "abc"},
			wantContains: []string{"DB_MAX_CONNS"},
		},
		{
			name:         "invalid DB_PROVIDER",
			env:          map[string]string{"DB_PROVIDER": "mysql"},
			wantContains: []string{"DB_PROVIDER"},
		},
		{
			name:         "invalid DB_POOL_MODE",
			env:          map[string]string{"DB_POOL_MODE": "statement"},
			wantContains: []string{"DB_POOL_MODE"},
		},
		{
			name:         "malformed TRUSTED_PROXIES CIDR",
			env:          map[string]string{"TRUSTED_PROXIES": "127.0.0.0/8, not-a-cidr"},
			wantContains: []string{"TRUSTED_PROXIES", "not-a-cidr"},
		},
		{
			name:         "invalid SESSION_COOKIE_SECURE",
			env:          map[string]string{"SESSION_COOKIE_SECURE": "maybe"},
			wantContains: []string{"SESSION_COOKIE_SECURE"},
		},
		{
			name:         "TLS cert without key",
			env:          map[string]string{"TLS_CERT_FILE": "/etc/nestova/tls/cert.pem"},
			wantContains: []string{"TLS_CERT_FILE", "TLS_KEY_FILE"},
		},
		{
			name:         "TLS key without cert",
			env:          map[string]string{"TLS_KEY_FILE": "/etc/nestova/tls/key.pem"},
			wantContains: []string{"TLS_CERT_FILE", "TLS_KEY_FILE"},
		},
		{
			name:         "negative HSTS_MAX_AGE when enabled",
			env:          map[string]string{"HSTS_ENABLED": "true", "HSTS_MAX_AGE": "-1h"},
			wantContains: []string{"HSTS_MAX_AGE"},
		},
		{
			name: "HSTS_PRELOAD without includeSubDomains",
			env: map[string]string{
				"HSTS_ENABLED": "true", "HSTS_PRELOAD": "true", "HSTS_MAX_AGE": "8760h",
			},
			wantContains: []string{"HSTS_PRELOAD", "HSTS_INCLUDE_SUBDOMAINS"},
		},
		{
			name: "HSTS_PRELOAD with too-short max-age",
			env: map[string]string{
				"HSTS_ENABLED": "true", "HSTS_PRELOAD": "true",
				"HSTS_INCLUDE_SUBDOMAINS": "true", "HSTS_MAX_AGE": "168h",
			},
			wantContains: []string{"HSTS_PRELOAD", "1 year"},
		},
		{
			name:         "negative DB_MAX_CONNS",
			env:          map[string]string{"DB_MAX_CONNS": "-1"},
			wantContains: []string{"DB_MAX_CONNS", ">= 0"},
		},
		{
			name:         "invalid duration",
			env:          map[string]string{"DB_CONNECT_TIMEOUT": "5x"},
			wantContains: []string{"DB_CONNECT_TIMEOUT"},
		},
		{
			name:         "external enabled without api key",
			env:          map[string]string{"RECIPES_EXTERNAL_ENABLED": "true", "RECIPES_API_BASE_URL": "https://api"},
			wantContains: []string{"RECIPES_API_KEY"},
		},
		{
			name:         "external enabled without base url",
			env:          map[string]string{"RECIPES_EXTERNAL_ENABLED": "true", "RECIPES_API_KEY": "k"},
			wantContains: []string{"RECIPES_API_BASE_URL"},
		},
		{
			name:         "non-boolean RECIPES_EXTERNAL_ENABLED",
			env:          map[string]string{"RECIPES_EXTERNAL_ENABLED": "yes-please"},
			wantContains: []string{"RECIPES_EXTERNAL_ENABLED"},
		},
		{
			name:         "malformed RECIPES_API_BASE_URL",
			env:          map[string]string{"RECIPES_EXTERNAL_ENABLED": "true", "RECIPES_API_KEY": "k", "RECIPES_API_BASE_URL": "not-a-url"},
			wantContains: []string{"RECIPES_API_BASE_URL"},
		},
		{
			name:         "relative RECIPES_API_BASE_URL is rejected",
			env:          map[string]string{"RECIPES_EXTERNAL_ENABLED": "true", "RECIPES_API_KEY": "k", "RECIPES_API_BASE_URL": "/api/recipes"},
			wantContains: []string{"RECIPES_API_BASE_URL", "absolute"},
		},
		{
			name:         "malformed PUBLIC_BASE_URL",
			env:          map[string]string{"PUBLIC_BASE_URL": "not-a-url"},
			wantContains: []string{"PUBLIC_BASE_URL", "absolute"},
		},
		{
			name:         "relative PUBLIC_BASE_URL is rejected",
			env:          map[string]string{"PUBLIC_BASE_URL": "/go/claim-task/1"},
			wantContains: []string{"PUBLIC_BASE_URL", "absolute"},
		},
		{
			name:         "PUBLIC_BASE_URL with a path is rejected",
			env:          map[string]string{"PUBLIC_BASE_URL": "https://nestova.tailnet.ts.net/go/claim-task/1"},
			wantContains: []string{"PUBLIC_BASE_URL", "origin only"},
		},
		{
			name:         "PUBLIC_BASE_URL with a query string is rejected",
			env:          map[string]string{"PUBLIC_BASE_URL": "https://nestova.tailnet.ts.net?foo=bar"},
			wantContains: []string{"PUBLIC_BASE_URL", "origin only"},
		},
		{
			name:         "PUBLIC_BASE_URL with a fragment is rejected",
			env:          map[string]string{"PUBLIC_BASE_URL": "https://nestova.tailnet.ts.net#section"},
			wantContains: []string{"PUBLIC_BASE_URL", "origin only"},
		},
		{
			name:         "PUBLIC_BASE_URL with userinfo is rejected",
			env:          map[string]string{"PUBLIC_BASE_URL": "https://user:pass@nestova.tailnet.ts.net"},
			wantContains: []string{"PUBLIC_BASE_URL", "origin only"},
		},
		{
			name:         "non-positive connect timeout",
			env:          map[string]string{"DB_CONNECT_TIMEOUT": "0s"},
			wantContains: []string{"DB_CONNECT_TIMEOUT", "positive"},
		},
		{
			name:         "non-positive session lifetime",
			env:          map[string]string{"SESSION_LIFETIME": "-5m"},
			wantContains: []string{"SESSION_LIFETIME", "positive"},
		},
		{
			name:         "invalid SERVER_REQUEST_TIMEOUT duration",
			env:          map[string]string{"SERVER_REQUEST_TIMEOUT": "5x"},
			wantContains: []string{"SERVER_REQUEST_TIMEOUT"},
		},
		{
			name:         "SERVER_REQUEST_TIMEOUT below the minimum",
			env:          map[string]string{"SERVER_REQUEST_TIMEOUT": "10s"},
			wantContains: []string{"SERVER_REQUEST_TIMEOUT", "at least"},
		},
		{
			name: "prod rejects default secret and missing oauth",
			env: map[string]string{
				"APP_ENV": "prod", "DATABASE_URL": "postgres://u:p@db/app",
			},
			wantContains: []string{"non-default", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_REDIRECT_URL"},
		},
		{
			name:         "test requires explicit DATABASE_URL (no dev fallback)",
			env:          map[string]string{"APP_ENV": "test"},
			wantContains: []string{"DATABASE_URL"},
		},
		{
			name: "prod requires explicit DATABASE_URL (no dev fallback)",
			env: map[string]string{
				"APP_ENV": "prod", "SESSION_SECRET": validSecret,
				"GOOGLE_CLIENT_ID": "id", "GOOGLE_CLIENT_SECRET": "secret",
				"GOOGLE_REDIRECT_URL": "https://app/callback",
			},
			wantContains: []string{"DATABASE_URL"},
		},
		{
			name: "prod rejects whitespace-only required values",
			env: map[string]string{
				"APP_ENV": "prod", "SESSION_SECRET": validSecret,
				"DATABASE_URL":     "   ",
				"GOOGLE_CLIENT_ID": "  ", "GOOGLE_CLIENT_SECRET": "secret",
				"GOOGLE_REDIRECT_URL": "https://app/callback",
			},
			wantContains: []string{"DATABASE_URL", "GOOGLE_CLIENT_ID"},
		},
		{
			name:         "non-hex encryption key is rejected in any env",
			env:          map[string]string{"ENCRYPTION_KEY": "not-hex!!"},
			wantContains: []string{"ENCRYPTION_KEY must be hex"},
		},
		{
			name:         "wrong-length encryption key is rejected",
			env:          map[string]string{"ENCRYPTION_KEY": "abcdef"},
			wantContains: []string{"ENCRYPTION_KEY must decode to 32 bytes"},
		},
		{
			name: "prod rejects a whitespace-only encryption key",
			env: map[string]string{
				"APP_ENV": "prod", "SESSION_SECRET": validSecret,
				"DATABASE_URL":     "postgres://u:p@db:5432/app",
				"GOOGLE_CLIENT_ID": "id", "GOOGLE_CLIENT_SECRET": "secret",
				"GOOGLE_REDIRECT_URL": "https://app/callback",
				"ENCRYPTION_KEY":      "   ", // trimmed to empty -> Key() fails fast in prod
			},
			wantContains: []string{"ENCRYPTION_KEY is not set"},
		},
		{
			name: "prod rejects the default encryption key",
			env: map[string]string{
				"APP_ENV": "prod", "SESSION_SECRET": validSecret,
				"DATABASE_URL":     "postgres://u:p@db:5432/app",
				"GOOGLE_CLIENT_ID": "id", "GOOGLE_CLIENT_SECRET": "secret",
				"GOOGLE_REDIRECT_URL": "https://app/callback",
				// ENCRYPTION_KEY unset -> falls back to the dev default, which prod rejects.
			},
			wantContains: []string{"ENCRYPTION_KEY must be set to a non-default value in prod"},
		},
		{
			name: "multiple problems are all reported",
			env: map[string]string{
				"APP_ENV": "staging", "DB_MAX_CONNS": "nope", "SESSION_SECRET": "x",
			},
			wantContains: []string{"APP_ENV", "DB_MAX_CONNS", "SESSION_SECRET"},
		},
		{
			name:         "invalid MEDIA_STORAGE_BACKEND",
			env:          map[string]string{"MEDIA_STORAGE_BACKEND": "azure-blob"},
			wantContains: []string{"MEDIA_STORAGE_BACKEND"},
		},
		{
			name:         "s3 backend without bucket or region",
			env:          map[string]string{"MEDIA_STORAGE_BACKEND": "s3"},
			wantContains: []string{"S3_BUCKET", "S3_REGION"},
		},
		{
			name:         "s3 backend without region",
			env:          map[string]string{"MEDIA_STORAGE_BACKEND": "s3", "S3_BUCKET": "photos"},
			wantContains: []string{"S3_REGION"},
		},
		{
			name: "s3 backend with a non-positive presign ttl",
			env: map[string]string{
				"MEDIA_STORAGE_BACKEND": "s3", "S3_BUCKET": "photos", "S3_REGION": "us-east-1",
				"S3_PRESIGN_TTL": "0s",
			},
			wantContains: []string{"S3_PRESIGN_TTL", "positive"},
		},
		{
			// S3 credential validation (like every other S3_* check) is gated
			// on the s3 backend actually being selected (NES-132 review) —
			// MEDIA_STORAGE_BACKEND=s3 plus the otherwise-required bucket/
			// region isolate this case to JUST the credential-pairing error.
			name: "s3 access key without secret",
			env: map[string]string{
				"MEDIA_STORAGE_BACKEND": "s3", "S3_BUCKET": "photos", "S3_REGION": "us-east-1",
				"S3_ACCESS_KEY_ID": "minioadmin",
			},
			wantContains: []string{"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"},
		},
		{
			name: "s3 secret without access key",
			env: map[string]string{
				"MEDIA_STORAGE_BACKEND": "s3", "S3_BUCKET": "photos", "S3_REGION": "us-east-1",
				"S3_SECRET_ACCESS_KEY": "minioadmin",
			},
			wantContains: []string{"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"},
		},
		{
			name:         "negative chore-proof retention",
			env:          map[string]string{"MEDIA_CHORE_PROOF_RETENTION_DAYS": "-1"},
			wantContains: []string{"MEDIA_CHORE_PROOF_RETENTION_DAYS", ">= 0"},
		},
		{
			// Regression test: days*24*time.Hour must be a CHECKED conversion,
			// not a raw multiplication that silently wraps time.Duration's
			// underlying int64 nanoseconds for a large-enough day count.
			name:         "oversized chore-proof retention overflows a raw duration conversion",
			env:          map[string]string{"MEDIA_CHORE_PROOF_RETENTION_DAYS": "999999999999"},
			wantContains: []string{"MEDIA_CHORE_PROOF_RETENTION_DAYS"},
		},
		{
			name:         "sms enabled without origination identity or region",
			env:          map[string]string{"NOTIFY_SMS_ENABLED": "true"},
			wantContains: []string{"SMS_ORIGINATION_IDENTITY", "SMS_REGION"},
		},
		{
			name: "sms enabled without region",
			env: map[string]string{
				"NOTIFY_SMS_ENABLED": "true", "SMS_ORIGINATION_IDENTITY": "+18005550100",
			},
			wantContains: []string{"SMS_REGION"},
		},
		{
			name: "sms enabled with a non-positive retry max attempts",
			env: map[string]string{
				"NOTIFY_SMS_ENABLED": "true", "SMS_ORIGINATION_IDENTITY": "+18005550100",
				"SMS_REGION": "us-east-1", "SMS_RETRY_MAX_ATTEMPTS": "0",
			},
			wantContains: []string{"SMS_RETRY_MAX_ATTEMPTS", "positive"},
		},
		{
			// SMS credential validation (like every other SMS_* check) is
			// gated on NOTIFY_SMS_ENABLED actually being true, mirroring
			// S3's identical credential-pairing checks above.
			name: "sms access key without secret",
			env: map[string]string{
				"NOTIFY_SMS_ENABLED": "true", "SMS_ORIGINATION_IDENTITY": "+18005550100",
				"SMS_REGION": "us-east-1", "SMS_ACCESS_KEY_ID": "AKIAEXAMPLE",
			},
			wantContains: []string{"SMS_ACCESS_KEY_ID", "SMS_SECRET_ACCESS_KEY"},
		},
		{
			name: "sms secret without access key",
			env: map[string]string{
				"NOTIFY_SMS_ENABLED": "true", "SMS_ORIGINATION_IDENTITY": "+18005550100",
				"SMS_REGION": "us-east-1", "SMS_SECRET_ACCESS_KEY": "secret",
			},
			wantContains: []string{"SMS_ACCESS_KEY_ID", "SMS_SECRET_ACCESS_KEY"},
		},
		{
			name:         "non-boolean NOTIFY_SMS_ENABLED",
			env:          map[string]string{"NOTIFY_SMS_ENABLED": "maybe"},
			wantContains: []string{"NOTIFY_SMS_ENABLED"},
		},
		{
			name: "sms enabled with a non-integer retry max attempts",
			env: map[string]string{
				"NOTIFY_SMS_ENABLED": "true", "SMS_ORIGINATION_IDENTITY": "+18005550100",
				"SMS_REGION": "us-east-1", "SMS_RETRY_MAX_ATTEMPTS": "abc",
			},
			wantContains: []string{"SMS_RETRY_MAX_ATTEMPTS"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.env)
			_, err := config.Load()
			if err == nil {
				t.Fatal("Load() = nil error, want error")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, want it to contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestTrustedProxies covers the unset-default vs explicit-empty distinction and
// the TrustedProxyPrefixes parsing, which the comparable struct cases above
// cannot fully express.
func TestTrustedProxies(t *testing.T) {
	t.Run("unset defaults to loopback", func(t *testing.T) {
		setEnv(t, nil) // sets every key to "" and registers restore
		// The loopback default applies only when TRUSTED_PROXIES is truly unset.
		_ = os.Unsetenv("TRUSTED_PROXIES")
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.Server.TrustedProxies != "127.0.0.0/8,::1/128" {
			t.Errorf("TrustedProxies = %q, want the loopback default", cfg.Server.TrustedProxies)
		}
		if got := cfg.Server.TrustedProxyPrefixes(); len(got) != 2 {
			t.Errorf("TrustedProxyPrefixes() len = %d, want 2", len(got))
		}
	})

	t.Run("explicit empty trusts nothing", func(t *testing.T) {
		setEnv(t, map[string]string{"TRUSTED_PROXIES": ""})
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.Server.TrustedProxies != "" {
			t.Errorf("TrustedProxies = %q, want empty", cfg.Server.TrustedProxies)
		}
		if got := cfg.Server.TrustedProxyPrefixes(); len(got) != 0 {
			t.Errorf("TrustedProxyPrefixes() len = %d, want 0 (trust nothing)", len(got))
		}
	})

	t.Run("prefixes are parsed and masked", func(t *testing.T) {
		setEnv(t, map[string]string{"TRUSTED_PROXIES": "192.168.1.5/24, ::1/128"})
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		want := []netip.Prefix{
			netip.MustParsePrefix("192.168.1.0/24"), // host bits masked off
			netip.MustParsePrefix("::1/128"),
		}
		if got := cfg.Server.TrustedProxyPrefixes(); !reflect.DeepEqual(got, want) {
			t.Errorf("TrustedProxyPrefixes() = %v, want %v", got, want)
		}
	})
}

// TestTLSConfigEnabled covers the listener-selection decision (NES-54) without
// binding a socket: TLS is enabled only when both files are present.
func TestTLSConfigEnabled(t *testing.T) {
	cases := []struct {
		name string
		tls  config.TLSConfig
		want bool
	}{
		{"both set", config.TLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}, true},
		{"neither set", config.TLSConfig{}, false},
		{"cert only", config.TLSConfig{CertFile: "c.pem"}, false},
		{"key only", config.TLSConfig{KeyFile: "k.pem"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tls.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
