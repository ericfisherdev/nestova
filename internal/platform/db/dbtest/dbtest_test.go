package dbtest

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

// The derivation and safety-rail logic is what stands between a typo'd DSN
// and a dropped real database, so it is unit-tested directly rather than
// only exercised through the gated packages that call NewIsolatedPool.

func TestDeriveDSN_URLForm_SwapsOnlyTheDatabaseName(t *testing.T) {
	dsn, name := deriveDSN(t, "postgres://u:p@localhost:5432/nestova_test?sslmode=disable", "tasks")

	if name != "nestova_test_tasks" {
		t.Errorf("derived name = %q, want %q", name, "nestova_test_tasks")
	}
	if want := "postgres://u:p@localhost:5432/nestova_test_tasks?sslmode=disable"; dsn != want {
		t.Errorf("derived DSN = %q, want %q", dsn, want)
	}
}

func TestDeriveDSN_KeyValueForm_SwapsOnlyTheDatabaseName(t *testing.T) {
	base := "host=localhost port=5432 user=u password=p dbname=nestova_test sslmode=require"
	dsn, name := deriveDSN(t, base, "auth")

	if name != "nestova_test_auth" {
		t.Errorf("derived name = %q, want %q", name, "nestova_test_auth")
	}
	if want := "host=localhost port=5432 user=u password=p dbname=nestova_test_auth sslmode=require"; dsn != want {
		t.Errorf("derived DSN = %q, want %q", dsn, want)
	}
}

// TestDeriveDSN_PreservesNonStandardOptions is the regression guard for the
// rewrite strategy: re-rendering the DSN from a parsed pgx config drops
// options pgx folds into the connection, which would silently change how
// gated tests connect (or stop them connecting at all).
// (sslrootcert is deliberately not exercised here: pgx.ParseConfig reads
// the referenced CA file eagerly, so covering it would mean shipping a real
// certificate fixture. The rewrite is string-level and option-agnostic —
// these three prove the property.)
func TestDeriveDSN_PreservesNonStandardOptions(t *testing.T) {
	base := "postgres://u:p@localhost:5432/nestova_test?sslmode=require&connect_timeout=7&application_name=nestova"
	dsn, _ := deriveDSN(t, base, "media")

	for _, want := range []string{
		"sslmode=require",
		"connect_timeout=7",
		"application_name=nestova",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("derived DSN dropped %q: %q", want, dsn)
		}
	}
	if !strings.Contains(dsn, "/nestova_test_media?") {
		t.Errorf("derived DSN missing the derived database name: %q", dsn)
	}
}

// TestDeriveDSN_PreservesEscapedPassword covers a password that must stay
// percent-encoded; re-escaping it by hand is exactly what the rewrite
// strategy avoids having to get right.
func TestDeriveDSN_PreservesEscapedPassword(t *testing.T) {
	base := "postgres://u:pa%20ss%40word@localhost:5432/nestova_test"
	dsn, _ := deriveDSN(t, base, "kiosk")

	if !strings.Contains(dsn, "pa%20ss%40word") {
		t.Errorf("derived DSN mangled the escaped password: %q", dsn)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("derived DSN is not parseable: %v", err)
	}
	pw, _ := parsed.User.Password()
	if pw != "pa ss@word" {
		t.Errorf("password decoded to %q, want %q", pw, "pa ss@word")
	}
}

func TestDeriveDSN_AcceptsBareTestDatabaseName(t *testing.T) {
	dsn, name := deriveDSN(t, "postgres://u:p@localhost:5432/test?sslmode=disable", "authx")
	if name != "test_authx" {
		t.Errorf("derived name = %q, want %q", name, "test_authx")
	}
	if want := "postgres://u:p@localhost:5432/test_authx?sslmode=disable"; dsn != want {
		t.Errorf("derived DSN = %q, want %q", dsn, want)
	}
}

func TestDeriveDSN_LowercasesSuffix(t *testing.T) {
	// The derived name becomes a real identifier; a mixed-case suffix would
	// otherwise produce a database name that only matches when quoted.
	dsn, name := deriveDSN(t, "postgres://u:p@localhost:5432/nestova_test", "Tasks")
	if name != "nestova_test_tasks" {
		t.Errorf("derived name = %q, want it lowercased", name)
	}
	if want := "postgres://u:p@localhost:5432/nestova_test_tasks"; dsn != want {
		t.Errorf("derived DSN = %q, want %q", dsn, want)
	}
}

// TestNewIsolatedPool_SkipsWithoutEnvVar documents the property that keeps
// `make test` hermetic: no configured DSN means the gated test skips rather
// than fails.
func TestNewIsolatedPool_SkipsWithoutEnvVar(t *testing.T) {
	t.Setenv(EnvVar, "")

	if os.Getenv(EnvVar) != "" {
		t.Fatal("precondition: env var should be empty")
	}
	// NewIsolatedPool calls t.Skip, which aborts this goroutine — so run it
	// in a subtest and assert the subtest was skipped rather than failed.
	result := t.Run("gated", func(sub *testing.T) {
		NewIsolatedPool(sub, "example")
		sub.Error("expected the helper to skip before reaching here")
	})
	if !result {
		t.Error("subtest failed; the helper should have skipped cleanly")
	}
}

