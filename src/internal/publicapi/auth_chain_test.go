package publicapi_test

// auth_chain_test.go — rows 61–65: the request chain of the public entry on
// a test route, with the real key authentication over the test database.
// The reference is the spec: one 401 body for every credential failure
// (US-4, "Limits"); failed authentications limited per client address, warm
// keys passing (NFR, OST-2-3); an internal error is no failure of the
// client (OST-3-4); requests limited per key (US-12); the key right checked
// before the operation (US-2); X-Skein-Worker-Id (US-5); no identity
// header trusted on the public port (INV-10, SC-025).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/agents"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/publicapi"
	"skein-api/internal/testsupport"
)

// seen is what the operation behind the chain observed.
type seen struct {
	userID     uuid.UUID
	role       string
	isExternal bool
	keyID      uuid.UUID
	holder     string
}

// chainRig is a router of test routes behind a chain with the real key
// authentication.
type chainRig struct {
	pool  *pgxpool.Pool
	clock *fakeClock
	h     http.Handler
	calls atomic.Int32
	mu    sync.Mutex
	last  seen
}

// switchAuthenticator delegates to the authenticator set last.
type switchAuthenticator struct {
	mu  sync.Mutex
	cur publicapi.KeyAuthenticator
}

func (s *switchAuthenticator) set(a publicapi.KeyAuthenticator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = a
}

func (s *switchAuthenticator) get() publicapi.KeyAuthenticator {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *switchAuthenticator) AuthenticateAPIKey(ctx context.Context, plain string) auth.APIKeyOutcome {
	return s.get().AuthenticateAPIKey(ctx, plain)
}

func (s *switchAuthenticator) TouchLastUsed(ctx context.Context, id uuid.UUID) error {
	return s.get().TouchLastUsed(ctx, id)
}

const (
	pathRead    = "/api/v1/test-things"
	pathWrite   = "/api/v1/test-things"
	pathGuarded = "/api/v1/test-things/{thing}/guarded"
)

// newChainRig builds the rig. authn nil = the real authenticator over pool.
func newChainRig(t *testing.T, pool *pgxpool.Pool, authn publicapi.KeyAuthenticator, failLimit, requestLimit int) *chainRig {
	t.Helper()
	r := &chainRig{pool: pool, clock: newFakeClock()}
	if authn == nil {
		authn = auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost)
	}
	chain := publicapi.NewChain(publicapi.ChainOptions{
		Authenticator: authn,
		Executors:     publicapi.PoolExecutors{Pool: pool},
		ClientIP:      publicapi.NewClientIP(nil),
		WarmKeys:      publicapi.NewWarmKeys(15*time.Minute, 100, r.clock.Now),
		AuthFailures:  publicapi.NewWindowLimiter(failLimit, time.Minute, 100, r.clock.Now),
		Requests:      publicapi.NewWindowLimiter(requestLimit, time.Minute, 100, r.clock.Now),
	})
	op := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.calls.Add(1)
		ctx := req.Context()
		var s seen
		s.userID, _ = auth.UserIDFrom(ctx)
		s.role, _ = auth.UserRoleFrom(ctx)
		s.isExternal = auth.IsExternalFrom(ctx)
		s.keyID, _ = auth.APIKeyIDFrom(ctx)
		s.holder = auth.SessionIDFrom(ctx)
		r.mu.Lock()
		r.last = s
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	r.h = publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second, Chain: chain}, []publicapi.Route{
		{OperationID: "testReadThings", Method: http.MethodGet, Path: pathRead, Auth: publicapi.AuthKey,
			Key: authz.KeyRequirement{Right: authz.KeyRightRead}, Handler: op},
		{OperationID: "testWriteThing", Method: http.MethodPost, Path: pathWrite, Auth: publicapi.AuthKey,
			Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Handler: op},
		{OperationID: "testGuardedThing", Method: http.MethodPost, Path: pathGuarded, Auth: publicapi.AuthKey,
			Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Guarded: true,
			Actions: []authz.Action{authz.ActionWrite}, Object: publicapi.ObjectPathIssueProject, Handler: op},
	})
	return r
}

// req is one request to the rig.
type req struct {
	method, path, ip, key string
	headers               map[string]string
}

