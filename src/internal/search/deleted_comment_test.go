package search_test

import (
	"testing"

	"github.com/pgvector/pgvector-go"

	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// TestSearchComments_ExcludesSoftDeleted asserts the contract of migration
// 000025: the text of a deleted comment never reaches a client. A soft
// delete keeps body, tsvectors and embedding in the row, so a comment whose
// deleted_at is set must not be returned (and so cannot lend its body to a
// snippet) by either the FTS-only or the hybrid (vector + FTS) query.
func TestSearchComments_ExcludesSoftDeleted(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	project, prefix := testsupport.SeedProject(t, pool, owner, "tsdel")
	tok := token(t)
	host := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "comment host", "open", 2)

	vector := make([]float32, 1024)
	vector[0] = 1
	embedding := pgvector.NewHalfVector(vector)

	live := testsupport.SeedComment(t, pool, host, owner, "live note "+tok).String()
	deleted := testsupport.SeedComment(t, pool, host, owner, "deleted secret "+tok).String()
	if _, err := pool.Exec(ctx(), `UPDATE comments SET embedding = $2 WHERE id::text = ANY($1)`,
		[]string{live, deleted}, embedding); err != nil {
		t.Fatalf("set embeddings: %v", err)
	}
	// The soft delete of issues.Store.DeleteComment: the row and its text stay.
	if _, err := pool.Exec(ctx(), `UPDATE comments SET deleted_at = now(), updated_at = now() WHERE id::text = $1`, deleted); err != nil {
		t.Fatalf("soft-delete comment: %v", err)
	}

	store := search.NewStore(pool)
	for _, mode := range []string{"fts", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			in := search.SearchInput{Query: tok, Project: &project, CallerID: owner, Limit: 50}
			if mode == "hybrid" {
				in.Vec, in.Embedding = vector, embedding
			}
			items, err := store.SearchComments(ctx(), in)
			if err != nil {
				t.Fatalf("SearchComments(%s): %v", mode, err)
			}
			got := idSet(items)
			if !got[live] {
				t.Errorf("SearchComments(%s) = %v, want the live comment %s", mode, got, live)
			}
			if got[deleted] {
				t.Errorf("SearchComments(%s) returned the soft-deleted comment %s", mode, deleted)
			}
			if len(got) != 1 {
				t.Errorf("SearchComments(%s) returned %d comments %v, want only %s", mode, len(got), got, live)
			}
		})
	}
}
