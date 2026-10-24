package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// seedActivityUser inserts a users row for activity tests. Cleans up
// via t.Cleanup. Mirrors the seedUserRow pattern from users_test.go but
// keeps the helper local so users_test.go's pre-existing seedProject
// helper signature stays untouched.
func seedActivityUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ac" + strings.ReplaceAll(id.String()[:18], "-", "")
	email := uname + "@test.local"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'unused', now(), '{}'::jsonb)`,
		id, email, uname, uname, role); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

// seedActivityProject creates a project owned by ownerID with a unique
// id_prefix. project_members cascade with the project on cleanup.
func seedActivityProject(t *testing.T, pool *pgxpool.Pool, ownerID uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	prefix := "ap" + strings.ReplaceAll(id.String()[:6], "-", "")
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      created_by, created_at, updated_at, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, $4, now(), now(), $5)`,
		id, prefix, prefix, ownerID, seedSpaceID); err != nil {
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
ON CONFLICT (space_id, user_id) DO NOTHING`, id, ownerID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0) ON CONFLICT (project_id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed project counter: %v", err)
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
	return id, prefix
}

// seedActivityIssue inserts a minimal issues row. Used as the parent
// row for issue.* events the tests fan in.
func seedActivityIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		idStr, projectID, title, creator); err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

// seedActivityEvent inserts a row into events with the given type +
// entity. project_id matches the project so the index is hit and the
// activity SELECT can find it. Returns the new id for cursor-based
// assertions.
func seedActivityEvent(
	t *testing.T,
	pool *pgxpool.Pool,
	eventType, entityType, entityID string,
	projectID uuid.UUID,
	actorUserID uuid.UUID,
	actorName string,
) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES (now(), $1, $2, $3, 'human', $4, $5, $6, '{}'::jsonb)
RETURNING id`,
		eventType, actorName, actorUserID, entityType, entityID, projectID,
	).Scan(&id); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

// callActivity invokes the handler with a chi router that decodes the
// {projectID} param the handler reads. ctx carries the auth headers so
// caller() resolves to the supplied userID + role.
func callActivity(
	t *testing.T,
	h *ActivityHandler,
	method, target string,
	userID uuid.UUID,
	role string,
) (*http.Response, []byte) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{projectID}/activity", h.ListProjectActivity)
	r.Get("/api/v1/me/activity", h.ListMyActivity)

	req := httptest.NewRequest(method, target, nil)
	ctx := auth.WithUserID(req.Context(), userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })

	body := rec.Body.Bytes()
	return res, body
}

