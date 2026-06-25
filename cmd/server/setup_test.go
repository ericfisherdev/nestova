package main

import (
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/setup"
)

// TestRun_LoadStateError exercises run()'s early error path without binding a
// socket or touching a database: pointing the state file at a directory makes
// LoadState's ReadFile fail, so run returns outcomeShutdown and a wrapped error.
func TestRun_LoadStateError(t *testing.T) {
	t.Setenv("NESTOVA_STATE_FILE", t.TempDir())

	result, err := run(slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected an error when the state file is unreadable")
	}
	if result != outcomeShutdown {
		t.Fatalf("outcome = %v, want outcomeShutdown", result)
	}
	if !strings.Contains(err.Error(), "load setup state") {
		t.Fatalf("error = %q, want it to mention load setup state", err)
	}
}

// TestOutcomeConstants guards the two outcomes against an accidental merge that
// would make main()'s restart decision a no-op.
func TestOutcomeConstants(t *testing.T) {
	if outcomeShutdown == outcomeRestart {
		t.Fatal("outcomeShutdown and outcomeRestart must be distinct")
	}
}

func TestMigrationDSN(t *testing.T) {
	cases := []struct {
		name string
		conn setup.Conn
		want string
	}{
		{
			name: "postgres unchanged",
			conn: setup.Conn{DSN: "postgres://u@h:5432/db?sslmode=disable"},
			want: "postgres://u@h:5432/db?sslmode=disable",
		},
		{
			name: "supabase session unchanged",
			conn: setup.Conn{DSN: "postgres://u@h:5432/db?sslmode=require", Provider: "supabase", PoolMode: "session"},
			want: "postgres://u@h:5432/db?sslmode=require",
		},
		{
			name: "supabase transaction routes migrations to the session port",
			conn: setup.Conn{DSN: "postgres://u@db.pooler.supabase.com:6543/postgres?sslmode=require", Provider: "supabase", PoolMode: "transaction"},
			want: "postgres://u@db.pooler.supabase.com:5432/postgres?sslmode=require",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := migrationDSN(tc.conn)
			if err != nil {
				t.Fatalf("migrationDSN: %v", err)
			}
			if got != tc.want {
				t.Fatalf("migrationDSN = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMigrationDSN_AppliesSSLRootCertAndForcesVerifyFull(t *testing.T) {
	conn := setup.Conn{
		DSN:         "postgres://u@db.pooler.supabase.com:6543/postgres?sslmode=require",
		Provider:    "supabase",
		PoolMode:    "transaction",
		SSLRootCert: "/etc/ssl/ca.crt",
	}
	got, err := migrationDSN(conn)
	if err != nil {
		t.Fatalf("migrationDSN: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Port() != "5432" {
		t.Fatalf("migrations must target the session port 5432, got %q (%s)", u.Port(), got)
	}
	if u.Query().Get("sslrootcert") != "/etc/ssl/ca.crt" || u.Query().Get("sslmode") != "verify-full" {
		t.Fatalf("SSL settings not applied: %s", got)
	}
}
