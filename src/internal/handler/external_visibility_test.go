// external_visibility_test — integration tests for sk-163 S6: an EXTERNAL
// caller must not enumerate people / activity from projects they are not a
// member of. Covers the directory, profile, and dashboard (scoring / race /
// in-progress / feed / agents) surfaces. Internal callers must be unchanged
// (full directory, global war-room). Seeds run against TEST_DATABASE_URL and
// reuse the seed helpers from users_test.go / dashboard_test.go (same
// package).
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/dashboard"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// ---------------- builders + seed helpers ----------------

func buildUsersHandlerX(t *testing.T, pool *pgxpool.Pool) *UsersHandler {
	t.Helper()
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	return NewUsersHandler(pool, usersStore, projectsStore)
}

func buildDashHandlerX(t *testing.T, pool *pgxpool.Pool) *DashboardHandler {
	t.Helper()
	return NewDashboardHandler(pool, dashboard.New(pool), issues.NewStore(pool))
}

// seedExternalHandlerUser inserts an is_external=true users row (role
// forced to 'user' — the 000033 CHECK forbids external+admin).
func seedExternalHandlerUser(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ex" + strings.ReplaceAll(id.String()[:18], "-", "")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, is_external, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', true, 'unused', now(), '{}'::jsonb)`,
		id, uname+"@ext.local", uname, uname); err != nil {
		t.Fatalf("seed external user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

// addHandlerMember adds userID to projectID as 'member'. The explicit
// cleanup (LIFO before the project/user cleanups) keeps added_by's ON
// DELETE RESTRICT happy.
func addHandlerMember(t *testing.T, pool *pgxpool.Pool, projectID, userID, addedBy uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, userID, addedBy); err != nil {
		t.Fatalf("add member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, projectID, userID)
	})
}

func setIssueInProgress(t *testing.T, pool *pgxpool.Pool, issueID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET status = 'in_progress', started_at = now() WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
}

func seedHandlerAgent(t *testing.T, pool *pgxpool.Pool, userID, projectID uuid.UUID) {
	t.Helper()
	sess := "sess-" + uuid.Must(uuid.NewRandom()).String()[:12]
	// The session with its work in projectID: a HOT ZONE agents zone counts
	// a session by its activity in the reach.
	if _, err := pool.Exec(context.Background(), `
WITH a AS (
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at)
VALUES ($1, $2, now(), now()) RETURNING user_id)
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type, actor_session_id, entity_type, entity_id, project_id, payload)
SELECT now(), 'issue.updated', 'agent', a.user_id, 'agent', $2, 'issue', 'agent-work', $3, '{}'::jsonb FROM a`,
		userID, sess, projectID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE actor_session_id = $1`, sess)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM agents WHERE user_id = $1 AND session_id = $2`, userID, sess)
	})
}

// ---------------- call helpers (with IsExternal) ----------------

func fullDashRouter(h *DashboardHandler) chi.Router {
	r := chi.NewRouter()
	r.Get("/api/v1/dashboard/scoring", h.Scoring)
	r.Get("/api/v1/dashboard/feed", h.Feed)
	r.Get("/api/v1/dashboard/in-progress", h.InProgress)
	r.Get("/api/v1/dashboard/race", h.Race)
	r.Get("/api/v1/dashboard/agents", h.Agents)
	r.Get("/api/v1/dashboard/projects", h.Projects)
	return r
}

func callDashEx(t *testing.T, h *DashboardHandler, target string, userID uuid.UUID, external bool) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := auth.WithUserID(req.Context(), userID)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithIsExternal(ctx, external)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	fullDashRouter(h).ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res, rec.Body.Bytes()
}

func callDirEx(t *testing.T, h *UsersHandler, callerID uuid.UUID, external bool, query string) (*http.Response, []byte) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/v1/users/directory", h.ListDirectory)
	target := "/api/v1/users/directory"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := auth.WithUserID(req.Context(), callerID)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithIsExternal(ctx, external)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res, rec.Body.Bytes()
}

func callProfEx(t *testing.T, h *UsersHandler, callerID uuid.UUID, external bool, username string) (*http.Response, []byte) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/v1/users/profile/{username}", h.GetUserProfile)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/profile/"+username, nil)
	ctx := auth.WithUserID(req.Context(), callerID)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithIsExternal(ctx, external)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res, rec.Body.Bytes()
}

