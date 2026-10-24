package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/monitorcreds"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------------------
// Pure-unit shape tests (no DB) — these run on every `go test ./...`.
// ---------------------------------------------------------------------------

func TestIsMonitorTokenShape(t *testing.T) {
	valid := MonitorTokenTag + strings.Repeat("a", monitorTokenRandomChars) // 47 chars
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid skm_", valid, true},
		{"empty", "", false},
		{"ska_ key (wrong tag+len)", APIKeyTag + strings.Repeat("a", 22), false},
		{"skm_ too short", MonitorTokenTag + "abc", false},
		{"skm_ too long", valid + "x", false},
		{"right length wrong tag", "ska_" + strings.Repeat("a", monitorTokenRandomChars), false}, // 47 chars, wrong tag
		{"garbage", "not-a-token-at-all", false},
		{"bare tag", MonitorTokenTag, false},
		// Correct tag + length but a non-base64url char in the body must be
		// rejected (Finding 3): '!' / '+' / '/' are outside the RawURLEncoding
		// alphabet, so they can never match a minted digest.
		{"bang in body", MonitorTokenTag + "!" + strings.Repeat("a", monitorTokenRandomChars-1), false},
		{"plus in body (std base64)", MonitorTokenTag + strings.Repeat("a", monitorTokenRandomChars-1) + "+", false},
		{"slash in body (std base64)", MonitorTokenTag + strings.Repeat("a", monitorTokenRandomChars-1) + "/", false},
		{"full base64url alphabet body", MonitorTokenTag + ("Az0-_" + strings.Repeat("b", monitorTokenRandomChars-5)), true},
		{"dash and underscore body", MonitorTokenTag + strings.Repeat("-", monitorTokenRandomChars), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMonitorTokenShape(tc.in); got != tc.want {
				t.Fatalf("IsMonitorTokenShape(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestMonitorTokenLenMatchesMonitorcreds is a drift guard: the auth-package
// length constant must equal the actual plaintext length minted by
// internal/monitorcreds (tag + base64.RawURLEncoding(32 bytes)). If T1 ever
// changes its entropy budget or tag, this fails loudly instead of silently
// 401-ing every freshly minted token.
func TestMonitorTokenLenMatchesMonitorcreds(t *testing.T) {
	wantLen := len(monitorcreds.TokenTag) + base64.RawURLEncoding.EncodedLen(monitorcreds.TokenRandomBytes)
	if MonitorTokenLen != wantLen {
		t.Fatalf("MonitorTokenLen = %d, want %d (monitorcreds tag+EncodedLen)", MonitorTokenLen, wantLen)
	}
	if MonitorTokenTag != monitorcreds.TokenTag {
		t.Fatalf("MonitorTokenTag = %q, want %q (monitorcreds.TokenTag)", MonitorTokenTag, monitorcreds.TokenTag)
	}
}

// TestShapeDoorsAreDisjoint is the REGRESSION guard for the two-door design:
// IsAPIKeyShape (the /mcp door, untouched here) must REJECT a skm_ monitor
// token and ACCEPT a real ska_ key, while IsMonitorTokenShape does the
// reverse. Proves the shapes are disjoint and that adding the monitor door
// did not alter RequireAPIKey's gate.
func TestShapeDoorsAreDisjoint(t *testing.T) {
	skm := MonitorTokenTag + strings.Repeat("a", monitorTokenRandomChars) // 47
	ska := APIKeyTag + strings.Repeat("b", 22)                            // 26

	if IsAPIKeyShape(skm) {
		t.Fatalf("IsAPIKeyShape accepted a monitor token %q — doors not disjoint", MaskMonitorToken(skm))
	}
	if !IsAPIKeyShape(ska) {
		t.Fatalf("IsAPIKeyShape rejected a real api key %q — RequireAPIKey gate changed", MaskAPIKey(ska))
	}
	if IsMonitorTokenShape(ska) {
		t.Fatalf("IsMonitorTokenShape accepted an api key %q — doors not disjoint", MaskAPIKey(ska))
	}
	if !IsMonitorTokenShape(skm) {
		t.Fatalf("IsMonitorTokenShape rejected a valid monitor token %q", MaskMonitorToken(skm))
	}
}

func TestMonitorClaimsContextRoundTrip(t *testing.T) {
	want := monitorcreds.Claims{
		UserID:       uuid.New(),
		ProjectID:    uuid.New(),
		ScopeIssueID: "sk-1",
		ScopeSubtree: true,
		EventTypes:   []string{"comment.created"},
	}
	ctx := WithMonitorClaims(context.Background(), want)
	got, ok := MonitorClaimsFrom(ctx)
	if !ok {
		t.Fatalf("MonitorClaimsFrom = !ok after WithMonitorClaims")
	}
	if got.UserID != want.UserID || got.ProjectID != want.ProjectID || got.ScopeIssueID != want.ScopeIssueID {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	// A bare context must report absence, not a zero-value claim.
	if _, ok := MonitorClaimsFrom(context.Background()); ok {
		t.Fatalf("MonitorClaimsFrom on empty ctx = ok, want !ok")
	}
}

// ---------------------------------------------------------------------------
// DB-integration tests — skip silently without TEST_DATABASE_URL.
// ---------------------------------------------------------------------------

const monitorTestAudience = "notifications:poll"

// monitorFixture wires a user + project + api key and a monitorcreds.Store
// ready to mint. Cleanup is LIFO: api key (CASCADE clears credentials) →
// project → user.
type monitorFixture struct {
	store     *monitorcreds.Store
	pool      *pgxpool.Pool
	userID    uuid.UUID
	projectID uuid.UUID
	apiKeyID  uuid.UUID
}

func newMonitorFixture(t *testing.T) monitorFixture {
	t.Helper()
	pool := newOIDCTestPool(t)
	u := seedMonitorUser(t, pool)
	p := seedMonitorProject(t, pool, u)
	k := seedMonitorAPIKey(t, pool, u)
	return monitorFixture{store: monitorcreds.New(pool, 6*time.Hour), pool: pool, userID: u, projectID: p, apiKeyID: k}
}

func (f monitorFixture) mintParams() monitorcreds.MintParams {
	return monitorcreds.MintParams{
		UserID:           f.userID,
		MintedByAPIKeyID: f.apiKeyID,
		Audience:         monitorTestAudience,
		Capability:       "read",
		ProjectID:        f.projectID,
		ScopeIssueID:     "mw-1",
		ScopeSubtree:     true,
		EventTypes:       []string{"comment.created"},
	}
}

func seedMonitorUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "mwu" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func seedMonitorProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := "mwtest_" + suffix
	prefix := "mw" + suffix[:6]
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(context.Background(), `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

// seedMonitorAPIKey inserts an active api_keys row owned by userID. The
// monitor_credentials FK is ON DELETE CASCADE, so dropping this key removes
// any credential minted by it.
func seedMonitorAPIKey(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	prefix := "ska_" + strings.ReplaceAll(id.String()[:12], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at)
VALUES ($1, $2, 'fake-hash-for-test', $3, 'monitor middleware test', now())`,
		id, userID, prefix)
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	return id
}

func readExpiresAt(t *testing.T, pool *pgxpool.Pool, plaintext string) time.Time {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	var ts time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT expires_at FROM monitor_credentials WHERE token_digest = $1`, digest[:]).Scan(&ts); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	return ts
}

// claimsCapture is a terminal handler that records the monitor claims the
// middleware injected, then writes 200. Used to assert both dispatch and the
// context plumbing in one shot.
func claimsCapture(captured *monitorcreds.Claims, reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		if c, ok := MonitorClaimsFrom(r.Context()); ok {
			*captured = c
		}
		// The MCP user-context MUST be empty on the monitor door — a monitor
		// token must never look like an api-key caller downstream.
		if _, ok := UserIDFrom(r.Context()); ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("unexpected MCP user-context on monitor request"))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func doMonitorRequest(t *testing.T, h http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll", nil)
	if token != "" {
		req.Header.Set(HeaderAPIKey, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRequireMonitorTokenValid(t *testing.T) {
	f := newMonitorFixture(t)
	ctx := context.Background()

	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Rewind expires_at to a near-future instant so a successful renew
	// (now + window) visibly advances it. Must stay in the future so the
	// now() < expires_at predicate still matches.
	digest := sha256.Sum256([]byte(plaintext))
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET expires_at = now() + interval '1 minute' WHERE token_digest = $1`,
		digest[:]); err != nil {
		t.Fatalf("rewind expires_at: %v", err)
	}
	before := readExpiresAt(t, f.pool, plaintext)

	var captured monitorcreds.Claims
	var reached bool
	h := RequireMonitorToken(f.store, monitorTestAudience)(claimsCapture(&captured, &reached))

	rec := doMonitorRequest(t, h, plaintext)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !reached {
		t.Fatalf("next handler not reached on valid token")
	}
	if captured.UserID != f.userID {
		t.Fatalf("claims.UserID = %s, want %s", captured.UserID, f.userID)
	}
	if captured.ProjectID != f.projectID {
		t.Fatalf("claims.ProjectID = %s, want %s", captured.ProjectID, f.projectID)
	}
	if captured.ScopeIssueID != "mw-1" || !captured.ScopeSubtree {
		t.Fatalf("claims scope = (%q, subtree=%v), want (mw-1, true)", captured.ScopeIssueID, captured.ScopeSubtree)
	}
	if len(captured.EventTypes) != 1 || captured.EventTypes[0] != "comment.created" {
		t.Fatalf("claims.EventTypes = %v, want [comment.created]", captured.EventTypes)
	}

	after := readExpiresAt(t, f.pool, plaintext)
	if !after.After(before) {
		t.Fatalf("renew did not advance expires_at: before=%s after=%s", before, after)
	}
}

func TestRequireMonitorTokenMalformed(t *testing.T) {
	f := newMonitorFixture(t)
	var captured monitorcreds.Claims
	var reached bool
	h := RequireMonitorToken(f.store, monitorTestAudience)(claimsCapture(&captured, &reached))

	for _, tok := range []string{"", "skm_short", "garbage"} {
		rec := doMonitorRequest(t, h, tok)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", tok, rec.Code)
		}
		if reached {
			t.Fatalf("token %q: next handler reached on malformed token", tok)
		}
	}
}

// TestRequireMonitorTokenAPIKeyShapeRejected proves an ska_ api key is
// rejected at the monitor door BEFORE any store/DB hit. The store is backed
// by a pool we close up front: if the middleware ever reached the store it
// would 500; a 401 proves the shape filter short-circuited.
func TestRequireMonitorTokenAPIKeyShapeRejected(t *testing.T) {
	pool := newOIDCTestPool(t)
	pool.Close() // any store call now errors → would surface as 500
	store := monitorcreds.New(pool, 6*time.Hour)

	var captured monitorcreds.Claims
	var reached bool
	h := RequireMonitorToken(store, monitorTestAudience)(claimsCapture(&captured, &reached))

	ska := APIKeyTag + strings.Repeat("a", 22) // valid ska_ shape, 26 chars
	rec := doMonitorRequest(t, h, ska)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ska_ at monitor door: status = %d, want 401 (got body=%s)", rec.Code, rec.Body.String())
	}
	if reached {
		t.Fatalf("ska_ token reached next handler at monitor door")
	}
}

// TestRequireAPIKeyRejectsMonitorToken is the other half of the two-door
// regression (G6): it proves the REAL /mcp door (RequireAPIKey) rejects a
// well-formed skm_ monitor token with 401 — not just the IsAPIKeyShape helper
// in isolation. The pool is closed up front so any store/DB hit would surface
// as 500; a 401 proves the ska_-only shape gate short-circuited before the DB.
// A skm_ token is therefore useless against /mcp by construction (spec D7/§5).
func TestRequireAPIKeyRejectsMonitorToken(t *testing.T) {
	pool := newOIDCTestPool(t)
	pool.Close() // any store call now errors → would surface as 500

	reached := false
	// bcryptCost is irrelevant on this path (shape gate fires before the DB /
	// dummy-hash seed) — pass a valid cost so the constructor is happy.
	h := RequireAPIKey(pool, 10, "")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	skm := MonitorTokenTag + strings.Repeat("a", monitorTokenRandomChars) // valid skm_ shape, 47 chars
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(HeaderAPIKey, skm)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("skm_ at /mcp door: status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if reached {
		t.Fatalf("skm_ monitor token reached the MCP handler — /mcp door accepted it")
	}
}

func TestRequireMonitorTokenWrongAudience(t *testing.T) {
	f := newMonitorFixture(t)
	ctx := context.Background()

	plaintext, _, err := f.store.Mint(ctx, f.mintParams()) // minted for notifications:poll
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	var captured monitorcreds.Claims
	var reached bool
	// Middleware expects a DIFFERENT audience → the UPDATE matches 0 rows.
	h := RequireMonitorToken(f.store, "some:other:audience")(claimsCapture(&captured, &reached))

	rec := doMonitorRequest(t, h, plaintext)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong audience: status = %d, want 401", rec.Code)
	}
	if reached {
		t.Fatalf("wrong audience reached next handler")
	}
}

