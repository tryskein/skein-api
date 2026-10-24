package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// listTestEnv bundles the per-test fixtures needed for a List call.
type listTestEnv struct {
	pool      *pgxpool.Pool
	handler   *IssuesHandler
	owner     uuid.UUID
	ownerName string
	projectID uuid.UUID
	prefix    string
}

func newListTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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

// seedListUser inserts a fresh users row and returns (id, username).
// Username pattern matches the [a-z0-9_-]{3,30} regex.
func seedListUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "lh" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, role); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

func seedListProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, prefix string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := prefix + "_" + suffix
	idPrefix := prefix + suffix[:6]
	ctx := t.Context()
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, idPrefix, owner, seedSpaceID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, id, owner); err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
	t.Cleanup(func() {
		// issues.project_id is ON DELETE RESTRICT (migration 000004), so
		// delete issues leaf-first (children before parents) before the
		// project, else the project DELETE silently fails and rows leak.
		for range 64 {
			ct, err := pool.Exec(context.Background(), `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (SELECT parent_id FROM issues WHERE project_id = $1 AND parent_id IS NOT NULL)`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id, idPrefix
}

// seedListIssue inserts an issues row with the supplied source / assignee /
// accepted_at / created_by fields. Pass empty assignee to NULL the column.
// acceptedAt nil means accepted_at IS NULL; non-nil writes the value plus
// accepted_by = creator (the only user we know exists in this fixture).
func seedListIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, source, assignee string, acceptedAt *time.Time) {
	t.Helper()
	var assigneeArg any
	if assignee == "" {
		assigneeArg = nil
	} else {
		assigneeArg = assignee
	}
	var acceptedAtArg, acceptedByArg any
	if acceptedAt != nil {
		acceptedAtArg = *acceptedAt
		acceptedByArg = creator
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, 'open', false, $3, NULL, $4::varchar,
        '', '', '', NULL, NULL, NULL, NULL, NULL, NULL,
        NULL, $5, now(), now(), $6::timestamptz, $7::uuid, '{}'::jsonb)`,
		idStr, projectID, source, assigneeArg, creator, acceptedAtArg, acceptedByArg); err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

// seedListIssueFull is the sk-273 multi-select test fixture — unlike
// seedListIssue (hardcoded status='open', type='task', priority=2), it
// takes status/type/priority explicitly so tests can seed a mix of values
// to filter across.
func seedListIssueFull(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, status, issueType string, priority int) {
	t.Helper()
	// issues_closed_consistency CHECK: closed_at is NOT NULL iff status =
	// 'closed' — the two must move together or the INSERT is rejected.
	var closedAtArg any
	if status == "closed" {
		closedAtArg = time.Now().UTC()
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', $3, $4, $5, false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL, NULL, $6::timestamptz, NULL,
        NULL, $7, now(), now(), NULL, NULL, '{}'::jsonb)`,
		idStr, projectID, issueType, priority, status, closedAtArg, creator); err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

// newListEnv bootstraps an authenticated owner + project and the handler.
func newListEnv(t *testing.T) listTestEnv {
	pool := newListTestPool(t)
	owner, ownerName := seedListUser(t, pool, "user")
	pid, prefix := seedListProject(t, pool, owner, "lh")
	projectsStore := projects.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	h := NewIssuesHandler(pool, projectsStore, depsStore)
	return listTestEnv{
		pool: pool, handler: h,
		owner: owner, ownerName: ownerName,
		projectID: pid, prefix: prefix,
	}
}

// callList builds a GET request with the supplied query string, splices
// the chi RouteContext {projectID}, attaches the auth ctx, and invokes
// the List handler.
func callList(t *testing.T, env listTestEnv, userID uuid.UUID, role, query string) (*http.Response, map[string]any) {
	t.Helper()
	url := "/api/v1/projects/" + env.projectID.String() + "/issues"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", env.projectID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	if userID != uuid.Nil {
		ctx = auth.WithUserID(ctx, userID)
		ctx = auth.WithUserRole(ctx, role)
	}
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	env.handler.List(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		// Decode the body for the caller to assert on errors.
		var body map[string]any
		_ = json.NewDecoder(res.Body).Decode(&body)
		return res, body
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return res, body
}

// listIDs extracts issue ids from the items[] array. Useful for asserting
// the contents of the page without comparing every field.
func listIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items is not an array, got %T (%v)", body["items"], body["items"])
	}
	ids := make([]string, 0, len(items))
	for _, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("item is not an object")
		}
		ids = append(ids, row["id"].(string))
	}
	return ids
}