// usernamesFromDirectory decodes the sk-285.5.5 `{items, hasMore}`
// envelope into a username set.
func usernamesFromDirectory(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var res struct {
		Items []struct {
			Username string `json:"username"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("decode directory: %v (body=%s)", err, string(body))
	}
	set := make(map[string]bool, len(res.Items))
	for _, r := range res.Items {
		set[r.Username] = true
	}
	return set
}

// ---------------- directory ----------------

// TestExternalDirectory_ScopedToCoMembership — an external caller sees only
// the co-membership set (people who share a project with them); an internal
// caller sees the full directory (no scoping).
func TestExternalDirectory_ScopedToCoMembership(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildUsersHandlerX(t, pool)

	xID, xName := seedUserRowFull(t, pool, "user") // owner of A
	yID, yName := seedUserRowFull(t, pool, "user") // member of A
	zID, zName := seedUserRowFull(t, pool, "user") // owner of B (unrelated)
	eID, eName := seedExternalHandlerUser(t, pool) // external, member of A

	projA, _ := seedDashboardProject(t, pool, xID)
	addHandlerMember(t, pool, projA, yID, xID)
	addHandlerMember(t, pool, projA, eID, xID)
	_, _ = seedDashboardProject(t, pool, zID) // project B; E is NOT a member

	// External E: co-set = members of A = {X, Y, E}; Z excluded.
	//
	// sk-285.5.5 — the directory is a bounded page over a shared test DB, so
	// each co-member is asked for by name (`q=`) rather than looked for in an
	// unbounded dump. Searching for the NON-co-member is the stronger form of
	// the leak assertion: the scoping has to survive a search that targets
	// them directly, not merely keep them off page 1.
	for _, want := range []string{xName, yName, eName} {
		res, body := callDirEx(t, h, eID, true, "q="+want)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("external directory: status %d body=%s", res.StatusCode, string(body))
		}
		if got := usernamesFromDirectory(t, body); !got[want] {
			t.Errorf("external directory missing co-member %q (body=%s)", want, string(body))
		}
	}
	res, body := callDirEx(t, h, eID, true, "q="+zName)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("external directory: status %d body=%s", res.StatusCode, string(body))
	}
	if got := usernamesFromDirectory(t, body); got[zName] {
		t.Errorf("external directory leaked non-co-member %q under a targeted search (body=%s)",
			zName, string(body))
	}

	// Internal X: unscoped directory reaches Z (no co-membership filter).
	res, body = callDirEx(t, h, xID, false, "q="+zName)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("internal directory: status %d body=%s", res.StatusCode, string(body))
	}
	if got := usernamesFromDirectory(t, body); !got[zName] {
		t.Errorf("internal directory should reach unrelated user %q (no scoping)", zName)
	}
}

// ---------------- profile ----------------

// TestExternalProfile_CoMembershipGate — external caller gets a co-member's
// profile (200) but a non-co-member and a nonexistent username both return
// 404 (existence-oracle closure). Internal callers are unaffected.
func TestExternalProfile_CoMembershipGate(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildUsersHandlerX(t, pool)

	xID, _ := seedUserRowFull(t, pool, "user")
	_, yName := seedUserRowFull(t, pool, "user")
	yID := mustUserID(t, pool, yName)
	zID, zName := seedUserRowFull(t, pool, "user")
	eID, _ := seedExternalHandlerUser(t, pool)

	projA, _ := seedDashboardProject(t, pool, xID)
	addHandlerMember(t, pool, projA, yID, xID)
	addHandlerMember(t, pool, projA, eID, xID)
	_, _ = seedDashboardProject(t, pool, zID) // Z in separate project B

	// co-member Y → 200
	res, body := callProfEx(t, h, eID, true, yName)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("external co-member profile: status %d, want 200 (body=%s)", res.StatusCode, string(body))
	}

	// non-co-member Z → 404
	res, body = callProfEx(t, h, eID, true, zName)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("external non-co-member profile: status %d, want 404 (body=%s)", res.StatusCode, string(body))
	}
	assertErrCode(t, body, "user_not_found")

	// nonexistent → 404 with the SAME body code as the non-co-member arm.
	// Both must be byte-identical (user_not_found) so probing the profile
	// surface can't distinguish "exists but not my collaborator" from "no such
	// user" — the existence oracle is fully closed (body + the equalized
	// query-count in the handler).
	ghost := "ghost" + uuid.Must(uuid.NewRandom()).String()[:8]
	res, body = callProfEx(t, h, eID, true, ghost)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("external nonexistent profile: status %d, want 404 (body=%s)", res.StatusCode, string(body))
	}
	assertErrCode(t, body, "user_not_found")

	// internal caller: nonexistent username stays a 422 business error
	// (unchanged behaviour).
	res, body = callProfEx(t, h, xID, false, ghost)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("internal nonexistent profile: status %d, want 422 (body=%s)", res.StatusCode, string(body))
	}
}

// TestExternalZeroMembership_SeesOnlySelf — sk-163.16 #2 / S6 zero-membership
// edge. An external user with NO project memberships must still see THEMSELVES
// in the directory (CoVisibleUsersFor always self-seeds — a self-profile
// lookup never 404s) and read their own profile (200), while every other user
// is filtered out. A membership-less external cannot enumerate any staff.
func TestExternalZeroMembership_SeesOnlySelf(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildUsersHandlerX(t, pool)

	// Two unrelated internal users the external shares NO project with.
	_, xName := seedUserRowFull(t, pool, "user")
	_, yName := seedUserRowFull(t, pool, "user")
	eID, eName := seedExternalHandlerUser(t, pool) // external, ZERO memberships

	// Directory: co-set = {self} only. No `q=` here on purpose — the point
	// is that the WHOLE page is one row, which only means something when the
	// page is unfiltered.
	res, body := callDirEx(t, h, eID, true, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("zero-membership external directory: status %d body=%s", res.StatusCode, string(body))
	}
	got := usernamesFromDirectory(t, body)
	if !got[eName] {
		t.Errorf("zero-membership external missing SELF %q in directory (body=%s)", eName, string(body))
	}
	if got[xName] || got[yName] {
		t.Errorf("zero-membership external leaked non-co-members (x=%v y=%v)", got[xName], got[yName])
	}
	if len(got) != 1 {
		t.Errorf("zero-membership external directory size = %d, want 1 (self only): %v", len(got), got)
	}

	// Own profile → 200 (self is always co-visible).
	res, body = callProfEx(t, h, eID, true, eName)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("zero-membership external own profile: status %d, want 200 (body=%s)", res.StatusCode, string(body))
	}
}

func mustUserID(t *testing.T, pool *pgxpool.Pool, username string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		t.Fatalf("lookup user %q: %v", username, err)
	}
	return id
}

func assertErrCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got, _ := raw["error"].(string); got != want {
		t.Errorf("error code = %q, want %q (body=%s)", got, want, string(body))
	}
}

// ---------------- dashboard: scoring ----------------

// TestExternalDashboard_Scoring_ExternalCountsOwnProjectsOnly — sk-674.18
// T030 (US-13: "an external user - only his own projects; the global picture for
// an external user is narrowed"): an external caller's scoring counts the projects he is
// a member of and nothing else; an internal caller counts his reach. Pinned
// to the freshly-seeded owner via user=<name> so ambient rows from other
// tests cannot perturb the exact count. (Replaces
// TestExternalDashboard_Scoring_GlobalForExternal, whose global war-room US-13
// retires.)
func TestExternalDashboard_Scoring_ExternalCountsOwnProjectsOnly(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildDashHandlerX(t, pool)

	owner, ownerName := seedDashboardUser(t, pool, "user")
	eID, _ := seedExternalHandlerUser(t, pool)
	projA, _ := seedDashboardProject(t, pool, owner) // E is a member
	projB, _ := seedDashboardProject(t, pool, owner) // E NOT a member
	addHandlerMember(t, pool, projA, eID, owner)

	now := time.Now().UTC()
	aID := "sc-a-" + uuid.Must(uuid.NewRandom()).String()[:8]
	bID := "sc-b-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, projA, owner, aID, "human")
	seedDashboardEvent(t, pool, "issue.created", aID, projA, ownerName, owner, "human", now.Add(-30*time.Minute))
	seedDashboardIssue(t, pool, projB, owner, bID, "human")
	seedDashboardEvent(t, pool, "issue.created", bID, projB, ownerName, owner, "human", now.Add(-30*time.Minute))

	readCreated := func(userID uuid.UUID, external bool) int64 {
		t.Helper()
		// Pin to the owner: their only two issue.created events are aID +
		// bID, so a global count is exactly 2 regardless of ambient rows.
		res, body := callDashEx(t, h, "/api/v1/dashboard/scoring?window=24h&user="+ownerName, userID, external)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("scoring status %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Metrics []struct {
				Key   string `json:"key"`
				Human int64  `json:"human"`
			} `json:"metrics"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode scoring: %v", err)
		}
		for _, m := range got.Metrics {
			if m.Key == "issues_created" {
				return m.Human
			}
		}
		t.Fatalf("issues_created metric missing")
		return 0
	}

	// External E: only project A, his own — B adds nothing.
	if got := readCreated(eID, true); got != 1 {
		t.Errorf("external issues_created = %d, want 1 (his project A only)", got)
	}

	// Internal owner sees both projects: 2.
	if got := readCreated(owner, false); got != 2 {
		t.Errorf("internal issues_created = %d, want 2 (both projects in his reach)", got)
	}
}

