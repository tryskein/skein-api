package themes

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// inspectCleanupInterval is how often the background goroutine sweeps
// expired entries; reads never return an expired one either way.
const inspectCleanupInterval = 60 * time.Second

// ErrInspectEntryTooLarge — the normalized entry (its compact JSON with
// capturedAt) exceeds InspectLimits.MaxEntryBytes; nothing is stored.
var ErrInspectEntryTooLarge = errors.New("inspect entry exceeds the size limit")

// InspectSelection is the wire shape POST /api/v1/me/theme/inspect
// accepts and skein_theme(action="describe") re-emits under
// selectedTargets. The
// frontend captures it in the browser inspector overlay; the agent reads
// it to know "the user is staring at THIS element right now" without
// asking for a description.
//
// ComputedStyle is a flat map of string values, so the frontend can grow it
// (e.g. add lineHeight, padding) without a backend change. Keys we
// currently expect: color, fontFamily, fontSize, fontWeight,
// backgroundColor. Since sk-977.7.10 only string values are accepted, and
// every field is bounded (InspectLimits).
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

	// ComputedStyle is the snapshot of getComputedStyle() at click time:
	// property name → serialized value.
	ComputedStyle map[string]string `json:"computedStyle"`

	// CapturedAt is set by the cache.Set call (not the client) so the
	// agent sees server-canonical time.
	CapturedAt time.Time `json:"capturedAt"`
}

// InspectCache is a process-local in-memory store of the users' recent
// "I am looking at THIS element" hints. Each inspector click appends one
// entry; an entry lives InspectLimits.TTL from its click.
//
// Each entry is kept as an independent compact JSON copy of the validated
// selection with its server-side capturedAt, and is accounted at
// InspectEntryCost of that copy. Three bounds hold at every moment, under
// one lock (sk-977.7.10):
//
//   - at most MaxEntriesPerUser entries per user — a new click drops that
//     user's oldest;
//   - at most CacheMaxBytes accounted bytes per process — a new click first
//     frees expired entries, then the oldest clicks of ALL users, in click
//     order (FIFO: reading an entry never extends its life);
//   - no entry larger than MaxEntryBytes is stored at all, so a large
//     entry never evicts others to make room for itself.
//
// Persistence: NONE. A pod restart wipes every selection: they are
// ephemeral UI state, not data.
type InspectCache struct {
	mu     sync.Mutex
	limits InspectLimits
	now    func() time.Time
	// all holds every entry in click order (oldest first); since every
	// entry has the same TTL this is also expiry order.
	all   *list.List
	users map[uuid.UUID]*list.List
	bytes int64
}

// inspectEntry is one stored click.
type inspectEntry struct {
	user       uuid.UUID
	raw        json.RawMessage
	capturedAt time.Time
	cost       int64
	inAll      *list.Element
	inUser     *list.Element
}

// NewInspectCache returns an empty cache bounded by l (validated by the
// caller, see InspectLimits.Validate).
func NewInspectCache(l InspectLimits) *InspectCache {
	return &InspectCache{
		limits: l,
		now:    time.Now,
		all:    list.New(),
		users:  make(map[uuid.UUID]*list.List),
	}
}

// Limits reports the bounds the cache was built with.
func (c *InspectCache) Limits() InspectLimits { return c.limits }

