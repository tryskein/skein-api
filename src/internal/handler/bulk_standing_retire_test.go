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

	"skein-api/internal/auth"
	"skein-api/internal/issues"
)

// sk-118 (F2e) — the standing retire-block must cover the SHARED close path,
// not just Store.Close(). The bulk endpoint (POST
// /projects/{pid}/issues/bulk) closes/archives via
// Store.UpdateStatus(StatusClosed) — NOT Store.Close() — so without the guard
// living in UpdateStatus as well, a bulk action:"close" / action:"archive"
// could retire a standing task while it still has non-closed children,
// silently stranding real work and defeating the structural invariant.
//
// These tests drive the real HTTP Bulk handler against the DB and assert:
//   - a standing parent with a non-closed child is REJECTED by both
//     action:"close" AND action:"archive" (issue stays standing, child
//     untouched);
//   - a standing parent whose only child is closed (and a childless standing
//     parent) still retires via bulk.

// setIssueStanding flips a seeded issue's status to 'standing' directly. The
// seedListIssue helper always inserts status='open'; promotion through
// UpdateStatus would itself bump version, so we bypass it and pin version=1
// for a predictable expectedVersion in the bulk path.
func setIssueStanding(t *testing.T, env listTestEnv, id string) {
	t.Helper()
	if _, err := env.pool.Exec(t.Context(),
		`UPDATE issues SET status = 'standing' WHERE id = $1`, id); err != nil {
		t.Fatalf("set %s standing: %v", id, err)
	}
}

func setIssueClosed(t *testing.T, env listTestEnv, id string) {
	t.Helper()
	if _, err := env.pool.Exec(t.Context(),
		`UPDATE issues SET status = 'closed', closed_at = now(), closed_reason = 'seed' WHERE id = $1`, id); err != nil {
		t.Fatalf("set %s closed: %v", id, err)
	}
}

func issueStatus(t *testing.T, env listTestEnv, id string) string {
	t.Helper()
	var status string
	if err := env.pool.QueryRow(t.Context(),
		`SELECT status FROM issues WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read status %s: %v", id, err)
	}
	return status
}

// callBulk builds POST /projects/{pid}/issues/bulk with the supplied action +
// ids, splices the chi RouteContext {projectID} and auth ctx, and invokes the
// real BulkHandler.Bulk.
func callBulk(t *testing.T, env listTestEnv, userID uuid.UUID, role, action string, ids []string) (*http.Response, bulkResponse) {
	t.Helper()
	bh := NewBulkHandler(env.pool, issues.NewStore(env.pool))

	payload := map[string]any{"ids": ids, "action": action}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal bulk req: %v", err)
	}
	url := "/api/v1/projects/" + env.projectID.String() + "/issues/bulk"
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", env.projectID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	bh.Bulk(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })

	var body bulkResponse
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

// TestBulk_StandingWithOpenChild_Rejected drives the two retire actions
// (close + archive) against a standing parent that has a non-closed child via
// the canonical parent-child dep edge. Both must be rejected with the F2e
// code, the parent must stay standing, and the child must be untouched.
func TestBulk_StandingWithOpenChild_Rejected(t *testing.T) {
	for _, action := range []string{BulkActionClose, BulkActionArchive} {
		action := action
		t.Run(action, func(t *testing.T) {
			env := newListEnv(t)
			parent := env.prefix + "-1"
			child := env.prefix + "-2"
			seedListIssue(t, env.pool, env.projectID, env.owner, parent, "human", env.ownerName, nil)
			seedListIssue(t, env.pool, env.projectID, env.owner, child, "human", env.ownerName, nil)
			// from=child, to=parent (canonical parent-child convention).
			treeSeedDep(t, env.pool, child, parent, "parent-child", env.owner)
			setIssueStanding(t, env, parent)

			res, body := callBulk(t, env, env.owner, "user", action, []string{parent})

			// All ids failed → 422.
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%+v", res.StatusCode, body)
			}
			if len(body.Succeeded) != 0 {
				t.Fatalf("succeeded = %v, want empty", body.Succeeded)
			}
			if len(body.Failed) != 1 {
				t.Fatalf("failed = %+v, want exactly 1", body.Failed)
			}
			if body.Failed[0].Code != "standing_has_open_children" {
				t.Fatalf("failure code = %q, want standing_has_open_children; item=%+v",
					body.Failed[0].Code, body.Failed[0])
			}

			// Parent stays standing; child untouched (still open).
			if got := issueStatus(t, env, parent); got != "standing" {
				t.Fatalf("parent status = %q, want standing (retire must be blocked)", got)
			}
			if got := issueStatus(t, env, child); got != "open" {
				t.Fatalf("child status = %q, want open (must be untouched)", got)
			}
		})
	}
}

// TestBulk_StandingAllChildrenClosed_Retires verifies the empty / all-closed
// retire still succeeds through bulk: a standing parent whose only child is
// already closed closes, and a childless standing parent closes.
func TestBulk_StandingAllChildrenClosed_Retires(t *testing.T) {
	t.Run("all-children-closed", func(t *testing.T) {
		env := newListEnv(t)
		parent := env.prefix + "-1"
		child := env.prefix + "-2"
		seedListIssue(t, env.pool, env.projectID, env.owner, parent, "human", env.ownerName, nil)
		seedListIssue(t, env.pool, env.projectID, env.owner, child, "human", env.ownerName, nil)
		treeSeedDep(t, env.pool, child, parent, "parent-child", env.owner)
		setIssueClosed(t, env, child)
		setIssueStanding(t, env, parent)

		res, body := callBulk(t, env, env.owner, "user", BulkActionClose, []string{parent})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%+v", res.StatusCode, body)
		}
		if len(body.Succeeded) != 1 || body.Succeeded[0] != parent {
			t.Fatalf("succeeded = %v, want [%s]; failed=%+v", body.Succeeded, parent, body.Failed)
		}
		if got := issueStatus(t, env, parent); got != "closed" {
			t.Fatalf("parent status = %q, want closed (empty retire allowed)", got)
		}
	})

	t.Run("childless", func(t *testing.T) {
		env := newListEnv(t)
		parent := env.prefix + "-1"
		seedListIssue(t, env.pool, env.projectID, env.owner, parent, "human", env.ownerName, nil)
		setIssueStanding(t, env, parent)

		res, body := callBulk(t, env, env.owner, "user", BulkActionArchive, []string{parent})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%+v", res.StatusCode, body)
		}
		if len(body.Succeeded) != 1 || body.Succeeded[0] != parent {
			t.Fatalf("succeeded = %v, want [%s]; failed=%+v", body.Succeeded, parent, body.Failed)
		}
		if got := issueStatus(t, env, parent); got != "closed" {
			t.Fatalf("parent status = %q, want closed (childless retire allowed)", got)
		}
	})
}
