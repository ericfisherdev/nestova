package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

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
		{
			name: "supabase transaction keyword/value dsn routes to the session port",
			conn: setup.Conn{DSN: "host=db.pooler.supabase.com port=6543 dbname=postgres sslmode=require", Provider: "supabase", PoolMode: "transaction"},
			want: "host=db.pooler.supabase.com port=5432 dbname=postgres sslmode=require",
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

// The wizard builds its own config.Config because setup mode never runs
// config.Load. httpserver.New subtracts a 5s margin from RequestTimeout to get
// the per-request context deadline, so a zero (or too small) value leaves every
// wizard request with an already-expired context and an empty response body —
// the NES-164 blank-setup-page regression.
func TestSetupServerConfig_RequestTimeoutLeavesAUsableDeadline(t *testing.T) {
	const httpserverRequestTimeoutMargin = 5 * time.Second

	cfg := setupServerConfig()
	if cfg.Server.RequestTimeout <= httpserverRequestTimeoutMargin {
		t.Fatalf("setupServerConfig().Server.RequestTimeout = %s, want more than the %s httpserver margin",
			cfg.Server.RequestTimeout, httpserverRequestTimeoutMargin)
	}
}

// logRecords decodes the JSON log lines captured in buf.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestResolveSetupToken_GeneratesAndLogsOnce(t *testing.T) {
	t.Setenv(setup.SetupTokenEnv, "")
	if err := os.Unsetenv(setup.SetupTokenEnv); err != nil {
		t.Fatalf("unset %s: %v", setup.SetupTokenEnv, err)
	}
	var buf bytes.Buffer

	token, err := resolveSetupToken(slog.New(slog.NewJSONHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("resolveSetupToken: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("generated token length = %d, want 64 hex chars", len(token))
	}

	records := logRecords(t, &buf)
	if len(records) != 1 {
		t.Fatalf("logged %d records, want exactly 1:\n%s", len(records), buf.String())
	}
	if got := records[0]["setup_token"]; got != token {
		t.Fatalf("logged setup_token = %v, want the returned token", got)
	}
}

func TestResolveSetupToken_EnvWinsAndIsSilent(t *testing.T) {
	t.Setenv(setup.SetupTokenEnv, "operator-pinned")
	var buf bytes.Buffer

	token, err := resolveSetupToken(slog.New(slog.NewJSONHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("resolveSetupToken: %v", err)
	}
	if token != "operator-pinned" {
		t.Fatalf("token = %q, want operator-pinned", token)
	}
	if records := logRecords(t, &buf); len(records) != 0 {
		t.Fatalf("logged %d records, want 0 (a pinned token is never logged):\n%s", len(records), buf.String())
	}
}
