package handler

import (
	"fmt"
	"testing"
	"time"
)

// fixedClock is a mutable clock for deterministic window-expiry tests.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func newTestLimiter(email, ip int, window time.Duration) (*loginLimiter, *fixedClock) {
	clk := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	l := newLoginLimiter(email, ip, window)
	l.now = clk.now
	return l, clk
}

// TestLoginLimiter_PerEmail_ThrottlesAfterThreshold pins that a single account
// is blocked only AFTER threshold failures, and not before.
func TestLoginLimiter_PerEmail_ThrottlesAfterThreshold(t *testing.T) {
	l, _ := newTestLimiter(3, 100, time.Minute)
	const email, ip = "victim@x.io", "203.0.113.7"

	for i := 0; i < 3; i++ {
		if l.blocked(email, ip) {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		l.recordFailure(email, ip)
	}
	if !l.blocked(email, ip) {
		t.Fatal("email should be blocked after reaching the per-email threshold")
	}
	// A different email from the same IP is NOT yet email-blocked (per-IP
	// threshold is high here), proving the buckets are independent.
	if l.blocked("other@x.io", ip) {
		t.Fatal("a different email must not inherit the first email's lockout")
	}
}

// TestLoginLimiter_PerIP_SprayVector pins the spray case: one IP walking many
// DIFFERENT emails, each well below the per-email threshold, still trips the
// per-IP bucket.
func TestLoginLimiter_PerIP_SprayVector(t *testing.T) {
	// per-email threshold high (never trips on a single email); per-IP = 5.
	l, _ := newTestLimiter(100, 5, time.Minute)
	const ip = "198.51.100.42"

	emails := []string{"a@x.io", "b@x.io", "c@x.io", "d@x.io", "e@x.io"}
	for i, e := range emails {
		if l.blocked(e, ip) {
			t.Fatalf("blocked too early on spray attempt %d", i)
		}
		l.recordFailure(e, ip) // one failure per distinct email
	}
	// Sixth distinct email from the same IP must be throttled even though no
	// single email reached its own (high) threshold.
	if !l.blocked("f@x.io", ip) {
		t.Fatal("per-IP spray bucket should be tripped after ipThreshold failures across emails")
	}
	// A DIFFERENT IP trying the same emails is unaffected.
	if l.blocked("a@x.io", "10.0.0.1") {
		t.Fatal("per-IP lockout must not leak to a different client IP")
	}
}

// TestLoginLimiter_BreakGlassEmailIsNotSpecial proves the limiter is purely
// count-based and email-agnostic: a "break-glass"-shaped email is throttled
// exactly like any other address (no allowlist, no exemption).
func TestLoginLimiter_BreakGlassEmailIsNotSpecial(t *testing.T) {
	l, _ := newTestLimiter(2, 100, time.Minute)
	const breakGlass, ip = "breakglass-admin@corp.example", "203.0.113.99"

	l.recordFailure(breakGlass, ip)
	l.recordFailure(breakGlass, ip)
	if !l.blocked(breakGlass, ip) {
		t.Fatal("break-glass email must be throttled like any other after its threshold")
	}
}

// TestLoginLimiter_WindowExpiry pins that the fixed window self-heals: after
// the window elapses, an account is no longer blocked.
func TestLoginLimiter_WindowExpiry(t *testing.T) {
	l, clk := newTestLimiter(2, 100, time.Minute)
	const email, ip = "u@x.io", "203.0.113.7"

	l.recordFailure(email, ip)
	l.recordFailure(email, ip)
	if !l.blocked(email, ip) {
		t.Fatal("should be blocked at threshold")
	}

	clk.t = clk.t.Add(time.Minute + time.Second) // advance past the window
	if l.blocked(email, ip) {
		t.Fatal("lockout should expire after the window elapses")
	}
}

// TestLoginLimiter_ResetEmailKeepsIPBucket pins the success-reset semantics:
// resetEmail clears the per-email bucket but deliberately leaves the per-IP
// (spray) signal intact.
func TestLoginLimiter_ResetEmailKeepsIPBucket(t *testing.T) {
	l, _ := newTestLimiter(2, 3, time.Minute)
	const ip = "198.51.100.7"

	// Fill the IP bucket to its threshold with distinct emails so the IP is
	// blocked but no single email is.
	l.recordFailure("a@x.io", ip)
	l.recordFailure("b@x.io", ip)
	l.recordFailure("c@x.io", ip)
	if !l.blocked("d@x.io", ip) {
		t.Fatal("IP bucket should be tripped")
	}

	// A success on one email resets only that email's bucket; the IP stays hot.
	l.resetEmail("a@x.io")
	if !l.blocked("a@x.io", ip) {
		t.Fatal("resetEmail must NOT clear the per-IP spray lockout")
	}
}

// TestLoginLimiter_EmptyIPSkipsIPBucket pins that an empty IP (no trustworthy
// address) only engages the per-email bucket.
func TestLoginLimiter_EmptyIPSkipsIPBucket(t *testing.T) {
	l, _ := newTestLimiter(100, 1, time.Minute)

	// With ip="", many failures across emails must NOT trip the per-IP bucket.
	l.recordFailure("a@x.io", "")
	l.recordFailure("b@x.io", "")
	if l.blocked("c@x.io", "") {
		t.Fatal("empty IP must not engage the per-IP bucket")
	}
}

// TestLoginLimiter_RecordAttempt_TripsUnconditionally pins recordAttempt's
// contract (sk-350.2): unlike recordFailure, it must count EVERY call —
// success or failure alike — toward the single-key bucket.
func TestLoginLimiter_RecordAttempt_TripsUnconditionally(t *testing.T) {
	l, _ := newTestLimiter(3, 100, time.Minute)
	const key = "user-123"

	for i := 0; i < 3; i++ {
		if l.blocked(key, "") {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		l.recordAttempt(key)
	}
	if !l.blocked(key, "") {
		t.Fatal("key should be blocked after reaching the threshold via recordAttempt")
	}
}

// TestLoginLimiter_MaxBucketsBound pins that the map is HARD-bounded even under
// high LIVE-key cardinality (window long enough that nothing expires) — proving
// pruneExpired alone is not relied on and the oldest-eviction path runs.
func TestLoginLimiter_MaxBucketsBound(t *testing.T) {
	l, _ := newTestLimiter(1000, 1000, time.Hour) // long window: no expiry
	l.maxBuckets = 8

	// 50 distinct emails + 50 distinct IPs = 100 distinct live keys.
	for i := 0; i < 50; i++ {
		l.recordFailure(fmt.Sprintf("e%d@x.io", i), fmt.Sprintf("10.0.0.%d", i))
	}

	if len(l.buckets) > l.maxBuckets {
		t.Fatalf("bucket map exceeded the cap: %d > %d", len(l.buckets), l.maxBuckets)
	}
}
