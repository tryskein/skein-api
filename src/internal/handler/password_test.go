package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/users"
)

// sk-303.20 — ChangeMyPassword's two new rejection paths (pwned corpus
// match, over-length new password) and the new GET /me/password/policy
// feature-detection endpoint.

// alwaysBreachedChecker is a users.PwnedChecker test double that always
// reports the candidate password as breached, without any network call.
type alwaysBreachedChecker struct{}

func (alwaysBreachedChecker) Check(_ context.Context, _ string) (bool, error) { return true, nil }

// seedUserWithPassword creates a real user row via usersStore.CreateUser
// (so it gets an actual bcrypt hash of password, not the 'unused' literal
// seedUserRowFull writes) — required for tests that exercise
// ChangeMyPassword's current-password verification. The CreateUser audit
// event has a FK on actor_user_id, so the actor must be an existing row
// (seedUserRow(t, pool, "admin")), not an arbitrary fresh uuid.
func seedUserWithPassword(t *testing.T, pool *pgxpool.Pool, usersStore *users.Store, password string) uuid.UUID {
	t.Helper()
	actorID := seedUserRow(t, pool, "admin")
	id := uuid.Must(uuid.NewRandom())
	uname := "pw" + strings.ReplaceAll(id.String()[:18], "-", "")
	dto, err := usersStore.CreateUser(context.Background(), users.CreateUserInput{
		Email:    uname + "@test.local",
		Username: uname,
		Name:     uname,
		Password: password,
		Role:     "user",
	}, users.HumanActor(actorID, "seed-actor"))
	if err != nil {
		t.Fatalf("seed user with password: %v", err)
	}
	return dto.ID
}

// callChangeMyPassword drives h.ChangeMyPassword with an authenticated
// context and a JSON body.
func callChangeMyPassword(t *testing.T, h *UsersHandler, callerID uuid.UUID, current, next string) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{
		"currentPassword": current,
		"newPassword":     next,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/password", bytes.NewReader(raw))
	ctx := auth.WithUserID(req.Context(), callerID)
	ctx = auth.WithUsername(ctx, "caller")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ChangeMyPassword(rec, req)
	res := rec.Result()
	var out map[string]any
	if res.ContentLength != 0 {
		_ = json.NewDecoder(res.Body).Decode(&out)
	}
	return res, out
}

// TestChangeMyPassword_PwnedRejected_422 — a PwnedChecker that always
// reports the new password as breached must produce 422 password_pwned,
// and must not rotate the hash.
func TestChangeMyPassword_PwnedRejected_422(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", alwaysBreachedChecker{})
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	callerID := seedUserWithPassword(t, pool, usersStore, "oldP@sswd1")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, callerID)
	})

	res, out := callChangeMyPassword(t, h, callerID, "oldP@sswd1", "newP@ssword2")
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%+v", res.StatusCode, out)
	}
	if out["error"] != "password_pwned" {
		t.Fatalf("error = %v, want password_pwned; body=%+v", out["error"], out)
	}
}

// TestChangeMyPassword_TooLong_400 — a new password exceeding bcrypt's
// 72-byte ceiling must be rejected with 400 password_too_long, not a
// generic 500 from an unhandled bcrypt.ErrPasswordTooLong.
func TestChangeMyPassword_TooLong_400(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	callerID := seedUserWithPassword(t, pool, usersStore, "oldP@sswd1")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, callerID)
	})

	tooLong := strings.Repeat("a", 73) // 73 bytes — past passwordMaxBytes
	res, out := callChangeMyPassword(t, h, callerID, "oldP@sswd1", tooLong)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%+v", res.StatusCode, out)
	}
	if out["error"] != "password_too_long" {
		t.Fatalf("error = %v, want password_too_long; body=%+v", out["error"], out)
	}
}

// sk-350.2 — defense-in-depth rate limit on POST /me/password. Three
// behaviours pinned by acceptance criteria: exceeding the budget rejects,
// ordinary frequency is untouched, and a mistyped-then-corrected current
// password (the one extra call every legitimate user occasionally makes)
// is never itself enough to trip the limiter.

