package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/users"
)

// testDatabaseURL — reuse the same env variable as the rest of the
// integration suite. Tests that need a real DB skip when it's not set so
// `go test ./...` without the local stack up still passes.
const testDatabaseURL = "TEST_DATABASE_URL"

func newAuthTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedAuthUser inserts a row with a real bcrypt hash so Login can verify
// it for the test, and optionally marks the user deactivated. Cleaned up
// via t.Cleanup.
func seedAuthUser(t *testing.T, pool *pgxpool.Pool, password string, deactivated bool) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ah" + strings.ReplaceAll(id.String()[:18], "-", "")
	email := uname + "@test.local"
	hash, err := auth.Hash(password, auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata, deactivated_at)
VALUES ($1, $2, $3, $4, 'user', $5, now(), '{}'::jsonb, CASE WHEN $6 THEN now() ELSE NULL END)`,
		id, email, uname, uname, hash, deactivated); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, email
}

// TestLogin_DeactivatedUser_RejectedWithInvalidCredentials verifies the
// R3.1 C1 fix: a deactivated user supplying their CORRECT password must
// be rejected with 422 invalid_credentials, NOT authenticated. Before
// the fix the deactivation gate was a separate Scan that fell through on
// any DB error; the fix folds deactivated_at into the credentials query
// so a single round-trip produces all auth-relevant fields.
func TestLogin_DeactivatedUser_RejectedWithInvalidCredentials(t *testing.T) {
	pool := newAuthTestPool(t)
	const password = "correct-horse-battery-staple"
	_, email := seedAuthUser(t, pool, password, true)

	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.Login(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.StatusCode)
	}
	var resp map[string]any
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := resp["error"]; got != "invalid credentials" {
		t.Fatalf("error = %v, want \"invalid credentials\"", got)
	}
	// Ensure the response shape did NOT include any user-DTO fields — a
	// successful login would surface id/email/role.
	if _, has := resp["id"]; has {
		t.Fatal("deactivated login leaked user id (would mean we authenticated them)")
	}
	if _, has := resp["role"]; has {
		t.Fatal("deactivated login leaked role (would mean we authenticated them)")
	}
}

// TestLogin_ActiveUser_HappyPath sanity-check: regression guard so the
// C1 fix doesn't accidentally start rejecting active users.
func TestLogin_ActiveUser_HappyPath(t *testing.T) {
	pool := newAuthTestPool(t)
	const password = "correct-horse-battery-staple"
	_, email := seedAuthUser(t, pool, password, false)

	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.Login(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var resp map[string]any
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := resp["email"]; got != email {
		t.Fatalf("email = %v, want %v", got, email)
	}
}

// TestLogin_PerEmailThrottle_EndToEnd is the integration proof (DB-backed) of
// the sk-163 S9 defense-in-depth limiter through the real handler flow: a
// seeded user receives loginLimiterEmailThreshold wrong-password 422s, and the
// next attempt is throttled with 429 BEFORE the bcrypt verify runs. The
// CORRECT password is then also rejected with 429, confirming the throttle
// gates the credential check rather than the credential value.
func TestLogin_PerEmailThrottle_EndToEnd(t *testing.T) {
	pool := newAuthTestPool(t)
	const password = "correct-horse-battery-staple"
	_, email := seedAuthUser(t, pool, password, false)

	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	const clientIP = "203.0.113.200"
	doLogin := func(pw string) int {
		body, _ := json.Marshal(map[string]string{"email": email, "password": pw})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.HeaderClientIP, clientIP)
		rec := httptest.NewRecorder()
		h.Login(rec, req)
		return rec.Code
	}

	// First loginLimiterEmailThreshold wrong-password attempts: each a 422.
	for i := 0; i < loginLimiterEmailThreshold; i++ {
		if code := doLogin("wrong-password"); code != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d: status = %d, want 422", i+1, code)
		}
	}

	// Next attempt is throttled with 429 — even with the CORRECT password,
	// proving the limiter gates before the credential check.
	if code := doLogin(password); code != http.StatusTooManyRequests {
		t.Fatalf("post-threshold status = %d, want 429", code)
	}
}

// TestLogin_PerIPSpray_EndToEnd is the DB-backed spray proof: many DIFFERENT
// (unknown) emails from one client IP, none reaching the per-email ceiling,
// still trip the per-IP bucket and produce a 429.
func TestLogin_PerIPSpray_EndToEnd(t *testing.T) {
	pool := newAuthTestPool(t)

	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	const clientIP = "198.51.100.250"
	doLogin := func(email string) int {
		body, _ := json.Marshal(map[string]string{"email": email, "password": "x"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.HeaderClientIP, clientIP)
		rec := httptest.NewRecorder()
		h.Login(rec, req)
		return rec.Code
	}

	// loginLimiterIPThreshold distinct unknown emails → each 422 (user not
	// found), none hitting the per-email ceiling.
	for i := 0; i < loginLimiterIPThreshold; i++ {
		email := fmt.Sprintf("spray-%d@nope.invalid", i)
		if code := doLogin(email); code != http.StatusUnprocessableEntity {
			t.Fatalf("spray attempt %d: status = %d, want 422", i+1, code)
		}
	}

	// One more distinct email from the same IP is now throttled by the per-IP
	// (spray) bucket.
	if code := doLogin("spray-final@nope.invalid"); code != http.StatusTooManyRequests {
		t.Fatalf("post-spray status = %d, want 429", code)
	}
}

// Row 56 (sk-674.18 K-16, INV-26): the response of a password login and of
// an OIDC provisioning — the answers after which skein-web issues a session
// — carries the user's CURRENT session epoch (accounts.AuthResponse, T012),
// and after the epoch is raised the next answer carries the new value. The
// reference is the stored users.session_epoch, read independently.
func TestAuthResponse_CarriesTheCurrentSessionEpoch(t *testing.T) {
	pool := newAuthTestPool(t)
	ctx := context.Background()
	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "local", "", auth.NewProvisioner(pool, true, "user", ""))
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	storedEpoch := func(id uuid.UUID) int64 {
		t.Helper()
		var e int64
		if err := pool.QueryRow(ctx, `SELECT session_epoch FROM users WHERE id = $1`, id).Scan(&e); err != nil {
			t.Fatalf("reading the stored epoch: %v", err)
		}
		return e
	}
	bump := func(id uuid.UUID) {
		t.Helper()
		if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := users.BumpSessionEpochInTx(ctx, tx, id)
			return err
		}); err != nil {
			t.Fatalf("BumpSessionEpochInTx: %v", err)
		}
	}
	// answer runs the handler and returns the sessionEpoch of its body; the
	// field must be present (a missing field is a defect, not a zero).
	answer := func(serve func(http.ResponseWriter, *http.Request), path string, body any) (string, int64) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		serve(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		v, ok := resp["sessionEpoch"].(float64)
		if !ok {
			t.Fatalf("%s: the response has no numeric sessionEpoch: %s", path, rec.Body.String())
		}
		id, _ := resp["id"].(string)
		return id, int64(v)
	}

	t.Run("password login", func(t *testing.T) {
		const password = "correct-horse-battery-staple"
		id, email := seedAuthUser(t, pool, password, false)
		creds := map[string]string{"email": email, "password": password}
		_, first := answer(h.Login, "/api/v1/auth/login", creds)
		if want := storedEpoch(id); first != want {
			t.Errorf("login: sessionEpoch %d, want the stored %d", first, want)
		}
		bump(id)
		_, second := answer(h.Login, "/api/v1/auth/login", creds)
		if want := storedEpoch(id); second != want || second == first {
			t.Errorf("login after the epoch rose: sessionEpoch %d (before %d), want the new stored %d", second, first, want)
		}
	})

	t.Run("OIDC provisioning", func(t *testing.T) {
		uname := "ep" + strings.ReplaceAll(uuid.NewString()[:18], "-", "")
		claims := map[string]string{"sub": "sub-" + uname, "email": uname + "@test.local", "username": uname, "name": uname}
		rawID, first := answer(h.OIDCProvision, "/api/v1/auth/oidc/provision", claims)
		id, err := uuid.Parse(rawID)
		if err != nil {
			t.Fatalf("provision: id %q: %v", rawID, err)
		}
		t.Cleanup(func() {
			// Provisioning gave the account its "Personal" (INV-8): its
			// dependent rows, then the space, then the account — every
			// foreign key here is RESTRICT, so the order matters and a
			// failure is reported, not ignored.
			c := context.Background()
			for _, q := range []string{
				`DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`,
				`DELETE FROM space_storage_usage WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`,
				`DELETE FROM spaces WHERE owner_id = $1`,
				`DELETE FROM users WHERE id = $1`,
			} {
				if _, err := pool.Exec(c, q, id); err != nil {
					t.Errorf("cleanup %q: %v", q, err)
				}
			}
		})
		if want := storedEpoch(id); first != want {
			t.Errorf("provision: sessionEpoch %d, want the stored %d", first, want)
		}
		bump(id)
		_, second := answer(h.OIDCProvision, "/api/v1/auth/oidc/provision", claims)
		if want := storedEpoch(id); second != want || second == first {
			t.Errorf("provision after the epoch rose: sessionEpoch %d (before %d), want the new stored %d", second, first, want)
		}
	})
}

// K-16, INV-26: a login authenticated
// against the password a concurrent reset replaces must not receive the
// reset's new epoch — the session would cross the reset boundary. The
// interleaving is driven by the database, not by timing: the reset holds
// the user's row (new hash, raised epoch) uncommitted while the login
// verifies the old password; the reset then commits. The login is refused
// with the generic invalid-credentials answer; with the new password it
// succeeds and carries the new epoch.
func TestLogin_RacingPasswordResetIsRefused(t *testing.T) {
	pool := newAuthTestPool(t)
	ctx := context.Background()
	const oldPassword, newPassword = "old-correct-horse-staple", "new-correct-horse-staple"
	id, email := seedAuthUser(t, pool, oldPassword, false)
	newHash, err := auth.Hash(newPassword, auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}
	login := func(password string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]string{"email": email, "password": password})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.Login(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	reset, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reset.Rollback(context.Background()) }()
	if _, err := reset.Exec(ctx, `
