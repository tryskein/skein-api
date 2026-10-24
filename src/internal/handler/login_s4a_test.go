package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

// sk-163 S4a — login gate matrix. The password channel must coexist with
// OIDC: local = everyone; hybrid = external (+ break-glass) only, with the
// internal-account rejection collapsed into the generic invalid-credentials
// shape (no class-leaking 403, no pre-bcrypt branch); keycloak = nobody
// except the break-glass admin.

// seedAuthUserClass inserts a user with an explicit role + is_external flag
// and a real bcrypt hash so Login can verify it. Cleaned up via t.Cleanup.
func seedAuthUserClass(t *testing.T, pool *pgxpool.Pool, password, role string, isExternal bool) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "s4a" + strings.ReplaceAll(id.String()[:18], "-", "")
	email := uname + "@test.local"
	hash, err := auth.Hash(password, auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, now(), '{}'::jsonb)`,
		id, email, uname, uname, role, hash, isExternal); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, email
}

// doLogin drives AuthHandler.Login and returns the status + decoded body.
func doLogin(t *testing.T, h *AuthHandler, email, password string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	var m map[string]any
	if res.Body != nil {
		_ = json.NewDecoder(res.Body).Decode(&m)
	}
	return res.StatusCode, m
}

// assertGenericInvalid asserts the response is the generic 422
// invalid-credentials shape and leaks NO user-DTO fields (id/role) — the
// authenticated-success tell.
func assertGenericInvalid(t *testing.T, status int, body map[string]any) {
	t.Helper()
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", status)
	}
	if got := body["error"]; got != "invalid credentials" {
		t.Fatalf("error = %v, want \"invalid credentials\"", got)
	}
	if _, has := body["id"]; has {
		t.Fatal("body leaked user id — would mean we authenticated")
	}
	if _, has := body["role"]; has {
		t.Fatal("body leaked role — would mean we authenticated")
	}
}

const s4aPassword = "correct-horse-battery-staple"

// --- local mode: unchanged, everyone may password-login ---

func TestLoginS4a_Local_InternalAndExternalOK(t *testing.T) {
	pool := newAuthTestPool(t)
	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	_, intEmail := seedAuthUserClass(t, pool, s4aPassword, "user", false)
	_, extEmail := seedAuthUserClass(t, pool, s4aPassword, "user", true)

	if status, body := doLogin(t, h, intEmail, s4aPassword); status != http.StatusOK {
		t.Fatalf("internal local login status = %d, want 200 (body=%v)", status, body)
	}
	if status, body := doLogin(t, h, extEmail, s4aPassword); status != http.StatusOK {
		t.Fatalf("external local login status = %d, want 200 (body=%v)", status, body)
	}
}

// --- hybrid mode ---

func TestLoginS4a_Hybrid_ExternalOK_InternalGenericFail(t *testing.T) {
	pool := newAuthTestPool(t)
	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "hybrid", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	_, extEmail := seedAuthUserClass(t, pool, s4aPassword, "user", true)
	_, intEmail := seedAuthUserClass(t, pool, s4aPassword, "user", false)

	// External + correct password → authenticates.
	if status, body := doLogin(t, h, extEmail, s4aPassword); status != http.StatusOK {
		t.Fatalf("hybrid external login status = %d, want 200 (body=%v)", status, body)
	} else if body["email"] != extEmail {
		t.Fatalf("hybrid external login email = %v, want %v", body["email"], extEmail)
	}

	// Internal + CORRECT password → generic invalid-credentials (NOT a
	// distinct 403, NOT a 200). This is the crucial gate behaviour.
	status, body := doLogin(t, h, intEmail, s4aPassword)
	assertGenericInvalid(t, status, body)
}

// TestLoginS4a_Hybrid_ConstantTimeShape — the constant-time discipline is
// asserted by RESPONSE SHAPE (status + body), not wall-clock: an internal
// account supplying the CORRECT password must be byte-for-byte
// indistinguishable from one supplying a WRONG password. If the gate fired
// before bcrypt or returned a distinct 403, these two would diverge.
func TestLoginS4a_Hybrid_ConstantTimeShape(t *testing.T) {
	pool := newAuthTestPool(t)
	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "hybrid", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}
	_, intEmail := seedAuthUserClass(t, pool, s4aPassword, "user", false)

	correctStatus, correctBody := doLogin(t, h, intEmail, s4aPassword)
	wrongStatus, wrongBody := doLogin(t, h, intEmail, "totally-wrong-password")

	assertGenericInvalid(t, correctStatus, correctBody)
	assertGenericInvalid(t, wrongStatus, wrongBody)

	if correctStatus != wrongStatus {
		t.Fatalf("status differs: correct=%d wrong=%d (account-class leak)", correctStatus, wrongStatus)
	}
	cb, _ := json.Marshal(correctBody)
	wb, _ := json.Marshal(wrongBody)
	if string(cb) != string(wb) {
		t.Fatalf("body differs: correct=%s wrong=%s (account-class leak)", cb, wb)
	}
}

func TestLoginS4a_Hybrid_BreakGlassAdminOK_OtherAdminFails(t *testing.T) {
	pool := newAuthTestPool(t)

	// Break-glass admin is an INTERNAL admin, identified by email.
	_, bgEmail := seedAuthUserClass(t, pool, s4aPassword, "admin", false)
	_, otherEmail := seedAuthUserClass(t, pool, s4aPassword, "admin", false)

	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "hybrid", bgEmail, nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	// Break-glass admin authenticates despite being internal.
	if status, body := doLogin(t, h, bgEmail, s4aPassword); status != http.StatusOK {
		t.Fatalf("break-glass login status = %d, want 200 (body=%v)", status, body)
	}

	// A different internal admin with the CORRECT password does NOT — same
	// generic failure as a wrong password.
	status, body := doLogin(t, h, otherEmail, s4aPassword)
	assertGenericInvalid(t, status, body)
}

// --- keycloak mode ---

func TestLoginS4a_Keycloak_All410ExceptBreakGlass(t *testing.T) {
	pool := newAuthTestPool(t)

	_, bgEmail := seedAuthUserClass(t, pool, s4aPassword, "admin", false)
	_, intEmail := seedAuthUserClass(t, pool, s4aPassword, "user", false)
	_, extEmail := seedAuthUserClass(t, pool, s4aPassword, "user", true)
	_, otherAdmin := seedAuthUserClass(t, pool, s4aPassword, "admin", false)

	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "keycloak", bgEmail, nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	// Non-break-glass callers — internal, external, and a different admin —
	// all get 410 (password permanently disabled).
	for _, email := range []string{intEmail, extEmail, otherAdmin} {
		status, body := doLogin(t, h, email, s4aPassword)
		if status != http.StatusGone {
			t.Fatalf("keycloak login for %s status = %d, want 410 (body=%v)", email, status, body)
		}
		if body["error"] != "login_disabled" {
			t.Fatalf("keycloak 410 error = %v, want login_disabled", body["error"])
		}
	}

	// Break-glass admin still password-logs-in under active OIDC.
	if status, body := doLogin(t, h, bgEmail, s4aPassword); status != http.StatusOK {
		t.Fatalf("keycloak break-glass login status = %d, want 200 (body=%v)", status, body)
	}

	// Break-glass admin with a WRONG password reaches the credential path
	// and gets 422 (not 410) — proves it bypassed the 410 short-circuit.
	if status, _ := doLogin(t, h, bgEmail, "nope-wrong"); status != http.StatusUnprocessableEntity {
		t.Fatalf("keycloak break-glass wrong-pw status = %d, want 422", status)
	}
}
