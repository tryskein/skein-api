package handler

import (
	"sync"
	"time"
)

// loginLimiter is a small in-memory, per-pod fixed-window rate limiter.
// Originally built to backstop /api/v1/auth/login (sk-163 S9); sk-350.2
// reuses the SAME struct (a second instance, its own thresholds) to throttle
// POST /api/v1/me/password — see the passwordChangeLimiter* constants and
// UsersHandler.pwChangeLimiter in users.go. Two independent consumers, one
// tested bucket/window/capacity implementation; the doc comment below still
// describes the login instance's two-bucket shape because that's the
// richer case — the password-change instance uses only the first (single)
// key and never engages the second.
//
// For /auth/login specifically: DEFENSE IN DEPTH, not the primary control —
// the BFF (skein-web LoginRateLimiter, Redis-backed and shared across pods)
// is the first line. This exists so that even if the BFF is bypassed — a
// second caller that holds the internal secret hitting the API directly —
// the password endpoint is never an unbounded brute-force oracle. That
// matters most for the break-glass admin (sk-163 S4a): a single privileged
// email that can password-login even under active OIDC.
//
// Two buckets, both consulted before any DB lookup or bcrypt so a throttled
// caller costs ~nothing:
//   - per-email: a single account locked after emailThreshold failures.
//   - per-IP:    a single client IP locked after ipThreshold failures across
//     ANY mix of emails (the spray vector). The IP comes ONLY from the
//     BFF-forwarded X-Skein-Client-Ip (trusted proxy chain) — there is
//     deliberately no RemoteAddr fallback (see loginClientIP).
//
// The break-glass email is NOT special-cased: it fills its per-email bucket
// like any other address and is throttled identically.
//
// Per-pod state is acceptable for a backstop: the BFF provides the
// cluster-wide guarantee; a horizontally-scaled API only divides an
// attacker's per-pod budget, never removes it.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*loginBucket

	emailThreshold int
	ipThreshold    int
	window         time.Duration

	// now is injectable so tests can advance the clock deterministically.
	now func() time.Time

	// maxBuckets is a HARD cap on the number of live buckets. Before inserting
	// a new key at the cap, expired buckets are pruned and, if that is not
	// enough, the bucket closest to expiry is evicted — so the map can never
	// grow past maxBuckets even under high live-key cardinality (a flood of
	// distinct emails/IPs). Login volume is low, so this rarely fires.
	maxBuckets int
}

// loginBucket is a fixed-window counter: count failures until resetAt, after
// which the window starts fresh on the next failure.
type loginBucket struct {
	count   int
	resetAt time.Time
}

// newLoginLimiter builds a limiter with the supplied thresholds and window.
// emailThreshold/ipThreshold are the failure counts that, once reached, cause
// the NEXT attempt to be blocked.
func newLoginLimiter(emailThreshold, ipThreshold int, window time.Duration) *loginLimiter {
	return &loginLimiter{
		buckets:        make(map[string]*loginBucket),
		emailThreshold: emailThreshold,
		ipThreshold:    ipThreshold,
		window:         window,
		now:            time.Now,
		maxBuckets:     4096,
	}
}

// blocked reports whether this (email, ip) pair should be rejected with 429
// BEFORE spending a DB round-trip or a bcrypt verify. ip may be empty (no
// trustworthy IP available), in which case only the per-email bucket applies.
func (l *loginLimiter) blocked(email, ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.bucketTrips(emailKey(email), l.emailThreshold, now) {
		return true
	}
	if ip != "" && l.bucketTrips(ipKey(ip), l.ipThreshold, now) {
		return true
	}
	return false
}

// bucketTrips returns true when the named bucket exists, has not expired, and
// has reached its threshold. Read-only — does not mutate or revive a bucket.
func (l *loginLimiter) bucketTrips(key string, threshold int, now time.Time) bool {
	b, ok := l.buckets[key]
	if !ok || !now.Before(b.resetAt) {
		return false
	}
	return b.count >= threshold
}

// recordFailure increments both the per-email and per-IP buckets after a
// failed attempt. Call on every non-success login outcome (unknown email, bad
// password, account-class rejection, ...) so the counters reflect real probe
// pressure regardless of which failure branch fired.
func (l *loginLimiter) recordFailure(email, ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.bump(emailKey(email), now)
	if ip != "" {
		l.bump(ipKey(ip), now)
	}
}