func (r *chainRig) do(t *testing.T, q req) *httptest.ResponseRecorder {
	t.Helper()
	method := q.method
	if method == "" {
		method = http.MethodGet
	}
	path := q.path
	if path == "" {
		path = pathRead
	}
	var hr *http.Request
	if method == http.MethodPost {
		hr = httptest.NewRequest(method, path, strings.NewReader(`{}`))
		hr.Header.Set("Content-Type", "application/json")
	} else {
		hr = httptest.NewRequest(method, path, nil)
	}
	hr.RemoteAddr = q.ip + ":40000"
	if q.key != "" {
		hr.Header.Set(auth.HeaderAPIKey, q.key)
	}
	for k, v := range q.headers {
		hr.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, hr)
	return rec
}

func (r *chainRig) lastSeen() seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// mintKey creates a key of user with grant and returns its plain text and
// id.
func mintKey(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, g apikeys.Grant) (string, uuid.UUID) {
	t.Helper()
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	c, err := apikeys.New(pool).Create(context.Background(), apikeys.NewKey{
		UserID: user, Label: "public chain", Prefix: prefix, Hash: hash, Grant: g,
	}, testsupport.KeyTTLPolicy)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE api_key_id = $1`, c.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, c.ID)
	})
	return plain, c.ID
}

func level(l apikeys.Level) apikeys.Grant {
	return apikeys.Grant{Level: l, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
}

// problem decodes the code and the members of a problem response.
func problem(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != publicapi.ProblemContentType {
		t.Fatalf("Content-Type %q, want %s (status %d, body %q)", ct, publicapi.ProblemContentType, rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("problem is not JSON: %v", err)
	}
	return m
}

func wantRetryAfter(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	s, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || s < 1 {
		t.Errorf("Retry-After %q, want whole seconds >= 1", rec.Header().Get("Retry-After"))
	}
}

// openExtraPool opens a pool of its own over the test database, for a test
// that closes it.
func openExtraPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), os.Getenv(testsupport.TestDatabaseURL))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestAuthChain_Credentials — row 61 (US-4, SC-004 REST, SC-024, OST-2-3,
// OST-3-4).
func TestAuthChain_Credentials(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")

	t.Run("every credential failure has the same 401 body", func(t *testing.T) {
		rig := newChainRig(t, pool, nil, 100, 100)
		valid, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
		expired, expiredID := mintKey(t, pool, owner, level(apikeys.LevelRead))
		revoked, revokedID := mintKey(t, pool, owner, level(apikeys.LevelRead))
		if _, err := pool.Exec(context.Background(), `UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, expiredID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, revokedID); err != nil {
			t.Fatal(err)
		}
		inactiveOwner, _ := testsupport.SeedUser(t, pool, "user")
		inactive, _ := mintKey(t, pool, inactiveOwner, level(apikeys.LevelRead))
		if _, err := pool.Exec(context.Background(), `UPDATE users SET deactivated_at = now() WHERE id = $1`, inactiveOwner); err != nil {
			t.Fatal(err)
		}
		unstored, _, _, err := auth.GenerateAPIKey(auth.MinBcryptCost)
		if err != nil {
			t.Fatal(err)
		}
		last := "a"
		if strings.HasSuffix(valid, "a") {
			last = "b"
		}
		wrong := valid[:len(valid)-1] + last

		var first []byte
		for i, tc := range []struct{ name, key string }{
			{"no key", ""},
			{"not a key", "not-a-key"},
			{"unknown key", unstored},
			{"wrong secret", wrong},
			{"expired", expired},
			{"revoked", revoked},
			{"owner inactive", inactive},
		} {
			rec := rig.do(t, req{ip: "198.51.100." + strconv.Itoa(10+i), key: tc.key})
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401", tc.name, rec.Code)
				continue
			}
			if first == nil {
				first = append([]byte(nil), rec.Body.Bytes()...)
				if code := problem(t, rec)["code"]; code != "unauthenticated" {
					t.Errorf("%s: code %v, want unauthenticated", tc.name, code)
				}
			} else if !bytes.Equal(first, rec.Body.Bytes()) {
				t.Errorf("%s: body %q differs from %q", tc.name, rec.Body.String(), first)
			}
		}
		if rig.calls.Load() != 0 {
			t.Errorf("the operation ran %d times for refused credentials", rig.calls.Load())
		}
	})

	t.Run("failures limit an address: cold refused, warm passes, other address free", func(t *testing.T) {
		rig := newChainRig(t, pool, nil, 2, 100)
		warm, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
		cold, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))

		if rec := rig.do(t, req{ip: "203.0.113.50", key: warm}); rec.Code != http.StatusOK {
			t.Fatalf("warming the key: %d", rec.Code)
		}
		for i := 1; i <= 2; i++ {
			if rec := rig.do(t, req{ip: "203.0.113.1", key: "ska_wrong_" + strconv.Itoa(i)}); rec.Code != http.StatusUnauthorized {
				t.Fatalf("failure %d: %d, want 401", i, rec.Code)
			}
		}
		rec := rig.do(t, req{ip: "203.0.113.1", key: cold})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("cold valid key from the limited address: %d, want 429", rec.Code)
		}
		if code := problem(t, rec)["code"]; code != "rate_limited" {
			t.Errorf("code %v, want rate_limited", code)
		}
		wantRetryAfter(t, rec)
		if rec := rig.do(t, req{ip: "203.0.113.1", key: warm}); rec.Code != http.StatusOK {
			t.Errorf("warm key from the limited address: %d, want 200", rec.Code)
		}
		if rec := rig.do(t, req{ip: "203.0.113.2", key: cold}); rec.Code != http.StatusOK {
			t.Errorf("cold valid key from another address: %d, want 200 (the limit is per address)", rec.Code)
		}
	})

	t.Run("an internal error on the warm path is no failure of the address", func(t *testing.T) {
		broken := openExtraPool(t)
		overBroken := auth.NewAPIKeyAuthenticator(broken, auth.MinBcryptCost)
		sw := &switchAuthenticator{cur: overBroken}
		rig := newChainRig(t, pool, sw, 3, 100)
		k, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
		cold, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))

		// The key's bcrypt result is cached in the authenticator over the
		// pool about to break.
		if rec := rig.do(t, req{ip: "203.0.113.60", key: k}); rec.Code != http.StatusOK {
			t.Fatalf("first authentication: %d", rec.Code)
		}
		sw.set(auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost))
		for i := 1; i <= 2; i++ { // limit − 1
			if rec := rig.do(t, req{ip: "203.0.113.3", key: "ska_wrong_" + strconv.Itoa(i)}); rec.Code != http.StatusUnauthorized {
				t.Fatalf("failure %d: %d, want 401", i, rec.Code)
			}
		}
		broken.Close()
		sw.set(overBroken) // bcrypt cached: the status read is what fails
		rec := rig.do(t, req{ip: "203.0.113.3", key: k})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status read on a closed pool: %d, want 503", rec.Code)
		}
		if code := problem(t, rec)["code"]; code != "database_unavailable" {
			t.Errorf("code %v, want database_unavailable", code)
		}
		sw.set(auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost))
		if rec := rig.do(t, req{ip: "203.0.113.3", key: cold}); rec.Code != http.StatusOK {
			t.Errorf("cold valid key after the internal error: %d, want 200 (the error counted as a failure)", rec.Code)
		}
	})

	t.Run("an internal error on the cold path is no failure of the address", func(t *testing.T) {
		broken := openExtraPool(t)
		broken.Close()
		sw := &switchAuthenticator{cur: auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost)}
		rig := newChainRig(t, pool, sw, 3, 100)
		k, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
		cold, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))

		for i := 1; i <= 2; i++ { // limit − 1
			if rec := rig.do(t, req{ip: "203.0.113.4", key: "ska_wrong_" + strconv.Itoa(i)}); rec.Code != http.StatusUnauthorized {
				t.Fatalf("failure %d: %d, want 401", i, rec.Code)
			}
		}
		sw.set(auth.NewAPIKeyAuthenticator(broken, auth.MinBcryptCost))
		rec := rig.do(t, req{ip: "203.0.113.4", key: k})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("key lookup on a closed pool: %d, want 503", rec.Code)
		}
		if code := problem(t, rec)["code"]; code != "database_unavailable" {
			t.Errorf("code %v, want database_unavailable", code)
		}
		sw.set(auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost))
		if rec := rig.do(t, req{ip: "203.0.113.4", key: cold}); rec.Code != http.StatusOK {
			t.Errorf("cold valid key after the internal error: %d, want 200 (the error counted as a failure)", rec.Code)
		}
	})
}