// decodeActivityResponse unmarshals the wire result. items is always
// non-nil (matches the writeActivityResult contract).
func decodeActivityResponse(t *testing.T, body []byte) (items []issues.ActivityEntry, nextCursor *string) {
	t.Helper()
	var resp struct {
		Items      []issues.ActivityEntry `json:"items"`
		NextCursor *string                `json:"nextCursor"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, string(body))
	}
	if resp.Items == nil {
		resp.Items = []issues.ActivityEntry{}
	}
	return resp.Items, resp.NextCursor
}

// TestListProjectActivity_HappyPath covers the basic shape: events
// land in newest-first order, summary is populated via FormatSummary,
// and the actor + entity references resolve to the seeded values.
func TestListProjectActivity_HappyPath(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "test issue")

	// Two events on the issue. We expect both to come back, newest
	// first, with field_changed.* audit rows excluded.
	_ = seedActivityEvent(t, pool, "issue.created", "issue", issueID, pid, owner, ownerName)
	_ = seedActivityEvent(t, pool, "issue.field_changed.priority", "issue", issueID, pid, owner, ownerName)
	last := seedActivityEvent(t, pool, "issue.status_changed", "issue", issueID, pid, owner, ownerName)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	target := fmt.Sprintf("/api/v1/projects/%s/activity", pid.String())
	res, body := callActivity(t, h, http.MethodGet, target, owner, "user")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", res.StatusCode, string(body))
	}
	items, next := decodeActivityResponse(t, body)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (audit field_changed must be filtered); items=%v", len(items), items)
	}
	if items[0].ID != last {
		t.Errorf("items[0].ID = %d, want %d (newest first)", items[0].ID, last)
	}
	if items[0].Type != "issue.status_changed" {
		t.Errorf("items[0].Type = %q, want issue.status_changed", items[0].Type)
	}
	if items[0].Actor.Username != ownerName {
		t.Errorf("items[0].Actor.Username = %q, want %q", items[0].Actor.Username, ownerName)
	}
	if items[0].Entity.Type != "issue" || items[0].Entity.ID != issueID {
		t.Errorf("items[0].Entity = %+v, want type=issue id=%s", items[0].Entity, issueID)
	}
	if items[0].Summary == "" {
		t.Error("items[0].Summary is empty; FormatSummary should populate it")
	}
	if next != nil {
		t.Errorf("nextCursor = %v, want nil (only 2 items)", *next)
	}
}

// TestListProjectActivity_CursorPagination verifies that the keyset
// cursor pages through events in disjoint chunks. We seed enough rows
// to force a second page and assert page 2's items strictly come
// before page 1's.
func TestListProjectActivity_CursorPagination(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "test issue")

	// 4 events; page size 2 → 2 pages, each with a cursor.
	ids := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		id := seedActivityEvent(t, pool, "issue.created", "issue", issueID, pid, owner, ownerName)
		ids = append(ids, id)
	}

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	// Page 1 (newest 2).
	target := fmt.Sprintf("/api/v1/projects/%s/activity?limit=2", pid.String())
	res, body := callActivity(t, h, http.MethodGet, target, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("page1 status = %d, body=%s", res.StatusCode, string(body))
	}
	page1, next := decodeActivityResponse(t, body)
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	if next == nil {
		t.Fatalf("page1 nextCursor = nil, want set")
	}

	// Page 2 (older 2).
	target2 := target + "&cursor=" + *next
	res, body = callActivity(t, h, http.MethodGet, target2, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("page2 status = %d, body=%s", res.StatusCode, string(body))
	}
	page2, next2 := decodeActivityResponse(t, body)
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	if next2 != nil {
		t.Errorf("page2 nextCursor = %v, want nil (no more rows)", *next2)
	}

	// Disjoint: every page2 id must be < min page1 id.
	minP1 := page1[0].ID
	for _, it := range page1 {
		if it.ID < minP1 {
			minP1 = it.ID
		}
	}
	for _, it := range page2 {
		if it.ID >= minP1 {
			t.Errorf("page2 id %d >= page1 min %d (pages should be disjoint)", it.ID, minP1)
		}
	}
}

// TestListProjectActivity_NonMember_404 verifies that a user who is
// not a member of a project gets 404 not_found (not 403) — vision §10
// "non-members must not learn that a project exists".
func TestListProjectActivity_NonMember_404(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	stranger, _ := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "secret")
	_ = seedActivityEvent(t, pool, "issue.created", "issue", issueID, pid, owner, ownerName)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	target := fmt.Sprintf("/api/v1/projects/%s/activity", pid.String())
	res, body := callActivity(t, h, http.MethodGet, target, stranger, "user")

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (got body=%s)", res.StatusCode, string(body))
	}
	var problem map[string]any
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if got := problem["error"]; got != "not_found" {
		t.Errorf("error code = %v, want not_found", got)
	}
}

// TestListMyActivity_AdminSeesNonMemberProjects verifies the admin
// bypass: an admin user gets activity rows for projects they have no
// explicit membership in.
func TestListMyActivity_AdminSeesNonMemberProjects(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	admin, _ := seedActivityUser(t, pool, "admin")
	pid, prefix := seedActivityProject(t, pool, owner) // admin is NOT a member
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "secret")
	wantID := seedActivityEvent(t, pool, "issue.created", "issue", issueID, pid, owner, ownerName)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	res, body := callActivity(t, h, http.MethodGet, "/api/v1/me/activity?limit=100", admin, "admin")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", res.StatusCode, string(body))
	}
	items, _ := decodeActivityResponse(t, body)
	// Admin sees activity globally; we filter to the seeded id to avoid
	// false positives from other tests' rows.
	found := false
	for _, it := range items {
		if it.ID == wantID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("admin did not see seeded event id=%d in /me/activity (got %d rows)", wantID, len(items))
	}
}

// TestListMyActivity_NonAdminEmptyVisible verifies the empty-visible
// short-circuit: a non-admin caller with no project memberships gets
// `{items:[], nextCursor:null}` immediately.
func TestListMyActivity_NonAdminEmptyVisible(t *testing.T) {
	pool := newAuthTestPool(t)
	stranger, _ := seedActivityUser(t, pool, "user")

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	res, body := callActivity(t, h, http.MethodGet, "/api/v1/me/activity", stranger, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", res.StatusCode, string(body))
	}
	items, next := decodeActivityResponse(t, body)
	if len(items) != 0 {
		t.Errorf("items len = %d, want 0", len(items))
	}
	if next != nil {
		t.Errorf("nextCursor = %v, want nil", *next)
	}
}

// TestListProjectActivity_AuditRowsFiltered verifies that
// issue.field_changed.* audit rows are excluded from the default feed.
// They live in the events table for audit trail completeness but are
// noise for the third-person timeline UX (matches Q1 finalization).
func TestListProjectActivity_AuditRowsFiltered(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "audit test")

	// 3 audit rows + 1 lifecycle row → only the lifecycle row should
	// surface.
	_ = seedActivityEvent(t, pool, "issue.field_changed.priority", "issue", issueID, pid, owner, ownerName)
	_ = seedActivityEvent(t, pool, "issue.field_changed.assignee", "issue", issueID, pid, owner, ownerName)
	_ = seedActivityEvent(t, pool, "issue.field_changed.title", "issue", issueID, pid, owner, ownerName)
	want := seedActivityEvent(t, pool, "comment.created", "issue", issueID, pid, owner, ownerName)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	target := fmt.Sprintf("/api/v1/projects/%s/activity", pid.String())
	res, body := callActivity(t, h, http.MethodGet, target, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", res.StatusCode, string(body))
	}
	items, _ := decodeActivityResponse(t, body)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (audit rows must be filtered); items=%+v", len(items), items)
	}
	if items[0].ID != want {
		t.Errorf("items[0].ID = %d, want %d", items[0].ID, want)
	}
}

// TestListProjectActivity_InvalidCursor surfaces a 400 with a clear
// error code when a malformed cursor is supplied.
func TestListProjectActivity_InvalidCursor(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, _ := seedActivityUser(t, pool, "user")
	pid, _ := seedActivityProject(t, pool, owner)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	target := fmt.Sprintf("/api/v1/projects/%s/activity?cursor=not-a-real-cursor", pid.String())
	res, body := callActivity(t, h, http.MethodGet, target, owner, "user")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", res.StatusCode, string(body))
	}
}

// TestListProjectActivity_ExistsButFails covers the edge case where
// the projectID is malformed at the URL layer — handler must surface a
// 400 rather than 404, since we never hit the visibility query.
func TestListProjectActivity_InvalidProjectID(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, _ := seedActivityUser(t, pool, "user")

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	res, body := callActivity(t, h, http.MethodGet, "/api/v1/projects/not-a-uuid/activity", owner, "user")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", res.StatusCode, string(body))
	}
}

// ----------------- P5 — per-issue activity -----------------

// seedActivityComment inserts a row into comments so the
// activityListSQL LEFT JOIN c on comment events resolves c.issue_id.
// commentID is a uuid string.
func seedActivityComment(t *testing.T, pool *pgxpool.Pool, commentID, issueID string, authorID uuid.UUID, authorName string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'human', 'test comment', now(), now())`,
		commentID, issueID, authorName, authorID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, commentID)
	})
}

