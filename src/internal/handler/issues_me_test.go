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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// Cross-project /me/* endpoint tests (Phase 2 ui-redesign — B7).
//
// Covers /me/assigned-to, /me/assigned-by, /me/created-by with the new
// status / origin / acceptedAt filter knobs. Together with
// issues_list_test.go (project-scoped path) these exercise both surfaces
// the frontend uses for the redesign Inbox / Outbox / My-Delegated views.

// ---------------- helpers ----------------

func newIMTestPool(t *testing.T) *pgxpool.Pool {
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

func seedIMUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "im" + strings.ReplaceAll(id.String()[:18], "-", "")
	if _, err := pool.Exec(t.Context(), `
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

// seedIMProject inserts a fresh project and adds `owner` as project_members
// row. Returns (projectID, idPrefix). Caller can pass `addMembers` to
// register additional users as `member` role.
func seedIMProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, addMembers ...uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	idPrefix := "im" + suffix[:6]
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(t.Context(), `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, idPrefix+"_"+suffix, idPrefix, owner, seedSpaceID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
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
		t.Fatalf("seed project owner: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0) ON CONFLICT (project_id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
	for _, m := range addMembers {
		if _, err := pool.Exec(t.Context(), `
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
			id, m, owner); err != nil {
			t.Fatalf("seed extra member: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		// issues.project_id is ON DELETE RESTRICT (not cascade), so the
		// project cannot be deleted while its issues exist. Delete issues
		// leaf-first (children before parents; deps/comments/etc. cascade),
		// then the project — otherwise the project DELETE silently fails and
		// rows leak into the shared integration DB. Mirrors
		// testsupport.SeedProject.
		for range 64 {
			ct, err := pool.Exec(ctx, `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (
       SELECT parent_id FROM issues
        WHERE project_id = $1 AND parent_id IS NOT NULL
   )`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	})
	return id, idPrefix
}

// seedIMIssue inserts one issues row for the cross-project tests. The
// caller supplies source / status / assignee text / accepted_at /
// created_by directly so each test scenario is explicit. When status is
// 'closed' we auto-stamp closed_at to satisfy the
// issues_closed_consistency CHECK constraint.
func seedIMIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, source, status, assignee string, acceptedAt *time.Time) {
	t.Helper()
	var assigneeArg any
	if assignee != "" {
		assigneeArg = assignee
	}
	var acceptedAtArg, acceptedByArg any
	if acceptedAt != nil {
		acceptedAtArg = *acceptedAt
		acceptedByArg = creator
	}
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
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, $3, false, $4, NULL, $5::varchar,
        '', '', '', NULL, NULL, NULL, NULL, $9::timestamptz, NULL,
        NULL, $6, now(), now(), $7::timestamptz, $8::uuid, '{}'::jsonb)`,
		idStr, projectID, status, source, assigneeArg, creator, acceptedAtArg, acceptedByArg, closedAtArg); err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

// callMe drives one of the three /me/* endpoints by name. mode is one of
// "assigned-to", "assigned-by", "created-by"; query is the verbatim
// query string (without leading "?"). Pass uuid.Nil + "" role for an
// unauthenticated call.
func callMe(t *testing.T, h *IssuesMeHandler, mode string, userID uuid.UUID, role, query string) (*http.Response, map[string]any) {
	t.Helper()
	url := "/api/v1/me/" + mode
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if userID != uuid.Nil {
		ctx := req.Context()
		ctx = auth.WithUserID(ctx, userID)
		ctx = auth.WithUserRole(ctx, role)
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	switch mode {
	case "assigned-to":
		h.AssignedTo(rec, req)
	case "assigned-by":
		h.AssignedBy(rec, req)
	case "created-by":
		h.CreatedBy(rec, req)
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

// idSet pulls issue ids out of items[] into a set for easy comparison.
func idSet(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items not array: %T (%v)", body["items"], body["items"])
	}
	for _, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("item not object")
		}
		out[row["id"].(string)] = true
	}
	return out
}

// ---------------- /me/assigned-to ----------------

// TestMeAssignedTo_AcceptedAtNull: assignee=me + acceptedAt=null returns
// only unaccepted issues assigned to caller.
func TestMeAssignedTo_AcceptedAtNull(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	now := time.Now().UTC()
	// 1. Assigned to me, NOT accepted → SHOULD appear.
	seedIMIssue(t, pool, pid, user, prefix+"-1", "human", "open", uname, nil)
	// 2. Assigned to me, accepted → must NOT appear.
	seedIMIssue(t, pool, pid, user, prefix+"-2", "human", "open", uname, &now)

	res, body := callMe(t, h, "assigned-to", user, "user", "acceptedAt=null")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	if !got[prefix+"-1"] || got[prefix+"-2"] || len(got) != 1 {
		t.Fatalf("ids = %v, want only [%s-1]", got, prefix)
	}
}

// TestMeAssignedTo_StatusAndOrigin: status=open + origin=human-driven
// excludes closed and agent_autonomous rows.
func TestMeAssignedTo_StatusAndOrigin(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	// Matches: open + human-driven (human or agent_directed)
	seedIMIssue(t, pool, pid, user, prefix+"-1", "human", "open", uname, nil)
	seedIMIssue(t, pool, pid, user, prefix+"-2", "agent_directed", "open", uname, nil)
	// Excluded by origin
	seedIMIssue(t, pool, pid, user, prefix+"-3", "agent_autonomous", "open", uname, nil)
	// Excluded by status
	seedIMIssue(t, pool, pid, user, prefix+"-4", "human", "closed", uname, nil)

	res, body := callMe(t, h, "assigned-to", user, "user",
		"status=open&origin=human-driven")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	want := map[string]bool{prefix + "-1": true, prefix + "-2": true}
	if len(got) != 2 {
		t.Fatalf("ids = %v, want [%s-1, %s-2]", got, prefix, prefix)
	}
	for k := range got {
		if !want[k] {
			t.Fatalf("unexpected id %s in result", k)
		}
	}
}

// ---------------- /me/assigned-by ----------------

// TestMeAssignedBy_AcceptedAtNull: returns delegated issues whose
// assignee hasn't accepted yet.
func TestMeAssignedBy_AcceptedAtNull(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	other, otherName := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user, other)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	now := time.Now().UTC()
	// Created by me, assigned to other, NOT accepted → SHOULD appear.
	seedIMIssue(t, pool, pid, user, prefix+"-1", "human", "open", otherName, nil)
	// Created by me, assigned to other, accepted → must NOT appear.
	seedIMIssue(t, pool, pid, user, prefix+"-2", "human", "open", otherName, &now)
	// Created by other → must NOT appear (scope = i.created_by = caller).
	seedIMIssue(t, pool, pid, other, prefix+"-3", "human", "open", otherName, nil)

	res, body := callMe(t, h, "assigned-by", user, "user", "acceptedAt=null")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	if !got[prefix+"-1"] || got[prefix+"-2"] || got[prefix+"-3"] || len(got) != 1 {
		t.Fatalf("ids = %v, want only [%s-1]", got, prefix)
	}
}

// ---------------- /me/created-by ----------------

// TestMeCreatedBy_OnlyCallerAuthored: returns only issues created by the
// caller, ignores issues created by others (even if assigned to caller).
func TestMeCreatedBy_OnlyCallerAuthored(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	other, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user, other)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	// Created by me → SHOULD appear.
	seedIMIssue(t, pool, pid, user, prefix+"-1", "human", "open", uname, nil)
	// Created by other, even with caller as assignee → must NOT appear.
	seedIMIssue(t, pool, pid, other, prefix+"-2", "human", "open", uname, nil)

	res, body := callMe(t, h, "created-by", user, "user", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	if !got[prefix+"-1"] || got[prefix+"-2"] || len(got) != 1 {
		t.Fatalf("ids = %v, want only [%s-1]", got, prefix)
	}
	// Total reflects the same WHERE clause (sans cursor) — one match.
	if total, _ := body["total"].(float64); int(total) != 1 {
		t.Fatalf("total = %v, want 1", body["total"])
	}
}

// TestMeCreatedBy_OriginAgentAutonomous: filter to caller's autonomous
// side-quests only.
func TestMeCreatedBy_OriginAgentAutonomous(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	seedIMIssue(t, pool, pid, user, prefix+"-1", "human", "open", uname, nil)
	seedIMIssue(t, pool, pid, user, prefix+"-2", "agent_directed", "open", uname, nil)
	seedIMIssue(t, pool, pid, user, prefix+"-3", "agent_autonomous", "open", uname, nil)

	res, body := callMe(t, h, "created-by", user, "user", "origin=agent-autonomous")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	if !got[prefix+"-3"] || len(got) != 1 {
		t.Fatalf("ids = %v, want only [%s-3]", got, prefix)
	}
}

// TestMeCreatedBy_StatusClosed: filter to closed-by-caller archive.
func TestMeCreatedBy_StatusClosed(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	seedIMIssue(t, pool, pid, user, prefix+"-1", "human", "open", uname, nil)
	seedIMIssue(t, pool, pid, user, prefix+"-2", "human", "closed", uname, nil)

	res, body := callMe(t, h, "created-by", user, "user", "status=closed")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	if !got[prefix+"-2"] || got[prefix+"-1"] || len(got) != 1 {
		t.Fatalf("ids = %v, want only [%s-2]", got, prefix)
	}
}

// TestMeCreatedBy_RespectsProjectMembership: caller can NOT see issues
// they authored in projects they're not a member of (legacy / removed
// member case). Verifies the cross-project visibility horizon enforced
// by loadVisibleProjects + ANY(visible) clause.
func TestMeCreatedBy_RespectsProjectMembership(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	otherOwner, _ := seedIMUser(t, pool, "user")
	// Project where caller IS a member.
	visiblePID, visiblePrefix := seedIMProject(t, pool, user)
	// Project owned by `otherOwner`; caller is NOT a member.
	hiddenPID, hiddenPrefix := seedIMProject(t, pool, otherOwner)

	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	// Issue created by caller in the visible project → must appear.
	seedIMIssue(t, pool, visiblePID, user, visiblePrefix+"-1", "human", "open", uname, nil)
	// Issue created by caller in a hidden project (legacy data: caller
	// authored before being removed). Caller no longer a member → must
	// NOT appear.
	seedIMIssue(t, pool, hiddenPID, user, hiddenPrefix+"-1", "human", "open", uname, nil)

	res, body := callMe(t, h, "created-by", user, "user", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	if !got[visiblePrefix+"-1"] {
		t.Fatalf("ids = %v, want %s-1 to be present", got, visiblePrefix)
	}
	if got[hiddenPrefix+"-1"] {
		t.Fatalf("ids = %v leaked %s-1 from a project caller is not a member of",
			got, hiddenPrefix)
	}
}

// TestMeAssignedTo_RespectsProjectMembership: same as the created-by
// variant but for the assignee predicate. Even when the caller's
// username matches `assignee` on a row in a hidden project, the
// visibility filter must hide it.
func TestMeAssignedTo_RespectsProjectMembership(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	otherOwner, _ := seedIMUser(t, pool, "user")
	visiblePID, visiblePrefix := seedIMProject(t, pool, user)
	hiddenPID, hiddenPrefix := seedIMProject(t, pool, otherOwner)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	seedIMIssue(t, pool, visiblePID, user, visiblePrefix+"-1", "human", "open", uname, nil)
	// Hidden project, assigned to caller by username — must NOT appear.
	seedIMIssue(t, pool, hiddenPID, otherOwner, hiddenPrefix+"-1", "human", "open", uname, nil)

	res, body := callMe(t, h, "assigned-to", user, "user", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	if !got[visiblePrefix+"-1"] {
		t.Fatalf("missing %s-1 in result %v", visiblePrefix, got)
	}
	if got[hiddenPrefix+"-1"] {
		t.Fatalf("leaked %s-1 from hidden project: %v", hiddenPrefix, got)
	}
}

// TestMeAssignedBy_RespectsProjectMembership: parallel coverage for the
// assigned-by route — caller can NOT see issues they authored in a
// project they aren't a member of.
func TestMeAssignedBy_RespectsProjectMembership(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	otherOwner, _ := seedIMUser(t, pool, "user")
	visiblePID, visiblePrefix := seedIMProject(t, pool, user)
	hiddenPID, hiddenPrefix := seedIMProject(t, pool, otherOwner)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))

	seedIMIssue(t, pool, visiblePID, user, visiblePrefix+"-1", "human", "open", uname, nil)
	seedIMIssue(t, pool, hiddenPID, user, hiddenPrefix+"-1", "human", "open", uname, nil)

	res, body := callMe(t, h, "assigned-by", user, "user", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	got := idSet(t, body)
	if !got[visiblePrefix+"-1"] {
		t.Fatalf("missing %s-1 in result %v", visiblePrefix, got)
	}
	if got[hiddenPrefix+"-1"] {
		t.Fatalf("leaked %s-1 from hidden project: %v", hiddenPrefix, got)
	}
}

// ---------------- validation ----------------

// TestMe_InvalidAcceptedAt_400: every endpoint rejects acceptedAt other
// than literal "null".
func TestMe_InvalidAcceptedAt_400(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))
	for _, mode := range []string{"assigned-to", "assigned-by", "created-by"} {
		res, body := callMe(t, h, mode, user, "user", "acceptedAt=foo")
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%v", mode, res.StatusCode, body)
		}
		if body["error"] != "validation_error" {
			t.Errorf("%s: error = %v, want validation_error", mode, body["error"])
		}
	}
}

// TestMe_InvalidOrigin_400: every endpoint rejects bad origin.
func TestMe_InvalidOrigin_400(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))
	for _, mode := range []string{"assigned-to", "assigned-by", "created-by"} {
		res, body := callMe(t, h, mode, user, "user", "origin=banana")
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%v", mode, res.StatusCode, body)
		}
		if body["error"] != "validation_error" {
			t.Errorf("%s: error = %v, want validation_error", mode, body["error"])
		}
	}
}

