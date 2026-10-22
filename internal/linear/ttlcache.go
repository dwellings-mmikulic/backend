package linear

import (
	"sync"
	"time"
)

// ttlCache is a bounded map whose entries expire after a fixed TTL. It
// backs the per-household lookups a customized playlist poll needs: a
// household's spans and its anchor on a channel change only when the
// mobile page writes, so between writes the values are served from memory
// and the store sees one read per household per TTL instead of one per
// poll. Writers call forget so their own box serves the change at once; a
// second API box would serve it within the TTL.
//
// The size cap matters because the keys come from unauthenticated requests
// (any ip= is a household id): the map must not grow with whatever ids a
// client cares to invent.
type ttlCache[K comparable, V any] struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[K]ttlEntry[V]
}

type ttlEntry[V any] struct {
	v  V
	at time.Time // when the entry was stored
}

func newTTLCache[K comparable, V any](max int, ttl time.Duration, now func() time.Time) *ttlCache[K, V] {
	return &ttlCache[K, V]{ttl: ttl, max: max, now: now, entries: map[K]ttlEntry[V]{}}
}

// get returns the value of k when its entry is younger than the TTL.
func (c *ttlCache[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		var zero V
		return zero, false
	}
	if c.now().Sub(e.at) >= c.ttl {
		delete(c.entries, k)
		var zero V
		return zero, false
	}
	return e.v, true
}

// put stores v under k, evicting first when the cache is full.
func (c *ttlCache[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[k]; !exists && len(c.entries) >= c.max {
		c.evictLocked()
	}
	c.entries[k] = ttlEntry[V]{v: v, at: c.now()}
}

// forget drops k so the next get reads through.
func (c *ttlCache[K, V]) forget(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, k)
}

// evictLocked makes room: expired entries first, then — when every entry
// is live, which under a flood of invented ids is the steady state — a
// tenth of the map in iteration order, so a put costs O(1) amortised rather
// than a full scan for the oldest entry per insert.
func (c *ttlCache[K, V]) evictLocked() {
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.at) >= c.ttl {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < c.max {
		return
	}
	drop := c.max / 10
	if drop < 1 {
		drop = 1
	}
	for k := range c.entries {
		if drop == 0 {
			return
		}
		delete(c.entries, k)
		drop--
	}
}
