// Package peer implements NSTR-124's server-side reachability probe against
// a configured sibling app (Nestorage's PEER_NESTORAGE_URL). Nothing here is
// ever called from the browser: the shell probes the peer's /healthz on the
// server and renders the cached verdict, so the browser itself never issues
// a cross-origin request and no external-host rule is ever at risk.
package peer

import (
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// DefaultProbeTimeout bounds a single /healthz request so sidebar rendering
// never blocks on a dead or slow peer — NSTR-124's own AC that "the page
// still renders at full speed" even with the peer down.
const DefaultProbeTimeout = 300 * time.Millisecond

// DefaultVerdictTTL caches a probe's outcome so a request landing within TTL
// of the last check never re-probes the peer, bounding probe traffic to at
// most one /healthz request per TTL window regardless of sidebar render
// volume — sf (below) is what makes that hold even when N renders miss the
// cache at the same instant, not just when they arrive serially.
const DefaultVerdictTTL = 30 * time.Second

// healthzFlightKey is the constant key every Reachable call shares with
// singleflight: a Prober only ever probes its own single baseURL, so there
// is exactly one in-flight request to coalesce, never a family of them.
const healthzFlightKey = "healthz"

// Prober is a cached, timeout-bounded reachability check against a peer
// app's /healthz. It satisfies the one-method reachability port the shell
// depends on (see cmd/server/home.go's shellPeerReachabilityChecker doc).
// The zero value is not usable — construct with NewProber.
type Prober struct {
	client  *http.Client
	baseURL string
	timeout time.Duration
	ttl     time.Duration

	mu        sync.Mutex
	checkedAt time.Time
	verdict   bool

	// sf collapses concurrent cache misses (a cold start, or N sidebar
	// renders landing the instant ttl expires) into a single in-flight
	// /healthz request, mirroring meals/adapter.ExternalRecipeSource's own
	// identical singleflight use against its own metered external call.
	// Its zero value is ready to use.
	sf singleflight.Group
}

// NewProber constructs a Prober against baseURL's /healthz, injecting client
// and the timeout/ttl so tests can point it at an httptest.Server and a
// fast-expiring ttl instead of a real network call and DefaultVerdictTTL's
// real-time wait. baseURL may be empty (no peer configured): Reachable is
// simply never called in that case — see cmd/server/home.go's shellPeer —
// so an empty baseURL is not itself rejected here.
func NewProber(client *http.Client, baseURL string, timeout, ttl time.Duration) *Prober {
	if client == nil {
		panic("peer: NewProber requires a non-nil *http.Client")
	}
	return &Prober{client: client, baseURL: baseURL, timeout: timeout, ttl: ttl}
}

// Reachable reports whether baseURL's /healthz answered 200 within timeout,
// caching the verdict for ttl so repeated sidebar renders within the same
// window never re-probe a peer that was just checked. ctx contributes
// values only (e.g. a logger or trace ID) — it does NOT bound or cancel the
// probe itself, only p.timeout does: the verdict is process-wide cached
// state shared with every other coalesced caller via sf, so one caller's
// context being canceled (a browser navigating away, a kiosk tab closing)
// must never poison that shared result.
func (p *Prober) Reachable(ctx context.Context) bool {
	if v, ok := p.cached(); ok {
		return v
	}

	v, _, _ := p.sf.Do(healthzFlightKey, func() (any, error) {
		return p.probe(ctx), nil
	})
	ok := v.(bool)

	p.mu.Lock()
	p.verdict = ok
	p.checkedAt = time.Now()
	p.mu.Unlock()

	return ok
}

// cached returns the last verdict and true when it is still within ttl, or
// (false, false) when a fresh probe is needed.
func (p *Prober) cached() (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.checkedAt.IsZero() || time.Since(p.checkedAt) >= p.ttl {
		return false, false
	}
	return p.verdict, true
}

// probe issues the actual GET {baseURL}/healthz, bounded by timeout.
//
// context.WithoutCancel keeps ctx's own values but detaches its
// cancellation before WithTimeout applies p.timeout: this fetch's result is
// shared with every request sf.Do coalesced onto it (see Reachable's own
// doc), so it must not be torn down just because whichever caller happened
// to become the singleflight leader had its own context canceled.
func (p *Prober) probe(ctx context.Context) bool {
	reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.baseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode == http.StatusOK
}
