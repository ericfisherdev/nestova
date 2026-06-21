// Command migrate applies, rolls back, and inspects database migrations using
// the embedded migration set. It reads DATABASE_URL via the standard config
// loader. New migrations are scaffolded with the `create` subcommand.
//
// Usage:
//
//	go run ./cmd/migrate up|down|status|reset
//	go run ./cmd/migrate create <name>   (run from the repo root)
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db/migrate"
)

// sourceMigrationsDir is the on-disk migrations directory, relative to the repo
// root. `create` writes here, so the subcommand must be run from the repo root.
const sourceMigrationsDir = "internal/platform/db/migrate/migrations"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: migrate <up|down|status|reset|create> [name]")
	}
	command := args[0]

	// Validate the command up front so an unknown command reports a helpful
	// error rather than failing later in config.Load. create needs no database.
	switch command {
	case "create":
		if len(args) != 2 {
			return fmt.Errorf("usage: migrate create <name>")
		}
		path, err := createMigration(sourceMigrationsDir, args[1])
		if err != nil {
			return err
		}
		fmt.Println("created", path)
		return nil
	case "up", "down", "status", "reset":
		if len(args) != 1 {
			return fmt.Errorf("usage: migrate %s (no arguments)", command)
		}
		// database-backed commands, handled below
	default:
		return fmt.Errorf("unknown command %q (want up|down|status|reset|create)", command)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Allow an operator to cancel a long-running migration with Ctrl-C / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn, opts := migrateSettings(cfg)

	switch command {
	case "up":
		return migrate.Up(ctx, dsn, opts...)
	case "down":
		return migrate.Down(ctx, dsn, opts...)
	case "status":
		return migrate.Status(ctx, dsn, opts...)
	default: // "reset" (the only remaining validated command)
		return migrate.Reset(ctx, dsn, opts...)
	}
}

// migrateSettings resolves the connection the migration tool should use and any
// connection options. It prefers MIGRATE_DATABASE_URL (cfg.DB.MigrateDSN) and
// falls back to DATABASE_URL. The pooler-safe simple protocol is enabled only
// when migrations would otherwise run through the Supabase transaction pooler —
// i.e. no dedicated migrate DSN was provided and the app DSN targets it; pointing
// MIGRATE_DATABASE_URL at the direct/session connection is the preferred path.
func migrateSettings(cfg config.Config) (string, []migrate.Option) {
	dsn := cfg.DB.MigrateDSN
	if dsn == "" {
		dsn = cfg.DB.DSN
	}

	var opts []migrate.Option
	if cfg.DB.MigrateDSN == "" &&
		cfg.DB.Provider == config.DBProviderSupabase &&
		cfg.DB.PoolMode == config.DBPoolModeTransaction {
		opts = append(opts, migrate.PoolerSafe())
	}
	return dsn, opts
}

var (
	migrationFilePrefix = regexp.MustCompile(`^(\d+)_`)
	nameSanitizer       = regexp.MustCompile(`[^a-z0-9]+`)
)

// createMigration writes a new sequential, empty goose migration into dir and
// returns its path.
func createMigration(dir, name string) (string, error) {
	slug := strings.Trim(nameSanitizer.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if slug == "" {
		return "", fmt.Errorf("migration name %q has no usable characters", name)
	}

	next, err := nextVersion(dir)
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, fmt.Sprintf("%05d_%s.sql", next, slug))
	const template = "-- +goose Up\n\n-- +goose Down\n"
	// O_EXCL: never overwrite an existing migration. Sequential numbering means
	// two migrations created without coordination could target the same number;
	// failing loudly here surfaces that immediately (create migrations serially).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("create migration file: %w", err)
	}
	if _, err := f.WriteString(template); err != nil {
		_ = f.Close()
		_ = os.Remove(path) // best-effort cleanup of the partial file
		return "", fmt.Errorf("write migration: %w", err)
	}
	// Check Close so a flush failure (e.g. disk full) is not silently dropped,
	// which could leave a truncated migration file behind.
	if err := f.Close(); err != nil {
		_ = os.Remove(path) // best-effort cleanup of the partial file
		return "", fmt.Errorf("close migration file: %w", err)
	}
	return path, nil
}

// nextVersion returns one greater than the highest existing migration number in
// dir, or 1 when there are none.
func nextVersion(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read migrations dir (run from the repo root): %w", err)
	}
	versions := make([]int, 0, len(entries))
	for _, e := range entries {
		m := migrationFilePrefix.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		versions = append(versions, n)
	}
	if len(versions) == 0 {
		return 1, nil
	}
	sort.Ints(versions)
	return versions[len(versions)-1] + 1, nil
}