// ---------------- dashboard: race ----------------

// TestExternalDashboard_Race_ExternalOwnProjectsOnly — sk-674.18 T030 (US-13): the race chart
// of an external caller covers his own projects only — nothing of the
// non-member project B, not even anonymized (he is in no space, INV-28);
// an internal participant of B's space gets B's contribution anonymized.
// (Replaces TestExternalDashboard_Race_GlobalForExternal: the global war-room for the external caller is
// what US-13 retires.)
func TestExternalDashboard_Race_ExternalOwnProjectsOnly(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildDashHandlerX(t, pool)

	ownerA, _ := seedDashboardUser(t, pool, "user")
	ownerB, _ := seedDashboardUser(t, pool, "user")
	vID, vName := seedDashboardUser(t, pool, "user") // closes in A (visible to E)
	wID, wName := seedDashboardUser(t, pool, "user") // closes in B (hidden from E)
	eID, _ := seedExternalHandlerUser(t, pool)
	projA, _ := seedDashboardProject(t, pool, ownerA)
	projB, _ := seedDashboardProject(t, pool, ownerB)
	addHandlerMember(t, pool, projA, eID, ownerA)

	now := time.Now().UTC()
	aID := "rc-a-" + uuid.Must(uuid.NewRandom()).String()[:8]
	bID := "rc-b-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, projA, ownerA, aID, "human")
	seedDashboardEvent(t, pool, "issue.closed", aID, projA, vName, vID, "human", now.Add(-2*time.Hour))
	seedDashboardIssue(t, pool, projB, ownerB, bID, "human")
	// W is seeded as a DOMINANT closer in the hidden project B (many
	// issue.closed events), so it is guaranteed to survive the topN cutoff
	// regardless of ambient closed-events that leak across handler tests in
	// the shared within-run DB. A count of 30 dwarfs any per-actor ambient.
	// (Race counts events via COUNT(*), so all 30 may sit on the one issue.)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
SELECT $1, 'issue.closed', $2, $3, 'human', 'issue', $4, $5, '{}'::jsonb
  FROM generate_series(1, 30)`,
		now.Add(-2*time.Hour), wName, wID, bID, projB); err != nil {
		t.Fatalf("seed W closes: %v", err)
	}

	raceUsers := func(userID uuid.UUID, external bool) map[string]bool {
		t.Helper()
		res, body := callDashEx(t, h, "/api/v1/dashboard/race?metric=closed&window=30d&topN=20", userID, external)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("race status %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Users []struct {
				Username string `json:"username"`
			} `json:"users"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode race: %v", err)
		}
		set := make(map[string]bool)
		for _, u := range got.Users {
			set[u.Username] = true
		}
		return set
	}

	// External E is a member of project A only. W acted ONLY in the hidden
	// project B — so W appearing in E's race chart is the direct proof of the
	// S6→S1 reversal (under the old scope-to-member behaviour W was absent).
	// W is a dominant closer, so its top-N presence is robust to ambient.
	// sk-674.18 T030 (US-13): the external caller's chart covers his own
	// projects only — W, who acted only in B, is absent.
	ext := raceUsers(eID, true)
	if ext[wName] {
		t.Errorf("external race includes actor %q of the non-member project B — US-13 narrows the external caller to his projects", wName)
	}

	// The internal caller (ownerA, a participant of the space of B) gets B's
	// contribution in his reach (INV-28): W is there.
	intern := raceUsers(ownerA, false)
	if !intern[wName] {
		t.Errorf("internal race should include actor %q: project B contributes to his reach (INV-28)", wName)
	}
}