// TestList_AssigneeMe_AcceptedAtNull_Inbox covers the headline Inbox
// query: `assignee=me&acceptedAt=null&status=open` returns ONLY the
// unaccepted, open, assigned-to-me issues.
func TestList_AssigneeMe_AcceptedAtNull_Inbox(t *testing.T) {
	env := newListEnv(t)
	other, otherName := seedListUser(t, env.pool, "user")

	// Make `other` a project member so we can assign issues to them.
	if _, err := env.pool.Exec(t.Context(), `
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
ON CONFLICT (space_id, user_id) DO NOTHING`,
		env.projectID, other, env.owner); err != nil {
		t.Fatalf("seed other member: %v", err)
	}

	now := time.Now().UTC()
	// 1. Assigned to me, NOT accepted, status=open → SHOULD appear.
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	// 2. Assigned to me, ALREADY accepted → must NOT appear (acceptedAt filter).
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-2",
		"human", env.ownerName, &now)
	// 3. Assigned to OTHER, not accepted → must NOT appear (assignee filter).
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-3",
		"human", otherName, nil)
	_ = otherName

	res, body := callList(t, env, env.owner, "user",
		"assignee=me&acceptedAt=null&status=open")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	if len(ids) != 1 || ids[0] != env.prefix+"-1" {
		t.Fatalf("ids = %v, want [%s-1]", ids, env.prefix)
	}
	// Total reflects the same WHERE clause as items[]; with one match
	// the count must be exactly 1.
	if total, _ := body["total"].(float64); int(total) != 1 {
		t.Fatalf("total = %v, want 1", body["total"])
	}
}

// TestList_CreatedByMe_Outbox covers the Outbox query: `createdBy=me`
// returns only issues authored by the caller.
func TestList_CreatedByMe_Outbox(t *testing.T) {
	env := newListEnv(t)
	other, _ := seedListUser(t, env.pool, "user")
	// Add `other` as project member so they can create issues here.
	if _, err := env.pool.Exec(t.Context(), `
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
ON CONFLICT (space_id, user_id) DO NOTHING`,
		env.projectID, other, env.owner); err != nil {
		t.Fatalf("seed other member: %v", err)
	}

	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, other, env.prefix+"-2",
		"human", env.ownerName, nil)

	res, body := callList(t, env, env.owner, "user", "createdBy=me")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	if len(ids) != 1 || ids[0] != env.prefix+"-1" {
		t.Fatalf("ids = %v, want [%s-1]", ids, env.prefix)
	}
}

// TestList_OriginHumanDriven_ExcludesAutonomous: origin=human-driven
// returns rows with source IN ('human','agent_directed'), excludes
// agent_autonomous.
func TestList_OriginHumanDriven_ExcludesAutonomous(t *testing.T) {
	env := newListEnv(t)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-2",
		"agent_directed", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-3",
		"agent_autonomous", env.ownerName, nil)

	res, body := callList(t, env, env.owner, "user", "origin=human-driven")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	want := map[string]bool{env.prefix + "-1": true, env.prefix + "-2": true}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2 entries (-1 and -2)", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id %s in human-driven page; want only %v", id, want)
		}
	}
}

// TestList_OriginAgentAutonomous_OnlyAutonomous: origin=agent-autonomous
// returns only rows with source = 'agent_autonomous'.
func TestList_OriginAgentAutonomous_OnlyAutonomous(t *testing.T) {
	env := newListEnv(t)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-2",
		"agent_autonomous", env.ownerName, nil)

	res, body := callList(t, env, env.owner, "user", "origin=agent-autonomous")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	if len(ids) != 1 || ids[0] != env.prefix+"-2" {
		t.Fatalf("ids = %v, want [%s-2]", ids, env.prefix)
	}
}

