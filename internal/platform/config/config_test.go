package config_test

import (
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
	"SESSION_SECRET", "SESSION_LIFETIME",
	"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_REDIRECT_URL",
	"ENCRYPTION_KEY",
	"RECIPES_EXTERNAL_ENABLED", "RECIPES_API_KEY", "RECIPES_API_BASE_URL",
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
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second},
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
				DB:      config.DBConfig{DSN: "postgres://test:test@localhost:5432/nestova_test", MaxConns: 0, ConnTimeout: 5 * time.Second},
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
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second},
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
				DB:      config.DBConfig{DSN: "postgres://custom:pwd@dbhost:5432/mydb", MaxConns: 0, ConnTimeout: 5 * time.Second},
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
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 10, ConnTimeout: 2 * time.Second},
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
				DB:      config.DBConfig{DSN: "postgres://u:p@db:5432/app", MaxConns: 0, ConnTimeout: 5 * time.Second},
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
				DB:      config.DBConfig{DSN: devDSN, MaxConns: 0, ConnTimeout: 5 * time.Second},
				Session: config.SessionConfig{Secret: devSecret, Secure: false, Lifetime: 12 * time.Hour},
				Crypto:  config.CryptoConfig{EncryptionKey: devEncKey},
				Media:   config.MediaConfig{Root: "./.localdata/media", MaxUploadBytes: 10 << 20},
				Recipes: config.RecipesConfig{ExternalEnabled: true, APIKey: "spoon-key", BaseURL: "https://api.spoonacular.com"},
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
