package handler

// hub_access_test.go — sk-622.21, T006, test matrix row 10 (US-9: rights =
// project visibility by the R3.1 rules). Reference — spec US-9 "A project can be announced
// only if the user sees it (a member or an internal admin;
// an external admin is limited by membership)", INV-9, and the contract godoc of
// projects/access_contract.go (T028), all written before the handler:
//
//   - a member sees their projects; an internal admin sees every project; an
//     external user sees only their memberships; a deactivated user nothing;
//   - allVisible yields visibleProjectIds for every user — for the internal
//     admin too, with internalAdmin as the role only (contract version 2,
//     sk-674.18 T008);
//   - the answer is keyed by user: in ONE batch each user gets their own;
//   - one database statement per call, at N = 1 and at N = 50 users.
//
// The external user here has role "user": an external admin cannot exist in
// the data (CHECK users_external_not_admin), so "external admin limited to
// memberships" is observed on the external-user row, which is the same rule.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// hubStatementCounter counts statements pgx sends while recording is on.
type hubStatementCounter struct {
	mu  sync.Mutex
	on  bool
	sql []string
}

func (c *hubStatementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on {
		c.sql = append(c.sql, data.SQL)
	}
	return ctx
}

func (c *hubStatementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *hubStatementCounter) record(fn func()) []string {
	c.mu.Lock()
	c.sql, c.on = nil, true
	c.mu.Unlock()
	fn()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.on = false
	return append([]string(nil), c.sql...)
}

// newHubTracedPool is a one-connection pool with the statement counter, so
// every statement the handler sends is observed.
func newHubTracedPool(t *testing.T) (*pgxpool.Pool, *hubStatementCounter) {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	counter := &hubStatementCounter{}
	cfg.ConnConfig.Tracer = counter
	cfg.MinConns, cfg.MaxConns = 1, 1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, counter
}

func callHubAccess(t *testing.T, pool *pgxpool.Pool, req projects.ProjectAccessRequest) projects.ProjectAccessResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rec := httptest.NewRecorder()
	NewHubAccessHandler(pool).ProjectAccess(rec,
		httptest.NewRequest(http.MethodPost, "/api/v1/hub/project-access", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp projects.ProjectAccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %s: %v", rec.Body.String(), err)
	}
	if len(resp.Users) != len(req.Users) {
		t.Fatalf("response has %d entries for %d requested", len(resp.Users), len(req.Users))
	}
	return resp
}