// ---------------- dashboard: in-progress ----------------

// TestExternalDashboard_InProgress_ExternalOwnProjectsOnly — sk-674.18 T030 (US-13): the in-progress snapshot
// of an external caller covers his own projects only — nothing of the
// non-member project B, not even anonymized (he is in no space, INV-28);
// an internal participant of B's space gets B's contribution anonymized.
// (Replaces TestExternalDashboard_InProgress_PresentButHardRedacted: the global war-room for the external caller is
// what US-13 retires.)
func TestExternalDashboard_InProgress_ExternalOwnProjectsOnly(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildDashHandlerX(t, pool)

	ownerA, _ := seedDashboardUser(t, pool, "user")
	ownerB, ownerBName := seedDashboardUser(t, pool, "user")
	eID, _ := seedExternalHandlerUser(t, pool)
	projA, _ := seedDashboardProject(t, pool, ownerA)
	projB, _ := seedDashboardProject(t, pool, ownerB)
	addHandlerMember(t, pool, projA, eID, ownerA)

	aID := "ip-a-" + uuid.Must(uuid.NewRandom()).String()[:8]
	bID := "ip-b-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, projA, ownerA, aID, "human")
	setIssueInProgress(t, pool, aID)
	seedDashboardIssue(t, pool, projB, ownerB, bID, "human")
	setIssueInProgress(t, pool, bID)
	// B keeps a (kept-under-Hard) assignee so the redacted row — whose
	// issueId is stripped — is still findable.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET assignee = $2 WHERE id = $1`, bID, ownerBName); err != nil {
		t.Fatalf("set B assignee: %v", err)
	}

	// Decode into raw maps so field ABSENCE (projectId dropped on Hard) is
	// observable, not just its zero value.
	fetch := func(userID uuid.UUID, external bool) []map[string]json.RawMessage {
		t.Helper()
		res, body := callDashEx(t, h, "/api/v1/dashboard/in-progress?limit=200", userID, external)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("in-progress status %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode in-progress: %v", err)
		}
		return got.Items
	}
	str := func(row map[string]json.RawMessage, key string) string {
		raw, ok := row[key]
		if !ok {
			return ""
		}
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	boolv := func(row map[string]json.RawMessage, key string) bool {
		raw, ok := row[key]
		if !ok {
			return false
		}
		var b bool
		_ = json.Unmarshal(raw, &b)
		return b
	}

	// External E: A present + full (issueId kept); B present + Hard-redacted.
	extItems := fetch(eID, true)
	var aRow, bRow map[string]json.RawMessage
	for _, it := range extItems {
		switch {
		case str(it, "issueId") == aID:
			aRow = it
		case boolv(it, "redacted") && str(it, "assignee") == ownerBName:
			bRow = it
		}
	}
	if aRow == nil {
		t.Fatalf("external in-progress missing visible issue %q", aID)
	}
	if boolv(aRow, "redacted") {
		t.Errorf("visible row %q must not be redacted", aID)
	}
	// sk-674.18 T030 (US-13): no row of B for the external caller, not even
	// a redacted one — he is in no space, B contributes nothing to him.
	if bRow != nil {
		t.Errorf("external in-progress carries a row of the non-member project B (assignee %q): %v", ownerBName, bRow)
	}

	// Internal ownerA (member of A, NOT B): global war-room shows B's row
	// SOFT-redacted — content stripped but issue id KEPT (unchanged).
	intItems := fetch(ownerA, false)
	var intB map[string]json.RawMessage
	for _, it := range intItems {
		if str(it, "issueId") == bID {
			intB = it
			break
		}
	}
	if intB == nil {
		t.Fatalf("internal in-progress should include non-member issue %q (war-room global)", bID)
	}
	if !boolv(intB, "redacted") {
		t.Errorf("internal war-room row %q should be redacted (non-member project)", bID)
	}
	if _, present := intB["projectId"]; !present {
		t.Errorf("internal SOFT-redacted row must KEEP projectId (identity preserved): %v", intB)
	}
}

