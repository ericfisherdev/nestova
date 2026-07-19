package cache

import (
	"context"
	"sync"
	"time"
)

// MemoryCache is an in-process Cache: a mutex-guarded map, with expiry
// checked lazily at Get (there is no background sweep of expired
// entries — an entry that is set and never read again simply occupies
// memory until the process exits, which is acceptable for its two
// intended uses: hermetic tests, and cmd/server's boot-time fallback
// when BadgerCache fails to open even after its own corruption-recovery
// retry (see that type's own doc) — a short-lived degraded mode, not a
// long-running production configuration.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]memoryEntry
}

// memoryEntry holds one cached value and its absolute expiry. A zero
// Expires means the entry never expires.
type memoryEntry struct {
	value   []byte
	expires time.Time
}

// Compile-time assurance the cache satisfies the port.
var _ Cache = (*MemoryCache)(nil)

// NewMemoryCache constructs an empty MemoryCache, ready to use
// immediately — it takes no dependencies and cannot fail to construct.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: make(map[string]memoryEntry)}
}

// Get returns a COPY of the stored value, so a caller mutating the
// returned slice can never corrupt what MemoryCache holds internally. An
// expired entry is lazily deleted the first time it is observed via Get,
// rather than left to accumulate — a Get scans, at most, the one key
// requested, so this costs nothing extra per call.
func (c *MemoryCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		return nil, false, nil
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		delete(c.entries, key)
		return nil, false, nil
	}
	value := make([]byte, len(e.value))
	copy(value, e.value)
	return value, true, nil
}

// Set stores a COPY of value under key, so a caller mutating the slice it
// passed in after Set returns can never corrupt what MemoryCache holds
// internally. A negative ttl returns ErrNegativeTTL.
func (c *MemoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		return ErrNegativeTTL
	}
	stored := make([]byte, len(value))
	copy(stored, value)

	var expires time.Time
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = memoryEntry{value: stored, expires: expires}
	return nil
}

// Delete removes key. It is a no-op, not an error, when key is already
// absent.
func (c *MemoryCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
	return nil
}
