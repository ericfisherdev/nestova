// Package dbtest provides the shared harness for database-gated tests
// (NES-149).
//
// Every gated test package used to reset and migrate the ONE database named
// by NESTOVA_TEST_DATABASE_URL. Go runs different packages' test binaries
// concurrently, so any multi-package run with that variable set raced the
// schema resets: one package's migrate.Reset dropped the schema out from
// under another package's in-flight test, and the fixture ended up corrupt
// (classically, goose_db_version claiming versions whose tables no longer
// exist). `go test -p 1` does not fix it — that serializes builds, not the
// test binaries themselves.
//
// NewIsolatedPool gives each package its OWN database derived from the
// configured one, so packages can no longer collide and `go test ./...`
// with the variable set is reliable.
//
// Operator prerequisite: the role in NESTOVA_TEST_DATABASE_URL must have
// the CREATEDB privilege, because the helper creates those derived
// databases on demand. See docs/testing.md.
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

// EnvVar names the DSN every gated test package derives its own database
// from. An unset value skips the gated tests entirely, keeping the default
// `go test ./...` run hermetic.
const EnvVar = "NESTOVA_TEST_DATABASE_URL"

// duplicateDatabaseCode is Postgres's SQLSTATE for "database already
// exists" — expected and harmless on every run after the first.
const duplicateDatabaseCode = "42P04"

const (
	setupTimeout   = 60 * time.Second
	cleanupTimeout = 30 * time.Second
	connectTimeout = 10 * time.Second
)

// PreResetHook runs against the derived DSN immediately before every
// migrate.Reset (both setup and cleanup). It exists for packages whose data
// can block a down-migration — media/adapter and cmd/storage sweep
// s3-stamped photo rows, which migration 00032's rollback guard otherwise
// refuses to drop. Hooks are best-effort: they must not fail the test.
type PreResetHook func(ctx context.Context, dsn string)

// Option customizes NewIsolatedPool.
type Option func(*options)

type options struct {
	preReset PreResetHook
}

// WithPreReset registers a hook to run just before each migrate.Reset.
func WithPreReset(hook PreResetHook) Option {
	return func(o *options) { o.preReset = hook }
}

