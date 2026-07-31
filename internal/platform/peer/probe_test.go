package peer_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/peer"
)

// longTTL is long enough that a test relying on the cache NOT expiring
// mid-run never flakes on scheduling jitter.
const longTTL = time.Minute

// shortTTL expires practically immediately, so a test asserting a SECOND
// probe fires never has to sleep for DefaultVerdictTTL's real 30s.
const shortTTL = time.Nanosecond

func TestReachable_HealthyPeerReturnsTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := peer.NewProber(srv.Client(), srv.URL, time.Second, longTTL)

	if !p.Reachable(context.Background()) {
		t.Error("Reachable() = false, want true for a healthy peer")
	}
}

func TestReachable_NonOKStatusReturnsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := peer.NewProber(srv.Client(), srv.URL, time.Second, longTTL)

	if p.Reachable(context.Background()) {
		t.Error("Reachable() = true, want false for a 503 peer")
	}
}

func TestReachable_UnreachablePeerReturnsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close() // closed before use: connections to srv.URL now refuse.

	p := peer.NewProber(http.DefaultClient, srv.URL, time.Second, longTTL)

	if p.Reachable(context.Background()) {
		t.Error("Reachable() = true, want false for a peer that refuses connections")
	}
}

func TestReachable_SlowPeerTimesOut(t *testing.T) {
	// block is closed before the deferred srv.Close(): defers run LIFO, so
	// declaring this defer AFTER srv.Close()'s means it runs FIRST,
	// unblocking the still-in-flight handler before Close waits on it —
	// the reverse order deadlocks Close waiting on a handler that is
	// itself waiting on this channel.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(block)

	p := peer.NewProber(srv.Client(), srv.URL, 10*time.Millisecond, longTTL)

	if p.Reachable(context.Background()) {
		t.Error("Reachable() = true, want false when the peer exceeds the probe timeout")
	}
}

// TestReachable_CachesVerdictWithinTTL covers NSTR-124's own probe-caching
// requirement: a second Reachable call within ttl must not re-probe the
// peer, bounding probe traffic regardless of sidebar render volume.
func TestReachable_CachesVerdictWithinTTL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := peer.NewProber(srv.Client(), srv.URL, time.Second, longTTL)

	p.Reachable(context.Background())
	p.Reachable(context.Background())

	if got := hits.Load(); got != 1 {
		t.Errorf("healthz hits = %d, want exactly 1 (second call should hit the cache)", got)
	}
}

// TestReachable_ReprobesAfterTTLExpires covers the complementary case: once
// ttl elapses, the next Reachable call must probe again rather than serving
// a stale verdict forever.
func TestReachable_ReprobesAfterTTLExpires(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := peer.NewProber(srv.Client(), srv.URL, time.Second, shortTTL)

	p.Reachable(context.Background())
	time.Sleep(time.Millisecond)
	p.Reachable(context.Background())

	if got := hits.Load(); got != 2 {
		t.Errorf("healthz hits = %d, want exactly 2 (ttl expired between calls)", got)
	}
}
