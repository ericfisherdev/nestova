package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

// Pi-tuned Options overrides (NES-140): badger's own defaults target a
// server with gigabytes of headroom (MemTableSize 64MB x NumMemtables 5 =
// up to 320MB just for memtables, plus a 256MB block cache) — this
// deployment's target is a Raspberry Pi running the WHOLE application,
// not badger alone, so every one of these is reduced well below default
// to keep BadgerCache's own resident footprint under ~100MB. The
// NumLevelZeroTables/Stall pair keeps badger's own ~1:2.5 ratio between
// them (matching the 5/15 default) rather than picking two arbitrary
// smaller numbers.
const (
	// badgerMemTableSize is the per-memtable size cap — 8MB, down from
	// the 64MB default.
	badgerMemTableSize = 8 << 20
	// badgerNumMemtables caps how many memtables badger keeps in memory
	// before stalling writes — 2 (the minimum that still lets one
	// memtable flush while another accepts writes), down from 5.
	badgerNumMemtables = 2
	// badgerNumLevelZeroTables/Stall mirror the memtable reduction at the
	// L0 SSTable level.
	badgerNumLevelZeroTables      = 2
	badgerNumLevelZeroTablesStall = 5
	// badgerBaseTableSize is left at badger's own 2MB default — already
	// small relative to the other defaults reduced here.
	badgerBaseTableSize = 2 << 20
	// badgerValueLogFileSize is 16MB, down from badger's 1GB default:
	// this cache's values are small JSON blobs (an external API
	// response), not bulk data, so gigabyte-sized value log segments
	// would sit mostly empty and cost real disk on a Pi's data volume for
	// no benefit. Must stay within badger's own documented valid range
	// [1MB, 2GB).
	badgerValueLogFileSize = 16 << 20
	// badgerNumCompactors is 2, down from the 4 default (badger reserves
	// one compactor exclusively for L0/L1, so 2 is the minimum that still
	// leaves one compactor free for higher levels).
	badgerNumCompactors = 2
	// badgerBlockCacheSize/IndexCacheSize bound badger's own read caches —
	// 16MB and 8MB respectively, down from 256MB and "unbounded" (0)
	// defaults. This cache serves a small, mostly-cold dataset (external
	// API responses, re-fetched at most daily by NES-140's first
	// consumer), so a large read cache buys little.
	badgerBlockCacheSize = 16 << 20
	badgerIndexCacheSize = 8 << 20
	// badgerValueThreshold is 1KB, down from badger's 1MB default. Below
	// this size, badger stores a value INLINE in the LSM tree rather than
	// in the separate value log — this cache's values (external API
	// responses, a few hundred bytes to a few KB of JSON) would otherwise
	// never touch the value log at all under the 1MB default, making
	// RunValueLogGC/RunGC dead code for this cache's actual workload.
	badgerValueThreshold = 1 << 10
)

// BadgerCache is the persistent, on-disk Cache implementation (NES-140),
// backed by github.com/dgraph-io/badger/v4. It is the ONLY file in this
// codebase allowed to import that package — every other consumer depends
// on the Cache port instead, enforced by a depguard rule in
// .golangci.yml scoped to this package. This keeps the dependency, and
// the corruption-recovery/GC operational concerns that come with an
// embedded LSM-tree store, fully contained here: nothing outside this
// file needs to know badger exists.
type BadgerCache struct {
	db     *badger.DB
	logger *slog.Logger
}

// Compile-time assurance the cache satisfies the port.
var _ Cache = (*BadgerCache)(nil)

// NewBadgerCache opens (or creates) a BadgerCache at dir.
//
// Corruption recovery: badger.Open failing is treated as a POTENTIALLY
// RECOVERABLE condition (e.g. an unclean shutdown after a Pi power loss,
// corrupting the value log or manifest), not a hard failure. It is
// logged loudly (Error level, with dir) and the ENTIRE directory is
// removed and Open retried exactly once. Losing the cache directory is,
// by this package's own design (see the package doc), always a
// non-event — every consumer recomputes or re-fetches on a miss — so
// discarding a corrupt cache is safe, and strictly better than either
// crashing boot over a cache or leaving a corrupt store the process can
// never open. If the retry ALSO fails, NewBadgerCache returns the error:
// the caller (cmd/server) is expected to fall back to MemoryCache rather
// than fail startup entirely — see that type's own doc.
func NewBadgerCache(dir string, logger *slog.Logger) (*BadgerCache, error) {
	if logger == nil {
		return nil, errors.New("cache: NewBadgerCache requires a non-nil logger")
	}

	db, err := openBadger(dir)
	if err != nil {
		logger.Error("cache: badger open failed, removing directory and retrying once",
			"dir", dir,
			"error", err,
		)
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return nil, fmt.Errorf("cache: remove corrupt badger directory %s: %w", dir, rmErr)
		}
		db, err = openBadger(dir)
		if err != nil {
			return nil, fmt.Errorf("cache: badger open failed even after removing %s: %w", dir, err)
		}
		logger.Info("cache: badger directory recreated after corruption recovery", "dir", dir)
	}
	return &BadgerCache{db: db, logger: logger}, nil
}