// TestChangeMyPassword_ThrottledBeforeStore_NoDB proves the limiter
// short-circuits BEFORE the request body is decoded or the store is
// touched, mirroring TestLogin_ThrottledBeforeBcrypt_NoDB. Built with a nil
// pool/store/projectsStore: if execution ever reached h.store.ChangePassword
// it would panic on the nil store, so a clean 429 proves the pre-check fired
// first.
func TestChangeMyPassword_ThrottledBeforeStore_NoDB(t *testing.T) {
	h := NewUsersHandler(nil, nil, nil)
	callerID := uuid.Must(uuid.NewRandom())
	key := callerID.String()

	for i := 0; i < passwordChangeLimiterThreshold; i++ {
		h.pwChangeLimiter.recordAttempt(key)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ChangeMyPassword reached the nil store instead of throttling first: %v", r)
		}
	}()
	res, out := callChangeMyPassword(t, h, callerID, "whatever", "newP@ssword2")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%+v", res.StatusCode, out)
	}
	if out["error"] != "rate_limited" {
		t.Fatalf("error = %v, want rate_limited; body=%+v", out["error"], out)
	}
}

// TestChangeMyPassword_ExceedsLimiter_429 drives the real handler (real DB,
// real store) past the per-user budget using genuine wrong-current-password
// calls — each a real 422 invalid_credentials with no state change to the
// seeded row — and confirms the call AFTER the threshold is throttled with
// 429 rate_limited instead of reaching the store again.
func TestChangeMyPassword_ExceedsLimiter_429(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	callerID := seedUserWithPassword(t, pool, usersStore, "oldP@sswd1")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, callerID)
	})

	for i := 0; i < passwordChangeLimiterThreshold; i++ {
		res, out := callChangeMyPassword(t, h, callerID, "wrong-current-password", "newP@ssword2")
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d: status = %d, want 422; body=%+v", i, res.StatusCode, out)
		}
	}

	// The next call must be throttled BEFORE the store is consulted at
	// all — even with the CORRECT current password, it still 429s.
	res, out := callChangeMyPassword(t, h, callerID, "oldP@sswd1", "newP@ssword2")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%+v", res.StatusCode, out)
	}
	if out["error"] != "rate_limited" {
		t.Fatalf("error = %v, want rate_limited; body=%+v", out["error"], out)
	}
}

// TestChangeMyPassword_NormalFrequency_NotThrottled pins that ordinary
// traffic — a couple of sequential, successful password rotations, well
// under the threshold — is never caught by the limiter.
func TestChangeMyPassword_NormalFrequency_NotThrottled(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	callerID := seedUserWithPassword(t, pool, usersStore, "oldP@sswd1")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, callerID)
	})

	res, out := callChangeMyPassword(t, h, callerID, "oldP@sswd1", "newP@ssword2")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("first rotation: status = %d, want 204; body=%+v", res.StatusCode, out)
	}
	res, out = callChangeMyPassword(t, h, callerID, "newP@ssword2", "thirdP@ssword3")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("second rotation: status = %d, want 204; body=%+v", res.StatusCode, out)
	}
}

// TestChangeMyPassword_RetryAfterWrongCurrentPassword_Succeeds pins the
// exact flow named in the acceptance criteria: a user who mistypes their
// current password once must still be able to succeed on the very next,
// correct attempt — the limiter must not treat that single prior failure
// as reason enough to throttle.
func TestChangeMyPassword_RetryAfterWrongCurrentPassword_Succeeds(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	callerID := seedUserWithPassword(t, pool, usersStore, "oldP@sswd1")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, callerID)
	})

	res, out := callChangeMyPassword(t, h, callerID, "typo-current", "newP@ssword2")
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mistyped attempt: status = %d, want 422; body=%+v", res.StatusCode, out)
	}
	res, out = callChangeMyPassword(t, h, callerID, "oldP@sswd1", "newP@ssword2")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("corrected attempt: status = %d, want 204; body=%+v", res.StatusCode, out)
	}
}

