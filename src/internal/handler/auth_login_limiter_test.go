package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"skein-api/internal/auth"
)

// loginReq builds a POST /api/v1/auth/login request carrying the trusted
// client-IP header the BFF forwards (sk-163 S9).
func loginReq(t *testing.T, email, password, clientIP string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if clientIP != "" {
		req.Header.Set(auth.HeaderClientIP, clientIP)
	}
	return req
}

// TestLoginClientIP_HeaderOnly_NoRemoteAddrFallback pins sk-163 S9 review fix:
// the per-IP key comes ONLY from the BFF-forwarded X-Skein-Client-Ip. When the
// header is absent there is NO RemoteAddr fallback (which on the BFF path is the
// BFF pod IP — a global-bucket DoS — and via chi RealIP is XFF-spoofable), so
// the per-IP bucket is simply skipped.
func TestLoginClientIP_HeaderOnly_NoRemoteAddrFallback(t *testing.T) {
	withHeader := httptest.NewRequest(http.MethodPost, "/", nil)
	withHeader.Header.Set(auth.HeaderClientIP, "203.0.113.7")
	if got := loginClientIP(withHeader); got != "203.0.113.7" {
		t.Fatalf("with header: got %q, want 203.0.113.7", got)
	}

	noHeader := httptest.NewRequest(http.MethodPost, "/", nil)
	noHeader.RemoteAddr = "10.1.2.3:5555" // must NOT be used
	if got := loginClientIP(noHeader); got != "" {
		t.Fatalf("absent header must yield \"\" (no RemoteAddr fallback), got %q", got)
	}
}

// TestLogin_ThrottledBeforeBcrypt_NoDB proves the defense-in-depth limiter
// short-circuits the handler BEFORE it touches the DB or bcrypt. The handler
// is built with a NIL pool: if execution ever reached GetUserCredentialsByEmail
// it would panic on the nil pool. A clean 429 therefore proves the limiter
// pre-check fired first (and the dummy/real bcrypt verify was skipped).
func TestLogin_ThrottledBeforeBcrypt_NoDB(t *testing.T) {
	h, err := NewAuthHandler(nil, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	const email, clientIP = "victim@x.io", "203.0.113.7"

	// Drive the per-email bucket to its threshold directly (no DB needed).
	for i := 0; i < loginLimiterEmailThreshold; i++ {
		h.limiter.recordFailure(email, clientIP)
	}

	rec := httptest.NewRecorder()
	// A panic here would mean the nil-pool DB path was reached — i.e. the
	// limiter did NOT run before bcrypt. The deferred check makes that an
	// explicit, readable failure rather than a raw stack trace.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Login reached the DB/bcrypt path (nil-pool panic) instead of throttling first: %v", r)
		}
	}()
	h.Login(rec, loginReq(t, email, "any-password", clientIP))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 (throttled before bcrypt), got %d", rec.Code)
	}
}

// TestLogin_BreakGlassThrottled_BeforeBcrypt proves the break-glass admin —
// the single privileged account that can password-login under active OIDC — is
// throttled like any other email, AND that the limiter runs before the
// keycloak 410 branch and before bcrypt. Built with a nil pool in keycloak
// mode; a throttled break-glass login must 429 (not 410, not panic).
func TestLogin_BreakGlassThrottled_BeforeBcrypt(t *testing.T) {
	const breakGlass, clientIP = "breakglass-admin@corp.example", "198.51.100.9"

	h, err := NewAuthHandler(nil, auth.MinBcryptCost, "keycloak", breakGlass, nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	for i := 0; i < loginLimiterEmailThreshold; i++ {
		h.limiter.recordFailure(breakGlass, clientIP)
	}

	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("break-glass login reached the DB/bcrypt path instead of throttling first: %v", r)
		}
	}()
	h.Login(rec, loginReq(t, breakGlass, "any-password", clientIP))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected break-glass login to be throttled with 429, got %d", rec.Code)
	}
}

// TestLogin_NotThrottled_KeycloakNonBreakGlass_Returns410 is the control: when
// the limiter is NOT tripped, a non-break-glass email in keycloak mode still
// hits the existing 410 branch — before any DB access (nil pool, no panic). It
// confirms the limiter pre-check passes traffic through unchanged on the happy
// path and does not false-trip.
func TestLogin_NotThrottled_KeycloakNonBreakGlass_Returns410(t *testing.T) {
	h, err := NewAuthHandler(nil, auth.MinBcryptCost, "keycloak", "breakglass@corp.example", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	rec := httptest.NewRecorder()
	h.Login(rec, loginReq(t, "someone-else@x.io", "pw", "203.0.113.50"))

	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410 for non-break-glass in keycloak mode, got %d", rec.Code)
	}
}
