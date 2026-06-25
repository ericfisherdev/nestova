package bootstrap_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/bootstrap"
)

func TestStatePath_DefaultAndOverride(t *testing.T) {
	t.Setenv(bootstrap.StateFileEnv, "")
	if got := bootstrap.StatePath(); got != "./.localdata/nestova.json" {
		t.Fatalf("default StatePath = %q, want ./.localdata/nestova.json", got)
	}
	t.Setenv(bootstrap.StateFileEnv, "/tmp/custom/state.json")
	if got := bootstrap.StatePath(); got != "/tmp/custom/state.json" {
		t.Fatalf("override StatePath = %q, want /tmp/custom/state.json", got)
	}
}

func TestSaveLoadState_RoundTripAndPermissions(t *testing.T) {
	// Place the file under a not-yet-existing subdirectory to exercise MkdirAll.
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	want := &bootstrap.State{
		DatabaseURL:   "postgres://u:p@localhost:5434/db?sslmode=disable",
		SessionSecret: "deadbeef",
		EncryptionKey: "cafebabe",
		Provider:      "supabase",
		PoolMode:      "transaction",
		SSLRootCert:   "/etc/ssl/supabase-ca.crt",
	}
	if err := bootstrap.SaveState(path, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 600", perm)
	}
	if dirInfo, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat state dir: %v", err)
	} else if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state dir mode = %o, want 700", perm)
	}

	got, err := bootstrap.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got == nil || *got != *want {
		t.Fatalf("LoadState = %+v, want %+v", got, want)
	}
}