// TestList_IAssigned_Computed verifies the IAssigned flag:
//   - issue created by caller AND assigned to other → iAssigned=true
//   - issue created by caller AND self-assigned    → iAssigned=false
//   - issue NOT created by caller                  → iAssigned=false
func TestList_IAssigned_Computed(t *testing.T) {
	env := newListEnv(t)
	other, otherName := seedListUser(t, env.pool, "user")
	if _, err := env.pool.Exec(t.Context(), `
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
ON CONFLICT (space_id, user_id) DO NOTHING`,
		env.projectID, other, env.owner); err != nil {
		t.Fatalf("seed other member: %v", err)
	}

	// 1. caller created + delegated → iAssigned=true
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", otherName, nil)
	// 2. caller created + self-assigned → iAssigned=false
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-2",
		"human", env.ownerName, nil)
	// 3. other created + assigned to caller → iAssigned=false
	seedListIssue(t, env.pool, env.projectID, other, env.prefix+"-3",
		"human", env.ownerName, nil)

	res, body := callList(t, env, env.owner, "user", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items, _ := body["items"].([]any)
	got := map[string]bool{}
	for _, raw := range items {
		row := raw.(map[string]any)
		// JSON omits iAssigned when false (omitempty on the struct tag).
		flag, _ := row["iAssigned"].(bool)
		got[row["id"].(string)] = flag
	}
	if got[env.prefix+"-1"] != true {
		t.Fatalf("issue 1 iAssigned = %v, want true", got[env.prefix+"-1"])
	}
	if got[env.prefix+"-2"] != false {
		t.Fatalf("issue 2 iAssigned = %v, want false", got[env.prefix+"-2"])
	}
	if got[env.prefix+"-3"] != false {
		t.Fatalf("issue 3 iAssigned = %v, want false", got[env.prefix+"-3"])
	}
}

// TestList_AcceptedAt_PassesThrough_To_JSON verifies that the
// acceptedAt column lands in the IssueSummary JSON (B1 → B5 wiring).
func TestList_AcceptedAt_PassesThrough_To_JSON(t *testing.T) {
	env := newListEnv(t)
	now := time.Now().UTC()
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, &now)

	res, body := callList(t, env, env.owner, "user", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	row := items[0].(map[string]any)
	if row["acceptedAt"] == nil {
		t.Fatalf("acceptedAt is nil; want a timestamp string")
	}
}

// TestList_InvalidAcceptedAt_400: any value other than the literal
// "null" returns 400 validation_error.
func TestList_InvalidAcceptedAt_400(t *testing.T) {
	env := newListEnv(t)
	res, body := callList(t, env, env.owner, "user", "acceptedAt=foo")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error", body["error"])
	}
}

// TestList_InvalidOrigin_400: bad origin token rejected.
func TestList_InvalidOrigin_400(t *testing.T) {
	env := newListEnv(t)
	res, body := callList(t, env, env.owner, "user", "origin=banana")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error", body["error"])
	}
}

// TestList_InvalidCreatedBy_400: only the literal "me" is accepted on
// the wire; raw UUIDs rejected.
func TestList_InvalidCreatedBy_400(t *testing.T) {
	env := newListEnv(t)
	res, body := callList(t, env, env.owner, "user", "createdBy="+env.owner.String())
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error", body["error"])
	}
}

// TestList_AssigneeMe_RequiresAuth: anonymous caller → 401.
//
// The handler short-circuits at the auth gate before even parsing the
// query string, so any unauthenticated request to List comes back 401
// regardless of the chosen filters. We use assignee=me here for the
// task's named scenario (the goal explicitly calls out this case) but
// the same is true for any other query.
func TestList_AssigneeMe_RequiresAuth(t *testing.T) {
	env := newListEnv(t)
	// uuid.Nil signals "no auth" — callList will skip the auth ctx
	// stamping when userID is the zero uuid.
	res, body := callList(t, env, uuid.Nil, "", "assignee=me")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%v", res.StatusCode, body)
	}
}

