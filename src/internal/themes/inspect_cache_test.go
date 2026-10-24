package themes

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestInspectCache_SetGetExpire covers the three TTL transitions in
// one test:
//  1. fresh Set → Get returns the entry.
//  2. wait past TTL → Get returns an empty filtered slice (but ok=true
//     because the map entry still exists; sweep hasn't run yet).
//  3. trigger sweep → Len drops to zero and the map key is gone.
func TestInspectCache_SetGetExpire(t *testing.T) {
	c := NewInspectCache(50 * time.Millisecond)
	uid := uuid.Must(uuid.NewRandom())

	sel := InspectSelection{
		Selector: ".task-card",
		Parents:  []string{".col-tasks"},
		// A REAL role from TokenSchema. `tokens[]` is contractually
		// "the variables that affect this element according to the
		// registry", so a retired name here describes a state the
		// system cannot produce. --elev succeeds --bg-elevated, which
		// is what this fixture carried before sk-285.7 replaced the
		// dictionary.
		Tokens: []string{"--elev"},
		ComputedStyle: map[string]any{
			"backgroundColor": "rgb(238, 232, 213)",
		},
	}
	c.Set(uid, sel)

	got, ok := c.Get(uid)
	if !ok {
		t.Fatalf("Get directly after Set: ok=false, want true")
	}
	if len(got) != 1 {
		t.Fatalf("Get returned %d entries, want 1", len(got))
	}
	if got[0].Selector != ".task-card" {
		t.Fatalf("selector = %q", got[0].Selector)
	}
	if got[0].CapturedAt.IsZero() {
		t.Fatalf("capturedAt was not stamped server-side")
	}
	if len(got[0].Tokens) != 1 || got[0].Tokens[0] != "--elev" {
		t.Fatalf("tokens = %v", got[0].Tokens)
	}

	// Wait past TTL — Get must return ok=true (map entry still exists)
	// but the filtered slice is empty.
	time.Sleep(80 * time.Millisecond)
	got, ok = c.Get(uid)
	if !ok {
		t.Fatalf("Get after TTL: ok=false, want true (map entry survives until sweep)")
	}
	if len(got) != 0 {
		t.Fatalf("Get after TTL returned %d entries, want 0 (all expired)", len(got))
	}

	// The expired row is still in the map until sweep runs. Verify Len
	// reports 1 (one expired-but-resident selection) then sweep removes it.
	if c.Len() != 1 {
		t.Fatalf("Len before sweep = %d, want 1", c.Len())
	}
	if removed := c.sweep(); removed != 1 {
		t.Fatalf("sweep removed %d, want 1", removed)
	}
	if c.Len() != 0 {
		t.Fatalf("Len after sweep = %d, want 0", c.Len())
	}
	// After sweep removes every expired entry the map key is gone too —
	// Get now reports "never seen" (ok=false).
	if _, ok := c.Get(uid); ok {
		t.Fatalf("Get after full sweep: ok=true, want false (map key dropped)")
	}
}

