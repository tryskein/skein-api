package publicapi

// ratelimit.go — the two limits of the public port (plan D-9): requests per
// API key and failed authentications per client address. Both are a fixed
// window of one minute kept in the memory of the replica; the real ceiling
// is the limit times the number of replicas.

import (
	"math"
	"sync"
	"time"
)

// LimitWindow is the window of both limits.
const LimitWindow = time.Minute

// limiterCapacity bounds the number of live windows of one limiter: at the
// cap, an insert first drops the ended windows and, if that frees nothing,
// evicts the window closest to its end.
const limiterCapacity = 100000

// WindowLimiter counts events per key in fixed windows: a key's window starts
// at its first event and lasts LimitWindow; the next event after the end
// starts a new one.
type WindowLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	capacity int
	now      func() time.Time
	windows  map[string]*limitWindow
}

type limitWindow struct {
	count int
	endAt time.Time
}

// NewWindowLimiter builds a limiter allowing limit events per window and
// key. now is the clock (time.Now in production).
func NewWindowLimiter(limit int, window time.Duration, capacity int, now func() time.Time) *WindowLimiter {
	if limit <= 0 {
		panic("publicapi: a limit must be > 0")
	}
	if now == nil {
		now = time.Now
	}
	return &WindowLimiter{
		limit:    limit,
		window:   window,
		capacity: capacity,
		now:      now,
		windows:  make(map[string]*limitWindow),
	}
}

// newProductLimiter is a limiter of the public server.
func newProductLimiter(limit int) *WindowLimiter {
	return NewWindowLimiter(limit, LimitWindow, limiterCapacity, time.Now)
}

// Take counts one event of key when the window still has room and reports
// whether it was admitted; when it was not, retryAfter is the time until the
// window ends.
func (l *WindowLimiter) Take(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.current(key, now)
	if w.count >= l.limit {
		return false, w.endAt.Sub(now)
	}
	w.count++
	return true, 0
}

// Exceeded reports, without counting, whether key used up its window; when
// it did, retryAfter is the time until the window ends.
func (l *WindowLimiter) Exceeded(key string) (exceeded bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w, ok := l.windows[key]
	if !ok || !now.Before(w.endAt) || w.count < l.limit {
		return false, 0
	}
	return true, w.endAt.Sub(now)
}

// Add counts one event of key, whatever the window holds.
func (l *WindowLimiter) Add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.current(key, l.now()).count++
}

// current returns the live window of key, starting a new one when there is
// none or it ended.
func (l *WindowLimiter) current(key string, now time.Time) *limitWindow {
	if w, ok := l.windows[key]; ok && now.Before(w.endAt) {
		return w
	}
	if _, ok := l.windows[key]; !ok && len(l.windows) >= l.capacity {
		l.makeRoom(now)
	}
	w := &limitWindow{endAt: now.Add(l.window)}
	l.windows[key] = w
	return w
}

// makeRoom drops the ended windows; when none ended, it evicts the window
// closest to its end.
func (l *WindowLimiter) makeRoom(now time.Time) {
	var soonest string
	var soonestEnd time.Time
	found := false
	for k, w := range l.windows {
		if !now.Before(w.endAt) {
			delete(l.windows, k)
			continue
		}
		if !found || w.endAt.Before(soonestEnd) {
			soonest, soonestEnd, found = k, w.endAt, true
		}
	}
	if len(l.windows) >= l.capacity && found {
		delete(l.windows, soonest)
	}
}

// RetryAfterSeconds is the Retry-After value for a wait of d: whole seconds,
// rounded up, at least 1 — a client told 0 would retry at once into the same
// window.
func RetryAfterSeconds(d time.Duration) int {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		return 1
	}
	return s
}
