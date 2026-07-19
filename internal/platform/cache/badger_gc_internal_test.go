package cache

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

// TestBadgerCache_LogGCOutcome_FiltersTheExpectedNoRewriteCase is a
// white-box unit test for logGCOutcome's own filtering logic, isolated
// from a real GC run: nil and badger.ErrNoRewrite (the routine
// nothing-to-reclaim result on almost every tick) must not be logged;
// any other error must be. Constructing a bare &BadgerCache{logger: ...}
// (no db) is safe here since logGCOutcome never touches db.
func TestBadgerCache_LogGCOutcome_FiltersTheExpectedNoRewriteCase(t *testing.T) {
	var buf bytes.Buffer
	c := &BadgerCache{logger: slog.New(slog.NewTextHandler(&buf, nil))}

	c.logGCOutcome(nil)
	if buf.Len() != 0 {
		t.Errorf("logGCOutcome(nil) logged %q, want nothing", buf.String())
	}

	c.logGCOutcome(badger.ErrNoRewrite)
	if buf.Len() != 0 {
		t.Errorf("logGCOutcome(ErrNoRewrite) logged %q, want nothing", buf.String())
	}

	sentinel := errors.New("disk full")
	c.logGCOutcome(sentinel)
	if !strings.Contains(buf.String(), "disk full") {
		t.Errorf("logGCOutcome(sentinel) logged %q, want it to mention %q", buf.String(), sentinel)
	}
}

// TestBadgerCache_ValueLogGC_BoundsDirectoryGrowth is a soak-style test for
// the value-log-GC AC: write and delete many sizeable entries so the value
// log accumulates reclaimable garbage, force badger to actually observe
// that garbage, run a GC pass, and confirm the on-disk space actually
// consumed shrinks back down from its post-write peak rather than simply
// growing unbounded.
//
// This is a white-box test (package cache, not cache_test) because it
// needs to construct a *badger.DB directly with one extra option
// (WithCompactL0OnClose) that this package's own openBadger does not set
// in production, and it needs the unexported db field to reach it. Facts
// below are confirmed by reading badger v4.9.4's own source directly, not
// assumed:
//
//  1. RunValueLogGC can only reclaim a value log file that is NOT the
//     currently-active one (value.go's pickLog: "if fid < vlog.maxFid").
//     A dataset too small to force badger to rotate past its first value
//     log file therefore has structurally nothing for GC to reclaim, no
//     matter how much of that one file's content has been deleted — this
//     is why the entry/value sizes below are sized well past
//     badgerValueLogFileSize.
//  2. A value log file only becomes GC-eligible once badger's LSM
//     compaction has actually processed the deleted keys' stale value
//     pointers and recorded discard stats for that file (levels.go calls
//     vlog.updateDiscardStats only as a side effect of compaction), and
//     this dataset (500 tiny key+pointer entries) is small enough to sit
//     in a single L0 table that badger's own background compaction never
//     has a reason to touch. WithCompactL0OnClose(true) — an exported
//     badger Option, confirmed via options.go, whose default is false —
//     forces exactly that compaction as part of Close, deterministically,
//     without depending on any of badger's own unexported internals. It
//     is deliberately NOT set in production openBadger: it adds
//     compaction work to every graceful shutdown, which the ticket never
//     asked for and which works against fast shutdown on a Pi. This test
//     opens its own *badger.DB directly (reusing this package's real
//     badgerValueThreshold/badgerValueLogFileSize constants, so it can
//     never silently drift from production tuning) rather than adding a
//     test-only knob to the package's public surface.
//  3. A freshly created (or reopened) .vlog/.mem file is immediately
//     truncated to a LARGE pre-allocated capacity — 2*ValueLogFileSize for
//     a vlog file (memtable.go's logFile.open, called with fsize =
//     2*vlog.opt.ValueLogFileSize) — for mmap sizing, not because that
//     much real data exists yet. On Linux this produces a SPARSE file:
//     its logical size (os.Stat's Size()) reports the full pre-allocated
//     capacity, while its actual disk footprint (the st_blocks the
//     filesystem really allocated) stays small until data is actually
//     written into it. Summing logical size would make simply reopening
//     the store look like it doubled in size even with no new data
//     written — diskUsage below sums real allocated blocks
//     (stat.Blocks*512) instead, which is also the metric that actually
//     matters for the AC ("keeps the directory bounded... under sustained
//     writes" means real disk consumption, not sparse logical size).
func TestBadgerCache_ValueLogGC_BoundsDirectoryGrowth(t *testing.T) {
	dir := t.TempDir()

	opts := badger.DefaultOptions(dir).
		WithValueLogFileSize(badgerValueLogFileSize).
		WithValueThreshold(badgerValueThreshold).
		WithCompactL0OnClose(true).
		WithLoggingLevel(badger.WARNING)
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("badger.Open: %v", err)
	}
	c := &BadgerCache{db: db}

	// entryCount*valueSize (~32MB) is well above badgerValueLogFileSize
	// (16MB), so at least one value log file fills and rotates out,
	// becoming eligible for GC once compacted. Each value is also well
	// above badgerValueThreshold, so every one lands in the value log
	// rather than inline in the LSM tree.
	const (
		entryCount = 500
		valueSize  = 64 << 10 // 64KB
	)
	value := bytes.Repeat([]byte("x"), valueSize)
	ctx := context.Background()
	keys := make([]string, entryCount)
	for i := range entryCount {
		keys[i] = "soak:entry:" + strconv.Itoa(i)
		if err := c.Set(ctx, keys[i], value, time.Hour); err != nil {
			t.Fatalf("Set(%d): %v", i, err)
		}
	}
	for _, key := range keys {
		if err := c.Delete(ctx, key); err != nil {
			t.Fatalf("Delete(%s): %v", key, err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close (CompactL0OnClose forces the deleted keys' tombstones to be processed): %v", err)
	}

	peakUsage, err := diskUsage(dir)
	if err != nil {
		t.Fatalf("diskUsage (peak): %v", err)
	}

	db2, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("badger.Open (reopen): %v", err)
	}
	c2 := &BadgerCache{db: db2}
	defer func() { _ = c2.Close() }()

	_ = c2.RunValueLogGCOnce(0.1)

	finalUsage, err := diskUsage(dir)
	if err != nil {
		t.Fatalf("diskUsage (final): %v", err)
	}
	if finalUsage >= peakUsage {
		t.Errorf("disk usage after GC = %d bytes, want less than the peak %d bytes (value-log GC should reclaim the deleted entries' space)", finalUsage, peakUsage)
	}
}

// diskUsage sums the actual disk blocks allocated to every file under dir
// (stat.Blocks*512, the traditional POSIX block size), not the files'
// logical sizes — see this test's own doc, point 3, for why that
// distinction matters here: badger pre-allocates new .vlog/.mem files as
// sparse files far larger than their real content.
func diskUsage(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			total += info.Size()
			return nil
		}
		total += st.Blocks * 512
		return nil
	})
	return total, err
}