// TestMe_InvalidStatus_400: every endpoint rejects bad status token.
func TestMe_InvalidStatus_400(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))
	for _, mode := range []string{"assigned-to", "assigned-by", "created-by"} {
		res, body := callMe(t, h, mode, user, "user", "status=banana")
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%v", mode, res.StatusCode, body)
		}
		// invalid_status is the canonical code from issues.ValidateStatus.
		if body["error"] != "invalid_status" {
			t.Errorf("%s: error = %v, want invalid_status", mode, body["error"])
		}
	}
}

// TestMe_RequiresAuth_401: anonymous → 401 across all three endpoints.
func TestMe_RequiresAuth_401(t *testing.T) {
	pool := newIMTestPool(t)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))
	for _, mode := range []string{"assigned-to", "assigned-by", "created-by"} {
		res, _ := callMe(t, h, mode, uuid.Nil, "", "")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", mode, res.StatusCode)
		}
	}
}

// TestMe_BlockedStatusIsRefusedAsBusinessError — sk-303.14, review A/9
// repair (finding E): a regression on the status code the three
// cross-project routes ACTUALLY write and which was missing from their
// specification.
//
// `status=blocked` is not a shape refusal, it is a DOMAIN refusal:
// "blocked" is a derived state computed from the dependency graph, and
// no status column holds that value. The request is well-formed; it
// cannot be answered — that is 422 by the engineering standard, and
// 422 is what the handler writes.
//
// Checked by behaviour, not by parsing: the annotation gate compares
// documentation with code, but the code on this path was pinned by
// nothing, so swapping 422 for 400 would have slipped past both checks.
func TestMe_BlockedStatusIsRefusedAsBusinessError(t *testing.T) {
	pool := newIMTestPool(t)
	h := NewIssuesMeHandler(pool, issues.NewStore(pool))
	user, _ := seedIMUser(t, pool, "user")

	for _, mode := range []string{"assigned-by", "assigned-to", "created-by"} {
		t.Run(mode, func(t *testing.T) {
			res, body := callMe(t, h, mode, user, "user", "status=blocked")
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422 — \"blocked\" is a derived "+
					"state: the request is well-formed but cannot be answered, and by "+
					"the engineering standard that is a business refusal, not 400", res.StatusCode)
			}
			if got := body["error"]; got != "blocked_is_derived" {
				t.Errorf("error code %v, want blocked_is_derived — from the code the client "+
					"learns that instead of status=blocked it needs isBlocked=true", got)
			}
		})
	}
}
