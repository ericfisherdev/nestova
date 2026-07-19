package cache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/cache"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestBadgerCache(t *testing.T) *cache.BadgerCache {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.NewBadgerCache(dir, discardLogger())
	if err != nil {
		t.Fatalf("NewBadgerCache: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

func TestBadgerCache_GetMiss_ReturnsOkFalse(t *testing.T) {
	c := newTestBadgerCache(t)
	value, ok, err := c.Get(context.Background(), "notify:missing:1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("Get(missing key) ok = true, want false")
	}
	if value != nil {
		t.Errorf("Get(missing key) value = %v, want nil", value)
	}
}

func TestBadgerCache_SetThenGet_RoundTrips(t *testing.T) {
	c := newTestBadgerCache(t)
	want := []byte(`{"hello":"world"}`)
	if err := c.Set(context.Background(), "recipes:externalfind:abc", want, time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(context.Background(), "recipes:externalfind:abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get value = %q, want %q", got, want)
	}
}

func TestBadgerCache_RestartPreservesData(t *testing.T) {
	// "Restarting the app preserves cached external responses" (NES-140
	// AC): open, write, close, reopen at the SAME directory, confirm the
	// value survives.
	dir := t.TempDir()
	c1, err := cache.NewBadgerCache(dir, discardLogger())
	if err != nil {
		t.Fatalf("NewBadgerCache (first open): %v", err)
	}
	if err := c1.Set(context.Background(), "k", []byte("v"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c2, err := cache.NewBadgerCache(dir, discardLogger())
	if err != nil {
		t.Fatalf("NewBadgerCache (reopen): %v", err)
	}
	defer func() { _ = c2.Close() }()

	got, ok, err := c2.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if !ok {
		t.Fatal("Get after restart ok = false, want true (data must survive a restart)")
	}
	if string(got) != "v" {
		t.Errorf("Get after restart value = %q, want %q", got, "v")
	}
}

func TestBadgerCache_TTLExpiry(t *testing.T) {
	// badger's Entry.WithTTL truncates ExpiresAt to whole Unix SECONDS
	// (confirmed by reading badger's own source: ExpiresAt =
	// uint64(time.Now().Add(dur).Unix())), not the finer-grained duration
	// passed in. A sub-second TTL can therefore appear already-expired the
	// instant it is set, since "now plus a few tens of milliseconds" often
	// truncates to the SAME whole second as "now" itself. Both the TTL and
	// the sleep here are sized well above one second so the test exercises
	// real expiry behavior rather than tripping over that truncation.
	c := newTestBadgerCache(t)
	if err := c.Set(context.Background(), "k", []byte("v"), 1100*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Still present immediately.
	if _, ok, err := c.Get(context.Background(), "k"); err != nil || !ok {
		t.Fatalf("Get (before expiry) = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	time.Sleep(2200 * time.Millisecond)

	_, ok, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get (after expiry): %v", err)
	}
	if ok {
		t.Error("Get (after expiry) ok = true, want false (badger must expire the entry itself)")
	}
}

func TestBadgerCache_Set_NegativeTTL_ReturnsErrNegativeTTL(t *testing.T) {
	c := newTestBadgerCache(t)
	err := c.Set(context.Background(), "k", []byte("v"), -time.Second)
	if !errors.Is(err, cache.ErrNegativeTTL) {
		t.Fatalf("Set(negative ttl) = %v, want %v", err, cache.ErrNegativeTTL)
	}
	// The rejected Set must not have written anything.
	_, ok, getErr := c.Get(context.Background(), "k")
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if ok {
		t.Error("Get ok = true after a rejected negative-TTL Set, want false")
	}
}

// TestBadgerCache_Set_SubSecondTTL_IsRoundedUp confirms a positive but
// sub-second TTL does not appear already-expired the instant it is set
// (badger's own TTL truncates to whole Unix seconds — see badgerMinTTL's
// own doc for the exact mechanism). Set rounds it up to badgerMinTTL (one
// second) instead.
func TestBadgerCache_Set_SubSecondTTL_IsRoundedUp(t *testing.T) {
	c := newTestBadgerCache(t)
	if err := c.Set(context.Background(), "k", []byte("v"), 10*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok, err := c.Get(context.Background(), "k"); err != nil || !ok {
		t.Fatalf("Get (immediately after a 10ms Set) = (ok=%v, err=%v), want (true, nil) — a sub-second TTL must round up, not appear already-expired", ok, err)
	}
}

func TestBadgerCache_ZeroTTL_NeverExpires(t *testing.T) {
	c := newTestBadgerCache(t)
	if err := c.Set(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, ok, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Error("Get ok = false, want true (zero TTL means no expiration)")
	}
}

func TestBadgerCache_Delete_RemovesTheEntry(t *testing.T) {
	c := newTestBadgerCache(t)
	if err := c.Set(context.Background(), "k", []byte("v"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("Get ok = true after Delete, want false")
	}
}

func TestBadgerCache_Delete_UnknownKey_IsNotAnError(t *testing.T) {
	c := newTestBadgerCache(t)
	if err := c.Delete(context.Background(), "never-set"); err != nil {
		t.Errorf("Delete(unknown key): %v, want nil", err)
	}
}

// TestBadgerCache_CorruptDirectory_RecoversOnOpen is the regression test
// for NES-140's corruption-on-open handling: deleting/corrupting the
// cache directory while the app is stopped must let it start cold with
// no error, not fail boot. We simulate corruption directly (garbage bytes
// over badger's own MANIFEST file, the one badger.Open cannot proceed
// without) rather than relying on an actual crash, since that is the
// deterministic, reproducible way to exercise this path in a unit test.
func TestBadgerCache_CorruptDirectory_RecoversOnOpen(t *testing.T) {
	dir := t.TempDir()

	// Create a valid store first, so there is a real MANIFEST to corrupt
	// (an empty directory is a valid "fresh" open, not a corruption case).
	seed, err := cache.NewBadgerCache(dir, discardLogger())
	if err != nil {
		t.Fatalf("NewBadgerCache (seed): %v", err)
	}
	if err := seed.Set(context.Background(), "k", []byte("v"), time.Hour); err != nil {
		t.Fatalf("Set (seed): %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close (seed): %v", err)
	}

	manifest := filepath.Join(dir, "MANIFEST")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("seed MANIFEST missing, cannot corrupt it: %v", err)
	}
	if err := os.WriteFile(manifest, []byte("this is not a valid badger manifest"), 0o600); err != nil {
		t.Fatalf("corrupt MANIFEST: %v", err)
	}

	// NewBadgerCache must recover: log loudly, wipe the directory, retry
	// once, and succeed — never return an error for a recoverable
	// corruption, and never crash.
	recovered, err := cache.NewBadgerCache(dir, discardLogger())
	if err != nil {
		t.Fatalf("NewBadgerCache (after corruption) = error %v, want a successful cold start", err)
	}
	defer func() { _ = recovered.Close() }()

	// The pre-corruption value is gone (the whole point of the wipe), but
	// the store is now healthy and immediately usable.
	if _, ok, err := recovered.Get(context.Background(), "k"); err != nil || ok {
		t.Errorf("Get after recovery = (ok=%v, err=%v), want (false, nil) — the corrupt data was wiped", ok, err)
	}
	if err := recovered.Set(context.Background(), "fresh", []byte("value"), time.Hour); err != nil {
		t.Fatalf("Set after recovery: %v", err)
	}
	got, ok, err := recovered.Get(context.Background(), "fresh")
	if err != nil || !ok || string(got) != "value" {
		t.Errorf("Get after recovery = (%q, %v, %v), want (\"value\", true, nil)", got, ok, err)
	}
}

// TestBadgerCache_RunGC_StopsOnContextCancellation confirms RunGC's
// goroutine actually returns once ctx is cancelled — the same shutdown
// contract cmd/server's other background workers (Dispatcher.Run,
// scheduler.Run) already rely on.
func TestBadgerCache_RunGC_StopsOnContextCancellation(t *testing.T) {
	c := newTestBadgerCache(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.RunGC(ctx, 5*time.Millisecond)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunGC did not return after context cancellation")
	}
}

// TestBadgerCache_RunGC_LogsRealFailures confirms RunGC surfaces a genuine
// GC failure (not the routine ErrNoRewrite steady-state case): closing the
// underlying db makes badger's own RunValueLogGC return ErrRejected
// ("Value log GC request rejected... after DB::Close has been called" —
// badger's own db.go doc), which RunGC must log rather than swallow.
func TestBadgerCache_RunGC_LogsRealFailures(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	c, err := cache.NewBadgerCache(dir, logger)
	if err != nil {
		t.Fatalf("NewBadgerCache: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.RunGC(ctx, 5*time.Millisecond)
	}()

	// Let a handful of ticks fire against the closed db before stopping.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunGC did not return after context cancellation")
	}

	if !strings.Contains(buf.String(), "value log GC failed") {
		t.Errorf("RunGC against a closed db did not log a GC failure; log output = %q", buf.String())
	}
}

// The value-log-GC soak test (TestBadgerCache_ValueLogGC_BoundsDirectoryGrowth)
// lives in badger_gc_internal_test.go as a white-box (package cache) test,
// not here — it needs direct access to the unexported db field to force a
// deterministic compaction via badger's own Flatten. See that file's doc
// for why.