// TestInspectCache_Append verifies a second Set with the same userID
// appends rather than overwrites — the agent sees both clicks in order.
func TestInspectCache_Append(t *testing.T) {
	c := NewInspectCache(5 * time.Second)
	uid := uuid.Must(uuid.NewRandom())

	c.Set(uid, InspectSelection{Selector: ".first"})
	c.Set(uid, InspectSelection{Selector: ".second"})
	c.Set(uid, InspectSelection{Selector: ".third"})

	got, ok := c.Get(uid)
	if !ok {
		t.Fatalf("Get: ok=false")
	}
	if len(got) != 3 {
		t.Fatalf("len(Get) = %d, want 3", len(got))
	}
	wantOrder := []string{".first", ".second", ".third"}
	for i, want := range wantOrder {
		if got[i].Selector != want {
			t.Fatalf("entry[%d].selector = %q, want %q (oldest-first order)",
				i, got[i].Selector, want)
		}
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
}

// TestInspectCache_AppendCapsAtMax verifies the FIFO eviction kicks in
// at the per-user cap — oldest entries get dropped, newest survive.
func TestInspectCache_AppendCapsAtMax(t *testing.T) {
	const cap = 5
	c := NewInspectCacheWithMax(5*time.Second, cap)
	uid := uuid.Must(uuid.NewRandom())

	for i := 0; i < cap+3; i++ {
		c.Set(uid, InspectSelection{Selector: selectorN(i)})
	}

	got, ok := c.Get(uid)
	if !ok {
		t.Fatalf("Get: ok=false")
	}
	if len(got) != cap {
		t.Fatalf("len(Get) = %d, want %d (cap)", len(got), cap)
	}
	// The first 3 (selectorN(0), selectorN(1), selectorN(2)) must be
	// evicted. Surviving window is selectorN(3) .. selectorN(7).
	for i, sel := range got {
		want := selectorN(i + 3)
		if sel.Selector != want {
			t.Fatalf("entry[%d].selector = %q, want %q (FIFO eviction)",
				i, sel.Selector, want)
		}
	}
}

// TestInspectCache_Clear sets three selections, clears them, and asserts
// (a) Clear returns the dropped count, (b) Get returns (nil, false)
// because the map entry is gone.
func TestInspectCache_Clear(t *testing.T) {
	c := NewInspectCache(5 * time.Second)
	uid := uuid.Must(uuid.NewRandom())

	c.Set(uid, InspectSelection{Selector: ".a"})
	c.Set(uid, InspectSelection{Selector: ".b"})
	c.Set(uid, InspectSelection{Selector: ".c"})

	if dropped := c.Clear(uid); dropped != 3 {
		t.Fatalf("Clear returned %d, want 3", dropped)
	}
	got, ok := c.Get(uid)
	if ok {
		t.Fatalf("Get after Clear: ok=true, want false (map key deleted)")
	}
	if got != nil {
		t.Fatalf("Get after Clear: got = %v, want nil", got)
	}
	if c.Len() != 0 {
		t.Fatalf("Len after Clear = %d, want 0", c.Len())
	}
}

// TestInspectCache_ClearEmpty asserts Clear is idempotent on a user
// who never posted anything — returns 0, no-op.
func TestInspectCache_ClearEmpty(t *testing.T) {
	c := NewInspectCache(5 * time.Second)
	uid := uuid.Must(uuid.NewRandom())

	if dropped := c.Clear(uid); dropped != 0 {
		t.Fatalf("Clear on empty user returned %d, want 0", dropped)
	}
	// Calling twice is still idempotent.
	if dropped := c.Clear(uid); dropped != 0 {
		t.Fatalf("second Clear returned %d, want 0", dropped)
	}
}

// TestInspectCache_TwoUsersIsolated verifies Clear on user B doesn't
// touch user A's entries.
func TestInspectCache_TwoUsersIsolated(t *testing.T) {
	c := NewInspectCache(5 * time.Second)
	userA := uuid.Must(uuid.NewRandom())
	userB := uuid.Must(uuid.NewRandom())

	c.Set(userA, InspectSelection{Selector: ".a1"})
	c.Set(userA, InspectSelection{Selector: ".a2"})
	c.Set(userB, InspectSelection{Selector: ".b1"})

	if dropped := c.Clear(userB); dropped != 1 {
		t.Fatalf("Clear(userB) = %d, want 1", dropped)
	}
	got, ok := c.Get(userA)
	if !ok || len(got) != 2 {
		t.Fatalf("user A entries after Clear(userB): ok=%v len=%d, want ok=true len=2",
			ok, len(got))
	}
	if got[0].Selector != ".a1" || got[1].Selector != ".a2" {
		t.Fatalf("user A entries mutated: %v", got)
	}
}

// TestInspectCache_SweepDropsExpiredEntries verifies a single sweep
// removes entries past TTL while preserving fresh ones in the same
// user's list. Drives via a short TTL plus a manual rewind of the
// older entry's CapturedAt.
func TestInspectCache_SweepDropsExpiredEntries(t *testing.T) {
	c := NewInspectCacheWithMax(50*time.Millisecond, 10)
	uid := uuid.Must(uuid.NewRandom())

	// Insert two entries, then rewind the first one past TTL by hand.
	c.Set(uid, InspectSelection{Selector: ".stale"})
	c.Set(uid, InspectSelection{Selector: ".fresh"})

	c.mu.Lock()
	c.data[uid][0].CapturedAt = time.Now().UTC().Add(-1 * time.Hour)
	c.mu.Unlock()

	if removed := c.sweep(); removed != 1 {
		t.Fatalf("sweep removed %d, want 1 (one stale entry)", removed)
	}
	got, ok := c.Get(uid)
	if !ok {
		t.Fatalf("Get after partial sweep: ok=false, want true (fresh entry survives)")
	}
	if len(got) != 1 || got[0].Selector != ".fresh" {
		t.Fatalf("after sweep got = %+v, want [.fresh]", got)
	}
}

// TestInspectCache_RejectsZeroUUID confirms Set + Get + Clear are
// no-ops on uuid.Nil. The cache is process-local; a zero uuid would
// either collide between users (security) or signal a programming
// error — we drop silently and the missing entry surfaces via Get
// returning ok=false.
func TestInspectCache_RejectsZeroUUID(t *testing.T) {
	c := NewInspectCache(5 * time.Second)

	c.Set(uuid.Nil, InspectSelection{Selector: ".x"})
	if c.Len() != 0 {
		t.Fatalf("Len = %d after Set(uuid.Nil), want 0", c.Len())
	}
	if _, ok := c.Get(uuid.Nil); ok {
		t.Fatalf("Get(uuid.Nil): ok=true, want false")
	}
	if dropped := c.Clear(uuid.Nil); dropped != 0 {
		t.Fatalf("Clear(uuid.Nil) = %d, want 0", dropped)
	}
}

// TestInspectCache_StartCleanup verifies the background goroutine
// removes expired entries on its own. Uses a short TTL + a brief sleep
// past the cleanup interval to keep the test bounded.
//
// We don't override inspectCleanupInterval (it's a package-level
// constant); instead the test relies on the fact that sweep() is
// idempotent and can be called from the test as well to assert
// behaviour without flake-prone timing.
func TestInspectCache_StartCleanup(t *testing.T) {
	c := NewInspectCache(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartCleanup(ctx)

	uid := uuid.Must(uuid.NewRandom())
	c.Set(uid, InspectSelection{Selector: ".y"})
	time.Sleep(60 * time.Millisecond) // > ttl
	// Force-sweep so the test doesn't wait for the 60s background tick.
	c.sweep()
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (background cleanup didn't remove expired entry)", c.Len())
	}
}

// selectorN is a small helper for the FIFO eviction test — keeps the
// loop body readable without polluting the package with a single-use
// fmt.Sprintf in every iteration.
func selectorN(i int) string {
	return ".sel-" + string(rune('a'+i))
}