func TestSaveState_TightensExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// Pre-create the file with loose permissions; SaveState must tighten it.
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := bootstrap.SaveState(path, &bootstrap.State{DatabaseURL: "x"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

func TestLoadState_MissingFileIsNotAnError(t *testing.T) {
	got, err := bootstrap.LoadState(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("LoadState of missing file errored: %v", err)
	}
	if got != nil {
		t.Fatalf("LoadState of missing file = %+v, want nil", got)
	}
}

func TestNeedsSetup(t *testing.T) {
	configured := &bootstrap.State{DatabaseURL: "postgres://x"}
	cases := []struct {
		name     string
		state    *bootstrap.State
		appEnv   string
		dbURL    string
		force    string
		expected bool
	}{
		{name: "prod, nothing configured -> setup", state: nil, appEnv: "prod", expected: true},
		{name: "test, nothing configured -> setup", state: nil, appEnv: "test", expected: true},
		{name: "dev, nothing configured -> no setup (localhost default)", state: nil, appEnv: "dev", expected: false},
		{name: "empty APP_ENV defaults to dev -> no setup", state: nil, appEnv: "", expected: false},
		{name: "prod but DATABASE_URL set -> no setup", state: nil, appEnv: "prod", dbURL: "postgres://x", expected: false},
		{name: "prod but state has DSN -> no setup", state: configured, appEnv: "prod", expected: false},
		{name: "dev but forced -> setup", state: nil, appEnv: "dev", force: "1", expected: true},
		{name: "configured but forced -> setup", state: configured, appEnv: "prod", force: "true", expected: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("APP_ENV", tc.appEnv)
			t.Setenv("DATABASE_URL", tc.dbURL)
			t.Setenv(bootstrap.ForceSetupEnv, tc.force)
			if got := bootstrap.NeedsSetup(tc.state); got != tc.expected {
				t.Fatalf("NeedsSetup = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestNeedsSetup_ClearingForceEscapesAfterConfigured(t *testing.T) {
	// Mirrors the main() restart loop: with NESTOVA_FORCE_SETUP set, a configured
	// app re-enters setup every boot (the loop would spin forever); clearing the
	// flag after setup completes lets the next NeedsSetup — now seeing persisted
	// state — return false so the restart boots normally.
	t.Setenv("APP_ENV", "prod")
	t.Setenv("DATABASE_URL", "")
	state := &bootstrap.State{DatabaseURL: "postgres://x"}

	t.Setenv(bootstrap.ForceSetupEnv, "1")
	if !bootstrap.NeedsSetup(state) {
		t.Fatal("with the force flag set, NeedsSetup should be true")
	}
	if err := os.Unsetenv(bootstrap.ForceSetupEnv); err != nil {
		t.Fatalf("unset force flag: %v", err)
	}
	if bootstrap.NeedsSetup(state) {
		t.Fatal("after clearing the force flag with persisted state, NeedsSetup should be false")
	}
}

// unsetEnv makes LookupEnv report key as absent for the duration of the test.
// t.Setenv registers restoration of the original value; os.Unsetenv then removes
// it so the code under test sees it as unset.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

func TestExportToEnv_EnvWins(t *testing.T) {
	// Secrets are independent and applied per variable; the DB-connection group is
	// applied as a unit. Here one DB var (DATABASE_URL) is already set, so the
	// whole persisted DB group must be skipped — no hybrid config.
	t.Setenv("DATABASE_URL", "postgres://preset")
	unsetEnv(t, "SESSION_SECRET")
	unsetEnv(t, "ENCRYPTION_KEY")
	unsetEnv(t, "DB_PROVIDER")
	unsetEnv(t, "DB_POOL_MODE")
	unsetEnv(t, "DB_SSL_ROOT_CERT")

	err := bootstrap.ExportToEnv(&bootstrap.State{
		DatabaseURL:   "postgres://fromstate",
		SessionSecret: "secret-from-state",
		EncryptionKey: "key-from-state",
		Provider:      "supabase",
		PoolMode:      "transaction",
		SSLRootCert:   "/etc/ssl/supabase-ca.crt",
	})
	if err != nil {
		t.Fatalf("ExportToEnv: %v", err)
	}
	if got := os.Getenv("DATABASE_URL"); got != "postgres://preset" {
		t.Fatalf("DATABASE_URL = %q, want the preset value to win", got)
	}
	if got := os.Getenv("SESSION_SECRET"); got != "secret-from-state" {
		t.Fatalf("SESSION_SECRET = %q, want state value applied", got)
	}
	if got := os.Getenv("ENCRYPTION_KEY"); got != "key-from-state" {
		t.Fatalf("ENCRYPTION_KEY = %q, want state value applied", got)
	}
	// The rest of the DB group must NOT be applied, since DATABASE_URL is set.
	for _, k := range []string{"DB_PROVIDER", "DB_POOL_MODE", "DB_SSL_ROOT_CERT"} {
		if got := os.Getenv(k); got != "" {
			t.Fatalf("%s = %q, want empty (DB group skipped when any DB env var is set)", k, got)
		}
	}
}

func TestExportToEnv_AppliesDBGroupWhenNoDBEnv(t *testing.T) {
	for _, k := range []string{"DATABASE_URL", "DB_PROVIDER", "DB_POOL_MODE", "DB_SSL_ROOT_CERT"} {
		unsetEnv(t, k)
	}
	err := bootstrap.ExportToEnv(&bootstrap.State{
		DatabaseURL: "postgres://fromstate",
		Provider:    "supabase",
		PoolMode:    "transaction",
		SSLRootCert: "/etc/ssl/supabase-ca.crt",
	})
	if err != nil {
		t.Fatalf("ExportToEnv: %v", err)
	}
	for k, want := range map[string]string{
		"DATABASE_URL":     "postgres://fromstate",
		"DB_PROVIDER":      "supabase",
		"DB_POOL_MODE":     "transaction",
		"DB_SSL_ROOT_CERT": "/etc/ssl/supabase-ca.crt",
	} {
		if got := os.Getenv(k); got != want {
			t.Fatalf("%s = %q, want %q applied from state", k, got, want)
		}
	}
}

func TestExportToEnv_NilStateIsNoop(t *testing.T) {
	if err := bootstrap.ExportToEnv(nil); err != nil {
		t.Fatalf("ExportToEnv(nil): %v", err)
	}
}

func TestGenerateSecret_LengthAndUniqueness(t *testing.T) {
	a, err := bootstrap.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	b, err := bootstrap.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	// 32 random bytes -> 64 hex chars, decoding to exactly 32 bytes (AES-256).
	if len(a) != 64 {
		t.Fatalf("secret length = %d, want 64 hex chars", len(a))
	}
	raw, err := hex.DecodeString(a)
	if err != nil {
		t.Fatalf("secret is not valid hex: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded secret = %d bytes, want 32", len(raw))
	}
	if a == b {
		t.Fatal("two generated secrets were identical")
	}
}
