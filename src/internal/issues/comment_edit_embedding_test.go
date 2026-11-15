package issues

import "testing"

// An edited comment's vector was built from the text the edit removed.
// UpdateComment must clear it in the same transaction, or semantic search
// keeps matching the comment by the meaning of the removed text until
// something else re-embeds the row.
func TestUpdateComment_ClearsEmbedding(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	author := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, author)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "comment embedding reset")

	store := NewStore(pool)
	actor := HumanActor(author, "tester")

	created, err := store.AddComment(ctx, issueID, "the original text", author,
		"tester", CommentAuthorTypeHuman, CommentAuthorKindHuman, actor)
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE comments SET embedding = $2::halfvec WHERE id = $1`,
		created.ID, bhUnitVectorLiteral()); err != nil {
		t.Fatalf("set embedding: %v", err)
	}

	if _, err := store.UpdateComment(ctx, created.ID, "a different text", created.Version, actor); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}

	var cleared bool
	if err := pool.QueryRow(ctx, `SELECT embedding IS NULL FROM comments WHERE id = $1`,
		created.ID).Scan(&cleared); err != nil {
		t.Fatalf("read embedding: %v", err)
	}
	if !cleared {
		t.Errorf("comment %s embedding after UpdateComment is set, want NULL", created.ID)
	}
}