UPDATE users SET password_hash = $2, session_epoch = session_epoch + 1 WHERE id = $1`, id, newHash); err != nil {
		t.Fatalf("the reset: %v", err)
	}

	type answer struct {
		code int
		body map[string]any
	}
	done := make(chan answer, 1)
	go func() {
		code, body := login(oldPassword)
		done <- answer{code, body}
	}()
	// The login waits on the user's row the reset holds.
	waited := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !waited; {
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		waited = n > 0
		if !waited {
			select {
			case a := <-done:
				done <- a
				deadline = time.Now()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	if !waited {
		t.Fatal("the login did not reach the user's row while the reset held it — the interleaving was not produced")
	}
	if err := reset.Commit(ctx); err != nil {
		t.Fatalf("commit the reset: %v", err)
	}
	var a answer
	select {
	case a = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the login did not finish")
	}
	if a.code != http.StatusUnprocessableEntity {
		t.Errorf("a login with the password the reset replaced = %d %v, want 422 invalid credentials", a.code, a.body)
	}

	code, body := login(newPassword)
	if code != http.StatusOK {
		t.Fatalf("login with the new password = %d %v", code, body)
	}
	var stored int64
	if err := pool.QueryRow(ctx, `SELECT session_epoch FROM users WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if got, _ := body["sessionEpoch"].(float64); int64(got) != stored {
		t.Errorf("login with the new password: sessionEpoch %v, want the stored %d", body["sessionEpoch"], stored)
	}
}