// openBadger opens dir with the Pi-tuned Options above.
func openBadger(dir string) (*badger.DB, error) {
	opts := badger.DefaultOptions(dir).
		WithMemTableSize(badgerMemTableSize).
		WithNumMemtables(badgerNumMemtables).
		WithNumLevelZeroTables(badgerNumLevelZeroTables).
		WithNumLevelZeroTablesStall(badgerNumLevelZeroTablesStall).
		WithBaseTableSize(badgerBaseTableSize).
		WithValueLogFileSize(badgerValueLogFileSize).
		WithNumCompactors(badgerNumCompactors).
		WithBlockCacheSize(badgerBlockCacheSize).
		WithIndexCacheSize(badgerIndexCacheSize).
		WithValueThreshold(badgerValueThreshold).
		WithLoggingLevel(badger.WARNING)
	return badger.Open(opts)
}

// Close closes the underlying badger database, flushing any pending
// writes. Safe to call once during shutdown; badger itself tolerates a
// repeated Close as a no-op.
func (c *BadgerCache) Close() error {
	if err := c.db.Close(); err != nil {
		return fmt.Errorf("cache: close: %w", err)
	}
	return nil
}

// badgerGCDiscardRatio is the discardRatio RunGC passes to
// RunValueLogGCOnce on every tick — badger's own quickstart example value
// (see that method's own doc).
const badgerGCDiscardRatio = 0.7

// badgerMinTTL is the smallest positive ttl BadgerCache.Set honors as
// "expires later than now." badger's own Entry.WithTTL truncates
// ExpiresAt to whole Unix seconds (confirmed by reading badger's
// structs.go source: ExpiresAt = uint64(time.Now().Add(dur).Unix())), so
// a positive ttl under one second can compute an ExpiresAt equal to (not
// after) the current second — making the entry appear already-expired
// the instant it is set. Set rounds any positive ttl below this up to
// exactly one second rather than let that surprise callers; see the
// Cache port's own doc for the resulting cross-backend precision
// difference.
const badgerMinTTL = time.Second

// RunGC runs a value-log GC pass (RunValueLogGCOnce) on a ticker until
// ctx is cancelled. interval controls how often a pass is attempted.
// Intended to run in its own goroutine for the process lifetime,
// following the same signal-cancelled-ctx shutdown pattern cmd/server's
// other background workers use (see that composition root's own doc).
func (c *BadgerCache) RunGC(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.logGCOutcome(c.RunValueLogGCOnce(badgerGCDiscardRatio))
		}
	}
}

// logGCOutcome reports the result of a single RunValueLogGCOnce call.
// badger's own ErrNoRewrite — "nothing left to reclaim this pass" — is the
// expected steady-state result on almost every tick and is not logged;
// anything else (I/O errors, corruption, disk-full, ErrRejected after a
// concurrent Close, ...) is a real problem RunGC's background loop must
// not swallow silently.
func (c *BadgerCache) logGCOutcome(err error) {
	if err == nil || errors.Is(err, badger.ErrNoRewrite) {
		return
	}
	c.logger.Error("cache: value log GC failed", "error", err)
}

// RunValueLogGCOnce runs badger's value log garbage collection
// (DB.RunValueLogGC) repeatedly at discardRatio until nothing more
// qualifies for reclaim, following badger's own documented
// retry-while-nil pattern: RunValueLogGC returning nil means a value log
// file WAS rewritten and reclaimed, so it is called again immediately in
// case another file also qualifies. Returns badger's own final stopping
// error (typically ErrNoRewrite) — callers that only care whether a GC
// pass ran, not the specific reason it stopped, can safely ignore it (see
// RunGC's own use). Exposed as its own method, not only reachable through
// RunGC's ticker loop, so a caller — a future admin action, or a test —
// can trigger a GC pass on demand without waiting out a real interval.
func (c *BadgerCache) RunValueLogGCOnce(discardRatio float64) error {
	for {
		if err := c.db.RunValueLogGC(discardRatio); err != nil {
			return err
		}
	}
}

// Get returns a copy of the value stored under key, or ok=false when the
// key does not exist or has expired (badger enforces TTL expiry itself —
// an expired key behaves exactly like an absent one to Get, per badger's
// own contract for a WithTTL entry).
func (c *BadgerCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	var value []byte
	err := c.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(key))
		if err != nil {
			return err
		}
		value, err = item.ValueCopy(nil)
		return err
	})
	switch {
	case err == nil:
		return value, true, nil
	case errors.Is(err, badger.ErrKeyNotFound):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("cache: get: %w", err)
	}
}

// Set stores value under key. A positive ttl is applied via badger's own
// NewEntry(...).WithTTL — badger expires and reclaims the entry itself;
// a zero ttl stores the entry with no expiration. A negative ttl returns
// ErrNegativeTTL. A positive ttl under badgerMinTTL is rounded up to it
// (see that constant's own doc) rather than honored at sub-second
// precision.
func (c *BadgerCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		return ErrNegativeTTL
	}
	entry := badger.NewEntry([]byte(key), value)
	if ttl > 0 {
		if ttl < badgerMinTTL {
			ttl = badgerMinTTL
		}
		entry = entry.WithTTL(ttl)
	}
	if err := c.db.Update(func(txn *badger.Txn) error {
		return txn.SetEntry(entry)
	}); err != nil {
		return fmt.Errorf("cache: set: %w", err)
	}
	return nil
}

// Delete removes key. It is not an error for key to already be absent —
// badger's own Txn.Delete has the identical contract.
func (c *BadgerCache) Delete(_ context.Context, key string) error {
	if err := c.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(key))
	}); err != nil {
		return fmt.Errorf("cache: delete: %w", err)
	}
	return nil
}
