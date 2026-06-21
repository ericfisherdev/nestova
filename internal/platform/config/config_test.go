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
	"TRUSTED_PROXIES",
	"SESSION_SECRET", "SESSION_LIFETIME", "SESSION_COOKIE_SECURE",
	"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_REDIRECT_URL",
	"ENCRYPTION_KEY",
	"RECIPES_EXTERNAL_ENABLED", "RECIPES_API_KEY", "RECIPES_API_BASE_URL",
	"TLS_CERT_FILE", "TLS_KEY_FILE",
	"HSTS_ENABLED", "HSTS_MAX_AGE", "HSTS_INCLUDE_SUBDOMAINS", "HSTS_PRELOAD",
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":9090"},
				DB:      config.DBConfig{DSN: "postgres://test:test@localhost:5432/nestova_test", MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
			},
		},
		{
			name: "colon-prefixed PORT is normalized",
			env:  map[string]string{"PORT": ":3000"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":3000"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
			},
		},
		{
			name: "DATABASE_URL override in dev",
			env:  map[string]string{"DATABASE_URL": "postgres://custom:pwd@dbhost:5432/mydb"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: "postgres://custom:pwd@dbhost:5432/mydb", MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
			},
		},
		{
			name: "parsed numeric and duration overrides",
			env: map[string]string{
				"DB_MAX_CONNS": "10", "DB_CONNECT_TIMEOUT": "2s", "SESSION_LIFETIME": "48h",
			},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 10, ConnTimeout: 2 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 48 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: "postgres://u:p@db:5432/app", MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: validSecret, Secure: true, Lifetime: 12 * time.Hour},
				OAuth:   config.OAuthConfig{GoogleClientID: "id", GoogleClientSecret: "secret", GoogleRedirectURL: "https://app/callback"},
				Crypto:  config.CryptoConfig{EncryptionKey: validEncryptionKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
				Recipes: config.RecipesConfig{ExternalEnabled: true, APIKey: "spoon-key", BaseURL: "https://api.spoonacular.com"},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: "postgres://u:p@pooler.supabase.com:5432/postgres?sslmode=require", MaxConns: 10, ConnTimeout: 5 * time.Second, Provider: config.DBProviderSupabase, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: "postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=verify-full", MaxConns: 5, ConnTimeout: 5 * time.Second, Provider: config.DBProviderSupabase, PoolMode: config.DBPoolModeTransaction, SSLRootCert: "/etc/ssl/supabase-ca.crt"},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: "postgres://u:p@pooler.supabase.com:6543/postgres?sslmode=require", MaxConns: 10, ConnTimeout: 5 * time.Second, Provider: config.DBProviderSupabase, PoolMode: config.DBPoolModeTransaction, MigrateDSN: "postgres://u:p@db.supabase.com:5432/postgres?sslmode=require"},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
			},
		},
		{
			// An explicit TRUSTED_PROXIES list is stored raw; TrustedProxyPrefixes
			// parses it (see TestTrustedProxies).
			name: "explicit trusted proxies list",
			env:  map[string]string{"TRUSTED_PROXIES": "10.0.0.0/8, 192.168.0.0/16"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080", TrustedProxies: "10.0.0.0/8, 192.168.0.0/16"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
			},
		},
		{
			// SESSION_COOKIE_SECURE=true forces Secure cookies even in dev.
			name: "SESSION_COOKIE_SECURE=true forces secure outside prod",
			env:  map[string]string{"SESSION_COOKIE_SECURE": "true"},
			want: config.Config{
				Env:     config.EnvDev,
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: true, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: "postgres://u:p@db:5432/app", MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: validSecret, Secure: false, Lifetime: 12 * time.Hour},
				OAuth:   config.OAuthConfig{GoogleClientID: "id", GoogleClientSecret: "secret", GoogleRedirectURL: "https://app/callback"},
				Crypto:  config.CryptoConfig{EncryptionKey: validEncryptionKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
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
				Server:  config.ServerConfig{Addr: ":8080"},
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second, Provider: config.DBProviderPostgres, PoolMode: config.DBPoolModeSession},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
				HSTS:    config.HSTSConfig{Enabled: true, MaxAge: 0, MaxAgeSet: true},
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