// ---------------- dashboard: feed ----------------

// TestExternalDashboard_Feed_ExternalOwnProjectsOnly — sk-674.18 T030 (US-13): the feed
// of an external caller covers his own projects only — nothing of the
// non-member project B, not even anonymized (he is in no space, INV-28);
// an internal participant of B's space gets B's contribution anonymized.
// (Replaces TestExternalDashboard_Feed_PresentButHardRedacted: the global war-room for the external caller is
// what US-13 retires.)
func TestExternalDashboard_Feed_ExternalOwnProjectsOnly(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildDashHandlerX(t, pool)

	ownerA, ownerAName := seedDashboardUser(t, pool, "user")
	ownerB, ownerBName := seedDashboardUser(t, pool, "user")
	eID, _ := seedExternalHandlerUser(t, pool)
	projA, _ := seedDashboardProject(t, pool, ownerA)
	projB, _ := seedDashboardProject(t, pool, ownerB)
	addHandlerMember(t, pool, projA, eID, ownerA)

	now := time.Now().UTC()
	aID := "fd-a-" + uuid.Must(uuid.NewRandom()).String()[:8]
	bID := "fd-b-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, projA, ownerA, aID, "human")
	seedDashboardEvent(t, pool, "issue.created", aID, projA, ownerAName, ownerA, "human", now.Add(-10*time.Minute))
	seedDashboardIssue(t, pool, projB, ownerB, bID, "human")
	seedDashboardEvent(t, pool, "issue.created", bID, projB, ownerBName, ownerB, "human", now.Add(-10*time.Minute))

	// Decode into raw maps so field ABSENCE (projectId dropped on Hard) is
	// observable; the entity sub-object is decoded lazily where needed.
	type rawRow struct {
		Actor    struct{ Username string } `json:"actor"`
		Redacted bool                      `json:"redacted"`
		Raw      map[string]json.RawMessage
	}
	fetch := func(userID uuid.UUID, external bool) map[string]rawRow {
		t.Helper()
		res, body := callDashEx(t, h, "/api/v1/dashboard/feed?limit=100", userID, external)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("feed status %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode feed: %v", err)
		}
		byActor := make(map[string]rawRow)
		for _, item := range got.Items {
			var r rawRow
			if err := json.Unmarshal(item, &r); err != nil {
				t.Fatalf("decode feed item: %v", err)
			}
			if err := json.Unmarshal(item, &r.Raw); err != nil {
				t.Fatalf("decode feed item raw: %v", err)
			}
			byActor[r.Actor.Username] = r
		}
		return byActor
	}
	entityField := func(r rawRow, key string) (string, bool) {
		raw, ok := r.Raw["entity"]
		if !ok {
			return "", false
		}
		var ent map[string]json.RawMessage
		if err := json.Unmarshal(raw, &ent); err != nil {
			return "", false
		}
		v, present := ent[key]
		if !present {
			return "", false
		}
		var s string
		_ = json.Unmarshal(v, &s)
		return s, true
	}

	// External E: A present + full (actor ownerAName), B present + Hard-redacted.
	ext := fetch(eID, true)
	aRow, okA := ext[ownerAName]
	if !okA {
		t.Fatalf("external feed missing visible-project row (actor %q)", ownerAName)
	}
	if aRow.Redacted {
		t.Errorf("visible-project row (actor %q) must not be redacted", ownerAName)
	}
	// sk-674.18 T030 (US-13): no row of B for the external caller, not even
	// a redacted one.
	if bRow, okB := ext[ownerBName]; okB {
		t.Errorf("external feed carries a row of the non-member project B (actor %q): %v", ownerBName, bRow.Raw)
	}

	// Internal ownerA (member of A, NOT B): global war-room shows B's row
	// SOFT-redacted — content stripped but projectId + prefix KEPT.
	intern := fetch(ownerA, false)
	intB, ok := intern[ownerBName]
	if !ok {
		t.Fatalf("internal feed should include non-member project B row (actor %q, global war-room)", ownerBName)
	}
	if !intB.Redacted {
		t.Errorf("internal war-room project B row should be redacted (ownerA not a member of B)")
	}
	if _, present := intB.Raw["projectId"]; !present {
		t.Errorf("internal SOFT-redacted row must KEEP top-level projectId (identity preserved): %v", intB.Raw)
	}
	if pfx, present := entityField(intB, "projectIdPrefix"); !present || pfx == "" {
		t.Errorf("internal SOFT-redacted row must KEEP entity.projectIdPrefix, got %q present=%v", pfx, present)
	}
}