// recordAttempt bumps the given key's bucket unconditionally, regardless of
// the caller's outcome. This is the single-key sibling of recordFailure:
// self-service password change (sk-350.2) wants to bound the number of
// CALLS to an expensive operation, not just failed guesses — a successful
// password change costs exactly as much bcrypt/network work as a rejected
// one (sk-303.20 raised that to up to two bcrypt-cost operations plus an
// optional outbound breach-check call), so success must count toward the
// budget too. It reuses the same bump/ensureCapacity machinery as
// recordFailure; only the accounting policy differs (what counts, and
// whether a success resets the bucket — it doesn't, here), which is why
// this is a separate method rather than calling recordFailure with a
// misleading name on a success path.
func (l *loginLimiter) recordAttempt(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// emailKey, not the raw key: blocked()/bucketTrips() look this key up
	// via emailKey(key) too (recordAttempt's callers always pass "" as the
	// ip half — see UsersHandler.pwChangeLimiter), so the namespacing
	// prefix must match on both the write and the read side.
	l.bump(emailKey(key), l.now())
}

// tryAcquire atomically checks key's bucket against emailThreshold AND, if
// still under it, reserves the call by bumping the bucket — check-and-
// increment under the SAME lock acquisition, not two separate ones.
// Returns true when the call is admitted (the bucket now reflects it),
// false when the bucket was already at or over threshold (unchanged).
//
// This exists for callers like ChangeMyPassword that used to call
// blocked() at the top of the handler and recordAttempt() only after the
// expensive work (bcrypt verify+hash, optionally a pwned-passwords
// network call) finished. Between those two separate lock acquisitions,
// EVERY concurrent caller observes the bucket still under threshold — none
// of them has recorded yet — so N simultaneous requests all pass the
// check and all pay the expensive-work cost before a single one is
// counted: a fixed-window cap bounds sequential abuse but not a
// concurrent burst from one identity. tryAcquire closes that window by
// making "may this call proceed" and "count this call" the same atomic
// step, so at most emailThreshold callers can ever be admitted inside one
// window regardless of how many arrive at once.
//
// Deliberately NOT used by /auth/login: that limiter counts FAILURES only
// (recordFailure, reset on success via resetEmail — see the type's doc
// comment) and needs the DB/bcrypt outcome before it knows whether to
// count anything at all, so its check and its record cannot be the same
// atomic step no matter how this method is shaped. tryAcquire's
// unconditional "count on admission, not on outcome" policy is specific
// to the sk-350.2 self-service password-change accounting (recordAttempt
// unconditionally counts too — see its own doc comment), so it reuses
// recordAttempt's emailKey namespacing rather than adding a third one.
func (l *loginLimiter) tryAcquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	k := emailKey(key)
	if l.bucketTrips(k, l.emailThreshold, now) {
		return false
	}
	l.bump(k, now)
	return true
}

// bump increments a bucket, starting a fresh window when the bucket is absent
// or expired. Inserting a brand-new key first enforces the capacity bound.
func (l *loginLimiter) bump(key string, now time.Time) {
	if b, ok := l.buckets[key]; ok && now.Before(b.resetAt) {
		b.count++
		return
	}
	// New key (or an expired bucket being recreated): enforce the cap on the
	// insert so the map size is bounded regardless of key cardinality.
	if _, exists := l.buckets[key]; !exists {
		l.ensureCapacity(now)
	}
	l.buckets[key] = &loginBucket{count: 1, resetAt: now.Add(l.window)}
}

// ensureCapacity guarantees there is room for one more key without exceeding
// maxBuckets: prune expired buckets first, then — if still at the cap with all
// buckets live — evict the one closest to expiry (it would lapse soonest, so
// dropping it loses the least throttle state).
func (l *loginLimiter) ensureCapacity(now time.Time) {
	if len(l.buckets) < l.maxBuckets {
		return
	}
	l.pruneExpired(now)
	for len(l.buckets) >= l.maxBuckets {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, b := range l.buckets {
			if first || b.resetAt.Before(oldest) {
				oldestKey, oldest, first = k, b.resetAt, false
			}
		}
		if first {
			return // map empty — nothing to evict (defensive)
		}
		delete(l.buckets, oldestKey)
	}
}

// resetEmail clears the per-email bucket on a successful login — a user who
// occasionally fumbles their password should not carry the failure count
// forward. The per-IP bucket is deliberately left intact: a single success
// (possibly a credential the sprayer just landed) must not wipe the spray
// signal, and the IP bucket self-heals on the window TTL anyway.
func (l *loginLimiter) resetEmail(email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, emailKey(email))
}

// pruneExpired drops every bucket whose window has elapsed. Called under lock
// from recordFailure when the map grows past maxBuckets.
func (l *loginLimiter) pruneExpired(now time.Time) {
	for k, b := range l.buckets {
		if !now.Before(b.resetAt) {
			delete(l.buckets, k)
		}
	}
}

func emailKey(email string) string { return "e:" + email }
func ipKey(ip string) string       { return "i:" + ip }