// NewIsolatedPool returns a pool against a database dedicated to the calling
// package — the configured database's name plus "_" plus suffix — freshly
// reset and migrated. It skips the test when EnvVar is unset.
//
// suffix must be a short, stable, package-identifying literal ("tasks",
// "auth", ...): it becomes part of a real database name, so two packages
// sharing a suffix would re-create the very race this helper removes.
//
// The derived database is created on demand (CREATEDB required) and left in
// place between runs; only its schema is reset, on both setup and cleanup.
func NewIsolatedPool(t *testing.T, suffix string, opts ...Option) *pgxpool.Pool {
	t.Helper()

	var cfg options
	for _, opt := range opts {
		opt(&cfg)
	}

	baseDSN := os.Getenv(EnvVar)
	if baseDSN == "" {
		t.Skipf("set %s to run the gated %s tests", EnvVar, suffix)
	}
	if strings.TrimSpace(suffix) == "" {
		t.Fatal("dbtest: suffix must be a non-empty package identifier")
	}

	derivedDSN, derivedName := deriveDSN(t, baseDSN, suffix)
	createDatabase(t, baseDSN, derivedName)

	setupCtx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if cfg.preReset != nil {
		cfg.preReset(setupCtx, derivedDSN)
	}
	if err := migrate.Reset(setupCtx, derivedDSN); err != nil {
		t.Fatalf("reset schema on %s: %v", derivedName, err)
	}
	if err := migrate.Up(setupCtx, derivedDSN); err != nil {
		t.Fatalf("apply migrations on %s: %v", derivedName, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancelCleanup()
		if cfg.preReset != nil {
			cfg.preReset(cleanupCtx, derivedDSN)
		}
		if err := migrate.Reset(cleanupCtx, derivedDSN); err != nil {
			t.Logf("cleanup reset on %s failed: %v", derivedName, err)
		}
	})

	poolCtx, cancelPool := context.WithTimeout(context.Background(), connectTimeout)
	defer cancelPool()
	pool, err := db.New(poolCtx, config.DBConfig{DSN: derivedDSN, ConnTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect pool to %s: %v", derivedName, err)
	}
	// Registered after the reset cleanup above, so it runs FIRST (LIFO): the
	// pool is closed before the final reset opens its own connection.
	t.Cleanup(pool.Close)
	return pool
}

// DSN returns the derived DSN for suffix without creating a pool, for the
// few tests that need the connection string itself (e.g. to drive a CLI).
// It performs the same creation and safety checks as NewIsolatedPool but
// does not reset or migrate.
func DSN(t *testing.T, suffix string) string {
	t.Helper()
	baseDSN := os.Getenv(EnvVar)
	if baseDSN == "" {
		t.Skipf("set %s to run the gated %s tests", EnvVar, suffix)
	}
	derivedDSN, derivedName := deriveDSN(t, baseDSN, suffix)
	createDatabase(t, baseDSN, derivedName)
	return derivedDSN
}

// deriveDSN validates the configured DSN and rewrites its database name to
// the per-package derived one, failing the test on any problem. The rules
// live in derive (below) so they are directly unit-testable, including the
// rejection paths — a t.Fatal cannot be asserted on from a parent test,
// since a failing subtest fails its parent too.
func deriveDSN(t *testing.T, baseDSN, suffix string) (dsn, name string) {
	t.Helper()
	dsn, name, err := derive(baseDSN, suffix)
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	return dsn, name
}

// isPackageIdentifier reports whether s is safe to splice into a DSN and
// use as an unquoted Postgres identifier fragment: ASCII letters, digits,
// and underscores only. Deliberately strict — the suffixes are short
// hand-written literals ("tasks", "auth"), so there is no reason to permit
// anything that could carry conninfo or SQL syntax.
func isPackageIdentifier(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
		default:
			return false
		}
	}
	return s != ""
}

// derive is deriveDSN's pure core: it validates baseDSN and suffix and
// returns the per-package DSN and database name, or an error.
//
// The safety rail is enforced on the BASE database name so a misconfigured
// DSN pointing at a real database is rejected before anything is created or
// dropped — previously only 5 of the 15 gated packages checked this at all.
func derive(baseDSN, suffix string) (dsn, name string, err error) {
	// Validated here rather than only in NewIsolatedPool so DSN() cannot
	// bypass it: an empty suffix would derive a single shared "<base>_"
	// database and quietly undo the isolation this package exists for.
	if strings.TrimSpace(suffix) == "" {
		return "", "", errors.New("suffix must be a non-empty package identifier")
	}
	// The suffix is spliced into a DSN, so it must not be able to carry
	// conninfo syntax. Without this, a suffix like "x dbname=production"
	// yields "dbname=nestova_test_x dbname=production" — and since the LAST
	// dbname wins (for libpq and for dbnameValueSpan alike), the connection
	// would target production, defeating the base-name safety rail entirely
	// and pointing migrate.Reset at a real database. Restricting the suffix
	// to a package-identifier character set closes that off, and keeps the
	// derived name a legal unquoted Postgres identifier besides.
	if !isPackageIdentifier(suffix) {
		return "", "", fmt.Errorf("suffix %q must contain only ASCII letters, digits, or underscores", suffix)
	}
	connCfg, parseErr := pgx.ParseConfig(baseDSN)
	if parseErr != nil {
		return "", "", fmt.Errorf("parse %s: %w", EnvVar, parseErr)
	}
	base := strings.ToLower(connCfg.Database)
	if base != "test" && !strings.HasSuffix(base, "_test") {
		return "", "", fmt.Errorf("refusing to use database %q; %s's database name must be %q or end with %q", base, EnvVar, "test", "_test")
	}

	derived := base + "_" + strings.ToLower(suffix)
	if len(derived) > 63 {
		// Postgres truncates identifiers past 63 bytes, which would silently
		// merge two packages' databases back into one.
		return "", "", fmt.Errorf("derived database name %q exceeds Postgres's 63-byte identifier limit; shorten the suffix", derived)
	}

	rewritten, rewriteErr := rewriteDatabase(baseDSN, derived)
	if rewriteErr != nil {
		return "", "", rewriteErr
	}
	return rewritten, derived, nil
}