func TestRequireMonitorTokenExpired(t *testing.T) {
	f := newMonitorFixture(t)
	ctx := context.Background()

	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Force the credential past its sliding window.
	digest := sha256.Sum256([]byte(plaintext))
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET expires_at = now() - interval '1 minute' WHERE token_digest = $1`,
		digest[:]); err != nil {
		t.Fatalf("force expiry: %v", err)
	}

	var captured monitorcreds.Claims
	var reached bool
	h := RequireMonitorToken(f.store, monitorTestAudience)(claimsCapture(&captured, &reached))

	rec := doMonitorRequest(t, h, plaintext)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: status = %d, want 401", rec.Code)
	}
	if reached {
		t.Fatalf("expired token reached next handler")
	}
}

func TestRequireMonitorTokenRevoked(t *testing.T) {
	f := newMonitorFixture(t)
	ctx := context.Background()

	plaintext, cred, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := f.store.RevokeOwned(ctx, cred.ID, f.userID); err != nil {
		t.Fatalf("RevokeOwned: %v", err)
	}

	var captured monitorcreds.Claims
	var reached bool
	h := RequireMonitorToken(f.store, monitorTestAudience)(claimsCapture(&captured, &reached))

	rec := doMonitorRequest(t, h, plaintext)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: status = %d, want 401", rec.Code)
	}
	if reached {
		t.Fatalf("revoked token reached next handler")
	}
}

// TestRequireMonitorTokenDBError forces a non-ErrInvalidMonitorToken error
// out of ValidateAndRenew (a cancelled request context) and asserts the
// middleware maps it to 5xx, NOT 401. A 401 here would make the poll runner
// discard a still-valid token on a transient DB blip.
func TestRequireMonitorTokenDBError(t *testing.T) {
	f := newMonitorFixture(t)
	ctx := context.Background()

	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	var captured monitorcreds.Claims
	var reached bool
	h := RequireMonitorToken(f.store, monitorTestAudience)(claimsCapture(&captured, &reached))

	// Cancelled context → the derived validate context is already done →
	// ValidateAndRenew's QueryRow returns a wrapped context error (not
	// pgx.ErrNoRows) → the store returns a non-sentinel error → 5xx.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll", nil).WithContext(cctx)
	req.Header.Set(HeaderAPIKey, plaintext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < 500 || rec.Code > 599 {
		t.Fatalf("DB error: status = %d, want 5xx (NOT 401)", rec.Code)
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("DB error masqueraded as 401 — runner would discard a valid token")
	}
	if reached {
		t.Fatalf("DB error reached next handler")
	}
}
