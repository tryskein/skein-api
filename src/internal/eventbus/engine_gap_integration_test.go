package eventbus

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// sk-105.4 (S3 engine gaps / C1) — multi-field PATCH event family.
//
// The existing TestWriteHybridUpdateEvents_ParentAndChildren asserts the
// row layout (1 parent + N children, parent_event_id stitching). This test
// adds the actor_session_id invariant: a multi-field write emits exactly
// one entity.updated parent + N field_changed children, ALL sharing the
// SAME non-null actor_session_id — the join key a conversation audit uses to
// stitch the agent's chat log to the Skein event stream.
//
// DB-backed; skips without TEST_DATABASE_URL (in-package newTestPool).
func TestWriteHybridUpdateEvents_SharedActorSessionID(t *testing.T) {
	pool := newTestPool(t)
	uid, pid, uname := seedActor(t, pool)
	ctx := context.Background()

	const sessionID = "sess-audit-join-key"
	entityID := "ebt-sess-1"

	changes := []FieldChange{
		{Field: "title", OldValue: "old", NewValue: "new"},
		{Field: "priority", OldValue: 1, NewValue: 5},
		{Field: "description", OldValue: "a", NewValue: "b"},
	}

	var parentID int64
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		id, _, err := WriteHybridUpdateEvents(
			ctx, tx,
			"issue.updated", "issue.field_changed",
			"issue", entityID, &pid,
			// Agent actor with a session id — the MCP path that populates
			// actor_session_id.
			Actor{UserID: uid, Username: uname, Role: "agent", SessionID: sessionID},
			changes,
			"",
		)
		if err != nil {
			return err
		}
		parentID = id
		return nil
	}); err != nil {
		t.Fatalf("WriteHybridUpdateEvents: %v", err)
	}

	// Exactly one parent (issue.updated) for this entity, carrying the session id.
	var parentCount int
	var parentSession *string
	if err := pool.QueryRow(ctx, `
SELECT count(*), max(actor_session_id)
  FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.updated'`,
		entityID).Scan(&parentCount, &parentSession); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if parentCount != 1 {
		t.Fatalf("expected exactly 1 issue.updated parent, got %d", parentCount)
	}
	if parentSession == nil || *parentSession != sessionID {
		t.Fatalf("parent actor_session_id = %v, want %q", parentSession, sessionID)
	}

	// Exactly N children (issue.field_changed), and EVERY one carries the
	// SAME session id (no NULLs, no mismatch). We assert via a count of
	// rows whose session id differs from (or is NULL relative to) the
	// expected value — must be zero.
	var childCount, childMismatch int
	if err := pool.QueryRow(ctx, `
SELECT
   count(*),
   count(*) FILTER (WHERE actor_session_id IS DISTINCT FROM $2)
  FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.field_changed'`,
		entityID, sessionID).Scan(&childCount, &childMismatch); err != nil {
		t.Fatalf("read children: %v", err)
	}
	if childCount != len(changes) {
		t.Fatalf("expected %d field_changed children, got %d", len(changes), childCount)
	}
	if childMismatch != 0 {
		t.Fatalf("%d field_changed children have a wrong/NULL actor_session_id (want all = %q)",
			childMismatch, sessionID)
	}

	// Cross-check: the whole family (parent + children) is exactly
	// 1 + len(changes) rows, all keyed on the one session id and all
	// pointing at the same actor.
	var familyCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1
   AND actor_session_id = $2
   AND actor_user_id = $3`,
		entityID, sessionID, uid).Scan(&familyCount); err != nil {
		t.Fatalf("read family: %v", err)
	}
	if familyCount != 1+len(changes) {
		t.Fatalf("event family size = %d, want %d (1 parent + %d children)",
			familyCount, 1+len(changes), len(changes))
	}
	_ = parentID
}
