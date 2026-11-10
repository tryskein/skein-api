package publicapi_test

import (
	"sync"
	"testing"
	"time"

	"skein-api/internal/publicapi"
)

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestWindowLimiter_FixedMinute — row 59 (US-12, D-9). A fixed window of one
// minute per key: N requests pass, request N+1 is refused with Retry-After =
// the seconds until the window ends, rounded up and at least 1 — also on the
// last fraction of a second; a new window starts afresh; keys are
// independent.
func TestWindowLimiter_FixedMinute(t *testing.T) {
	const n = 3
	clock := newFakeClock()
	l := publicapi.NewWindowLimiter(n, time.Minute, 1000, clock.Now)

	for i := 1; i <= n; i++ {
		if ok, _ := l.Take("key-a"); !ok {
			t.Fatalf("request %d of %d refused", i, n)
		}
	}

	clock.Advance(20*time.Second + 300*time.Millisecond)
	ok, retry := l.Take("key-a")
	if ok {
		t.Fatalf("request %d admitted, want refused", n+1)
	}
	if got := publicapi.RetryAfterSeconds(retry); got != 40 {
		t.Errorf("Retry-After %d after 20.3 s of the window, want 40 (39.7 s rounded up)", got)
	}

	if ok, _ := l.Take("key-b"); !ok {
		t.Error("another key refused in the same window: keys are not independent")
	}

	clock.Advance(39*time.Second + 600*time.Millisecond) // 0.1 s left
	ok, retry = l.Take("key-a")
	if ok {
		t.Fatal("admitted 0.1 s before the window ends")
	}
	if got := publicapi.RetryAfterSeconds(retry); got != 1 {
		t.Errorf("Retry-After %d on the last fraction of a second, want 1", got)
	}

	clock.Advance(100 * time.Millisecond) // the window ended
	for i := 1; i <= n; i++ {
		if ok, _ := l.Take("key-a"); !ok {
			t.Fatalf("request %d of the new window refused", i)
		}
	}
	if ok, _ := l.Take("key-a"); ok {
		t.Error("request past the limit of the new window admitted")
	}
}

// TestWindowLimiter_ExceededDoesNotCount — the failed-authentication limit
// reads the window without counting and counts failures with Add.
func TestWindowLimiter_ExceededDoesNotCount(t *testing.T) {
	clock := newFakeClock()
	l := publicapi.NewWindowLimiter(2, time.Minute, 1000, clock.Now)

	for range 5 {
		if over, _ := l.Exceeded("ip"); over {
			t.Fatal("reading the window counted toward it")
		}
	}
	l.Add("ip")
	if over, _ := l.Exceeded("ip"); over {
		t.Fatal("over the limit after 1 of 2 failures")
	}
	l.Add("ip")
	over, retry := l.Exceeded("ip")
	if !over {
		t.Fatal("not over the limit after 2 of 2 failures")
	}
	if got := publicapi.RetryAfterSeconds(retry); got != 60 {
		t.Errorf("Retry-After %d at the start of the window, want 60", got)
	}
	clock.Advance(time.Minute)
	if over, _ := l.Exceeded("ip"); over {
		t.Error("still over the limit after the window ended")
	}
}