// callIssueActivity invokes ListIssueActivity with the chi router shape
// the handler reads ({projectID} + {issueID}).
func callIssueActivity(
	t *testing.T,
	h *ActivityHandler,
	target string,
	userID uuid.UUID,
	role string,
) (*http.Response, []byte) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{projectID}/issues/{issueID}/activity", h.ListIssueActivity)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := auth.WithUserID(req.Context(), userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })

	return res, rec.Body.Bytes()
}

// TestListIssueActivity_MixedEntities covers the core P5 requirement:
// an issue's feed must surface direct issue events, comment events on
// that issue, AND dep events involving that issue — even when the
// project has unrelated noise that would have flushed them off the
// project-wide top-100.
func TestListIssueActivity_MixedEntities(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	target := prefix + "-1"
	other := prefix + "-2"
	seedActivityIssue(t, pool, pid, owner, target, "subject")
	seedActivityIssue(t, pool, pid, owner, other, "noise")

	// Three relevant events for the target issue.
	directID := seedActivityEvent(t, pool, "issue.created", "issue", target, pid, owner, ownerName)

	commentUUID := uuid.Must(uuid.NewRandom()).String()
	seedActivityComment(t, pool, commentUUID, target, owner, ownerName)
	commentEventID := seedActivityEvent(t, pool, "comment.created", "comment", commentUUID, pid, owner, ownerName)

	// Dep entity_id is "fromID::toID::depType" — tested both fromID side
	// (target → other) and toID side (other → target).
	depFromID := seedActivityEvent(t, pool, "dependency.created", "dep",
		target+"::"+other+"::blocks", pid, owner, ownerName)
	depToID := seedActivityEvent(t, pool, "dependency.created", "dep",
		other+"::"+target+"::blocks", pid, owner, ownerName)

	// Noise that must NOT come back: events on the OTHER issue + an
	// audit field_changed row on the target (excluded by default-types
	// filter).
	_ = seedActivityEvent(t, pool, "issue.created", "issue", other, pid, owner, ownerName)
	_ = seedActivityEvent(t, pool, "issue.field_changed.priority", "issue", target, pid, owner, ownerName)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	url := fmt.Sprintf("/api/v1/projects/%s/issues/%s/activity", pid.String(), target)
	res, body := callIssueActivity(t, h, url, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", res.StatusCode, string(body))
	}
	items, next := decodeActivityResponse(t, body)

	wantSet := map[int64]bool{
		directID:       false,
		commentEventID: false,
		depFromID:      false,
		depToID:        false,
	}
	for _, it := range items {
		if _, ok := wantSet[it.ID]; ok {
			wantSet[it.ID] = true
		} else {
			t.Errorf("unexpected event id=%d type=%s entityType=%s in feed",
				it.ID, it.Type, it.Entity.Type)
		}
	}
	for id, seen := range wantSet {
		if !seen {
			t.Errorf("expected event id=%d in issue feed, not present", id)
		}
	}

	// Newest-first ordering (depToID is the youngest seeded row above).
	if len(items) > 0 && items[0].ID != depToID {
		t.Errorf("items[0].ID = %d, want %d (newest first)", items[0].ID, depToID)
	}

	if next != nil {
		t.Errorf("nextCursor = %v, want nil (only %d items, default limit)", *next, len(items))
	}
}