// Set stores a new click of userID. capturedAt is stamped server-side so a
// client with a skewed clock cannot backdate or extend its selections. The
// stored copy is the compact JSON of sel with capturedAt; when it exceeds
// MaxEntryBytes nothing changes and ErrInspectEntryTooLarge is returned.
func (c *InspectCache) Set(userID uuid.UUID, sel InspectSelection) error {
	if userID == uuid.Nil {
		return nil
	}
	sel.CapturedAt = c.now().UTC()
	raw, err := json.Marshal(sel)
	if err != nil {
		return fmt.Errorf("encoding inspect entry: %w", err)
	}
	if len(raw) > c.limits.MaxEntryBytes {
		return ErrInspectEntryTooLarge
	}
	e := &inspectEntry{user: userID, raw: raw, capturedAt: sel.CapturedAt, cost: InspectEntryCost(len(raw))}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked(sel.CapturedAt)
	if ul := c.users[userID]; ul != nil {
		for ul.Len() >= c.limits.MaxEntriesPerUser {
			c.removeLocked(ul.Front().Value.(*inspectEntry))
			ul = c.users[userID]
			if ul == nil {
				break
			}
		}
	}
	for c.bytes+e.cost > c.limits.CacheMaxBytes && c.all.Len() > 0 {
		c.removeLocked(c.all.Front().Value.(*inspectEntry))
	}
	ul := c.users[userID]
	if ul == nil {
		ul = list.New()
		c.users[userID] = ul
	}
	e.inAll = c.all.PushBack(e)
	e.inUser = ul.PushBack(e)
	c.bytes += e.cost
	return nil
}

// Get returns the user's live entries, oldest first, as their stored
// compact JSON (each an object with selector, parents, tokens,
// computedStyle and capturedAt). The slice is a fresh copy; the JSON values
// are never modified after Set. An empty slice when the user has none.
func (c *InspectCache) Get(userID uuid.UUID) []json.RawMessage {
	out := []json.RawMessage{}
	if userID == uuid.Nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ul := c.users[userID]
	if ul == nil {
		return out
	}
	cutoff := c.now().UTC().Add(-c.limits.TTL)
	for el := ul.Front(); el != nil; el = el.Next() {
		if e := el.Value.(*inspectEntry); !e.capturedAt.Before(cutoff) {
			out = append(out, e.raw)
		}
	}
	return out
}

// Clear removes every entry of the user and returns how many were live or
// expired-but-unswept. Idempotent.
func (c *InspectCache) Clear(userID uuid.UUID) int {
	if userID == uuid.Nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ul := c.users[userID]
	if ul == nil {
		return 0
	}
	n := ul.Len()
	for ul.Len() > 0 {
		c.removeLocked(ul.Front().Value.(*inspectEntry))
		if c.users[userID] == nil {
			break
		}
	}
	return n
}

// Len is the number of stored entries of every user (expired-but-unswept
// included); Bytes their accounted size. For tests and metrics.
func (c *InspectCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.all.Len()
}

// Bytes is the accounted size of every stored entry.
func (c *InspectCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// removeLocked unlinks e from both lists, drops an emptied user list and
// gives its cost back.
func (c *InspectCache) removeLocked(e *inspectEntry) {
	c.all.Remove(e.inAll)
	if ul := c.users[e.user]; ul != nil {
		ul.Remove(e.inUser)
		if ul.Len() == 0 {
			delete(c.users, e.user)
		}
	}
	c.bytes -= e.cost
	e.inAll, e.inUser, e.raw = nil, nil, nil
}

// dropExpiredLocked frees every entry clicked before now - TTL; they sit at
// the front of the click-ordered list.
func (c *InspectCache) dropExpiredLocked(now time.Time) int {
	cutoff := now.Add(-c.limits.TTL)
	removed := 0
	for el := c.all.Front(); el != nil; el = c.all.Front() {
		e := el.Value.(*inspectEntry)
		if !e.capturedAt.Before(cutoff) {
			break
		}
		c.removeLocked(e)
		removed++
	}
	return removed
}

// StartCleanup spawns a background goroutine that frees expired entries
// every inspectCleanupInterval until ctx ends.
func (c *InspectCache) StartCleanup(ctx context.Context) {
	go c.cleanupLoop(ctx)
}

func (c *InspectCache) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(inspectCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if removed := c.sweep(); removed > 0 {
				slog.DebugContext(ctx, "inspect cache sweep", "removed", removed)
			}
		}
	}
}

// sweep frees the expired entries and returns how many.
func (c *InspectCache) sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropExpiredLocked(c.now().UTC())
}
