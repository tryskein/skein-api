package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeClock returns a controllable time source for cache TTL tests.
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) Advance(d time.Duration) {
	f.now = f.now.Add(d)
}

func newTestEntry(ok bool) apiKeyCacheEntry {
	return apiKeyCacheEntry{
		keyID:  uuid.New(),
		userID: uuid.New(),
		ok:     ok,
	}
}

func TestAPIKeyCache_HitMiss(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	c := newAPIKeyCache(8, 60*time.Second, clock.Now)

	if _, ok := c.Get("ska_unknown"); ok {
		t.Fatalf("expected miss on empty cache")
	}

	want := newTestEntry(true)
	c.Put("ska_known", want)

	got, ok := c.Get("ska_known")
	if !ok {
		t.Fatalf("expected hit on cached key")
	}
	if got.keyID != want.keyID || got.userID != want.userID || got.ok != want.ok {
		t.Fatalf("entry mismatch on hit: got=%+v want=%+v", got, want)
	}
}

func TestAPIKeyCache_Expiry(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	c := newAPIKeyCache(8, 60*time.Second, clock.Now)

	c.Put("ska_e", newTestEntry(true))

	// One second before TTL: still a hit.
	clock.Advance(59 * time.Second)
	if _, ok := c.Get("ska_e"); !ok {
		t.Fatalf("expected hit just before TTL expiry")
	}

	// Cross the boundary: should miss.
	clock.Advance(2 * time.Second)
	if _, ok := c.Get("ska_e"); ok {
		t.Fatalf("expected miss after TTL expiry")
	}
}

func TestAPIKeyCache_EvictionAtCapacity(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	c := newAPIKeyCache(3, 60*time.Second, clock.Now)

	c.Put("a", newTestEntry(true))
	c.Put("b", newTestEntry(true))
	c.Put("c", newTestEntry(true))

	if got := c.Len(); got != 3 {
		t.Fatalf("expected len=3 at capacity, got %d", got)
	}

	// Insert past capacity → "a" (oldest) should be evicted.
	c.Put("d", newTestEntry(true))

	if got := c.Len(); got != 3 {
		t.Fatalf("expected len=3 after eviction, got %d", got)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatalf("expected 'a' to be evicted as the LRU tail")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("expected %q to remain in cache", k)
		}
	}
}

func TestAPIKeyCache_NegativeCache(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	c := newAPIKeyCache(8, 60*time.Second, clock.Now)

	// Negative entries are how we avoid re-running bcrypt for a typo'd key.
	c.Put("ska_typo", apiKeyCacheEntry{ok: false})

	got, ok := c.Get("ska_typo")
	if !ok {
		t.Fatalf("expected negative-cache hit")
	}
	if got.ok {
		t.Fatalf("expected ok=false from negative-cache entry, got true")
	}
}

func TestAPIKeyCache_LRUOrderingOnHit(t *testing.T) {
	// A Get() should bump the entry to the head so it survives the next
	// eviction. This is what keeps "actively-used" keys in the cache
	// during burst traffic.
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	c := newAPIKeyCache(3, 60*time.Second, clock.Now)

	c.Put("a", newTestEntry(true))
	c.Put("b", newTestEntry(true))
	c.Put("c", newTestEntry(true))

	// Access "a" so it becomes most-recently-used.
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("expected 'a' hit before LRU bump")
	}

	// Insert "d" — now "b" should be the eviction victim, not "a".
	c.Put("d", newTestEntry(true))

	if _, ok := c.Get("b"); ok {
		t.Fatalf("expected 'b' to be evicted (was LRU after 'a' was bumped)")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("expected 'a' to survive (it was bumped to MRU)")
	}
}
