package service

import (
	"sync"
	"time"
)

// ttlCache is a small in-memory cache for upstream results. Backtests cost
// seconds and count against a per-endpoint rate limit, and their inputs only
// change once a trading day, so caching is what keeps the public API cheap.
//
// An entry is fresh until its TTL. After that it may be kept for a grace
// period as the last good copy, served only when a refresh fails — never
// instead of one.
type ttlCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value   any
	expires time.Time // fresh until
	keep    time.Time // held as the last good copy until
}

func newTTLCache() *ttlCache { return &ttlCache{entries: map[string]cacheEntry{}} }

// get returns a fresh value.
func (c *ttlCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.value, true
}

// lastGood returns a value that may be past its TTL but is inside its grace
// period.
func (c *ttlCache) lastGood(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.keep) {
		return nil, false
	}
	return e.value, true
}

// expire makes an entry stale without dropping it: it stops counting as fresh
// but can still answer as the last good copy while it is replaced.
func (c *ttlCache) expire(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		e.expires = time.Now().Add(-time.Second)
		c.entries[key] = e
	}
}

func (c *ttlCache) set(key string, v any, ttl, grace time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Opportunistic sweep keeps memory bounded without a janitor goroutine.
	if len(c.entries) > 512 {
		for k, e := range c.entries {
			if now.After(e.keep) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = cacheEntry{value: v, expires: now.Add(ttl), keep: now.Add(ttl + grace)}
}