// ---------------- dashboard: agents ----------------

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestExternalDashboard_Agents_ExternalOwnProjectsOnly — sk-674.18 T030,
// row 204 (US-13, INV-28 last sentence, K-6). Replaces
// TestExternalDashboard_Agents_GlobalForExternal: the global roster for the
// external caller is what US-13 retires — an external user is in no space,
// so his agents zone covers the people of his own projects only. The agent
// of a co-member of his project is shown; the agent of a person who works
// only in another space is not — while an internal participant of that
// space does see it (the positive control: the person is a real agent).
func TestExternalDashboard_Agents_ExternalOwnProjectsOnly(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildDashHandlerX(t, pool)

	owner, ownerName := seedDashboardUser(t, pool, "user")
	eID, _ := seedExternalHandlerUser(t, pool)
	projA, _ := seedDashboardProject(t, pool, owner) // E is a member of A only
	addHandlerMember(t, pool, projA, eID, owner)
	seedHandlerAgent(t, pool, owner, projA)

	// A person of another space with a project of his own and an agent.
	stranger, strangerName := seedDashboardUser(t, pool, "user")
	strangerProject, _ := testsupport.SeedProjectInSpace(t, pool, stranger, testsupport.SeedSpace(t, pool, stranger), "agx")
	seedHandlerAgent(t, pool, stranger, strangerProject)

	decodeAgents := func(userID uuid.UUID, external bool) []string {
		t.Helper()
		res, body := callDashEx(t, h, "/api/v1/dashboard/agents", userID, external)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("agents status %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Items []struct {
				Username string `json:"username"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode agents: %v", err)
		}
		names := make([]string, 0, len(got.Items))
		for _, it := range got.Items {
			names = append(names, it.Username)
		}
		return names
	}

	external := decodeAgents(eID, true)
	if !contains(external, ownerName) {
		t.Errorf("external agents miss the co-member of his project %q: %v", ownerName, external)
	}
	if contains(external, strangerName) {
		t.Errorf("external agents carry %q, who works only in another space: %v", strangerName, external)
	}
	if own := decodeAgents(stranger, false); !contains(own, strangerName) {
		t.Errorf("the stranger's own agents zone misses him (control): %v", own)
	}
}

// ---------------- dashboard: projects (sk-183.1) ----------------

// metricValueRaw mirrors dashboard.MetricValue for tests that need to
// read the nested per-contributor breakdown without importing the store
// package's uuid-typed struct.
type metricValueRaw struct {
	Count        int64 `json:"count"`
	Contributors []struct {
		Username string `json:"username"`
		Count    int64  `json:"count"`
	} `json:"contributors"`
	OtherContributorsCount int64 `json:"otherContributorsCount"`
}

func decodeMetric(t *testing.T, row map[string]json.RawMessage, key string) metricValueRaw {
	t.Helper()
	raw, ok := row[key]
	if !ok {
		t.Fatalf("row missing metric %q: %v", key, row)
	}
	var mv metricValueRaw
	if err := json.Unmarshal(raw, &mv); err != nil {
		t.Fatalf("decode metric %q: %v", key, err)
	}
	return mv
}

func metricHasContributor(mv metricValueRaw, username string) bool {
	for _, c := range mv.Contributors {
		if c.Username == username {
			return true
		}
	}
	return false
}

// TestExternalDashboard_Projects_ExternalOwnProjectsOnly — sk-674.18 T030 (US-13): the hot projects
// of an external caller covers his own projects only — nothing of the
// non-member project B, not even anonymized (he is in no space, INV-28);
// an internal participant of B's space gets B's contribution anonymized.
// (Replaces TestExternalDashboard_Projects_ProjectIdentityOnlyRedaction: the global war-room for the external caller is
// what US-13 retires.)
func TestExternalDashboard_Projects_ExternalOwnProjectsOnly(t *testing.T) {
	pool := newDashboardTestPool(t)
	h := buildDashHandlerX(t, pool)

	ownerA, ownerAName := seedDashboardUser(t, pool, "user")
	ownerB, ownerBName := seedDashboardUser(t, pool, "user")
	eID, _ := seedExternalHandlerUser(t, pool)
	projA, _ := seedDashboardProject(t, pool, ownerA)
	projB, _ := seedDashboardProject(t, pool, ownerB)
	addHandlerMember(t, pool, projA, eID, ownerA)

	now := time.Now().UTC()
	aID := "pr-a-" + uuid.Must(uuid.NewRandom()).String()[:8]
	bID := "pr-b-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, projA, ownerA, aID, "human")
	seedDashboardEvent(t, pool, "issue.created", aID, projA, ownerAName, ownerA, "human", now.Add(-10*time.Minute))
	seedDashboardIssue(t, pool, projB, ownerB, bID, "human")
	seedDashboardEvent(t, pool, "issue.created", bID, projB, ownerBName, ownerB, "human", now.Add(-10*time.Minute))
	// Give B a commit too, so Commits/contributors are non-trivially
	// populated on the row we expect to stay content-visible.
	seedRaceIssueCommit(t, pool, bID, ownerB, now.Add(-5*time.Minute))

	fetch := func(userID uuid.UUID, external bool) (rows []map[string]json.RawMessage, all map[string]json.RawMessage) {
		t.Helper()
		res, body := callDashEx(t, h, "/api/v1/dashboard/projects?window=24h", userID, external)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("projects status %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			All  map[string]json.RawMessage   `json:"all"`
			Rows []map[string]json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode projects: %v (body=%s)", err, string(body))
		}
		return got.Rows, got.All
	}
	boolv := func(row map[string]json.RawMessage, key string) bool {
		raw, ok := row[key]
		if !ok {
			return false
		}
		var b bool
		_ = json.Unmarshal(raw, &b)
		return b
	}
	findRowByContributor := func(rows []map[string]json.RawMessage, username string) map[string]json.RawMessage {
		for _, row := range rows {
			if metricHasContributor(decodeMetric(t, row, "events"), username) {
				return row
			}
		}
		return nil
	}

	// ---- external caller (member of A only) ----
	extRows, extAll := fetch(eID, true)

	aRow := findRowByContributor(extRows, ownerAName)
	if aRow == nil {
		t.Fatalf("external projects missing visible project A row (contributor %q)", ownerAName)
	}
	if boolv(aRow, "redacted") {
		t.Errorf("visible project A row must not be redacted: %v", aRow)
	}
	if _, present := aRow["projectId"]; !present {
		t.Errorf("visible project A row must KEEP projectId: %v", aRow)
	}

	// sk-674.18 T030 (US-13): no row of B for the external caller, not even
	// an anonymized one — he is in no space, B contributes nothing to him.
	if bRow := findRowByContributor(extRows, ownerBName); bRow != nil {
		t.Errorf("external projects carries a row of the non-member project B (contributor %q): %v", ownerBName, bRow)
	}

	// ---- internal caller (ownerA — member of A, NOT B) ----
	// This surface does NOT distinguish internal vs external redaction:
	// the same non-admin/non-member rule applies to both.
	intRows, intAll := fetch(ownerA, false)
	intB := findRowByContributor(intRows, ownerBName)
	if intB == nil {
		t.Fatalf("internal projects missing project B row (contributor %q)", ownerBName)
	}
	if !boolv(intB, "redacted") {
		t.Errorf("internal non-member project B row must also be redacted=true: %v", intB)
	}
	if _, present := intB["projectId"]; present {
		t.Errorf("internal non-member project B row must NOT carry projectId either (no Soft/Hard split on this surface): %v", intB)
	}
	// Counts + contributor usernames survive the anonymization untouched —
	// the defining trait of this surface (INV-28 (2): B contributes to the
	// reach of a participant of its space).
	bEvents := decodeMetric(t, intB, "events")
	if bEvents.Count != 1 || !metricHasContributor(bEvents, ownerBName) {
		t.Errorf("anonymized project B row events metric must keep count+contributor: %+v", bEvents)
	}
	bCommits := decodeMetric(t, intB, "commits")
	if bCommits.Count != 1 || !metricHasContributor(bCommits, ownerBName) {
		t.Errorf("anonymized project B row commits metric must keep count+contributor: %+v", bCommits)
	}
	if raw, present := intB["topActor"]; !present || !strings.Contains(string(raw), ownerBName) {
		t.Errorf("anonymized project B row topActor must still name %q: %s", ownerBName, string(raw))
	}

	// ---- the `all` row: never redacted, no project-identity fields; it
	// counts each caller's reach (US-13), so the external caller's — his
	// project A only — is smaller than the internal one's, who also gets
	// B's contribution. ----
	for _, all := range []map[string]json.RawMessage{extAll, intAll} {
		if boolv(all, "redacted") {
			t.Errorf(`"all" row must never be redacted: %v`, all)
		}
		for _, key := range []string{"projectId", "projectName", "projectPrefix"} {
			if _, present := all[key]; present {
				t.Errorf(`"all" row must carry no project-identity fields, found %q: %v`, key, all)
			}
		}
	}
	extAllEvents := decodeMetric(t, extAll, "events")
	intAllEvents := decodeMetric(t, intAll, "events")
	if extAllEvents.Count >= intAllEvents.Count {
		t.Errorf(`"all".events.count: external=%d, internal=%d — the external reach (A) must count less than the internal one (A + B's contribution)`, extAllEvents.Count, intAllEvents.Count)
	}
}
