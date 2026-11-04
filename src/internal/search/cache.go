package search

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
)

// resultCacheCapacity bounds each in-process cache namespace. 256 distinct queries
// over a 60-second TTL fit a small team comfortably; under heavier load
// the LRU evicts coldest entries. The cache is a *latency optimisation*
// for mode-aware cursor pagination: a miss rebuilds the query in its saved
// ranking mode. Legacy cursors and unavailable hybrid rankings require a
// fresh search when the pool is gone.
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
	mode      rankingMode
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
	c.removeExpired(time.Now())
	entry, ok := c.cache.Get(key)
	if !ok {
		return resultEntry{}, false
	}
	return entry, true
}

// removeExpired sweeps the bounded LRU on activity, not only when an expired
// generation's own key is read again. No background goroutine is required.
// The caller holds mu; Peek leaves the surviving entries' recency unchanged.
func (c *resultCache) removeExpired(now time.Time) {
	for _, key := range c.cache.Keys() {
		if entry, ok := c.cache.Peek(key); ok && now.After(entry.expiresAt) {
			c.cache.Remove(key)
		}
	}
}

// ownedBy reports whether the entry was filled for caller with scope — the
// only condition under which it may answer (K-5).
func (e resultEntry) ownedBy(caller uuid.UUID, scope string) bool {
	return e.caller == caller && e.scope == scope
}

// Put stores a result-set under key, owned by caller with scope.
// Subsequent reads within ttl return the same slice, in the same order, so
// cursor offsets stay stable.
func (c *resultCache) Put(key string, items []Item, caller uuid.UUID, scope string, mode rankingMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.removeExpired(now)
	c.cache.Add(key, resultEntry{
		items:     items,
		expiresAt: now.Add(c.ttl),
		caller:    caller,
		scope:     scope,
		mode:      mode,
	})
}

// rankedCacheKey partitions a query's pool by the ranking that produced
// it. A recovered embedder must not make an FTS cursor slice a hybrid
// pool, or an outage make a hybrid cursor slice an FTS pool.
func rankedCacheKey(queryKey string, mode rankingMode) string {
	sum := sha256.Sum256([]byte(queryKey + "|ranking=" + string(mode)))
	return hex.EncodeToString(sum[:])
}

// snapshotCacheKey binds a fresh scoped pool to the same query/ranking key
// plus its opaque generation. A new first page cannot replace an old pool.
func snapshotCacheKey(rankedKey, snapshot string) string {
	sum := sha256.Sum256([]byte(rankedKey + "|snapshot=" + snapshot))
	return hex.EncodeToString(sum[:])
}

// CacheKey is a deterministic SHA-256 fingerprint of the query and its
// visibility. The service adds the actual ranking mode with rankedCacheKey
// before storing a pool or issuing a cursor. Data edits can still change a
// rebuilt pool: cursors are continuations of a search, not database snapshots.
// Additive mutable issue selectors use scopedQueryKey's typed, versioned
// domain. Requests without them keep the legacy identity below unchanged.
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
//   - optional issue assignee, authorship, acceptance, blocked, component and origin selectors
//
// We deliberately do NOT include the limit or cursor — the cache stores a
// fixed top-200 superset, so the same key serves any page-size up to 200.
func CacheKey(p Params) string {
	if p.Filters.mutableIssueSelection() {
		return scopedQueryKey(p)
	}
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

// scopedQueryKey uses a separate typed, length-prefixed hash domain for
// the additive issue selectors. No caller text can impersonate another
// field boundary, and optional values remain distinct from absent values.
// The legacy CacheKey branch is kept unchanged for existing continuations.
func scopedQueryKey(p Params) string {
	payload := []byte("scoped-search-query-v1\x00")
	writeString := func(value string) {
		payload = binary.AppendUvarint(payload, uint64(len(value)))
		payload = append(payload, value...)
	}
	writeStrings := func(values []string) {
		values = append([]string(nil), values...)
		sort.Strings(values)
		payload = binary.AppendUvarint(payload, uint64(len(values)))
		for _, value := range values {
			writeString(value)
		}
	}
	writeUUID := func(value *uuid.UUID) {
		if value == nil {
			payload = append(payload, 0)
			return
		}
		payload = append(payload, 1)
		payload = append(payload, value[:]...)
	}
	writeOptionalString := func(value *string) {
		if value == nil {
			payload = append(payload, 0)
			return
		}
		payload = append(payload, 1)
		writeString(*value)
	}
	writeBool := func(value *bool) {
		if value == nil {
			payload = append(payload, 0)
		} else if *value {
			payload = append(payload, 2)
		} else {
			payload = append(payload, 1)
		}
	}
	fold := func(value string) string {
		if utf8.ValidString(value) {
			return strings.ToLower(value)
		}
		// Preserve invalid input bytes instead of aliasing a valid replacement
		// character; input validation/database errors keep their own meaning.
		return value
	}
	writeString(fold(strings.TrimSpace(p.Query)))
	writeUUID(p.Project)
	writeString(p.Space)
	kinds := make([]string, len(p.Kinds))
	for i, kind := range p.Kinds {
		kinds[i] = string(kind)
	}
	writeStrings(kinds)
	payload = append(payload, p.CallerID[:]...)
	writeString(p.visibility)
	f := p.Filters
	writeStrings(f.Statuses)
	priorities := append([]int(nil), f.Priorities...)
	sort.Ints(priorities)
	payload = binary.AppendUvarint(payload, uint64(len(priorities)))
	for _, priority := range priorities {
		payload = binary.AppendVarint(payload, int64(priority))
	}
	writeStrings(f.Types)
	writeStrings(f.Labels)
	writeOptionalString(f.Source)
	writeStrings(f.CommentAuthorKinds)
	writeUUID(f.AssigneeUserID)
	if f.Assignee != nil {
		value := fold(*f.Assignee)
		f.Assignee = &value
	}
	writeOptionalString(f.Assignee)
	writeUUID(f.CreatedBy)
	writeBool(&f.DelegatedLens)
	writeUUID(f.CreatedByExclude)
	writeBool(f.AcceptedAtNull)
	writeBool(f.AcceptedAtNotNull)
	writeBool(f.IsBlocked)
	writeUUID(f.ComponentID)
	writeStrings(f.OriginSources)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