// sortedUUIDs is a sorted copy of ids, for comparing sets.
func sortedUUIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func sameIDs(got, want []uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestHubAccess_RightsFollowProjectVisibility — four kinds of user in ONE
// batch, each answered by their own rights.
func TestHubAccess_RightsFollowProjectVisibility(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	member, _ := testsupport.SeedUser(t, pool, "user")
	admin, _ := testsupport.SeedUser(t, pool, "admin")
	external, _ := testsupport.SeedUser(t, pool, "user")
	deactivated, _ := testsupport.SeedUser(t, pool, "user")
	if _, err := pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, external); err != nil {
		t.Fatalf("mark external: %v", err)
	}

	p, _ := testsupport.SeedProject(t, pool, member, "hubp")
	q, _ := testsupport.SeedProject(t, pool, admin, "hubq")
	testsupport.AddProjectMember(t, pool, q, external, "member")
	testsupport.AddProjectMember(t, pool, p, deactivated, "member")
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, deactivated); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	missing := uuid.New() // no such project
	// "Personal" spaces of the admin and of the member (INV-8, INV-1 a/d): the admin's
	// set holds his own, never another's.
	adminPersonal := testsupport.SeedSpaceOfKind(t, pool, admin, "personal")
	adminL, _ := testsupport.SeedProjectInSpace(t, pool, admin, adminPersonal, "hubla")
	memberPersonal := testsupport.SeedSpaceOfKind(t, pool, member, "personal")
	memberL, _ := testsupport.SeedProjectInSpace(t, pool, member, memberPersonal, "hublm")

	resp := callHubAccess(t, pool, projects.ProjectAccessRequest{Users: []projects.ProjectAccessQuery{
		{UserID: member, ProjectIDs: []uuid.UUID{p, q}},
		{UserID: external, ProjectIDs: []uuid.UUID{p, q}},
		{UserID: admin, ProjectIDs: []uuid.UUID{p, q, missing}},
		{UserID: deactivated, ProjectIDs: []uuid.UUID{p}},
		{UserID: member, AllVisible: true},
		{UserID: admin, AllVisible: true},
	}})

	type want struct {
		who                     string
		active, internal        bool
		allowed, denied, member []uuid.UUID
	}
	for i, w := range []want{
		{who: "member of P", active: true, allowed: []uuid.UUID{p}, denied: []uuid.UUID{q}},
		{who: "external, member of Q only", active: true, allowed: []uuid.UUID{q}, denied: []uuid.UUID{p}},
		{who: "internal admin", active: true, internal: true, allowed: []uuid.UUID{p, q}, denied: []uuid.UUID{missing}},
		{who: "deactivated member of P", allowed: []uuid.UUID{}, denied: []uuid.UUID{p}},
		{who: "member, all visible", active: true, allowed: []uuid.UUID{}, denied: []uuid.UUID{}, member: []uuid.UUID{p, memberL}},
		// Contract version 2 (row 196): the admin gets the SET, not a
		// wildcard — every non-personal project plus his own "Personal", never
		// another's. The database is shared, so the set is asserted by its
		// members: P, Q and his own "Personal" in, the member's "Personal" out.
		{who: "internal admin, all visible", active: true, internal: true, allowed: []uuid.UUID{}, denied: []uuid.UUID{}},
	} {
		got := resp.Users[i]
		if got.Active != w.active || got.InternalAdmin != w.internal {
			t.Errorf("%s: active/internalAdmin = %v/%v, want %v/%v", w.who, got.Active, got.InternalAdmin, w.active, w.internal)
		}
		if !sameIDs(got.Allowed, w.allowed) || !sameIDs(got.Denied, w.denied) {
			t.Errorf("%s: allowed/denied = %v / %v, want %v / %v", w.who, got.Allowed, got.Denied, w.allowed, w.denied)
		}
		if w.who == "internal admin, all visible" {
			for name, id := range map[string]uuid.UUID{"P": p, "Q": q, "his own Personal": adminL} {
				if !slices.Contains(got.VisibleProjectIDs, id) {
					t.Errorf("%s: visibleProjectIds lacks %s (%s)", w.who, name, id)
				}
			}
			if slices.Contains(got.VisibleProjectIDs, memberL) {
				t.Errorf("%s: visibleProjectIds holds another user's Personal %s", w.who, memberL)
			}
			continue
		}
		// visibleProjectIds is a set: the contract promises no order.
		if !sameIDs(sortedUUIDs(got.VisibleProjectIDs), sortedUUIDs(w.member)) {
			t.Errorf("%s: visibleProjectIds = %v, want %v", w.who, got.VisibleProjectIDs, w.member)
		}
	}
	if !resp.Users[1].External {
		t.Errorf("external user: external = false, want true")
	}
}

// TestHubAccess_OneStatementPerCall — one database statement per call at
// N = 1 and at N = 50 users.
func TestHubAccess_OneStatementPerCall(t *testing.T) {
	pool, counter := newHubTracedPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	p, _ := testsupport.SeedProject(t, pool, owner, "hubn")

	batch := func(n int) projects.ProjectAccessRequest {
		req := projects.ProjectAccessRequest{}
		for i := 0; i < n; i++ {
			u, _ := testsupport.SeedUser(t, pool, "user")
			if i%2 == 0 {
				testsupport.AddProjectMember(t, pool, p, u, "member")
			}
			req.Users = append(req.Users, projects.ProjectAccessQuery{
				UserID: u, ProjectIDs: []uuid.UUID{p}, AllVisible: i%3 == 0,
			})
		}
		return req
	}
	for _, n := range []int{1, 50} {
		req := batch(n)
		stmts := counter.record(func() { callHubAccess(t, pool, req) })
		if len(stmts) != 1 {
			t.Errorf("N = %d: the call sent %d statements, want 1:\n%v", n, len(stmts), stmts)
		}
	}
}

// TestHubAccess_BodyIsExactlyOneJSONValue — review fix (codex warning) of the
// T028 contract «400 — the body is not JSON of the documented shape … or
// exceeds 16 MiB»: data after the first JSON value is a syntax error, and so
// is padding that crosses the limit; trailing whitespace is not. No database:
// every case is decided before the access query (an empty batch never reaches
// it).
func TestHubAccess_BodyIsExactlyOneJSONValue(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"single value with trailing newline", `{"users":[]}` + "\n", http.StatusOK},
		{"garbage after the value", `{"users":[]} garbage`, http.StatusBadRequest},
		{"second JSON value", `{"users":[]}{"users":[]}`, http.StatusBadRequest},
		{"whitespace padding past 16 MiB", `{"users":[]}` + strings.Repeat(" ", hubAccessMaxBytes), http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		NewHubAccessHandler(nil).ProjectAccess(rec,
			httptest.NewRequest(http.MethodPost, "/api/v1/hub/project-access", strings.NewReader(c.body)))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d; body %s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}
