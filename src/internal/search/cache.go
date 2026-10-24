package search

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
)

// resultCacheCapacity bounds the in-process cache. 256 distinct queries
// over a 60-second TTL fit a small team comfortably; under heavier load
// the LRU evicts coldest entries. The cache is a *latency optimisation*
// for cursor pagination — a miss simply re-runs the same query, so the
// upper bound here is about CPU cost, not correctness.
const resultCacheCapacity = 256

// resultCacheTTL is how long a cached top-K stays warm. Sixty seconds
// keeps cursor pagination consistent for an interactive session without
// holding stale data through a multi-minute background tab. Configurable
// via Config.SearchResultCacheTTL.
//
// K-5 (sk-674.18 T031): an entry remembers whose pool it is — the caller and
// the fingerprint of his scope (his visible set) when it was filled. A
// cursor is untrusted base64 a client can rewrite; a page is served from an
// entry only when both match the current request (Service.Search), so a
// cursor of another user, one with a forged key, or one outliving a change
// of access gets ErrInvalidCursor, never another pool.
type resultEntry struct {
	items     []Item
	expiresAt time.Time
	caller    uuid.UUID
	scope     string
}

// resultCache is the result-set LRU. It maps cacheKey → ordered slice of
// up to 200 items (the materialised top-K from a single search request).
// The cursor protocol stores (cacheKey, offset) so the next page is just
// a slice of the cached entry.
type resultCache struct {
	cache *lru.Cache[string, resultEntry]
	ttl   time.Duration
	mu    sync.Mutex // protects expiry checks across goroutines
}

func newResultCache(ttl time.Duration) (*resultCache, error) {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	c, err := lru.New[string, resultEntry](resultCacheCapacity)
	if err != nil {
		return nil, err
	}
	return &resultCache{cache: c, ttl: ttl}, nil
}

// Get returns the cached entry, or (zero, false) on miss / expiry.
func (c *resultCache) Get(key string) (resultEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache.Get(key)
	if !ok {
		return resultEntry{}, false
	}
	if time.Now().After(entry.expiresAt) {
		c.cache.Remove(key)
		return resultEntry{}, false
	}
	return entry, true
}

// ownedBy reports whether the entry was filled for caller with scope — the
// only condition under which it may answer (K-5).
func (e resultEntry) ownedBy(caller uuid.UUID, scope string) bool {
	return e.caller == caller && e.scope == scope
}

// Put stores a result-set under key, owned by caller with scope.
// Subsequent reads within ttl return the same slice, in the same order, so
// cursor offsets stay stable.
func (c *resultCache) Put(key string, items []Item, caller uuid.UUID, scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache.Add(key, resultEntry{
		items:     items,
		expiresAt: time.Now().Add(c.ttl),
		caller:    caller,
		scope:     scope,
	})
}

// CacheKey is a deterministic SHA-256 hex digest of every input that
// influences the result set. Two requests with the same key are
// guaranteed to yield the same items — which is what makes cursor
// pagination work.
//
// The inputs:
//   - normalized query (lowercased, trimmed)
//   - project filter (UUID string or "")
//   - sorted comma-joined kinds
//   - caller ID (permissions filter changes per user)
//   - the fingerprint of the caller's visible set (Params.visibility):
//     a change of what the caller sees — demotion, a lost or gained
//     membership — is a different key, so a pool cached under the old
//     view never answers
//   - sorted joined statuses, priorities, types, labels
//   - source filter (human/agent) when set
//   - sorted comment-author-kinds filter when set
//
// We deliberately do NOT include the limit or cursor — the cache stores a
// fixed top-200 superset, so the same key serves any page-size up to 200.
func CacheKey(p Params) string {
	var b strings.Builder
	b.Grow(128)
	b.WriteString("q=")
	b.WriteString(strings.ToLower(strings.TrimSpace(p.Query)))
	b.WriteString("|p=")
	if p.Project != nil {
		b.WriteString(p.Project.String())
	}
	b.WriteString("|k=")
	kinds := make([]string, len(p.Kinds))
	for i, k := range p.Kinds {
		kinds[i] = string(k)
	}
	sort.Strings(kinds)
	b.WriteString(strings.Join(kinds, ","))
	if p.Space != "" {
		b.WriteString("|sp=")
		b.WriteString(p.Space)
	}
	b.WriteString("|u=")
	b.WriteString(p.CallerID.String())
	if p.visibility != "" {
		b.WriteString("|v=")
		b.WriteString(p.visibility)
	}

	if len(p.Filters.Statuses) > 0 {
		s := append([]string(nil), p.Filters.Statuses...)
		sort.Strings(s)
		b.WriteString("|s=")
		b.WriteString(strings.Join(s, ","))
	}
	if len(p.Filters.Priorities) > 0 {
		ps := make([]string, len(p.Filters.Priorities))
		for i, n := range p.Filters.Priorities {
			ps[i] = strconv.Itoa(n)
		}
		sort.Strings(ps)
		b.WriteString("|pr=")
		b.WriteString(strings.Join(ps, ","))
	}
	if len(p.Filters.Types) > 0 {
		t := append([]string(nil), p.Filters.Types...)
		sort.Strings(t)
		b.WriteString("|t=")
		b.WriteString(strings.Join(t, ","))
	}
	if len(p.Filters.Labels) > 0 {
		l := append([]string(nil), p.Filters.Labels...)
		sort.Strings(l)
		b.WriteString("|l=")
		b.WriteString(strings.Join(l, ","))
	}
	if p.Filters.Source != nil {
		b.WriteString("|src=")
		b.WriteString(*p.Filters.Source)
	}
	if len(p.Filters.CommentAuthorKinds) > 0 {
		ak := append([]string(nil), p.Filters.CommentAuthorKinds...)
		sort.Strings(ak)
		b.WriteString("|cak=")
		b.WriteString(strings.Join(ak, ","))
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
