package themes

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testClock is a controlled clock for the cache.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestCache(l InspectLimits) (*InspectCache, *testClock) {
	clk := &testClock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	c := NewInspectCache(l)
	c.now = clk.now
	return c, clk
}

func selectors(t *testing.T, raws []json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0, len(raws))
	for _, r := range raws {
		var s InspectSelection
		if err := json.Unmarshal(r, &s); err != nil {
			t.Fatalf("stored entry %s: %v", r, err)
		}
		out = append(out, s.Selector)
	}
	return out
}

func mustSet(t *testing.T, c *InspectCache, u uuid.UUID, sel string) {
	t.Helper()
	if err := c.Set(u, InspectSelection{Selector: sel, Parents: []string{}, Tokens: []string{}, ComputedStyle: map[string]string{}}); err != nil {
		t.Fatalf("Set %s: %v", sel, err)
	}
}

// One stored entry is the compact JSON of the selection with the server's
// capturedAt; Get returns the user's entries oldest first, only his own.
func TestInspectCache_StoresCompactCopyPerUser(t *testing.T) {
	c, clk := newTestCache(DefaultInspectLimits())
	a, b := uuid.New(), uuid.New()
	if err := c.Set(a, InspectSelection{
		Selector: ".card", Parents: []string{".col"}, Tokens: []string{"--fg"},
		ComputedStyle: map[string]string{"color": "rgb(1, 2, 3)"},
		CapturedAt:    time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), // client value is ignored
	}); err != nil {
		t.Fatal(err)
	}
	mustSet(t, c, a, ".second")
	mustSet(t, c, b, ".other")

	got := c.Get(a)
	if s := selectors(t, got); strings.Join(s, ",") != ".card,.second" {
		t.Fatalf("user a sees %v, want [.card .second]", s)
	}
	want := `{"selector":".card","parents":[".col"],"tokens":["--fg"],"computedStyle":{"color":"rgb(1, 2, 3)"},"capturedAt":"` +
		clk.now().Format(time.RFC3339Nano) + `"}`
	if string(got[0]) != want {
		t.Fatalf("stored copy\n%s\nwant\n%s", got[0], want)
	}
	if s := selectors(t, c.Get(b)); strings.Join(s, ",") != ".other" {
		t.Fatalf("user b sees %v, want [.other]", s)
	}
	if n := len(c.Get(uuid.New())); n != 0 {
		t.Fatalf("a user without clicks sees %d entries", n)
	}
}

// The per-user cap drops that user's oldest click only.
func TestInspectCache_PerUserCapDropsOldest(t *testing.T) {
	l := DefaultInspectLimits()
	l.MaxEntriesPerUser = 3
	c, _ := newTestCache(l)
	a, b := uuid.New(), uuid.New()
	mustSet(t, c, b, ".b1")
	for _, s := range []string{".a1", ".a2", ".a3", ".a4"} {
		mustSet(t, c, a, s)
	}
	if s := selectors(t, c.Get(a)); strings.Join(s, ",") != ".a2,.a3,.a4" {
		t.Fatalf("user a %v, want [.a2 .a3 .a4]", s)
	}
	if s := selectors(t, c.Get(b)); strings.Join(s, ",") != ".b1" {
		t.Fatalf("user b %v, want untouched [.b1]", s)
	}
}

// An entry is hidden exactly when its TTL has passed, and a write frees
// expired entries first.
func TestInspectCache_TTL(t *testing.T) {
	l := DefaultInspectLimits()
	c, clk := newTestCache(l)
	u := uuid.New()
	mustSet(t, c, u, ".old")
	clk.advance(l.TTL)
	if n := len(c.Get(u)); n != 1 {
		t.Fatalf("at exactly TTL the entry is gone (%d), want still visible", n)
	}
	clk.advance(time.Nanosecond)
	if n := len(c.Get(u)); n != 0 {
		t.Fatalf("past TTL %d entries visible, want 0", n)
	}
	before := c.Bytes()
	mustSet(t, c, u, ".new")
	if c.Len() != 1 || c.Bytes() >= 2*before {
		t.Fatalf("after a write past TTL: len %d, bytes %d; the expired entry was not freed", c.Len(), c.Bytes())
	}
	clk.advance(l.TTL + time.Second)
	if removed := c.sweep(); removed != 1 || c.Len() != 0 || c.Bytes() != 0 {
		t.Fatalf("sweep removed %d, left len %d bytes %d; want 1, 0, 0", removed, c.Len(), c.Bytes())
	}
}