// TestListIssueActivity_NoActivity verifies the empty-page contract:
// a fresh issue with zero events returns `{items:[], nextCursor:null}`.
func TestListIssueActivity_NoActivity(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, _ := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "quiet")

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	url := fmt.Sprintf("/api/v1/projects/%s/issues/%s/activity", pid.String(), issueID)
	res, body := callIssueActivity(t, h, url, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", res.StatusCode, string(body))
	}
	items, next := decodeActivityResponse(t, body)
	if len(items) != 0 {
		t.Errorf("items len = %d, want 0", len(items))
	}
	if next != nil {
		t.Errorf("nextCursor = %v, want nil", *next)
	}
}

// TestListIssueActivity_NonMember_404 verifies that a stranger to the
// project gets 404 (matches project-feed visibility — vision §10
// "non-members must not learn that a project exists").
func TestListIssueActivity_NonMember_404(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	stranger, _ := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "secret")
	_ = seedActivityEvent(t, pool, "issue.created", "issue", issueID, pid, owner, ownerName)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	url := fmt.Sprintf("/api/v1/projects/%s/issues/%s/activity", pid.String(), issueID)
	res, body := callIssueActivity(t, h, url, stranger, "user")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", res.StatusCode, string(body))
	}
	var problem map[string]any
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if got := problem["error"]; got != "not_found" {
		t.Errorf("error code = %v, want not_found", got)
	}
}

// TestListIssueActivity_CrossProjectIssue_404 covers the anti-leak path:
// a caller who IS a member of project A asks for an issue id that
// belongs to project B via project A's URL — should pretend the issue
// doesn't exist (404, not 200 with another project's data).
func TestListIssueActivity_CrossProjectIssue_404(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	pidA, prefixA := seedActivityProject(t, pool, owner)
	pidB, prefixB := seedActivityProject(t, pool, owner)
	issueB := prefixB + "-1"
	seedActivityIssue(t, pool, pidB, owner, issueB, "in B")
	_ = seedActivityEvent(t, pool, "issue.created", "issue", issueB, pidB, owner, ownerName)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	// Caller is a member of pidA but asks for issueB under pidA.
	_ = prefixA
	url := fmt.Sprintf("/api/v1/projects/%s/issues/%s/activity", pidA.String(), issueB)
	res, body := callIssueActivity(t, h, url, owner, "user")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", res.StatusCode, string(body))
	}
}

