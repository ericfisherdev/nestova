// Package bootstrap handles first-run detection and persistence of the runtime
// configuration that must exist before the database does.
//
// When Nestova starts with no database configured, the setup wizard
// (internal/platform/setup) collects connection details and generated secrets,
// and SaveState persists them to a small JSON state file. Subsequent boots load
// that file and feed it into the normal env-based config.Load via ExportToEnv,
// so config.go needs no changes to its environment-first contract. The file
// holds secrets, so it is written 0600 under a 0700 directory.
//
// The mechanics (state file format, env var derivation, NeedsSetup's rules) now
// live in github.com/ericfisherdev/nestcore/bootstrap, generalized to take the
// caller's own app name; this package binds that generic package to "nestova"
// so every existing consumer keeps referencing bootstrap.X unchanged, with the
// exact same NESTOVA_STATE_FILE / NESTOVA_FORCE_SETUP env vars and
// ./.localdata/nestova.json default it always had.
package bootstrap

import (
	"fmt"

	ncbootstrap "github.com/ericfisherdev/nestcore/bootstrap"
)

// app identifies Nestova to nestcore's bootstrap package, deriving
// NESTOVA_STATE_FILE, NESTOVA_FORCE_SETUP, and the ./.localdata/nestova.json
// default. "nestova" is a compile-time literal that always satisfies
// ncbootstrap.NewApp's validation, so the error is never actually reachable;
// it is still checked (rather than ignored) to fail loudly instead of
// silently on an App zero value if that ever changed.
var app = func() ncbootstrap.App {
	a, err := ncbootstrap.NewApp("nestova")
	if err != nil {
		panic(fmt.Sprintf("bootstrap: %v", err))
	}
	return a
}()

// StateFileEnv overrides the default state-file path.
var StateFileEnv = app.StateFileEnv()

// ForceSetupEnv forces setup mode when truthy, even where the trigger would
// not otherwise fire. It lets dev (which keeps a localhost default DSN)
// exercise the wizard on demand.
var ForceSetupEnv = app.ForceSetupEnv()

// State is the persisted first-run configuration. Each field maps to an
// environment variable that config.Load consumes, applied via ExportToEnv.
type State = ncbootstrap.State

// StatePath returns the configured state-file path (NESTOVA_STATE_FILE) or the
// default ./.localdata/nestova.json.
func StatePath() string {
	return app.StatePath()
}

// LoadState reads and parses the state file at path. A missing file is not an
// error: it returns (nil, nil), which NeedsSetup reads as "not configured".
func LoadState(path string) (*State, error) {
	return ncbootstrap.LoadState(path)
}

// SaveState writes s to path as indented JSON with owner-only permissions (0600
// file under a 0700 directory), since the file holds the database password and
// the session/encryption secrets. The parent directory is created when missing.
func SaveState(path string, s *State) error {
	return ncbootstrap.SaveState(path, s)
}

// NeedsSetup reports whether the app should enter first-run setup mode rather
// than booting normally. Setup is needed only when nothing is configured: no
// persisted DSN (state) and no DATABASE_URL in the environment. To preserve the
// dev happy-path (config.Load's localhost default), dev is exempt unless
// NESTOVA_FORCE_SETUP is set; the force flag also lets any environment exercise
// the wizard on demand. A configured-but-unreachable database therefore stays
// fail-fast and never drops a live server into reconfigure mode.
func NeedsSetup(state *State) bool {
	return app.NeedsSetup(state)
}

// ExportToEnv sets the persisted configuration into the process environment for
// variables that are not already set — the real environment always wins,
// mirroring godotenv — so the unchanged env-based config.Load can consume it.
func ExportToEnv(s *State) error {
	return ncbootstrap.ExportToEnv(s)
}

// GenerateSecret returns a cryptographically random secret encoded as hex. It
// backs the session secret and the at-rest encryption key when the operator
// has not supplied them via the environment.
func GenerateSecret() (string, error) {
	return ncbootstrap.GenerateSecret()
}
