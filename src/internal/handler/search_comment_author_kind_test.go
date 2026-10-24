// search_comment_author_kind_test.go covers the REST door for the
// comment-author-kind filter on GET /api/v1/search (sk-142.10 / T10).
//
// Reuses the newSearchEnv / callSearch / searchItems harness from
// search_numeric_id_test.go (FTS-only, nil embedder, project-pinned).
// Asserts the CSV `commentAuthorKind` query param filters COMMENT results,
// an empty param returns all kinds, and an unknown value is rejected 400.

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedSearchComment inserts a comment carrying tok in its body with an
// explicit author_kind. Generated tsv columns populate from body so the
// comment is immediately FTS-hittable.
func seedSearchComment(t *testing.T, pool *pgxpool.Pool, issueID string, author uuid.UUID, body, kind string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', $4, $5, now(), now())`,
		id, issueID, author, kind, body)
	if err != nil {
		t.Fatalf("seed comment kind=%s on %s: %v", kind, issueID, err)
	}
	return id
}

func searchItemIDSet(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range searchItems(t, body) {
		out[m["id"].(string)] = true
	}
	return out
}

// TestRESTSearch_CommentAuthorKind_Filter asserts commentAuthorKind=human
// restricts COMMENT results to the human comment, excluding the
// agent_autonomous one; omitting the param returns both.
func TestRESTSearch_CommentAuthorKind_Filter(t *testing.T) {
	env := newSearchEnv(t)
	tok := "rcak" + uuid.Must(uuid.NewRandom()).String()[:8]

	issueID := env.prefix + "-1"
	seedListIssue(t, env.pool, env.projectID, env.owner, issueID, "human", "", nil)
	humanID := seedSearchComment(t, env.pool, issueID, env.owner, "human note "+tok, "human")
	autoID := seedSearchComment(t, env.pool, issueID, env.owner, "agent note "+tok, "agent_autonomous")

	// Unfiltered (kind=comment) → both comments.
	res, body := callSearch(t, env, env.owner, "user", "q="+tok+"&kind=comment")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unfiltered status = %d, want 200 (%v)", res.StatusCode, body)
	}
	ids := searchItemIDSet(t, body)
	if !ids[humanID.String()] || !ids[autoID.String()] {
		t.Fatalf("unfiltered ids = %v, want both human(%s)+auto(%s)", ids, humanID, autoID)
	}

	// Filter human → only the human comment.
	res, body = callSearch(t, env, env.owner, "user", "q="+tok+"&kind=comment&commentAuthorKind=human")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("filtered status = %d, want 200 (%v)", res.StatusCode, body)
	}
	ids = searchItemIDSet(t, body)
	if !ids[humanID.String()] {
		t.Errorf("human comment %s missing under human filter; ids=%v", humanID, ids)
	}
	if ids[autoID.String()] {
		t.Errorf("agent_autonomous comment %s leaked under human filter; ids=%v", autoID, ids)
	}
}

// TestRESTSearch_CommentAuthorKind_Invalid asserts an unknown value yields
// 400 invalid_comment_author_kind. callSearch fatals on a non-JSON body, so
// the 400 must still be a JSON problem document (it is).
func TestRESTSearch_CommentAuthorKind_Invalid(t *testing.T) {
	env := newSearchEnv(t)
	res, body := callSearch(t, env, env.owner, "user", "q=anything&commentAuthorKind=banana")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", res.StatusCode, body)
	}
	if body["error"] != "invalid_comment_author_kind" {
		t.Fatalf("error code = %v, want invalid_comment_author_kind (body=%v)", body["error"], body)
	}
}
