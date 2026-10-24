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
	"skein-api/internal/issues"
)

// sk-105.5 (S4) — comment author-only edit boundary.
//
// UpdateComment (handler/issues.go ~1461-1545) gates the edit on
// AUTHOR-ONLY: meta.AuthorUserID must equal the caller's user id, else
// 403 forbidden. The gate lives in the HANDLER (the store's UpdateComment
// does not re-check authorship), so this test drives the real handler.
//
// The security claim: a DIFFERENT user — even one who is a project OWNER
// (i.e. has full project visibility + admin-over-the-project authority) —
// cannot edit someone else's comment. Visibility alone does not grant
// edit rights; only authorship does.
//
// Matrix:
//   author edits own comment            → 200 + body changed
//   project owner (non-author) edits it → 403 forbidden, body unchanged
//   plain member (non-author) edits it  → 403 forbidden, body unchanged

// withChiCommentParams stuffs project/issue/comment path params into the
// request context (the chi router would inject these in production).
func withChiCommentParams(req *http.Request, projectID, issueID, commentID string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID)
	rctx.URLParams.Add("issueID", issueID)
	rctx.URLParams.Add("commentID", commentID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// callUpdateComment drives the UpdateComment handler as `userID` and
// returns the response + decoded body.
func callUpdateComment(t *testing.T, h *IssuesHandler, projectID, issueID, commentID string, userID uuid.UUID, role string, reqBody map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPut, "/comments/"+commentID, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := req.Context()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)
	req = withChiCommentParams(req, projectID, issueID, commentID)

	rec := httptest.NewRecorder()
	h.UpdateComment(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var parsed map[string]any
	_ = json.NewDecoder(res.Body).Decode(&parsed)
	return res, parsed
}

func commentBody(t *testing.T, pool *pgxpool.Pool, commentID uuid.UUID) string {
	t.Helper()
	var body string
	if err := pool.QueryRow(context.Background(),
		`SELECT body FROM comments WHERE id = $1`, commentID).Scan(&body); err != nil {
		t.Fatalf("read comment body: %v", err)
	}
	return body
}

func TestUpdateComment_AuthorOnly_Boundary(t *testing.T) {
	pool := newIMTestPool(t)

	author, authorName := seedIMUser(t, pool, "user")
	// owner is a SECOND project owner — full visibility + owner authority,
	// but NOT the comment author.
	owner, _ := seedIMUser(t, pool, "user")
	member, _ := seedIMUser(t, pool, "user")

	// author owns the project; owner is added as a co-owner; member as a
	// plain member. All three can SEE the project (so EnsureProjectVisible
	// passes for each) — the only thing separating them is authorship.
	pid, prefix := seedIMProject(t, pool, author, member)
	if _, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, owner, author); err != nil {
		t.Fatalf("add co-owner: %v", err)
	}

	issueID := prefix + "-1"
	seedCmtIssue(t, pool, pid, author, issueID, "human")
	h := newCmtHandler(t, pool)

	// author posts a comment via the store (the real authorship path).
	store := issues.NewStore(pool)
	dto, err := store.AddComment(context.Background(), issueID, "original body",
		author, authorName, issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
		issues.HumanActor(author, authorName))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	commentID := dto.ID
	ver := dto.Version

	// --- co-owner (non-author) is REJECTED ---
	res, body := callUpdateComment(t, h, pid.String(), issueID, commentID.String(), owner, "user",
		map[string]any{"body": "owner tampering", "expectedVersion": ver, "kind": "directed"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("co-owner edit status = %d, want 403; body=%v", res.StatusCode, body)
	}
	if body["error"] != "forbidden" {
		t.Fatalf("co-owner edit error = %v, want forbidden", body["error"])
	}
	if got := commentBody(t, pool, commentID); got != "original body" {
		t.Fatalf("body after rejected co-owner edit = %q, want unchanged", got)
	}

	// --- plain member (non-author) is REJECTED ---
	res, body = callUpdateComment(t, h, pid.String(), issueID, commentID.String(), member, "user",
		map[string]any{"body": "member tampering", "expectedVersion": ver, "kind": "directed"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("member edit status = %d, want 403; body=%v", res.StatusCode, body)
	}
	if body["error"] != "forbidden" {
		t.Fatalf("member edit error = %v, want forbidden", body["error"])
	}
	if got := commentBody(t, pool, commentID); got != "original body" {
		t.Fatalf("body after rejected member edit = %q, want unchanged", got)
	}

	// --- author edits their own comment → 200, body changes ---
	res, body = callUpdateComment(t, h, pid.String(), issueID, commentID.String(), author, "user",
		map[string]any{"body": "edited by author", "expectedVersion": ver, "kind": "directed"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("author edit status = %d, want 200; body=%v", res.StatusCode, body)
	}
	if got := commentBody(t, pool, commentID); got != "edited by author" {
		t.Fatalf("body after author edit = %q, want \"edited by author\"", got)
	}
}