// TestAuthChain_RequestLimitPerKey — row 62 (US-12, SC-012). Past the limit
// of a key: 429 with Retry-After and the operation does not run; a second
// key from the same address in the same window passes — the limit is per
// key, not per address.
func TestAuthChain_RequestLimitPerKey(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	rig := newChainRig(t, pool, nil, 100, 2)
	k1, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
	k2, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))

	for i := 1; i <= 2; i++ {
		if rec := rig.do(t, req{ip: "203.0.113.20", key: k1}); rec.Code != http.StatusOK {
			t.Fatalf("request %d of 2: %d", i, rec.Code)
		}
	}
	rec := rig.do(t, req{ip: "203.0.113.20", key: k1})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 3 of 2: %d, want 429", rec.Code)
	}
	if code := problem(t, rec)["code"]; code != "rate_limited" {
		t.Errorf("code %v, want rate_limited", code)
	}
	wantRetryAfter(t, rec)
	if got := rig.calls.Load(); got != 2 {
		t.Errorf("the operation ran %d times, want 2 (not for the refused request)", got)
	}
	if rec := rig.do(t, req{ip: "203.0.113.20", key: k2}); rec.Code != http.StatusOK {
		t.Errorf("second key, same address, same window: %d, want 200", rec.Code)
	}
}

