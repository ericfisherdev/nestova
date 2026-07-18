package main

import (
	"fmt"

	mediaapp "github.com/ericfisherdev/nestova/internal/media/app"
)

// printMigrateProgress prints one row's outcome as photoMigrator processes
// it — the "batch progress logging per class (count done/total)" NES-133's
// ticket calls for. It is passed to newPhotoMigrator as the progress
// callback; the migrator itself stays print/logging-free (see photoMigrator's
// doc).
func printMigrateProgress(p migrateProgress) {
	switch p.Outcome {
	case migrateOutcomeMigrated:
		if p.DeletedLocal {
			fmt.Printf("[%s] %d/%d migrated (local file deleted): %s\n", p.Class, p.Done, p.Total, p.Ref)
		} else {
			fmt.Printf("[%s] %d/%d migrated: %s\n", p.Class, p.Done, p.Total, p.Ref)
		}
	case migrateOutcomeAlreadyMigrated:
		fmt.Printf("[%s] %d/%d already migrated (resumed): %s\n", p.Class, p.Done, p.Total, p.Ref)
	case migrateOutcomeHashMismatch:
		fmt.Printf("[%s] %d/%d HASH MISMATCH, row kept on local: %s\n", p.Class, p.Done, p.Total, p.Ref)
	case migrateOutcomeTargetIntegrityFailed:
		fmt.Printf("[%s] %d/%d TARGET INTEGRITY FAILED, row kept on local: %s: %v\n", p.Class, p.Done, p.Total, p.Ref, p.Err)
	case migrateOutcomeError:
		fmt.Printf("[%s] %d/%d ERROR on %s: %v\n", p.Class, p.Done, p.Total, p.Ref, p.Err)
	}
}

// printMigrateSummary prints the final per-class tally after Migrate
// returns.
func printMigrateSummary(result migrateResult) {
	fmt.Println("\nmigrate summary:")
	for _, c := range result.Classes {
		fmt.Printf("  %s: migrated=%d already_done=%d hash_mismatches=%d target_integrity_failures=%d errors=%d deleted_local=%d\n",
			c.Class, c.Migrated, c.AlreadyDone, c.HashMismatches, c.TargetIntegrityFailures, c.Errors, c.DeletedLocal)
	}
}

// printVerifyResult prints every finding verify collected, grouped by
// class and kind.
func printVerifyResult(result verifyResult) {
	fmt.Println("verify: s3 cross-check")
	for _, c := range result.S3 {
		fmt.Printf("  %s: rows_without_object=%d objects_without_row=%d cross_prefix_rows=%d\n",
			c.Class, len(c.RowsWithoutObject), len(c.ObjectsWithoutRow), len(c.CrossPrefixRows))
		for _, ref := range c.RowsWithoutObject {
			fmt.Printf("    DATA LOSS: row references %s, no matching bucket object\n", ref)
		}
		for _, ref := range c.CrossPrefixRows {
			fmt.Printf("    CROSS-PREFIX: row references %s, under the wrong class prefix\n", ref)
		}
		for _, ref := range c.ObjectsWithoutRow {
			fmt.Printf("    reaper candidate: object %s has no referencing row\n", ref)
		}
	}
	fmt.Println("verify: local file check")
	for _, l := range result.Local {
		fmt.Printf("  %s: missing_files=%d\n", l.Class, len(l.MissingFiles))
		for _, ref := range l.MissingFiles {
			fmt.Printf("    DATA LOSS: row references %s, local file is missing\n", ref)
		}
	}
}

// printReapResult prints a destructive reap Run's summary.
func printReapResult(result mediaapp.ReaperResult) {
	fmt.Printf("reap: retention_rows_deleted=%d\n", result.RetentionRowsDeleted)
	for class, n := range result.OrphansDeleted {
		fmt.Printf("  %s: orphans_deleted=%d\n", class, n)
	}
}

// printReapDryRun prints a --dry-run preview: exactly what the next Run
// would delete.
func printReapDryRun(result mediaapp.DryRunResult) {
	fmt.Printf("reap --dry-run: retention_rows_would_delete=%d\n", result.RetentionRowsWouldDelete)
	for class, refs := range result.OrphansWouldDelete {
		fmt.Printf("  %s: orphans_would_delete=%d\n", class, len(refs))
		for _, ref := range refs {
			fmt.Printf("    would delete: %s\n", ref)
		}
	}
}
