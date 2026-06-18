package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNextVersion(t *testing.T) {
	t.Run("empty dir starts at 1", func(t *testing.T) {
		got, err := nextVersion(t.TempDir())
		if err != nil {
			t.Fatalf("nextVersion() error: %v", err)
		}
		if got != 1 {
			t.Errorf("nextVersion() = %d, want 1", got)
		}
	})

	t.Run("increments past the highest existing number", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"00001_baseline.sql", "00002_auth.sql", "notes.txt"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := nextVersion(dir)
		if err != nil {
			t.Fatalf("nextVersion() error: %v", err)
		}
		if got != 3 {
			t.Errorf("nextVersion() = %d, want 3", got)
		}
	})

	t.Run("missing dir is an error", func(t *testing.T) {
		if _, err := nextVersion(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
			t.Error("nextVersion() = nil error, want error for missing dir")
		}
	})
}

func TestCreateMigration(t *testing.T) {
	t.Run("writes a sequential, slugged goose file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "00001_baseline.sql"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		path, err := createMigration(dir, "Add Widgets!!")
		if err != nil {
			t.Fatalf("createMigration() error: %v", err)
		}
		if want := filepath.Join(dir, "00002_add_widgets.sql"); path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read created file: %v", err)
		}
		if got := string(body); got != "-- +goose Up\n\n-- +goose Down\n" {
			t.Errorf("template = %q, want the goose Up/Down skeleton", got)
		}
	})

	t.Run("rejects a name with no usable characters", func(t *testing.T) {
		if _, err := createMigration(t.TempDir(), "!!!"); err == nil {
			t.Error("createMigration() = nil error, want error for empty slug")
		}
	})
}

func TestRunUnknownCommand(t *testing.T) {
	err := run([]string{"frobnicate"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run() error = %v, want an unknown-command error", err)
	}
}

func TestRunNoArgs(t *testing.T) {
	if err := run(nil); err == nil {
		t.Error("run() = nil error, want usage error for no args")
	}
}

func TestRunRejectsTrailingArgs(t *testing.T) {
	// Validation happens before config.Load, so these need no database.
	cases := [][]string{
		{"reset", "now"},     // trailing arg on a destructive DB command
		{"create"},           // missing name
		{"create", "a", "b"}, // extra arg would be silently dropped
	}
	for _, args := range cases {
		if err := run(args); err == nil {
			t.Errorf("run(%q) = nil error, want usage error", args)
		}
	}
}
