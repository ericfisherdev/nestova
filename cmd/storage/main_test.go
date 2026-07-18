package main

import (
	"strings"
	"testing"

	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

func TestParseClassFlag(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []mediadomain.PhotoClass
		wantErr bool
	}{
		{"unset means every class", "", nil, false},
		{"album", "album", []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}, false},
		{"chore", "chore", []mediadomain.PhotoClass{mediadomain.PhotoClassChoreProof}, false},
		{"uppercase is normalized", "ALBUM", []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}, false},
		{"whitespace is trimmed", "  chore  ", []mediadomain.PhotoClass{mediadomain.PhotoClassChoreProof}, false},
		{"unknown value is rejected", "reward", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClassFlag(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseClassFlag(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseClassFlag(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseClassFlag(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseClassFlag(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestExitCodeConstantsAreDistinct(t *testing.T) {
	if exitOK == exitFindings || exitOK == exitUsageOrConfig || exitFindings == exitUsageOrConfig {
		t.Fatalf("exit codes must be pairwise distinct: OK=%d Findings=%d UsageOrConfig=%d", exitOK, exitFindings, exitUsageOrConfig)
	}
}

// TestRunRejectsTrailingPositionalArgs is a regression test for a CRITICAL
// review finding: flag.Parse does NOT reject a stray positional argument —
// it just stops parsing and leaves it (and everything after it) in
// fs.Args(). Without an explicit fs.NArg() check in every subcommand,
// `storage reap dry-run` (a typo for `--dry-run`, missing its leading
// dashes) would silently leave *dryRun at its false default and run the
// DESTRUCTIVE Run path instead of the preview. Every subcommand must
// reject any leftover positional argument outright, BEFORE ever touching
// config.Load or wireMediaStorage — this test asserts on the error message
// itself (not just the exit code) so it fails loudly if any subcommand's
// rejection is ever accidentally reordered to run after some other check.
func TestRunRejectsTrailingPositionalArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"migrate: stray positional", []string{"migrate", "extra"}},
		{"migrate: --class as a stray positional (missing --)", []string{"migrate", "class=album"}},
		{"verify: stray positional", []string{"verify", "extra"}},
		// The critical case: a typo'd --dry-run, missing its leading
		// dashes, must NEVER be silently treated as "no flags given" (dry
		// run defaults false) and fall through to the destructive Run path.
		{"reap: dry-run typo must not silently run destructively", []string{"reap", "dry-run"}},
		{"reap: grace typo (missing --)", []string{"reap", "grace=1h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := run(tc.args)
			if code != exitUsageOrConfig {
				t.Fatalf("run(%v) code = %d, want exitUsageOrConfig (%d)", tc.args, code, exitUsageOrConfig)
			}
			if err == nil {
				t.Fatalf("run(%v) err = nil, want a usage error naming the unexpected argument", tc.args)
			}
			if !strings.Contains(err.Error(), "unexpected argument") {
				t.Fatalf("run(%v) err = %q, want it to mention the unexpected positional argument (proving rejection happened at flag parsing, not some later, unrelated failure)", tc.args, err)
			}
		})
	}
}

// TestRunAcceptsProperFlagsPastPositionalCheck is
// TestRunRejectsTrailingPositionalArgs' positive control: a PROPERLY
// dashed flag (e.g. the real `--dry-run`, not the `dry-run` typo above)
// must NOT be rejected as a stray positional argument. It necessarily
// fails further along (no DATABASE_URL/MEDIA_STORAGE_BACKEND configured in
// this hermetic test), but that failure must be a config error, never the
// "unexpected argument" usage error — proving the positional-argument
// check itself isn't so strict it also rejects legitimate flags.
func TestRunAcceptsProperFlagsPastPositionalCheck(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MEDIA_STORAGE_BACKEND", "")

	cases := [][]string{
		{"reap", "--dry-run"},
		{"reap", "--grace=1h"},
		{"migrate", "--class=album"},
		{"migrate", "--delete-local"},
		{"verify"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := run(args)
			if err != nil && strings.Contains(err.Error(), "unexpected argument") {
				t.Fatalf("run(%v) was rejected as a stray positional argument: %v", args, err)
			}
		})
	}
}
