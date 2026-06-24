package main

import (
	"log/slog"
	"strings"
	"testing"
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