// TestDeriveDSN_KeyValueForm_PreservesQuotedValues guards the conninfo
// scanner: a naive whitespace split would collapse the spaces inside these
// quoted values, silently changing the password the tests connect with.
func TestDeriveDSN_KeyValueForm_PreservesQuotedValues(t *testing.T) {
	base := "host=localhost password='pa  ss' application_name='nestova tests' dbname=nestova_test"
	dsn, _ := deriveDSN(t, base, "tasks")

	want := "host=localhost password='pa  ss' application_name='nestova tests' dbname=nestova_test_tasks"
	if dsn != want {
		t.Errorf("derived DSN = %q, want %q", dsn, want)
	}
}

// TestDeriveDSN_KeyValueForm_QuotedDatabaseName covers a dbname that is
// itself quoted: the whole quoted value is replaced, not just its interior.
func TestDeriveDSN_KeyValueForm_QuotedDatabaseName(t *testing.T) {
	dsn, name := deriveDSN(t, "host=localhost dbname='nestova_test' user=u", "auth")

	if name != "nestova_test_auth" {
		t.Errorf("derived name = %q, want %q", name, "nestova_test_auth")
	}
	if want := "host=localhost dbname=nestova_test_auth user=u"; dsn != want {
		t.Errorf("derived DSN = %q, want %q", dsn, want)
	}
}

// TestDeriveDSN_KeyValueForm_IgnoresDbnameInsideAnotherValue confirms the
// scanner matches the dbname KEY, not the text "dbname=" appearing inside
// some other option's value.
func TestDeriveDSN_KeyValueForm_IgnoresDbnameInsideAnotherValue(t *testing.T) {
	base := "application_name='dbname=decoy' dbname=nestova_test"
	dsn, _ := deriveDSN(t, base, "kiosk")

	if want := "application_name='dbname=decoy' dbname=nestova_test_kiosk"; dsn != want {
		t.Errorf("derived DSN = %q, want %q", dsn, want)
	}
}

// TestDeriveDSN_KeyValueForm_LastDbnameWins matches libpq's precedence: a
// conninfo assembled from fragments can repeat a key, and the last one is
// the one that takes effect — so it is the one that must be rewritten.
func TestDeriveDSN_KeyValueForm_LastDbnameWins(t *testing.T) {
	base := "dbname=ignored_test host=localhost dbname=nestova_test"
	dsn, name := deriveDSN(t, base, "tasks")

	if name != "nestova_test_tasks" {
		t.Errorf("derived name = %q, want %q", name, "nestova_test_tasks")
	}
	if want := "dbname=ignored_test host=localhost dbname=nestova_test_tasks"; dsn != want {
		t.Errorf("derived DSN = %q, want %q", dsn, want)
	}
}

// TestDerive_RejectsUnsafeInput covers the rejection paths — the safety
// rail itself. Every other test here feeds an input that should be
// accepted; these prove derive refuses anything that could put a real
// database in front of migrate.Reset, or silently collapse two packages
// onto one database.
func TestDerive_RejectsUnsafeInput(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		suffix  string
		wantErr string
	}{
		{
			name:    "database is not a test database",
			dsn:     "postgres://u:p@localhost:5432/production?sslmode=disable",
			suffix:  "tasks",
			wantErr: "refusing to use database",
		},
		{
			// "testing" neither equals "test" nor ends in "_test": a
			// prefix/substring match here would be a real hole.
			name:    "database name merely starts with test",
			dsn:     "postgres://u:p@localhost:5432/testing?sslmode=disable",
			suffix:  "tasks",
			wantErr: "refusing to use database",
		},
		{
			name:    "empty suffix would collapse packages onto one database",
			dsn:     "postgres://u:p@localhost:5432/nestova_test?sslmode=disable",
			suffix:  "   ",
			wantErr: "non-empty package identifier",
		},
		{
			// The critical case: an unquoted conninfo DSN would become
			// "dbname=nestova_test_x dbname=production", and since the LAST
			// dbname wins, migrate.Reset would target production despite the
			// base DSN passing the name check.
			name:    "suffix smuggles a second dbname into the conninfo",
			dsn:     "host=localhost user=u dbname=nestova_test",
			suffix:  "x dbname=production",
			wantErr: "ASCII letters, digits, or underscores",
		},
		{
			name:    "suffix carries a URL query delimiter",
			dsn:     "postgres://u:p@localhost:5432/nestova_test",
			suffix:  "x?sslmode=disable",
			wantErr: "ASCII letters, digits, or underscores",
		},
		{
			name:    "suffix carries a quote",
			dsn:     "host=localhost dbname=nestova_test",
			suffix:  "x'y",
			wantErr: "ASCII letters, digits, or underscores",
		},
		{
			name:    "suffix pushes the name past Postgres's identifier limit",
			dsn:     "postgres://u:p@localhost:5432/nestova_test?sslmode=disable",
			suffix:  strings.Repeat("x", 60),
			wantErr: "63-byte identifier limit",
		},
		{
			name:    "DSN has no database at all",
			dsn:     "host=localhost user=u",
			suffix:  "tasks",
			wantErr: "refusing to use database",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dsn, name, err := derive(tc.dsn, tc.suffix)
			if err == nil {
				t.Fatalf("derive(%q, %q) = (%q, %q), want an error", tc.dsn, tc.suffix, dsn, name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			if dsn != "" || name != "" {
				t.Errorf("derive returned (%q, %q) alongside its error; both must be empty", dsn, name)
			}
		})
	}
}