// TestPasswordPolicy_ReflectsStoreState — GET /me/password/policy must
// mirror whatever PwnedChecker the Store was built with, in both
// directions (nil → false, non-nil → true), with no other side effect.
func TestPasswordPolicy_ReflectsStoreState(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	projectsStore := projects.NewStore(pool)

	callerID := seedUserRow(t, pool, "user")

	callPolicy := func(h *UsersHandler) (*http.Response, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me/password/policy", nil)
		ctx := auth.WithUserID(req.Context(), callerID)
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		h.PasswordPolicy(rec, req)
		res := rec.Result()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res, out
	}

	disabledStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	res, out := callPolicy(NewUsersHandler(pool, disabledStore, projectsStore))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if out["pwnedCheckEnabled"] != false {
		t.Fatalf("pwnedCheckEnabled = %v, want false for a nil checker", out["pwnedCheckEnabled"])
	}

	enabledStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", alwaysBreachedChecker{})
	res, out = callPolicy(NewUsersHandler(pool, enabledStore, projectsStore))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if out["pwnedCheckEnabled"] != true {
		t.Fatalf("pwnedCheckEnabled = %v, want true for a non-nil checker", out["pwnedCheckEnabled"])
	}
}

// TestPasswordPolicy_Unauthenticated — same auth gate as ChangeMyPassword
// itself.
func TestPasswordPolicy_Unauthenticated(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/password/policy", nil)
	rec := httptest.NewRecorder()
	h.PasswordPolicy(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// TestChangeMyPassword_ConcurrentBurst_LimiterAdmitsAtMostThreshold is the
// sk-350.2 round-2 regression (claude review, 2026-09-05): the limiter used
// to be check-then-work-then-record — h.pwChangeLimiter.blocked() consulted
// at the top of the handler, h.pwChangeLimiter.recordAttempt() called only
// AFTER h.store.ChangePassword returned, with the full bcrypt-cost verify
// (and, when SKEIN_PWNED_PASSWORDS_ENABLED=true, an outbound network call)
// running in between. Between those two SEPARATE lock acquisitions, every
// concurrent caller observed the bucket still under threshold — none of
// them had recorded yet — so N simultaneous requests from one identity all
// passed the check and all paid the expensive-work cost before a single one
// was counted. A fixed-window cap bounds a caller looping SEQUENTIALLY; it
// did not bound a concurrent burst, which is exactly the "hold pool
// connections cheaply by hammering the endpoint" vector this limiter exists
// to bound in the first place.
//
// This fires passwordChangeLimiterThreshold+extra goroutines at
// ChangeMyPassword through a start barrier that releases them all at
// (nearly) the same instant, with a deliberately WRONG current password so
// every admitted call still pays the full bcrypt-cost verify
// (Store.ChangePassword's step 4) and returns 422 invalid_credentials —
// never touching the password_hash column, so there is no
// compare-and-swap password_change_conflict to confound the count. At most
// passwordChangeLimiterThreshold calls may be admitted (non-429); the rest
// MUST be 429 rate_limited. On the pre-fix check-then-record code this test
// is red: more than threshold calls slip past the check before any of them
// records.
func TestChangeMyPassword_ConcurrentBurst_LimiterAdmitsAtMostThreshold(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	callerID := seedUserWithPassword(t, pool, usersStore, "oldP@sswd1")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, callerID)
	})

	const extra = 10
	const n = passwordChangeLimiterThreshold + extra

	var barrier sync.WaitGroup
	barrier.Add(1)
	var done sync.WaitGroup
	done.Add(n)
	results := make([]int, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer done.Done()
			barrier.Wait() // released all at once, below
			res, _ := callChangeMyPassword(t, h, callerID, "definitely-wrong-current-password", "newP@ssword2")
			results[idx] = res.StatusCode
		}(i)
	}
	barrier.Done() // release every goroutine at (nearly) the same instant
	done.Wait()

	var admitted, throttled int
	for _, code := range results {
		switch code {
		case http.StatusUnprocessableEntity:
			admitted++
		case http.StatusTooManyRequests:
			throttled++
		default:
			t.Fatalf("unexpected status %d among concurrent results: %v", code, results)
		}
	}

	if admitted > passwordChangeLimiterThreshold {
		t.Fatalf("admitted %d concurrent calls past the limiter, want at most %d (threshold): "+
			"check-then-record is not atomic — a concurrent burst outran the fixed-window "+
			"counter (statuses=%v)", admitted, passwordChangeLimiterThreshold, results)
	}
	if admitted+throttled != n {
		t.Fatalf("admitted(%d)+throttled(%d) != n(%d): unexpected status leaked in (statuses=%v)",
			admitted, throttled, n, results)
	}
}