// rewriteDatabase returns baseDSN with its database name replaced, editing
// the ORIGINAL string rather than re-rendering a parsed config. Rebuilding
// from parsed fields would silently drop options pgx folds into the
// connection (sslrootcert, connect_timeout, application_name, ...) and
// would need to re-escape values like a password containing spaces — both
// of which change how the test connects. Swapping just the name keeps
// every other option exactly as the operator wrote it.
func rewriteDatabase(baseDSN, newName string) (string, error) {
	// URL form: postgres://user:pass@host:port/dbname?opts
	if u, err := url.Parse(baseDSN); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + newName
		return u.String(), nil
	}

	// Key/value (conninfo) form: host=... dbname=... — splice a new value
	// over just the dbname one, leaving every other byte untouched.
	start, end, ok := dbnameValueSpan(baseDSN)
	if !ok {
		return "", fmt.Errorf("cannot derive a database name from %s: no dbname= key and not a postgres:// URL", EnvVar)
	}
	return baseDSN[:start] + newName + baseDSN[end:], nil
}

// dbnameValueSpan locates the effective dbname value inside a libpq
// conninfo string, returning its half-open byte range. It is quote-aware on
// purpose: a naive strings.Fields split would corrupt values containing
// spaces (password='pa  ss' comes back with its whitespace collapsed, and a
// value containing "dbname=" would be mistaken for the key itself). When
// dbname appears more than once — as happens when a DSN is assembled from
// fragments — the LAST occurrence is the one libpq uses, so it is the one
// rewritten.
func dbnameValueSpan(conninfo string) (start, end int, ok bool) {
	i := 0
	for i < len(conninfo) {
		// Skip whitespace between key=value pairs.
		for i < len(conninfo) && isConninfoSpace(conninfo[i]) {
			i++
		}
		if i >= len(conninfo) {
			break
		}
		keyStart := i
		for i < len(conninfo) && conninfo[i] != '=' && !isConninfoSpace(conninfo[i]) {
			i++
		}
		key := conninfo[keyStart:i]
		if i >= len(conninfo) || conninfo[i] != '=' {
			continue // malformed fragment; let pgx report it
		}
		i++ // consume '='
		valStart := i
		if i < len(conninfo) && conninfo[i] == '\'' {
			i++ // opening quote
			for i < len(conninfo) {
				if conninfo[i] == '\\' && i+1 < len(conninfo) {
					i += 2
					continue
				}
				if conninfo[i] == '\'' {
					i++ // closing quote
					break
				}
				i++
			}
		} else {
			for i < len(conninfo) && !isConninfoSpace(conninfo[i]) {
				if conninfo[i] == '\\' && i+1 < len(conninfo) {
					i++
				}
				i++
			}
		}
		if key == "dbname" {
			start, end, ok = valStart, i, true // keep scanning: last wins
		}
	}
	return start, end, ok
}

func isConninfoSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// createDatabase creates the derived database if it does not exist, via the
// "postgres" maintenance database on the same server. A concurrent creation
// by another package's test binary surfaces as 42P04 and is a no-op.
func createDatabase(t *testing.T, baseDSN, derivedName string) {
	t.Helper()
	adminCfg, err := pgx.ParseConfig(baseDSN)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvVar, err)
	}
	adminCfg.Database = "postgres"

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("connect to maintenance database to create %q: %v", derivedName, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// derivedName is built from a validated base name plus a
	// caller-supplied literal, but quote it anyway — CREATE DATABASE takes
	// no parameters, so the identifier is necessarily interpolated.
	_, err = conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{derivedName}.Sanitize()))
	if err == nil {
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == duplicateDatabaseCode {
		return // already exists — the normal case after the first run
	}
	t.Fatalf("create database %q (the test role needs CREATEDB; see docs/testing.md): %v", derivedName, err)
}
