package issues

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// addProjectMember adds an existing user to a project (helper for the
// mention visibility tests — only project members resolve, plus admins
// via role bypass).
func addProjectMember(t *testing.T, pool *pgxpool.Pool, projectID, userID, addedBy uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, userID, addedBy)
	if err != nil {
		t.Fatalf("add project member: %v", err)
	}
}

// usernameFor reads users.username for a given user id — handy for
// constructing comment bodies without hard-coding the prefix logic.
func usernameFor(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) string {
	t.Helper()
	var u string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, userID).Scan(&u); err != nil {
		t.Fatalf("read username for %s: %v", userID, err)
	}
	return u
}

// countNotifications returns the number of notifications rows for a
// (user, type) pair in the given project — primary assertion target
// for the mention tests.
func countNotifications(t *testing.T, pool *pgxpool.Pool, userID, projectID uuid.UUID, evtType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM notifications
 WHERE user_id = $1 AND type = $2 AND project_id = $3`,
		userID, evtType, projectID).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// hasWatcherWithReason returns true if (issueID, userID) has a watcher
// row with the given reason in its reasons[] array.
func hasWatcherWithReason(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID, reason string) bool {
	t.Helper()
	var has bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS(
    SELECT 1 FROM issue_watchers
     WHERE issue_id = $1 AND user_id = $2 AND $3 = ANY(reasons)
)`, issueID, userID, reason).Scan(&has); err != nil {
		t.Fatalf("check watcher: %v", err)
	}
	return has
}

// TestMentions_AddComment_HappyPath covers the basic flow: a comment
// body contains @bob; bob is a project member; bob auto-watches with
// reason='mention' and gets a comment.mention notification.
func TestMentions_AddComment_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	bob := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	addProjectMember(t, pool, pid, bob, owner)
	bobName := usernameFor(t, pool, bob)

	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "test issue")

	store := NewStore(pool)
	body := "Hey @" + bobName + " please review this"
	if _, err := store.AddComment(context.Background(), issueID, body, owner, "owner-name", CommentAuthorTypeHuman, CommentAuthorKindHuman, HumanActor(owner, "owner-name")); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	// US-8 / INV-8: being mentioned delivers the mention and creates NO
	// subscription. Before sk-303.13 a single @-mention silently signed the
	// person up for everything that happened on the issue afterwards.
	if hasWatcherWithReason(t, pool, issueID, bob, "mention") {
		t.Errorf("a mention must not create a subscription (US-8)")
	}
	if got := countNotifications(t, pool, bob, pid, "comment.mention"); got != 1 {
		t.Errorf("bob should have 1 comment.mention notification, got %d", got)
	}
}

// TestMentions_AddComment_NonMemberDropped covers the visibility
// guarantee: a username that exists in `users` but is NOT a project
// member must not resolve, must not get a watcher row, must not get
// a notification. The plain @username text remains in the comment
// body (verified via ListComments).
func TestMentions_AddComment_NonMemberDropped(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	outsider := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	// outsider deliberately NOT added as a member.
	outsiderName := usernameFor(t, pool, outsider)

	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "test issue")

	store := NewStore(pool)
	body := "ping @" + outsiderName
	if _, err := store.AddComment(context.Background(), issueID, body, owner, "owner-name", CommentAuthorTypeHuman, CommentAuthorKindHuman, HumanActor(owner, "owner-name")); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	if hasWatcherWithReason(t, pool, issueID, outsider, "mention") {
		t.Errorf("outsider must NOT be auto-watched (non-member)")
	}
	if got := countNotifications(t, pool, outsider, pid, "comment.mention"); got != 0 {
		t.Errorf("outsider must have 0 comment.mention notifications, got %d", got)
	}

	// And the plain text mention persists in the body — the parse step
	// is non-destructive.
	cs, _, err := store.ListComments(context.Background(), issueID, CommentOriginAll, ListCommentsOpts{})
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(cs) != 1 || !strings.Contains(cs[0].Body, "@"+outsiderName) {
		t.Errorf("plain @mention should remain in comment body, got %q", cs[0].Body)
	}
}

// TestMentions_SelfMention covers §F-5: an actor mentioning themselves
// gets no notification (matches self-action exclusion in FanOut).
func TestMentions_SelfMention(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ownerName := usernameFor(t, pool, owner)

	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "test issue")

	store := NewStore(pool)
	body := "Note for @" + ownerName + " (myself)"
	if _, err := store.AddComment(context.Background(), issueID, body, owner, "owner-name", CommentAuthorTypeHuman, CommentAuthorKindHuman, HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if got := countNotifications(t, pool, owner, pid, "comment.mention"); got != 0 {
		t.Errorf("self-mention must not produce a notification, got %d", got)
	}
}

