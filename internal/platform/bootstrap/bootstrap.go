// Package bootstrap handles first-run detection and persistence of the runtime
// configuration that must exist before the database does.
//
// When Nestova starts with no database configured, the setup wizard
// (internal/platform/setup) collects connection details and generated secrets,
// and SaveState persists them to a small JSON state file. Subsequent boots load
// that file and feed it into the normal env-based config.Load via ExportToEnv,
// so config.go needs no changes to its environment-first contract. The file
// holds secrets, so it is written 0600 under a 0700 directory.
package bootstrap

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// StateFileEnv overrides the default state-file path.
	StateFileEnv = "NESTOVA_STATE_FILE"
	// ForceSetupEnv forces setup mode when truthy, even where the trigger would
	// not otherwise fire. It lets dev (which keeps a localhost default DSN)
	// exercise the wizard on demand.
	ForceSetupEnv = "NESTOVA_FORCE_SETUP"

	// defaultStatePath is where the state file lives when StateFileEnv is unset.
	// It reuses the ./.localdata convention shared with the MEDIA_ROOT default.
	defaultStatePath = "./.localdata/nestova.json"

	// stateFileMode/stateDirMode keep the file (which holds the database password
	// and the session/encryption secrets) readable only by the owner.
	stateFileMode fs.FileMode = 0o600
	stateDirMode  fs.FileMode = 0o700

	// secretLen is the number of random bytes per generated secret. 32 bytes
	// hex-encode to 64 characters: comfortably past SESSION_SECRET's 32-byte
	// minimum and exactly the 32-byte key ENCRYPTION_KEY decodes to (AES-256).
	secretLen = 32

	// envDev mirrors config.EnvDev without importing the config package, keeping
	// this bootstrap step dependency-free so it can run before config.Load.
	envDev = "dev"
)

// State is the persisted first-run configuration. Each field maps to an
// environment variable that config.Load consumes, applied via ExportToEnv.
type State struct {
	DatabaseURL   string `json:"database_url"`
	SessionSecret string `json:"session_secret"`
	EncryptionKey string `json:"encryption_key"`
}

// StatePath returns the configured state-file path (NESTOVA_STATE_FILE) or the
// default ./.localdata/nestova.json.
func StatePath() string {
	if p := strings.TrimSpace(os.Getenv(StateFileEnv)); p != "" {
		return p
	}
	return defaultStatePath
}

// LoadState reads and parses the state file at path. A missing file is not an
// error: it returns (nil, nil), which NeedsSetup reads as "not configured".
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state file %q: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state file %q: %w", path, err)
	}
	return &s, nil
}

// SaveState writes s to path as indented JSON with owner-only permissions (0600
// file under a 0700 directory), since the file holds the database password and
// the session/encryption secrets. The parent directory is created when missing.
func SaveState(path string, s *State) error {
	if s == nil {
		return errors.New("bootstrap: SaveState requires a non-nil state")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return fmt.Errorf("create state dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.WriteFile(path, data, stateFileMode); err != nil {
		return fmt.Errorf("write state file %q: %w", path, err)
	}
	// WriteFile's mode is only applied when creating the file; chmod afterwards
	// so an existing, looser-permissioned file is also tightened to 0600.
	if err := os.Chmod(path, stateFileMode); err != nil {
		return fmt.Errorf("chmod state file %q: %w", path, err)
	}
	return nil
}

// NeedsSetup reports whether the app should enter first-run setup mode rather
// than booting normally. Setup is needed only when nothing is configured: no
// persisted DSN (state) and no DATABASE_URL in the environment. To preserve the
// dev happy-path (config.Load's localhost default), dev is exempt unless
// NESTOVA_FORCE_SETUP is set; the force flag also lets any environment exercise
// the wizard on demand. A configured-but-unreachable database therefore stays
// fail-fast and never drops a live server into reconfigure mode.
func NeedsSetup(state *State) bool {
	if forced, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(ForceSetupEnv))); forced {
		return true
	}
	if state != nil && strings.TrimSpace(state.DatabaseURL) != "" {
		return false
	}
	if strings.TrimSpace(os.Getenv("DATABASE_URL")) != "" {
		return false
	}
	env := strings.TrimSpace(os.Getenv("APP_ENV"))
	if env == "" {
		env = envDev
	}
	return env != envDev
}

// ExportToEnv sets DATABASE_URL, SESSION_SECRET, and ENCRYPTION_KEY from s into
// the process environment, but only for variables that are not already set — the
// real environment always wins, mirroring godotenv. This lets the unchanged
// env-based config.Load consume the persisted first-run configuration.
func ExportToEnv(s *State) error {
	if s == nil {
		return nil
	}
	for _, kv := range []struct{ key, val string }{
		{"DATABASE_URL", s.DatabaseURL},
		{"SESSION_SECRET", s.SessionSecret},
		{"ENCRYPTION_KEY", s.EncryptionKey},
	} {
		if kv.val == "" {
			continue
		}
		if _, ok := os.LookupEnv(kv.key); ok {
			continue
		}
		if err := os.Setenv(kv.key, kv.val); err != nil {
			return fmt.Errorf("set %s: %w", kv.key, err)
		}
	}
	return nil
}

// GenerateSecret returns a cryptographically random secretLen-byte value encoded
// as hex (64 characters). It backs the session secret and the at-rest encryption
// key when the operator has not supplied them via the environment.
func GenerateSecret() (string, error) {
	b := make([]byte, secretLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}
