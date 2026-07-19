// Package cache provides a small Cache port (NES-140) for data that is
// derived, re-computable, or externally sourced — an external API
// response, a normalized/aggregated read that is expensive to recompute
// but never the sole source of truth for it. It is explicitly NOT for
// domain reads (the database is the source of truth for those), sessions
// (scs already owns that), or point balances (NES-95 governs balance
// caching directly). Losing the cache directory — corruption, a wiped
// disk, a fresh deployment — must always be a non-event: every consumer
// recomputes or re-fetches on a miss, never fails because the cache is
// empty or unavailable.
//
// Two implementations: MemoryCache (in-process, for hermetic tests and as
// cmd/server's own boot-time fallback when the persistent backend fails
// to open) and BadgerCache (on-disk, persists across restarts — see that
// type's own doc for why it is the ONLY package outside this one allowed
// to import github.com/dgraph-io/badger/v4, enforced by a depguard rule
// in .golangci.yml).
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrNegativeTTL is returned by Set when ttl is negative. Only a ZERO ttl
// means "no expiration" (see Set's own doc); a negative ttl is always a
// caller bug — most likely a miscalculated duration — so both
// implementations reject it outright rather than silently treating it as
// "cache forever."
var ErrNegativeTTL = errors.New("cache: ttl must not be negative")

// Cache is the port every cache consumer depends on.
//
// Key namespace convention: every key SHOULD be namespaced
// "<domain>:<purpose>:<id>" (e.g. "recipes:externalfind:<sha256 hex>" —
// see meals/adapter.ExternalRecipeSource's own cache-aside for the first
// real example) so consumers from different bounded contexts never
// collide on a bare key, and so a key's owner and purpose are obvious
// from a cache dump or a metrics label alone. This is a naming
// convention callers are expected to follow, not something the port
// itself validates — Set does not reject an unnamespaced key.
//
// TTL precision: callers must not rely on sub-second TTL accuracy.
// MemoryCache preserves the full time.Duration, but BadgerCache honors TTL
// only to whole-second resolution (badger's own WithTTL truncates to whole
// Unix seconds) and rounds a positive sub-second ttl up to one second
// rather than let it appear already-expired the instant it is set. Every
// real caller of this port so far uses TTLs measured in hours, so this
// only matters if a future caller passes a sub-second value.
type Cache interface {
	// Get returns the cached value for key. ok is false when the key does
	// not exist OR has expired; callers must treat both the same way (a
	// cache miss), never distinguish them.
	Get(ctx context.Context, key string) (value []byte, ok bool, err error)

	// Set stores value under key, expiring it after ttl. A zero ttl means
	// "no expiration" — almost every derived/external cache entry should
	// instead carry a bounded TTL, so a stale value is never served
	// indefinitely once its source of truth has moved on. A negative ttl
	// returns ErrNegativeTTL rather than being treated as "no expiration."
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes key. It is not an error for key to already be
	// absent.
	Delete(ctx context.Context, key string) error
}