// TestListIssueActivity_CursorPagination smoke-tests keyset paging:
// 4 events with limit=2 → two disjoint pages, second has no nextCursor.
func TestListIssueActivity_CursorPagination(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "paginate me")

	for i := 0; i < 4; i++ {
		_ = seedActivityEvent(t, pool, "issue.created", "issue", issueID, pid, owner, ownerName)
	}

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	base := fmt.Sprintf("/api/v1/projects/%s/issues/%s/activity?limit=2", pid.String(), issueID)
	res, body := callIssueActivity(t, h, base, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("page1 status = %d (body=%s)", res.StatusCode, string(body))
	}
	page1, next := decodeActivityResponse(t, body)
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	if next == nil {
		t.Fatalf("page1 nextCursor = nil, want set")
	}

	res, body = callIssueActivity(t, h, base+"&cursor="+*next, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("page2 status = %d (body=%s)", res.StatusCode, string(body))
	}
	page2, next2 := decodeActivityResponse(t, body)
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	if next2 != nil {
		t.Errorf("page2 nextCursor = %v, want nil", *next2)
	}

	// Disjoint pages.
	minP1 := page1[0].ID
	for _, it := range page1 {
		if it.ID < minP1 {
			minP1 = it.ID
		}
	}
	for _, it := range page2 {
		if it.ID >= minP1 {
			t.Errorf("page2 id %d >= page1 min %d (pages should be disjoint)", it.ID, minP1)
		}
	}
}

// TestListIssueActivity_OutcomeChangedIsInTheDefaultFeed — row 66 of the
// sk-489 coverage matrix, the server half of the US-3 acceptance "the issue
// history shows the outcome-change event".
//
// ⚠ HOW THIS DIFFERS FROM THE "EVENT IS RECORDED" CHECK (row 11). That one reads
// the journal directly or with an EXPLICIT type filter and is green exactly when
// the record was made. This one reads the feed THE WAY THE FRONTEND READS IT - with no type
// filter at all (skein-frontend src/api/activity.ts#listIssueActivity sends
// only limit and cursor) - and the content of an unfiltered feed is decided entirely by
// issues/store.go#DefaultActivityEventTypes. The halves are green separately,
// the acceptance lives exactly in the pair: an implementation that wrote the event and did not add
// its type to the list would return an empty history with row 11 green.
//
// ⚠ WHY THE NEGATIVE CONTROL. Without it the assertion would pass on an
// implementation that does not apply the type filter at all (returns EVERYTHING) - and that is
// another defect, no less visible. issue.pinned is not in the default list and
// must not be, and its absence proves the filter works.
func TestListIssueActivity_OutcomeChangedIsInTheDefaultFeed(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	target := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, target, "outcome changed")

	outcomeID := seedActivityEvent(t, pool, "issue.outcome_changed", "issue", target, pid, owner, ownerName)
	pinnedID := seedActivityEvent(t, pool, "issue.pinned", "issue", target, pid, owner, ownerName)

	issuesStore := issues.NewStore(pool)
	h := NewActivityHandler(pool, issuesStore)

	// A URL with no filter parameter at all - exactly what the frontend sends.
	url := fmt.Sprintf("/api/v1/projects/%s/issues/%s/activity", pid.String(), target)
	res, body := callIssueActivity(t, h, url, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", res.StatusCode, string(body))
	}
	items, _ := decodeActivityResponse(t, body)

	var sawOutcome, sawPinned bool
	for _, it := range items {
		switch it.ID {
		case outcomeID:
			sawOutcome = true
			if it.Type != "issue.outcome_changed" {
				t.Errorf("the outcome-change row came with type %q", it.Type)
			}
		case pinnedID:
			sawPinned = true
		}
	}
	if !sawOutcome {
		t.Error("the feed without a type filter does not contain issue.outcome_changed - " +
			"the type is not added to issues/store.go#DefaultActivityEventTypes, and the US-3 acceptance " +
			"is not met whatever else is done right in the feature")
	}
	if sawPinned {
		t.Error("the feed returned issue.pinned - the default list does not exist or is not applied, " +
			"and the positive assertion above proves nothing")
	}
}
