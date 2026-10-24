package accounts

// ratelimit.go — the api-side rate limit of the public account actions
// (sk-674.18 T038, T039; plan K-17, "Samples and their limits").
//
// It is the BACKSTOP: the primary limit of the public routes is skein-web's
// Redis limiter, shared by every pod. This one lives in the memory of one
// pod (the pattern of handler/loginlimiter.go), so that a caller holding the
// internal secret cannot turn signup or the reset request into an unbounded
// oracle even past the BFF.
//
// An action has up to two buckets, each a fixed window:
//
//   - per subject — the action's email, trimmed and lower-cased (or, for a
//     logged-in action, the caller's user id);
//   - per client IP — X-Skein-Client-Ip, stamped by skein-web
//     (handler/auth.go#loginClientIP has the trust argument); an absent IP
//     skips this bucket, the subject bucket still applies.
//
// An action whose threshold for a bucket is zero has no such bucket: the
// link routes key on the IP alone (the link carries no email), the
// confirmation request on the user alone (E-26).
//
// Unlike the login limiter, which counts failures, these count ATTEMPTS: a
// signup that succeeds costs the same as one that is refused — the thing
// limited is how often the action can be asked for. An attempt that is
// refused here costs nothing: both buckets are checked, and only when both
// admit it are both counted. Nothing of the request body other than the
// email takes part in the key.

import (
	"strings"
	"sync"
	"time"
)

// Action is one rate-limited public action and its thresholds.
type Action struct {
	// Name keys the buckets; two actions never share a bucket.
	Name string
	// PerEmail is how many attempts one email (subject) may make per
	// Window; 0 = no subject bucket.
	PerEmail int
	// PerIP is how many attempts one client IP may make per Window; 0 = no
	// IP bucket.
	PerIP int
	// Window is the length of the fixed window.
	Window time.Duration
}

// The thresholds are far above what a person filling in a form reaches and
// low enough that a scripted caller gets a few attempts per window.
var (
	// ActionSignup is POST /api/v1/auth/signup.
	ActionSignup = Action{Name: "signup", PerEmail: 5, PerIP: 20, Window: time.Hour}
	// ActionUsernameSuggestions is GET /api/v1/auth/username-suggestions —
	// pressed by hand, a few times per form.
	ActionUsernameSuggestions = Action{Name: "username_suggestions", PerEmail: 20, PerIP: 60, Window: 10 * time.Minute}
)

// RateLimiter is the in-pod limiter of the public actions. Safe for
// concurrent use.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
	// now is injectable so tests drive the clock.
	now func() time.Time
	// maxBuckets is a hard cap on live buckets: at the cap the expired ones
	// are pruned, then the one closest to expiry is evicted, so a flood of
	// distinct emails cannot grow the map without bound.
	maxBuckets int
}

type rateBucket struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter builds a limiter on the wall clock.
func NewRateLimiter() *RateLimiter {
	return NewRateLimiterWithClock(time.Now)
}

// NewRateLimiterWithClock builds a limiter reading time from now.
func NewRateLimiterWithClock(now func() time.Time) *RateLimiter {
	return &RateLimiter{buckets: make(map[string]*rateBucket), now: now, maxBuckets: 8192}
}

// Allow reports whether one more attempt of action for subject from ip is
// admitted, and counts it when it is. subject is normalised here; an empty
// ip skips the IP bucket.
func (l *RateLimiter) Allow(action Action, subject, ip string) bool {
	type bucket struct {
		key       string
		threshold int
	}
	var keys []bucket
	if action.PerEmail > 0 {
		keys = append(keys, bucket{action.Name + "|subject|" + strings.ToLower(strings.TrimSpace(subject)), action.PerEmail})
	}
	if ip = strings.TrimSpace(ip); ip != "" && action.PerIP > 0 {
		keys = append(keys, bucket{action.Name + "|ip|" + ip, action.PerIP})
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, b := range keys {
		if l.full(b.key, b.threshold, now) {
			return false
		}
	}
	for _, b := range keys {
		l.count(b.key, action.Window, now)
	}
	return true
}

// full reports whether key has used up threshold attempts in its live
// window.
func (l *RateLimiter) full(key string, threshold int, now time.Time) bool {
	b, ok := l.buckets[key]
	if !ok || !now.Before(b.resetAt) {
		return false
	}
	return b.count >= threshold
}

func (l *RateLimiter) count(key string, window time.Duration, now time.Time) {
	b, ok := l.buckets[key]
	if ok && now.Before(b.resetAt) {
		b.count++
		return
	}
	if !ok {
		l.makeRoom(now)
	}
	l.buckets[key] = &rateBucket{count: 1, resetAt: now.Add(window)}
}

// makeRoom keeps the map under maxBuckets before an insert.
func (l *RateLimiter) makeRoom(now time.Time) {
	if len(l.buckets) < l.maxBuckets {
		return
	}
	for k, b := range l.buckets {
		if !now.Before(b.resetAt) {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < l.maxBuckets {
		return
	}
	var (
		oldest   string
		earliest time.Time
	)
	for k, b := range l.buckets {
		if oldest == "" || b.resetAt.Before(earliest) {
			oldest, earliest = k, b.resetAt
		}
	}
	delete(l.buckets, oldest)
}
