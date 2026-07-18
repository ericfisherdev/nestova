// Command storage is Nestova's operational tooling for enabling S3 photo
// storage on an existing local-backend install (NES-133): a resumable
// local-to-S3 migrator, a bucket/database consistency checker, and the
// operator-facing surface for the storage reaper (NES-132's ReaperService,
// constructed but never invoked until this ticket). See docs/storage.md for
// the full enable-S3 runbook.
//
// Usage:
//
//	go run ./cmd/storage migrate [--class=album|chore] [--delete-local]
//	go run ./cmd/storage verify
//	go run ./cmd/storage reap [--dry-run] [--grace=720h]
//
// Every subcommand requires MEDIA_STORAGE_BACKEND=s3 (plus S3_BUCKET,
// S3_REGION, and any other S3_* settings) to already be set in the
// environment — see wireMediaStorage's doc for why.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mediaapp "github.com/ericfisherdev/nestova/internal/media/app"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// Exit codes. migrate/reap use exitOK/exitUsageOrConfig only (a migrate row
// finding is reported via exitFindings, mirroring verify — see runMigrate);
// verify additionally uses exitFindings for a data-loss finding, per
// NES-133's documented 0/1/2 contract.
const (
	exitOK            = 0
	exitFindings      = 1
	exitUsageOrConfig = 2
)

// defaultReapGrace is `storage reap`'s --grace default: 30 days, matching
// the ticket's documented operator default.
const defaultReapGrace = 30 * 24 * time.Hour

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "storage:", err)
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) == 0 {
		return exitUsageOrConfig, fmt.Errorf("usage: storage <migrate|verify|reap> [flags]")
	}
	command, rest := args[0], args[1:]
	switch command {
	case "migrate":
		return runMigrate(rest)
	case "verify":
		return runVerify(rest)
	case "reap":
		return runReap(rest)
	default:
		return exitUsageOrConfig, fmt.Errorf("unknown command %q (want migrate|verify|reap)", command)
	}
}

// runMigrate wires up a photoMigrator and runs one migrate pass. Exit code:
// exitFindings when any class hit a hash mismatch or a hard per-row error
// (see migrateResult.HasFindings), exitOK otherwise — a partial/interrupted
// run is expected to be re-run (idempotent; see photoMigrator's doc), not
// treated as a usage error.
func runMigrate(args []string) (int, error) {
	fs := flag.NewFlagSet("storage migrate", flag.ContinueOnError)
	classFlag := fs.String("class", "", "restrict migration to one class: album|chore (default: both)")
	deleteLocal := fs.Bool("delete-local", false, "delete a photo's local file once its move to S3 is verified (also sweeps leftover local files from a prior non-delete run)")
	if err := fs.Parse(args); err != nil {
		return exitUsageOrConfig, err
	}
	// flag.Parse stops at the first non-flag argument and leaves it (and
	// everything after it) in fs.Args() rather than rejecting it — so a
	// typo'd flag (e.g. "class=album" or "delete-local" missing its "--")
	// would otherwise be silently ignored as a stray positional argument
	// instead of failing loudly. Reject any leftover positional argument
	// outright.
	if fs.NArg() > 0 {
		return exitUsageOrConfig, fmt.Errorf("usage: storage migrate [--class=album|chore] [--delete-local] (unexpected argument %q)", fs.Arg(0))
	}
	classes, err := parseClassFlag(*classFlag)
	if err != nil {
		return exitUsageOrConfig, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return exitUsageOrConfig, err
	}
	wiring, err := wireMediaStorage(ctx, cfg)
	if err != nil {
		return exitUsageOrConfig, err
	}
	defer wiring.pool.Close()

	migrator, err := newPhotoMigrator(
		wiring.localStore, wiring.targetStore, wiring.targetBackend,
		wiring.photos, wiring.choreProofPhotos, cfg.Media.MaxUploadBytes,
		printMigrateProgress,
	)
	if err != nil {
		return exitUsageOrConfig, err
	}

	result, err := migrator.Migrate(ctx, migrateOptions{Classes: classes, DeleteLocal: *deleteLocal})
	if err != nil {
		return exitUsageOrConfig, err
	}
	printMigrateSummary(result)
	if result.HasFindings() {
		return exitFindings, nil
	}
	return exitOK, nil
}

