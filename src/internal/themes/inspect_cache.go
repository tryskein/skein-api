package themes

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// InspectCacheDefaultTTL is the per-entry TTL for an inspect selection.
// Ten minutes matches the "user clicks an element → switches to chat with
// the agent → asks the agent to change something" workflow. Shorter would
// expire mid-conversation; longer would leak stale selections into the
// agent's view after the user has clearly moved on.
const InspectCacheDefaultTTL = 10 * time.Minute

// InspectCacheDefaultMax is the per-user hard cap on stored selections.
// Twenty covers a normal back-and-forth between user and agent (each
// click in the inspector adds one entry; the agent calls
// skein_theme(action="inspect_clear") after processing them to flush
// the list).
// Without a cap a runaway frontend could pile selections forever; we
// fall back to FIFO eviction when the cap is reached.
const InspectCacheDefaultMax = 20

// inspectCleanupInterval is how often the background goroutine sweeps
// expired entries. 60 seconds is conservative — most entries are read
// long before they expire (the agent fetches them via
// skein_theme(action="describe")
// almost immediately after the frontend posts them), so cleanup is purely
// hygienic.
const inspectCleanupInterval = 60 * time.Second

// InspectSelection is the wire shape POST /api/v1/me/theme/inspect
// accepts and skein_theme(action="describe") re-emits under
// selectedTargets. The
// frontend captures it in the browser inspector overlay; the agent reads
// it to know "the user is staring at THIS element right now" without
// asking for a description.
//
// ComputedStyle is left as map[string]any so the frontend can grow it
// (e.g. add lineHeight, padding) without a backend change. Keys we
// currently expect: color, fontFamily, fontSize, fontWeight,
// backgroundColor — but accept-anything is the deliberate contract.
type InspectSelection struct {
	// Selector is the most-specific selector the inspector picked,
	// already trimmed. Non-empty by handler validation.
	Selector string `json:"selector"`

	// Parents is the chain of parent selectors (closest first). Empty
	// for a top-level pick. We keep them so the agent can reason about
	// "what container is this in".
	Parents []string `json:"parents"`

	// Tokens is the list of theme variable keys the frontend deduced
	// from the selector + class chain. May be empty when the frontend
	// can't map the click to a single token — the agent can still see
	// the computed style and decide which variable to nudge.
	Tokens []string `json:"tokens"`

	// ComputedStyle is the snapshot of getComputedStyle() at click time.
	// Free-form on the wire — see the package doc.
	ComputedStyle map[string]any `json:"computedStyle"`

	// CapturedAt is set by the cache.Set call (not the client) so the
	// agent sees server-canonical time.
	CapturedAt time.Time `json:"capturedAt"`
}

// InspectCache is a process-local in-memory store of the user's recent
// "I am looking at THIS element" hints. Multi-entry per user — each
// inspector click appends a new selection (oldest evicted FIFO at the
// per-user cap). TTL-bounded so a long-idle user doesn't leak stale
// selections into the agent's context.
//
// Thread-safety: every operation takes the embedded sync.RWMutex.
// Cleanup runs in a separate goroutine driven by StartCleanup.
//
// Persistence: NONE. A pod restart wipes every selection. This is
// intentional — selections are ephemeral UI state, not data. The
// frontend re-posts on the next inspector click.
//
// sk-10.6 — switched from single-entry-per-user (overwrite-on-write)
// to a bounded list (append-on-write, FIFO eviction at max). The agent
// is expected to call skein_theme(action="inspect_clear") after
// processing the
// list so it does not pile up across sessions.
type InspectCache struct {
	mu   sync.RWMutex
	ttl  time.Duration
	max  int
	data map[uuid.UUID][]InspectSelection
}

// NewInspectCache returns a fresh cache with the supplied TTL and the
// default per-user cap (InspectCacheDefaultMax). Pass
// InspectCacheDefaultTTL in production wiring; tests pass shorter
// values to exercise expiry without time.Sleep'ing a real ten minutes.
func NewInspectCache(ttl time.Duration) *InspectCache {
	return NewInspectCacheWithMax(ttl, InspectCacheDefaultMax)
}

// NewInspectCacheWithMax is the test-tunable constructor — accepts an
// explicit per-user cap so the FIFO-eviction tests don't have to set
// twenty entries to exercise the trim path. Production wiring uses
// NewInspectCache which delegates here with InspectCacheDefaultMax.
func NewInspectCacheWithMax(ttl time.Duration, max int) *InspectCache {
	if ttl <= 0 {
		ttl = InspectCacheDefaultTTL
	}
	if max <= 0 {
		max = InspectCacheDefaultMax
	}
	return &InspectCache{
		ttl:  ttl,
		max:  max,
		data: make(map[uuid.UUID][]InspectSelection),
	}
}

