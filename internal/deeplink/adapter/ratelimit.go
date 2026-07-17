package adapter

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// confirmRateEvery and confirmRateBurst tune perKeyLimiter for the /go/
// POST confirm actions (NES-129). A legitimate confirm is a single deliberate
// tap the member just made by scanning a QR code, so a small burst comfortably
// covers someone confirming a few chores back-to-back, while the steady rate
// still bounds a scripted hammer against the claim/complete/redeem endpoints
// far below anything a human tapping a phone screen could produce.
const (
	confirmRateEvery = 2 * time.Second
	confirmRateBurst = 5
)

// perKeyLimiter rate-limits by an arbitrary string key — here, the confirming
// member's id — so one member spamming confirm cannot exhaust a shared budget
// with the rest of the household. It wraps one golang.org/x/time/rate.Limiter
// per key, created lazily on first use.
//
// This is deliberately a small, deep-link-specific type rather than a
// platform middleware: /go/ POST confirms are its only consumer (NES-129). If
// a second caller needs per-key rate limiting, promote this to
// internal/platform/httpserver/middleware as a general keyed-limiter
// middleware instead of duplicating it.
//
// The key set grows for the lifetime of the process (never evicted), which is
// an accepted tradeoff for Nestova's deployment shape: a single-household,
// local-first appliance (see internal/kiosk's own doc comments) with a small,
// stable member roster — unlike a multi-tenant SaaS, this can never grow
// unbounded.
type perKeyLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	every    rate.Limit
	burst    int
}

// newPerKeyLimiter constructs a perKeyLimiter allowing burst immediate
// actions per key, refilling at one per every thereafter.
func newPerKeyLimiter(every time.Duration, burst int) *perKeyLimiter {
	return &perKeyLimiter{
		limiters: make(map[string]*rate.Limiter),
		every:    rate.Every(every),
		burst:    burst,
	}
}

// allow reports whether the action keyed by key is currently permitted,
// consuming one token from that key's bucket when it is.
func (l *perKeyLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.limiters[key]
	if !ok {
		lim = rate.NewLimiter(l.every, l.burst)
		l.limiters[key] = lim
	}
	return lim.Allow()
}
