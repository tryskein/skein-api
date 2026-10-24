package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

var _ = issues.CommentAuthorKindHuman // ensure issues import is always used

// sk-16 — REST handler tests for POST/GET /api/v1/projects/{pid}/issues/{iid}/comments.
//
// Coverage matrix (per the sk-16 spec):
//   - POST with cookie session  → author_kind=human (kind field ignored)
//   - POST with api-key + kind=directed   → author_kind=agent_directed
//   - POST with api-key + kind=autonomous → author_kind=agent_autonomous
//   - POST with api-key WITHOUT kind      → 400 validation_error
//   - GET origin=human-driven     → only human + agent_directed rows
//   - GET origin=agent-autonomous → only agent_autonomous rows
//   - GET (no origin)             → returns everything (default all)
//   - GET origin=garbage          → 400 invalid_origin

func newCmtHandler(t *testing.T, pool *pgxpool.Pool) *IssuesHandler {
	t.Helper()
	return NewIssuesHandler(pool, projects.NewStore(pool), dependencies.NewStore(pool))
}

// withChiURLParams stuffs the project/issue path params into a request
// context so calling the handler method directly mirrors what the chi
// router would inject in production.
func withChiURLParams(req *http.Request, projectID, issueID string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID)
	rctx.URLParams.Add("issueID", issueID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// callPostComment posts a JSON body to the AddComment handler and
// returns the parsed response.
//
// When apiKeyID is uuid.Nil, the ctx mimics a cookie session (no
// APIKeyID); otherwise the api-key middleware path is simulated.
func callPostComment(t *testing.T, h *IssuesHandler, projectID, issueID string, userID uuid.UUID, role string, apiKeyID uuid.UUID, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/comments", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := req.Context()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	if apiKeyID != uuid.Nil {
		ctx = auth.WithAPIKeyID(ctx, apiKeyID)
	}
	req = req.WithContext(ctx)
	req = withChiURLParams(req, projectID, issueID)

	rec := httptest.NewRecorder()
	h.AddComment(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var parsed map[string]any
	_ = json.NewDecoder(res.Body).Decode(&parsed)
	return res, parsed
}

// callGetComments calls the ListComments handler with optional origin query.
func callGetComments(t *testing.T, h *IssuesHandler, projectID, issueID string, userID uuid.UUID, role, origin string) (*http.Response, []map[string]any) {
	t.Helper()
	url := "/comments"
	if origin != "" {
		url += "?origin=" + origin
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := req.Context()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)
	req = withChiURLParams(req, projectID, issueID)

	rec := httptest.NewRecorder()
	h.ListComments(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		return res, nil
	}
	var rows []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&rows)
	return res, rows
}

func seedCmtIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, source string) {
	t.Helper()
	// Reuse seedIMIssue's INSERT shape — copy/paste so we don't tightly
	// couple the comments-handler test file to the issues_me one.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, 'open', false, $3, '', '',
        '', '', '', NULL, NULL, NULL, NULL, NULL, NULL,
        NULL, $4, now(), now(), NULL, NULL, '{}'::jsonb)`,
		idStr, projectID, source, creator); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
}

// ----- POST tests -----

func TestRestComment_CookieForcesHumanIgnoresKind(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	h := newCmtHandler(t, pool)

	// Cookie session = no APIKeyID in ctx. Even with kind="autonomous"
	// on the wire, the result must be author_kind='human'.
	res, body := callPostComment(t, h, pid.String(), issueID, user, "user", uuid.Nil,
		map[string]any{"body": "cookie-posted note", "kind": "autonomous"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%v", res.StatusCode, body)
	}
	if body["authorKind"] != "human" {
		t.Fatalf("authorKind = %v, want \"human\"", body["authorKind"])
	}
	if body["authorType"] != "human" {
		t.Fatalf("authorType = %v, want \"human\"", body["authorType"])
	}
}

func TestRestComment_AgentDirected(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	h := newCmtHandler(t, pool)

	apiKeyID := uuid.Must(uuid.NewRandom())
	res, body := callPostComment(t, h, pid.String(), issueID, user, "user", apiKeyID,
		map[string]any{"body": "agent reply on your behalf", "kind": "directed"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%v", res.StatusCode, body)
	}
	if body["authorKind"] != "agent_directed" {
		t.Fatalf("authorKind = %v, want \"agent_directed\"", body["authorKind"])
	}
	// REST path always writes author_type='human' — only the MCP tool
	// flips it to 'agent'. The 'agent' classification on the kind axis
	// is the new signal.
	if body["authorType"] != "human" {
		t.Fatalf("authorType = %v, want \"human\" (REST always writes human transport)", body["authorType"])
	}
}

func TestRestComment_AgentAutonomous(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	h := newCmtHandler(t, pool)

	apiKeyID := uuid.Must(uuid.NewRandom())
	res, body := callPostComment(t, h, pid.String(), issueID, user, "user", apiKeyID,
		map[string]any{"body": "side-quest finding", "kind": "autonomous"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%v", res.StatusCode, body)
	}
	if body["authorKind"] != "agent_autonomous" {
		t.Fatalf("authorKind = %v, want \"agent_autonomous\"", body["authorKind"])
	}
}

func TestRestComment_AgentKindRequired(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	h := newCmtHandler(t, pool)

	apiKeyID := uuid.Must(uuid.NewRandom())
	res, body := callPostComment(t, h, pid.String(), issueID, user, "user", apiKeyID,
		map[string]any{"body": "no kind, agent caller"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, body)
	}
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error", body["error"])
	}
}

func TestRestComment_AgentKindUnknownRejected(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	h := newCmtHandler(t, pool)

	apiKeyID := uuid.Must(uuid.NewRandom())
	res, body := callPostComment(t, h, pid.String(), issueID, user, "user", apiKeyID,
		map[string]any{"body": "garbage kind", "kind": "manual"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, body)
	}
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v, want validation_error", body["error"])
	}
}

// ----- GET origin filter tests -----

// seedMixedComments inserts three comments — one human, one agent_directed,
// one agent_autonomous — so the origin filter has at least one row to
// drop in each scenario.
func seedMixedComments(t *testing.T, pool *pgxpool.Pool, issueID string, author uuid.UUID, authorName string) {
	t.Helper()
	rows := []struct {
		body string
		kind string
	}{
		{"human note", issues.CommentAuthorKindHuman},
		{"directed note", issues.CommentAuthorKindAgentDirected},
		{"autonomous note", issues.CommentAuthorKindAgentAutonomous},
	}
	for _, r := range rows {
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(t.Context(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'human', $5, $6, now(), now())`,
			id, issueID, authorName, author, r.kind, r.body); err != nil {
			t.Fatalf("seed comment %s: %v", r.kind, err)
		}
	}
}

func TestRestComment_GET_OriginHumanDriven(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	seedMixedComments(t, pool, issueID, user, uname)
	h := newCmtHandler(t, pool)

	res, rows := callGetComments(t, h, pid.String(), issueID, user, "user", "human-driven")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (human + agent_directed); rows=%v", len(rows), rows)
	}
	kinds := map[string]bool{}
	for _, row := range rows {
		kinds[row["authorKind"].(string)] = true
	}
	if !kinds["human"] || !kinds["agent_directed"] || kinds["agent_autonomous"] {
		t.Fatalf("kinds = %v, want {human, agent_directed} only", kinds)
	}
}

func TestRestComment_GET_OriginAgentAutonomous(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	seedMixedComments(t, pool, issueID, user, uname)
	h := newCmtHandler(t, pool)

	res, rows := callGetComments(t, h, pid.String(), issueID, user, "user", "agent-autonomous")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (agent_autonomous only)", len(rows))
	}
	if rows[0]["authorKind"] != "agent_autonomous" {
		t.Fatalf("authorKind = %v, want \"agent_autonomous\"", rows[0]["authorKind"])
	}
}

func TestRestComment_GET_OriginAllReturnsEverything(t *testing.T) {
	pool := newIMTestPool(t)
	user, uname := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	seedMixedComments(t, pool, issueID, user, uname)
	h := newCmtHandler(t, pool)

	// No origin query → default all.
	res, rows := callGetComments(t, h, pid.String(), issueID, user, "user", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}

	// Explicit origin=all → identical.
	res, rows = callGetComments(t, h, pid.String(), issueID, user, "user", "all")
	if res.StatusCode != http.StatusOK || len(rows) != 3 {
		t.Fatalf("explicit origin=all: status=%d rows=%d, want 200 + 3", res.StatusCode, len(rows))
	}
}

func TestRestComment_GET_OriginInvalid_400(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, user, issueID, "human")
	h := newCmtHandler(t, pool)

	url := "/comments?origin=bogus"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := req.Context()
	ctx = auth.WithUserID(ctx, user)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)
	req = withChiURLParams(req, pid.String(), issueID)
	rec := httptest.NewRecorder()
	h.ListComments(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body["error"] != "invalid_origin" {
		t.Fatalf("error = %v, want invalid_origin", body["error"])
	}
}