// seedAdminUser creates a users row with role='admin'.
func seedAdminUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ad" + strings.ReplaceAll(id.String()[:18], "-", "")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'admin', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestMentions_CreateIssue covers the issue.created path — mentions
// in description / notes / acceptance_criteria / design all trigger
// issue.mention events with payload.fields populated.
func TestMentions_CreateIssue_MultiField(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	bob := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	addProjectMember(t, pool, pid, bob, owner)
	bobName := usernameFor(t, pool, bob)

	store := NewStore(pool)
	issue, err := store.CreateIssue(context.Background(), pid, prefix, "counter",
		CreateIssueInput{
			Title:              "Title without mention",
			Description:        "ping @" + bobName + " in description",
			Notes:              "ping @" + bobName + " in notes",
			AcceptanceCriteria: "no mention",
			Design:             "no mention",
			Type:               "task",
			Priority:           2,
		},
		owner, HumanActor(owner, "owner-name"),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Same rule on the issue-text path: the mention arrives, the subscription
	// does not (US-8 / INV-8).
	if hasWatcherWithReason(t, pool, issue.ID, bob, "mention") {
		t.Errorf("a mention must not create a subscription (US-8)")
	}
	// Single event per recipient (not per field) — multi-field
	// mentions of the same user coalesce into one event with
	// payload.fields = [description, notes].
	if got := countNotifications(t, pool, bob, pid, "issue.mention"); got != 1 {
		t.Errorf("bob should have 1 issue.mention notification (coalesced), got %d", got)
	}

	// Verify payload.fields contains both description and notes.
	var payload []byte
	if err := pool.QueryRow(context.Background(), `
SELECT e.payload
  FROM events e
  JOIN notifications n ON n.event_id = e.id
 WHERE n.user_id = $1 AND n.type = 'issue.mention' AND n.project_id = $2`,
		bob, pid).Scan(&payload); err != nil {
		t.Fatalf("read mention event payload: %v", err)
	}
	pStr := string(payload)
	if !strings.Contains(pStr, `"description"`) || !strings.Contains(pStr, `"notes"`) {
		t.Errorf("payload.fields should mention both description and notes; got %q", pStr)
	}
}

// TestMentions_PatchIssue_DiffOnly covers §F-9: only newly-introduced
// usernames trigger notifications. Add @bob to description in CREATE,
// then in PATCH leave bob in place and add @charlie. Bob should NOT
// get a fresh notification (already mentioned); Charlie should get
// exactly one.
func TestMentions_PatchIssue_DiffOnly(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	bob := seedUser(t, pool)
	charlie := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	addProjectMember(t, pool, pid, bob, owner)
	addProjectMember(t, pool, pid, charlie, owner)
	bobName := usernameFor(t, pool, bob)
	charlieName := usernameFor(t, pool, charlie)

	store := NewStore(pool)

	// CREATE with bob mentioned.
	issue, err := store.CreateIssue(context.Background(), pid, prefix, "counter",
		CreateIssueInput{
			Title:       "PATCH diff test",
			Description: "ping @" + bobName,
			Type:        "task",
			Priority:    2,
		}, owner, HumanActor(owner, "owner-name"))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	bobBefore := countNotifications(t, pool, bob, pid, "issue.mention")
	if bobBefore != 1 {
		t.Fatalf("expected bob to have 1 notification after CREATE, got %d", bobBefore)
	}
	charlieBefore := countNotifications(t, pool, charlie, pid, "issue.mention")
	if charlieBefore != 0 {
		t.Fatalf("expected charlie to have 0 notifications after CREATE, got %d", charlieBefore)
	}

	// PATCH: keep bob mention, add charlie. Bob should remain at 1
	// (already-watching, no fresh event); charlie jumps to 1.
	newDesc := "ping @" + bobName + " and @" + charlieName
	if _, err := store.PatchIssue(context.Background(), issue.ID,
		PatchIssueInput{
			ExpectedVersion: issue.Version,
			Description:     &newDesc,
		}, HumanActor(owner, "owner-name")); err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}

	bobAfter := countNotifications(t, pool, bob, pid, "issue.mention")
	if bobAfter != 1 {
		t.Errorf("bob should still have 1 notification (no diff change), got %d", bobAfter)
	}
	charlieAfter := countNotifications(t, pool, charlie, pid, "issue.mention")
	if charlieAfter != 1 {
		t.Errorf("charlie should have 1 notification after PATCH, got %d", charlieAfter)
	}
}

// TestMentions_PatchIssue_DroppedDoesNotFire covers a sibling case to
// the diff test: when a mention is REMOVED in a PATCH (text edited to
// drop @bob entirely), no notification should fire for bob — and
// bob's existing notification stays in the inbox (we don't garbage-
// collect).
func TestMentions_PatchIssue_DroppedDoesNotFire(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	bob := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	addProjectMember(t, pool, pid, bob, owner)
	bobName := usernameFor(t, pool, bob)

	store := NewStore(pool)
	issue, err := store.CreateIssue(context.Background(), pid, prefix, "counter",
		CreateIssueInput{
			Title:       "drop test",
			Description: "ping @" + bobName,
			Type:        "task",
			Priority:    2,
		}, owner, HumanActor(owner, "owner-name"))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if got := countNotifications(t, pool, bob, pid, "issue.mention"); got != 1 {
		t.Fatalf("expected 1 mention notification post-create, got %d", got)
	}

	// Drop the mention in a PATCH.
	newDesc := "no mentions here"
	if _, err := store.PatchIssue(context.Background(), issue.ID,
		PatchIssueInput{
			ExpectedVersion: issue.Version,
			Description:     &newDesc,
		}, HumanActor(owner, "owner-name")); err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}

	if got := countNotifications(t, pool, bob, pid, "issue.mention"); got != 1 {
		t.Errorf("dropping a mention should not produce or remove notifications; got %d", got)
	}
}

