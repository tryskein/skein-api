package issues

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

// TestWriteIssueEvent_PopulatesActorSessionID asserts that the
// MCP-session id flowing through the agent Actor lands in
// events.actor_session_id. This is the cross-system audit join key —
// A conversation-capturing system matches its logs to skein audit on string
// equality of this column, so a regression here silently breaks the
// integration without any local error.
func TestWriteIssueEvent_PopulatesActorSessionID(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	creator := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, creator)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, creator, issueID, "with session")

	// Build an Actor as an MCP-driven write would: SessionID populated
	// from auth.SessionIDFrom(ctx). We construct via context to mirror
	// the production flow (resolveAgentActor → Actor) rather than
	// hand-crafting the struct.
	const wantSession = "mcp-session-test-12345"
	authCtx := auth.WithSessionID(ctx, wantSession)
	actor := Actor{
		UserID:    creator,
		Username:  "tester",
		Role:      "agent",
		SessionID: auth.SessionIDFrom(authCtx),
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = WriteIssueEvent(ctx, tx, EventIssueCreated, issueID, pid,
		actor, "", nil, map[string]any{"title": "with session"}, "")
	if err != nil {
		t.Fatalf("WriteIssueEvent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var gotSession string
	err = pool.QueryRow(ctx, `
SELECT COALESCE(actor_session_id, '')
  FROM events
 WHERE entity_type = 'issue' AND entity_id = $1
   AND type = 'issue.created'
 ORDER BY id DESC
 LIMIT 1`, issueID).Scan(&gotSession)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if gotSession != wantSession {
		t.Fatalf("actor_session_id = %q, want %q", gotSession, wantSession)
	}
}

// TestWriteCommentEvent_PopulatesActorSessionID covers the comment
// writer path — comment creation is the most common agent write, and
// the most useful join target ("show me what I said in chat
// while I was posting this comment").
func TestWriteCommentEvent_PopulatesActorSessionID(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	creator := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, creator)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, creator, issueID, "comment session test")

	const wantSession = "mcp-session-comment-abc"

	// Insert a comment row first so the events row references a real
	// comment id (events.entity_id is text with no FK, but the
	// realistic flow does this in lockstep).
	commentID := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type,
                      body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'agent', 'hi', now(), now())`,
		commentID, issueID, creator); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM comments WHERE id = $1`, commentID)
	})

	authCtx := auth.WithSessionID(ctx, wantSession)
	actor := Actor{
		UserID:    creator,
		Username:  "tester",
		Role:      "agent",
		SessionID: auth.SessionIDFrom(authCtx),
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = WriteCommentEvent(ctx, tx, EventCommentCreated, commentID, issueID, pid,
		actor, nil, map[string]any{"body": "hi"}, "")
	if err != nil {
		t.Fatalf("WriteCommentEvent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var gotSession string
	err = pool.QueryRow(ctx, `
SELECT COALESCE(actor_session_id, '')
  FROM events
 WHERE entity_type = 'comment' AND entity_id = $1
   AND type = 'comment.created'
 ORDER BY id DESC
 LIMIT 1`, commentID.String()).Scan(&gotSession)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if gotSession != wantSession {
		t.Fatalf("actor_session_id = %q, want %q", gotSession, wantSession)
	}
}

// TestWriteIssueEvent_NullWhenNoSession asserts the column comes back
// as SQL NULL (not empty string) when the writer is called from a ctx
// with no Mcp-Session-Id — the web-UI cookie path and the system
// reaper both look like this. NULL vs "" matters for such a system's
// query: a no-session web-UI event must not match an agent's empty
// sessionId by accident.
func TestWriteIssueEvent_NullWhenNoSession(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	creator := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, creator)
	issueID := prefix + "-2"
	seedIssue(t, pool, pid, creator, issueID, "no session")

	// HumanActor has no SessionID — mirrors the cookie-auth REST path
	// where the BFF acts on behalf of a logged-in user.
	actor := HumanActor(creator, "tester")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = WriteIssueEvent(ctx, tx, EventIssueCreated, issueID, pid,
		actor, "", nil, map[string]any{"title": "no session"}, "")
	if err != nil {
		t.Fatalf("WriteIssueEvent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var sessionIsNull bool
	err = pool.QueryRow(ctx, `
SELECT actor_session_id IS NULL
  FROM events
 WHERE entity_type = 'issue' AND entity_id = $1
   AND type = 'issue.created'
 ORDER BY id DESC
 LIMIT 1`, issueID).Scan(&sessionIsNull)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if !sessionIsNull {
		t.Fatalf("actor_session_id should be SQL NULL when actor has no session id")
	}
}

// TestHandleIssueMentions_PopulatesActorSessionID covers the
// mention-event writer path. Mentions are written by a separate code
// path (writeMentionEvent) that has historically been a candidate for
// drift — assert the Actor.SessionID still flows through.
func TestHandleIssueMentions_PopulatesActorSessionID(t *testing.T) {
	pool := newTestPool(t)
	ctx := t.Context()

	// Seed two users in the same project. mentioner authors the issue;
	// mentioned is referenced via @username in the description.
	mentioner := seedUser(t, pool)
	mentioned := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, mentioner)

	// Add the mentioned user as a project member so Resolve picks them
	// up (mentions silently drop non-members).
	mentionedUsername := readUsername(t, pool, mentioned)
	mentionerUsername := readUsername(t, pool, mentioner)
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, mentioned, mentioner); err != nil {
		t.Fatalf("seed mentioned member: %v", err)
	}

	issueID := prefix + "-3"
	seedIssue(t, pool, pid, mentioner, issueID, "mention session")

	const wantSession = "mcp-session-mention-xyz"
	authCtx := auth.WithSessionID(ctx, wantSession)
	actor := Actor{
		UserID:    mentioner,
		Username:  mentionerUsername,
		Role:      "agent",
		SessionID: auth.SessionIDFrom(authCtx),
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	texts := IssueMentionTexts{
		Description: "ping @" + mentionedUsername + " please review",
	}
	if err := HandleIssueMentions(ctx, tx, issueID, pid, actor, texts, nil); err != nil {
		t.Fatalf("HandleIssueMentions: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var gotSession string
	err = pool.QueryRow(ctx, `
SELECT COALESCE(actor_session_id, '')
  FROM events
 WHERE entity_type = 'issue' AND entity_id = $1
   AND type = 'issue.mention'
 ORDER BY id DESC
 LIMIT 1`, issueID).Scan(&gotSession)
	if err != nil {
		t.Fatalf("query mention event: %v", err)
	}
	if gotSession != wantSession {
		t.Fatalf("mention actor_session_id = %q, want %q", gotSession, wantSession)
	}
}

// readUsername reads users.username for the given id — small helper so
// the mention test can build the @-handle without re-implementing
// seedUser's username derivation.
func readUsername(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) string {
	t.Helper()
	var u string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, userID).Scan(&u); err != nil {
		t.Fatalf("read username: %v", err)
	}
	return u
}