// Set appends a new selection to the user's list. CapturedAt is stamped
// server-side so the wire value reflects the server's clock — clients
// with skewed clocks can't backdate selections to extend the effective
// TTL. When the list length exceeds the cap, the OLDEST entries are
// dropped (FIFO) so the agent always sees the most-recent window.
func (c *InspectCache) Set(userID uuid.UUID, sel InspectSelection) {
	if userID == uuid.Nil {
		return
	}
	sel.CapturedAt = time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	list := append(c.data[userID], sel)
	// FIFO trim — drop the oldest leading entries so length == max.
	// We allocate a fresh slice to release the underlying array of the
	// dropped prefix (otherwise the array stays pinned for the lifetime
	// of the cache).
	if len(list) > c.max {
		trimmed := make([]InspectSelection, c.max)
		copy(trimmed, list[len(list)-c.max:])
		list = trimmed
	}
	c.data[userID] = list
}

// Get returns the user's non-expired selections, oldest first.
//
// Return semantics:
//
//   - (nil, false) when the user has NEVER posted a selection (no map
//     entry exists at all). Distinguishes "user never clicked" from
//     "user clicked but all entries expired".
//   - ([]InspectSelection{}, true) when the user has a map entry but
//     every selection is past TTL (the sweeper will drop the empty key
//     on its next tick).
//   - (slice, true) for the normal case. Slice is a fresh copy filtered
//     for TTL; callers may mutate it without affecting the cache.
//
// RLock-only — TTL filtering does NOT persist back to the map (sweep
// handles eviction). The RLock is held across the FILTER LOOP, not just
// the map lookup: releasing it earlier would race with sweep's in-place
// `entries[:0]; kept = append(kept, e)` compaction, which writes to
// positions Get is iterating. Holding the RLock costs microseconds on
// the cheap read path; sweep runs once per minute and contention is
// effectively zero.
func (c *InspectCache) Get(userID uuid.UUID) ([]InspectSelection, bool) {
	if userID == uuid.Nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries, ok := c.data[userID]
	if !ok {
		return nil, false
	}
	now := time.Now().UTC()
	cutoff := now.Add(-c.ttl)
	// Filter non-expired entries into a fresh slice — also serves as a
	// snapshot so the caller can't mutate the cache's backing array.
	out := make([]InspectSelection, 0, len(entries))
	for _, e := range entries {
		if e.CapturedAt.After(cutoff) || e.CapturedAt.Equal(cutoff) {
			out = append(out, e)
		}
	}
	return out, true
}

// Clear removes every selection for the user and returns the count of
// dropped entries. Used by skein_theme(action="inspect_clear") so the
// agent can flush the list after processing it (otherwise selections
// pile up across describe calls).
//
// Idempotent — clearing a user with no entry returns 0.
func (c *InspectCache) Clear(userID uuid.UUID) int {
	if userID == uuid.Nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, ok := c.data[userID]
	if !ok {
		return 0
	}
	delete(c.data, userID)
	return len(entries)
}

// Len returns the TOTAL number of selections across every user
// (sum of per-user list lengths), including expired-but-not-yet-swept
// entries. Used by tests to assert append/eviction/sweep behaviour.
//
// sk-10.6 migration: pre-multi-entry this returned the number of users
// with a resident entry (which collapsed to "0 or 1 per user"). Now it
// counts selections — a single user with three clicks reports Len()==3.
func (c *InspectCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	total := 0
	for _, list := range c.data {
		total += len(list)
	}
	return total
}

// StartCleanup spawns a background goroutine that wakes every
// inspectCleanupInterval and drops entries whose age exceeds ttl.
// Lifetime is tied to ctx — when ctx.Done fires the goroutine returns
// after the in-flight sweep.
//
// Single goroutine per cache. Calling twice spawns two reapers; tests
// should construct a fresh cache instead.
func (c *InspectCache) StartCleanup(ctx context.Context) {
	go c.cleanupLoop(ctx)
}

// cleanupLoop is the actual reaper. Separated from StartCleanup so a
// test could drive a single sweep synchronously by calling sweep()
// directly (kept private to avoid exposing the timing surface).
func (c *InspectCache) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(inspectCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed := c.sweep()
			if removed > 0 {
				slog.DebugContext(ctx, "inspect cache sweep",
					"removed", removed)
			}
		}
	}
}

// sweep walks every user's list, drops expired selections, and removes
// the map key when the resulting list is empty. Returns the total
// number of dropped entries across all users for the debug log line.
func (c *InspectCache) sweep() int {
	now := time.Now().UTC()
	cutoff := now.Add(-c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for uid, entries := range c.data {
		kept := entries[:0]
		for _, e := range entries {
			if e.CapturedAt.After(cutoff) || e.CapturedAt.Equal(cutoff) {
				kept = append(kept, e)
			} else {
				removed++
			}
		}
		if len(kept) == 0 {
			delete(c.data, uid)
		} else {
			c.data[uid] = kept
		}
	}
	return removed
}