// The process budget: at exact fill every entry stays; one more click
// evicts the oldest click of ANY user; expired entries go before live ones.
func TestInspectCache_ByteBudget(t *testing.T) {
	probe, _ := newTestCache(DefaultInspectLimits())
	mustSet(t, probe, uuid.New(), ".x0")
	one := probe.Bytes()

	l := DefaultInspectLimits()
	l.CacheMaxBytes = 3 * one
	c, clk := newTestCache(l)
	a, b := uuid.New(), uuid.New()
	mustSet(t, c, a, ".a0")
	clk.advance(time.Second)
	mustSet(t, c, b, ".b0")
	clk.advance(time.Second)
	mustSet(t, c, a, ".a1")
	if c.Len() != 3 || c.Bytes() != 3*one {
		t.Fatalf("at exact fill: len %d bytes %d, want 3 and %d", c.Len(), c.Bytes(), 3*one)
	}
	clk.advance(time.Second)
	mustSet(t, c, b, ".b1") // evicts .a0, the oldest click of all
	if s := selectors(t, c.Get(a)); strings.Join(s, ",") != ".a1" {
		t.Fatalf("user a %v, want [.a1] (oldest .a0 evicted)", s)
	}
	if s := selectors(t, c.Get(b)); strings.Join(s, ",") != ".b0,.b1" {
		t.Fatalf("user b %v, want [.b0 .b1]", s)
	}
	if c.Bytes() > l.CacheMaxBytes {
		t.Fatalf("bytes %d over the budget %d", c.Bytes(), l.CacheMaxBytes)
	}

	// Clear gives the user's bytes back.
	if n := c.Clear(b); n != 2 || c.Bytes() != one || c.Len() != 1 {
		t.Fatalf("Clear: removed %d, bytes %d, len %d; want 2, %d, 1", n, c.Bytes(), one, c.Len())
	}
	if n := c.Clear(b); n != 0 {
		t.Fatalf("second Clear removed %d, want 0", n)
	}
}

// An entry over MaxEntryBytes is refused and evicts nothing.
func TestInspectCache_RefusesOversizedEntryWithoutEviction(t *testing.T) {
	l := DefaultInspectLimits()
	l.MaxEntryBytes = 200
	c, _ := newTestCache(l)
	u := uuid.New()
	mustSet(t, c, u, ".kept")
	err := c.Set(u, InspectSelection{Selector: strings.Repeat("s", 300)})
	if err != ErrInspectEntryTooLarge {
		t.Fatalf("Set of an oversized entry: %v, want ErrInspectEntryTooLarge", err)
	}
	if s := selectors(t, c.Get(u)); strings.Join(s, ",") != ".kept" {
		t.Fatalf("after the refusal %v, want [.kept]", s)
	}
}

// Concurrent writers, readers and clears keep the accounting exact: when
// they are done, the bytes are those of the entries left.
func TestInspectCache_ConcurrentAccounting(t *testing.T) {
	l := DefaultInspectLimits()
	l.CacheMaxBytes = 64 << 10
	c, _ := newTestCache(l)
	users := make([]uuid.UUID, 8)
	for i := range users {
		users[i] = uuid.New()
	}
	var wg sync.WaitGroup
	for i, u := range users {
		wg.Add(1)
		go func(i int, u uuid.UUID) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = c.Set(u, InspectSelection{Selector: strings.Repeat("x", (i*37+j)%300+1)})
				_ = c.Get(u)
				if j%50 == 49 {
					c.Clear(u)
				}
			}
		}(i, u)
	}
	wg.Wait()
	var want int64
	total := 0
	for _, u := range users {
		for _, raw := range c.Get(u) {
			want += InspectEntryCost(len(raw))
			total++
		}
	}
	if c.Bytes() != want || c.Len() != total {
		t.Fatalf("bytes %d len %d, want %d and %d", c.Bytes(), c.Len(), want, total)
	}
	if c.Bytes() > l.CacheMaxBytes {
		t.Fatalf("bytes %d over the budget %d", c.Bytes(), l.CacheMaxBytes)
	}
}

func TestInspectEntryCost(t *testing.T) {
	for n, want := range map[int]int64{1: 513, 2: 514, 3: 516, 512: 1024, 513: 1536, 1000: 1536, 8192: 8704} {
		if got := InspectEntryCost(n); got != want {
			t.Errorf("InspectEntryCost(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestInspectLimits_Validate(t *testing.T) {
	if err := DefaultInspectLimits().Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for name, mut := range map[string]func(*InspectLimits){
		"zero body":            func(l *InspectLimits) { l.MaxBodyBytes = 0 },
		"negative tokens":      func(l *InspectLimits) { l.MaxTokens = -1 },
		"zero ttl":             func(l *InspectLimits) { l.TTL = 0 },
		"describe below entry": func(l *InspectLimits) { l.DescribeMaxBytes = l.MaxEntryBytes + 1 },
		"cache below entry":    func(l *InspectLimits) { l.CacheMaxBytes = InspectEntryCost(l.MaxEntryBytes) - 1 },
	} {
		l := DefaultInspectLimits()
		mut(&l)
		if err := l.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}