// runVerify wires up a verifier and runs one consistency check. Exit code:
// exitFindings when any data-loss finding was reported (verifyResult.
// HasDataLoss), exitOK when clean.
func runVerify(args []string) (int, error) {
	fs := flag.NewFlagSet("storage verify", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsageOrConfig, err
	}
	if fs.NArg() > 0 {
		return exitUsageOrConfig, fmt.Errorf("usage: storage verify (no arguments) (unexpected argument %q)", fs.Arg(0))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return exitUsageOrConfig, err
	}
	wiring, err := wireMediaStorage(ctx, cfg)
	if err != nil {
		return exitUsageOrConfig, err
	}
	defer wiring.pool.Close()

	v, err := newVerifier(wiring.localStore, wiring.targetStore, wiring.photos, wiring.choreProofPhotos)
	if err != nil {
		return exitUsageOrConfig, err
	}

	result, err := v.Verify(ctx)
	if err != nil {
		return exitUsageOrConfig, err
	}
	printVerifyResult(result)
	if result.HasDataLoss() {
		return exitFindings, nil
	}
	return exitOK, nil
}

// runReap wires up NES-132's ReaperService and either previews (--dry-run)
// or runs one reap pass. reap has no exitFindings case of its own: a
// successful destructive Run or a successful preview both exit exitOK — the
// operator reads the printed summary, there is no separate "finding" state
// beyond what Run/DryRun themselves report.
func runReap(args []string) (int, error) {
	fs := flag.NewFlagSet("storage reap", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "list what would be deleted without deleting")
	grace := fs.Duration("grace", defaultReapGrace, "how old an unreferenced object must be before it is eligible for deletion")
	if err := fs.Parse(args); err != nil {
		return exitUsageOrConfig, err
	}
	// CRITICAL: flag.Parse does NOT reject a stray positional argument — it
	// just stops parsing and leaves it in fs.Args(). Without this check,
	// `storage reap dry-run` (a typo for `--dry-run`) would silently run
	// the DESTRUCTIVE Run path instead of the preview, since *dryRun stays
	// at its false default. Reject it outright.
	if fs.NArg() > 0 {
		return exitUsageOrConfig, fmt.Errorf("usage: storage reap [--dry-run] [--grace=DURATION] (unexpected argument %q)", fs.Arg(0))
	}
	if *grace <= 0 {
		return exitUsageOrConfig, fmt.Errorf("--grace must be positive, got %v", *grace)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return exitUsageOrConfig, err
	}
	wiring, err := wireMediaStorage(ctx, cfg)
	if err != nil {
		return exitUsageOrConfig, err
	}
	defer wiring.pool.Close()

	lister, ok := wiring.targetStore.(mediadomain.ObjectLister)
	if !ok {
		return exitUsageOrConfig, fmt.Errorf("storage: target photo store does not support ObjectLister")
	}
	reaper, err := mediaapp.NewReaperService(
		lister, wiring.targetStore, wiring.targetBackend,
		wiring.photos, wiring.choreProofPhotos,
		*grace, cfg.Media.ChoreProofRetention,
	)
	if err != nil {
		return exitUsageOrConfig, err
	}

	now := time.Now().UTC()
	if *dryRun {
		result, err := reaper.DryRun(ctx, now)
		if err != nil {
			return exitUsageOrConfig, err
		}
		printReapDryRun(result)
		return exitOK, nil
	}
	result, err := reaper.Run(ctx, now)
	if err != nil {
		return exitUsageOrConfig, err
	}
	printReapResult(result)
	return exitOK, nil
}

// parseClassFlag parses --class's value into the classes migrate/verify
// should restrict themselves to: nil means "every class" (the flag's unset
// default).
func parseClassFlag(s string) ([]mediadomain.PhotoClass, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return nil, nil
	case "album":
		return []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}, nil
	case "chore":
		return []mediadomain.PhotoClass{mediadomain.PhotoClassChoreProof}, nil
	default:
		return nil, fmt.Errorf("--class must be album|chore, got %q", s)
	}
}