// TestList_ComponentIDFilter_RestrictsToLinked verifies that adding
// `componentId={uuid}` to the query restricts the result to issues
// pinned via the issue_components junction. Two issues seeded — only
// one is linked, only one comes back.
func TestList_ComponentIDFilter_RestrictsToLinked(t *testing.T) {
	env := newListEnv(t)

	// Two issues; component links only the second one.
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-2",
		"human", env.ownerName, nil)

	store := projects.NewStore(env.pool)
	comp, err := store.CreateComponent(t.Context(), env.projectID, env.owner,
		projects.CreateComponentArgs{Name: "alpha-test"},
		projects.HumanActor(env.owner, env.ownerName))
	if err != nil {
		t.Fatalf("create component: %v", err)
	}
	if _, err := env.pool.Exec(t.Context(), `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
		env.prefix+"-2", comp.ID); err != nil {
		t.Fatalf("link issue: %v", err)
	}

	res, body := callList(t, env, env.owner, "user",
		"componentId="+comp.ID.String())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	if len(ids) != 1 || ids[0] != env.prefix+"-2" {
		t.Fatalf("ids = %v, want [%s-2]", ids, env.prefix)
	}
}

// TestList_ComponentIDInvalid_400 verifies the handler rejects a
// malformed UUID with 400 invalid_component_id.
func TestList_ComponentIDInvalid_400(t *testing.T) {
	env := newListEnv(t)
	res, body := callList(t, env, env.owner, "user", "componentId=not-a-uuid")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, body)
	}
}

// TestList_MultiStatusFilter_ANY: status=open,in_progress (CSV) returns
// rows matching EITHER status, excludes closed — sk-273.
func TestList_MultiStatusFilter_ANY(t *testing.T) {
	env := newListEnv(t)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-1", "open", "task", 2)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-2", "in_progress", "task", 2)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-3", "closed", "task", 2)

	res, body := callList(t, env, env.owner, "user", "status=open,in_progress")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	want := map[string]bool{env.prefix + "-1": true, env.prefix + "-2": true}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2 entries (-1 and -2)", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id %s; want only %v", id, want)
		}
	}
}

// TestList_MultiPriorityFilter_ANY: priority=0,2 (CSV) returns rows with
// EITHER priority, excludes others — sk-273 (priority was single-value
// only before this change).
func TestList_MultiPriorityFilter_ANY(t *testing.T) {
	env := newListEnv(t)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-1", "open", "task", 0)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-2", "open", "task", 2)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-3", "open", "task", 4)

	res, body := callList(t, env, env.owner, "user", "priority=0,2")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	want := map[string]bool{env.prefix + "-1": true, env.prefix + "-2": true}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2 entries (-1 and -2)", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id %s; want only %v", id, want)
		}
	}
}

// TestList_TypeFilter_SingleAndMulti: type= is a brand-new query param on
// this handler (sk-273 — it was never parsed at all before). Covers both a
// single value and a CSV pair.
func TestList_TypeFilter_SingleAndMulti(t *testing.T) {
	env := newListEnv(t)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-1", "open", "bug", 2)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-2", "open", "epic", 2)
	seedListIssueFull(t, env.pool, env.projectID, env.owner, env.prefix+"-3", "open", "chore", 2)

	res, body := callList(t, env, env.owner, "user", "type=bug")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	if len(ids) != 1 || ids[0] != env.prefix+"-1" {
		t.Fatalf("ids = %v, want [%s-1]", ids, env.prefix)
	}

	res, body = callList(t, env, env.owner, "user", "type=bug,epic")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids = listIDs(t, body)
	want := map[string]bool{env.prefix + "-1": true, env.prefix + "-2": true}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2 entries (-1 and -2)", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id %s; want only %v", id, want)
		}
	}
}

// TestList_MultiOriginGroups_UnionsSourceLists:
// origin=human-driven,agent-autonomous unions BOTH groups' source lists —
// every row shows up, exercising OriginGroupsToSources with 2 explicit
// groups (not the "all" sentinel) — sk-273.
func TestList_MultiOriginGroups_UnionsSourceLists(t *testing.T) {
	env := newListEnv(t)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-2",
		"agent_directed", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-3",
		"agent_autonomous", env.ownerName, nil)

	res, body := callList(t, env, env.owner, "user", "origin=human-driven,agent-autonomous")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	ids := listIDs(t, body)
	if len(ids) != 3 {
		t.Fatalf("ids = %v, want all 3 (union of both groups)", ids)
	}
}

// TestList_InvalidPriorityCSV_400: a malformed element in a multi-value
// priority list is rejected the same way a lone bad value is — sk-273.
func TestList_InvalidPriorityCSV_400(t *testing.T) {
	env := newListEnv(t)
	res, body := callList(t, env, env.owner, "user", "priority=1,abc")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if body["error"] != "invalid_priority" {
		t.Fatalf("error = %v, want invalid_priority", body["error"])
	}
}

// TestList_InvalidTypeCSV_400: a bad type token in a multi-value list is
// rejected — sk-273 (type filter is new on this handler).
func TestList_InvalidTypeCSV_400(t *testing.T) {
	env := newListEnv(t)
	res, body := callList(t, env, env.owner, "user", "type=bug,banana")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if body["error"] != "invalid_type" {
		t.Fatalf("error = %v, want invalid_type", body["error"])
	}
}