// agentActor builds the actor an MCP write produces: the OWNER's users.id
// (that is the permission boundary — the api key identifies the person) with
// role 'agent' and the MCP session that did it. Mirrors
// mcp/tools.IssuesAgentActor, which the issues package cannot import.
func agentActor(userID uuid.UUID, username string) Actor {
	return Actor{
		UserID:    userID,
		Username:  username,
		Role:      "agent",
		SessionID: "mcp-session-" + uuid.Must(uuid.NewRandom()).String(),
	}
}

// countMentionEvents counts mention events of one type addressed to a user,
// so the EVENT and the NOTIFICATION are asserted separately: a fix that
// produced the row without the audit trail (or the other way round) would
// otherwise pass.
func countMentionEvents(t *testing.T, pool *pgxpool.Pool, mentionedUserID, projectID uuid.UUID, evtType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE type = $1 AND project_id = $2
   AND payload->>'mentionedUserId' = $3::text`,
		evtType, projectID, mentionedUserID.String()).Scan(&n); err != nil {
		t.Fatalf("count mention events: %v", err)
	}
	return n
}

// TestMentions_AgentMentioningItsOwner_IsDelivered — matrix row 14
// ("agent→owner is not suppressed") and INV-8: "`@owner` from an agent is
// the call the reason exists for".
//
// An MCP agent writes under its owner's api key, so Actor.UserID on every
// agent write IS the owner's users.id. Comparing ids alone therefore reads
// "@alice, take a look" from Alice's agent as Alice talking to herself and drops
// it — no event, no notification, whatever the settings say. The party that
// wrote it is the agent, not Alice, so the mention is a real call and must
// arrive. Both call sites are covered: the comment and the issue text.
func TestMentions_AgentMentioningItsOwner_IsDelivered(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ownerName := usernameFor(t, pool, owner)

	store := NewStore(pool)
	ctx := context.Background()

	// (1) The issue-text path.
	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{
			Title:       "Title without mention",
			Description: "@" + ownerName + " this item is on you",
			Type:        "task",
			Priority:    2,
		},
		owner, agentActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issue.ID) })

	if got := countNotifications(t, pool, owner, pid, "issue.mention"); got != 1 {
		t.Errorf("issue.mention notifications for the owner = %d, want 1 — "+
			"an agent naming its owner is the call the reason exists for", got)
	}
	if got := countMentionEvents(t, pool, owner, pid, "issue.mention"); got != 1 {
		t.Errorf("issue.mention events for the owner = %d, want 1", got)
	}

	// (2) The comment path.
	body := "@" + ownerName + " take a look, this needs your decision"
	if _, err := store.AddComment(ctx, issue.ID, body, owner, ownerName,
		CommentAuthorTypeAgent, CommentAuthorKindAgentAutonomous,
		agentActor(owner, ownerName)); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if got := countNotifications(t, pool, owner, pid, "comment.mention"); got != 1 {
		t.Errorf("comment.mention notifications for the owner = %d, want 1", got)
	}
	if got := countMentionEvents(t, pool, owner, pid, "comment.mention"); got != 1 {
		t.Errorf("comment.mention events for the owner = %d, want 1", got)
	}
}

// TestMentions_HumanSelfMention_StaysSuppressed_OnBothPaths — the other side of
// the same rule, and the guard against "fix it by deleting the exclusion". A
// person writing their own handle knows what they wrote; nothing is delivered,
// on the comment path and on the issue-text path alike.
func TestMentions_HumanSelfMention_StaysSuppressed_OnBothPaths(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ownerName := usernameFor(t, pool, owner)

	store := NewStore(pool)
	ctx := context.Background()

	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{
			Title:       "Title without mention",
			Description: "reminder to self: @" + ownerName,
			Type:        "task",
			Priority:    2,
		},
		owner, HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issue.ID) })

	if _, err := store.AddComment(ctx, issue.ID, "reminder to self: @"+ownerName, owner, ownerName,
		CommentAuthorTypeHuman, CommentAuthorKindHuman, HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if got := countNotifications(t, pool, owner, pid, "comment.mention"); got != 0 {
		t.Errorf("comment.mention notifications for a human self-mention = %d, want 0", got)
	}
	if got := countMentionEvents(t, pool, owner, pid, "comment.mention"); got != 0 {
		t.Errorf("comment.mention events for a human self-mention = %d, want 0", got)
	}

	if got := countNotifications(t, pool, owner, pid, "issue.mention"); got != 0 {
		t.Errorf("issue.mention notifications for a human self-mention = %d, want 0", got)
	}
	if got := countMentionEvents(t, pool, owner, pid, "issue.mention"); got != 0 {
		t.Errorf("issue.mention events for a human self-mention = %d, want 0", got)
	}
}
