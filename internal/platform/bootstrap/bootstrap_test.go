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

func TestExportToEnv_EnvWins(t *testing.T) {
	// A pre-set variable must not be overwritten; an unset one must be applied.
	// t.Setenv registers restoration of the original value; os.Unsetenv then makes
	// LookupEnv report the var as absent for the duration of the test.
	unset := func(key string) {
		t.Helper()
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
	t.Setenv("DATABASE_URL", "postgres://preset")
	unset("SESSION_SECRET")
	unset("ENCRYPTION_KEY")

	err := bootstrap.ExportToEnv(&bootstrap.State{
		DatabaseURL:   "postgres://fromstate",
		SessionSecret: "secret-from-state",
		EncryptionKey: "key-from-state",
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
