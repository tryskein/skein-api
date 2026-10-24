package issues

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-201 — comment.* events must carry the containing issue's id in
// events.payload. entity_id on a comment event is the comment uuid, so a
// live subscriber holding only the NATS envelope has no route back to the
// issue whose thread needs invalidating; the frontend router reads exactly
// envelope.payload.issueId. Before this fix all three writers stored the
// literal '{}', the dispatcher's rawIfPresent dropped the empty object from
// the envelope, and every comment invalidation silently no-opped — comment
// edits from other actors never appeared without an F5.
//
// These tests drive the real Store methods (not WriteCommentEvent directly)
// so a future call site that bypasses the helper is caught here.

// readCommentEventIssueID returns payload->>'issueId' for the newest event
// of the given type on the given comment. A SQL NULL (payload without the
// key, i.e. the pre-sk-201 '{}') comes back as "" rather than a scan error,
// which keeps the failure message readable.
func readCommentEventIssueID(t *testing.T, pool *pgxpool.Pool, commentID uuid.UUID, eventType string) string {
	t.Helper()
	var issueID string
	err := pool.QueryRow(t.Context(), `
SELECT COALESCE(payload->>'issueId', '')
  FROM events
 WHERE entity_type = 'comment' AND entity_id = $1 AND type = $2
 ORDER BY id DESC
 LIMIT 1`, commentID.String(), eventType).Scan(&issueID)
	if err != nil {
		t.Fatalf("query %s event for comment %s: %v", eventType, commentID, err)
	}
	return issueID
}

func TestCommentEvents_PayloadCarriesIssueID(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	author := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, author)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "comment payload test")

	store := NewStore(pool)
	actor := HumanActor(author, "tester")

	created, err := store.AddComment(ctx, issueID, "first body", author,
		"tester", CommentAuthorTypeHuman, CommentAuthorKindHuman, actor)
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	commentID := created.ID

	if got := readCommentEventIssueID(t, pool, commentID, EventCommentCreated); got != issueID {
		t.Errorf("comment.created payload->>'issueId' = %q, want %q", got, issueID)
	}

	if _, err := store.UpdateComment(ctx, commentID, "edited body", created.Version, actor); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if got := readCommentEventIssueID(t, pool, commentID, EventCommentEdited); got != issueID {
		t.Errorf("comment.edited payload->>'issueId' = %q, want %q", got, issueID)
	}

	if err := store.DeleteComment(ctx, commentID, "no longer relevant", actor); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if got := readCommentEventIssueID(t, pool, commentID, EventCommentDeleted); got != issueID {
		t.Errorf("comment.deleted payload->>'issueId' = %q, want %q", got, issueID)
	}
}

// TestCommentEvents_AuditColumnsUnchanged pins AC-1's other half: payload is
// additive, old_data / new_data stay the audit trail they were. Guards
// against a "move issueId into payload" refactor that quietly drops the
// existing new_data keys other consumers (skein-web's SignalR hub, the
// activity feed) already read.
func TestCommentEvents_AuditColumnsUnchanged(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	author := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, author)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "audit columns test")

	store := NewStore(pool)
	actor := HumanActor(author, "tester")

	created, err := store.AddComment(ctx, issueID, "original body", author,
		"tester", CommentAuthorTypeHuman, CommentAuthorKindHuman, actor)
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if _, err := store.UpdateComment(ctx, created.ID, "new body", created.Version, actor); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}

	var oldBody, newBody string
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(old_data->>'body', ''), COALESCE(new_data->>'body', '')
  FROM events
 WHERE entity_type = 'comment' AND entity_id = $1 AND type = $2
 ORDER BY id DESC
 LIMIT 1`, created.ID.String(), EventCommentEdited).Scan(&oldBody, &newBody); err != nil {
		t.Fatalf("query comment.edited audit columns: %v", err)
	}
	if oldBody != "original body" {
		t.Errorf("comment.edited old_data->>'body' = %q, want %q", oldBody, "original body")
	}
	if newBody != "new body" {
		t.Errorf("comment.edited new_data->>'body' = %q, want %q", newBody, "new body")
	}
}

// TestIssueEvents_PayloadStaysEmpty pins AC-2: widening writeEvent with a
// payload argument must not leak a payload onto the issue.* writers. Those
// events' entity_id already IS the issue id, and the dispatcher relies on
// '{}' to omit the field from the envelope entirely.
// A blank issueID must be refused outright. {"issueId": ""} is a non-empty
// object, so it would pass rawIfPresent and land on the wire looking like a
// real payload while the frontend router (non-empty string required) drops it
// — the exact silent failure sk-201 fixed. The guard runs before the helper
// touches the tx, so a nil tx never gets dereferenced here.
func TestWriteCommentEvent_RejectsEmptyIssueID(t *testing.T) {
	if _, err := WriteCommentEvent(t.Context(), nil, EventCommentCreated,
		uuid.Must(uuid.NewRandom()), "", uuid.Must(uuid.NewRandom()),
		HumanActor(uuid.Must(uuid.NewRandom()), "tester"), nil, nil, ""); err == nil {
		t.Fatal("WriteCommentEvent with empty issueID: got nil error, want rejection")
	}
}

func TestIssueEvents_PayloadStaysEmpty(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	creator := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, creator)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, creator, issueID, "issue payload test")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := WriteIssueEvent(ctx, tx, EventIssueCreated, issueID, pid,
		HumanActor(creator, "tester"), "", nil,
		map[string]any{"title": "issue payload test"}, ""); err != nil {
		t.Fatalf("WriteIssueEvent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var payload string
	if err := pool.QueryRow(ctx, `
SELECT payload::text
  FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = $2
 ORDER BY id DESC
 LIMIT 1`, issueID, EventIssueCreated).Scan(&payload); err != nil {
		t.Fatalf("query issue.created payload: %v", err)
	}
	if payload != "{}" {
		t.Errorf("issue.created payload = %s, want {}", payload)
	}
}
