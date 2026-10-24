package auth

import (
	"container/list"
	"sync"
	"time"

	"github.com/google/uuid"
)

// apiKeyCacheTTL bounds how long a verified (or negatively-cached) lookup
// stays in memory. Matches the SQL touchThrottle window in
// internal/apikeys/store.go: a revoked key remains usable for at most one
// TTL, which matches the existing audit granularity. Shortening this
// reduces revoke latency at the cost of more bcrypt verifies; lengthening
// it amortises better but lets revoked keys live longer in the hot path.
const apiKeyCacheTTL = 60 * time.Second

// apiKeyCacheCapacity caps memory. 1024 entries × ~120 B/entry ≈ 120 KB
// per replica, well below any concern. The eviction is strict LRU — the
// oldest entry is dropped when the cache is full and a new entry needs
// to be inserted.
const apiKeyCacheCapacity = 1024

// apiKeyCacheEntry is what we cache against the plain key. The plain key
// is the cache key (the client sent it in cleartext over TLS already; map
// lookups don't need constant-time semantics here — bcrypt itself is the
// timing-safe primitive).
//
// Negative entries (ok=false) cover the "wrong-key" case: a typo'd key
// would otherwise pay a full bcrypt verify on every retry. We do NOT
// negative-cache "prefix not found" — that path is intentionally kept
// going through LookupByPrefix so the W1 dummy-bcrypt mitigation gets
// the same code path each time.
//
// A positive entry maps the key to its id and owner ONLY (sk-674.18 K-16):
// whether the key is still usable, and the owner's role, external flag,
// deactivation and deletion, are read on every request
// (apikeys.Store.Status), so no cached entry outlives a revocation.
type apiKeyCacheEntry struct {
	keyID     uuid.UUID
	userID    uuid.UUID
	ok        bool
	expiresAt time.Time
}

// apiKeyCache is a fixed-capacity LRU for (plain key) → (resolved
// identity). Used by the MCP middleware to avoid running
// bcrypt.CompareHashAndPassword on every single /mcp request — the
// realistic hot path is one client repeatedly calling tools/list +
// tools/call within a single conversation, so amortising bcrypt to
// "once per key per minute" is enough.
type apiKeyCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	now      func() time.Time // injectable for tests
	ll       *list.List
	index    map[string]*list.Element
}

// apiKeyCacheItem is the value stored in each list element. Combining
// key + entry in the list node lets us evict the LRU tail without a
// reverse-lookup back into the map.
type apiKeyCacheItem struct {
	key   string
	entry apiKeyCacheEntry
}

// newAPIKeyCache constructs an LRU. capacity must be > 0; ttl must be > 0.
// nowFn is normally time.Now and can be overridden by tests.
func newAPIKeyCache(capacity int, ttl time.Duration, nowFn func() time.Time) *apiKeyCache {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &apiKeyCache{
		capacity: capacity,
		ttl:      ttl,
		now:      nowFn,
		ll:       list.New(),
		index:    make(map[string]*list.Element, capacity),
	}
}

// Get returns the cached entry for plain or (zero, false) when missing /
// expired. On a hit it bumps the entry to the front of the LRU. On
// expiry it evicts the entry and reports a miss so the caller redoes
// the DB + bcrypt path.
func (c *apiKeyCache) Get(plain string) (apiKeyCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.index[plain]
	if !ok {
		return apiKeyCacheEntry{}, false
	}
	item := el.Value.(*apiKeyCacheItem)
	if !item.entry.expiresAt.After(c.now()) {
		c.ll.Remove(el)
		delete(c.index, plain)
		return apiKeyCacheEntry{}, false
	}
	c.ll.MoveToFront(el)
	return item.entry, true
}

// Put inserts or refreshes an entry. Expiry is set to now+ttl.
// Eviction (oldest entry) runs when capacity is exceeded.
func (c *apiKeyCache) Put(plain string, entry apiKeyCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry.expiresAt = c.now().Add(c.ttl)

	if el, ok := c.index[plain]; ok {
		el.Value.(*apiKeyCacheItem).entry = entry
		c.ll.MoveToFront(el)
		return
	}

	item := &apiKeyCacheItem{key: plain, entry: entry}
	el := c.ll.PushFront(item)
	c.index[plain] = el

	for c.ll.Len() > c.capacity {
		old := c.ll.Back()
		if old == nil {
			return
		}
		c.ll.Remove(old)
		delete(c.index, old.Value.(*apiKeyCacheItem).key)
	}
}

// Len returns the current number of cached entries (live + still-indexed
// expired). Used by tests.
func (c *apiKeyCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