// TestAuthChain_KeyRightBeforeTheOperation — row 63 (US-2, SC-002). A key
// without the right of the operation gets 403 insufficient_scope naming the
// right and the operationId, before the operation runs; force=true on a
// guarded operation raises the right to manage.
func TestAuthChain_KeyRightBeforeTheOperation(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	rig := newChainRig(t, pool, nil, 100, 100)
	read, _ := mintKey(t, pool, owner, level(apikeys.LevelRead))
	write, _ := mintKey(t, pool, owner, level(apikeys.LevelWrite))
	manage, _ := mintKey(t, pool, owner, level(apikeys.LevelManage))
	guarded := "/api/v1/test-things/x/guarded"

	for _, tc := range []struct {
		name, key, path, right, op string
	}{
		{"read key on a write operation", read, pathWrite, "write", "testWriteThing"},
		{"write key on a guarded operation with force=true", write, guarded + "?force=true", "manage", "testGuardedThing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := rig.calls.Load()
			rec := rig.do(t, req{method: http.MethodPost, path: tc.path, ip: "203.0.113.30", key: tc.key})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", rec.Code)
			}
			p := problem(t, rec)
			if p["code"] != "insufficient_scope" || p["requiredRight"] != tc.right || p["operationId"] != tc.op {
				t.Errorf("problem %v, want insufficient_scope, requiredRight %s, operationId %s", p, tc.right, tc.op)
			}
			if rig.calls.Load() != before {
				t.Error("the operation ran despite the missing right")
			}
		})
	}

	for _, tc := range []struct {
		name, key, path string
		status          int
	}{
		{"write key on a guarded operation without force", write, guarded, http.StatusOK},
		{"write key on a guarded operation with force=false", write, guarded + "?force=false", http.StatusOK},
		{"manage key on a guarded operation with force=true", manage, guarded + "?force=true", http.StatusOK},
		{"force neither true nor false", manage, guarded + "?force=yes", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rig.do(t, req{method: http.MethodPost, path: tc.path, ip: "203.0.113.30", key: tc.key})
			if rec.Code != tc.status {
				t.Errorf("status %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

// TestAuthChain_WorkerID — row 64 (US-5, SC-026). X-Skein-Worker-Id empty,
// 65 characters long or with '/' is 400; a valid one is the holder in the
// context; a request of worker W extends the activity of W only.
func TestAuthChain_WorkerID(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	rig := newChainRig(t, pool, nil, 100, 100)
	key, keyID := mintKey(t, pool, owner, level(apikeys.LevelRead))

	for _, bad := range []string{"", strings.Repeat("w", 65), "a/b"} {
		rec := rig.do(t, req{ip: "203.0.113.40", key: key, headers: map[string]string{publicapi.WorkerIDHeader: bad}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("worker id %q: %d, want 400", bad, rec.Code)
			continue
		}
		if code := problem(t, rec)["code"]; code != "validation_error" {
			t.Errorf("worker id %q: code %v, want validation_error", bad, code)
		}
	}

	// Two executors of the key, both last seen an hour ago.
	ctx := context.Background()
	for _, w := range []string{"w-1", "w-2"} {
		if _, err := agents.TouchRESTExecutor(ctx, pool, owner, keyID, w, agents.LastSeenGranularity); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET last_seen_at = now() - interval '1 hour' WHERE api_key_id = $1`, keyID); err != nil {
		t.Fatal(err)
	}

	rec := rig.do(t, req{ip: "203.0.113.40", key: key, headers: map[string]string{publicapi.WorkerIDHeader: "w-1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid worker id: %d", rec.Code)
	}
	if want := "api:" + keyID.String() + ":w-1"; rig.lastSeen().holder != want {
		t.Errorf("holder %q, want %q", rig.lastSeen().holder, want)
	}
	stale := func(w string) bool {
		var old bool
		if err := pool.QueryRow(ctx, `
SELECT last_seen_at < now() - interval '30 minutes' FROM agents
 WHERE api_key_id = $1 AND worker_id = $2`, keyID, w).Scan(&old); err != nil {
			t.Fatalf("read executor %s: %v", w, err)
		}
		return old
	}
	if stale("w-1") {
		t.Error("the request of w-1 did not extend w-1's activity")
	}
	if !stale("w-2") {
		t.Error("the request of w-1 extended w-2's activity")
	}

	if rec := rig.do(t, req{ip: "203.0.113.40", key: key}); rec.Code != http.StatusOK {
		t.Fatalf("no worker id: %d", rec.Code)
	}
	if want := "api:" + keyID.String(); rig.lastSeen().holder != want {
		t.Errorf("holder without a worker id %q, want the key %q", rig.lastSeen().holder, want)
	}
}

// TestAuthChain_IdentityHeadersIgnored — row 65 (INV-10, SC-025). Forged
// X-Skein-User-*, X-Skein-Surface and Mcp-Session-Id change neither the
// identity (the key's owner), nor the surface (the key of an internal owner
// passes without the internal marker: sk-163 does not apply here; so does
// the key of an external owner), nor the holder (from X-Skein-Worker-Id).
func TestAuthChain_IdentityHeadersIgnored(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	internalOwner, _ := testsupport.SeedUser(t, pool, "user")
	externalOwner, _ := testsupport.SeedUser(t, pool, "user")
	if _, err := pool.Exec(context.Background(), `UPDATE users SET is_external = true WHERE id = $1`, externalOwner); err != nil {
		t.Fatal(err)
	}
	other, _ := testsupport.SeedUser(t, pool, "admin")
	rig := newChainRig(t, pool, nil, 100, 100)

	forged := map[string]string{
		auth.HeaderUserID:         other.String(),
		auth.HeaderUserRole:       "admin",
		auth.HeaderUserIsExternal: "false",
		auth.HeaderSurface:        "public",
		auth.HeaderMcpSessionID:   "mcp-session-forged",
		publicapi.WorkerIDHeader:  "w-9",
	}
	for _, tc := range []struct {
		name     string
		owner    uuid.UUID
		external bool
	}{
		{"internal owner", internalOwner, false},
		{"external owner", externalOwner, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, keyID := mintKey(t, pool, tc.owner, level(apikeys.LevelRead))
			rec := rig.do(t, req{ip: "203.0.113.70", key: key, headers: forged})
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			s := rig.lastSeen()
			if s.userID != tc.owner || s.role != "user" || s.isExternal != tc.external || s.keyID != keyID {
				t.Errorf("identity %+v, want the key's owner %s (role user, external %v)", s, tc.owner, tc.external)
			}
			if want := "api:" + keyID.String() + ":w-9"; s.holder != want {
				t.Errorf("holder %q, want %q from X-Skein-Worker-Id", s.holder, want)
			}
		})
	}
}
