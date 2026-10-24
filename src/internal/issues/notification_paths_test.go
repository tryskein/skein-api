package issues

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
)

// notification_paths_test.go — sk-303.13 / T009, coverage-matrix row 16:
// no route to a comment or to a mention creates a subscription, and the
// subscriptions that ARE legitimate survive untouched.
//
// The rule exists because a subscription belongs to the OWNER OF THE API KEY
// while comments and mentions are mostly written by that owner's agents: "for
// a comment" quietly subscribed a person to everything their agent walked
// past, and no setting could have saved them — "nobody will find the setting".

// countWatcherRows returns how many issue_watchers rows a user has on an issue
// (0 or 1 — the table is keyed on the pair).
func countWatcherRows(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		issueID, userID).Scan(&n); err != nil {
		t.Fatalf("count watcher rows: %v", err)
	}
	return n
}

func watcherReasons(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) []string {
	t.Helper()
	var reasons []string
	err := pool.QueryRow(context.Background(),
		`SELECT reasons FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		issueID, userID).Scan(&reasons)
	if err != nil {
		return nil
	}
	return reasons
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// TestNotificationPaths_NoAutoWatchOnAnyCommentOrMentionRoute — row 16.
// All four routes a mention can appear through, plus commenting itself.
func TestNotificationPaths_NoAutoWatchOnAnyCommentOrMentionRoute(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	bob := seedUser(t, pool)
	bobName := usernameFor(t, pool, bob)
	carol := seedUser(t, pool)
	carolName := usernameFor(t, pool, carol)
	commenter := seedUser(t, pool)
	commenterName := usernameFor(t, pool, commenter)

	pid, prefix := seedProject(t, pool, owner)
	for _, u := range []uuid.UUID{bob, carol, commenter} {
		addProjectMember(t, pool, pid, u, owner)
	}

	store := NewStore(pool)

	// Route 1 — a mention in the text of a NEW issue.
	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{
			Title:       "no auto-watch please",
			Description: "ping @" + bobName + " to look",
			Type:        "task",
			Priority:    2,
		},
		owner, HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if n := countWatcherRows(t, pool, issue.ID, bob); n != 0 {
		t.Errorf("mention in a new issue's text created %d subscription rows for the mentioned user, want 0", n)
	}
	if got := countNotifications(t, pool, bob, pid, "issue.mention"); got != 1 {
		t.Errorf("the mention itself must still be delivered: got %d issue.mention notifications, want 1", got)
	}

	// Route 2 — a mention in a NEW comment, written by a third person.
	c, err := store.AddComment(ctx, issue.ID, "hey @"+bobName+" and @"+carolName,
		commenter, commenterName, CommentAuthorTypeHuman, CommentAuthorKindHuman,
		HumanActor(commenter, commenterName))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	for _, u := range []uuid.UUID{bob, carol} {
		if n := countWatcherRows(t, pool, issue.ID, u); n != 0 {
			t.Errorf("mention in a comment created %d subscription rows, want 0", n)
		}
	}
	// Route 3 — the COMMENTER. This is the route the rule was written for:
	// agents comment dozens of times a session under their owner's key.
	if n := countWatcherRows(t, pool, issue.ID, commenter); n != 0 {
		t.Errorf("commenting created %d subscription rows for the author, want 0 (US-8)", n)
	}

	// Route 4 — EDITING a comment so that a NEW mention appears in it.
	if _, err := store.UpdateComment(ctx, c.ID,
		"hey @"+bobName+" and @"+carolName+" and now also @"+ownerName,
		c.Version, HumanActor(commenter, commenterName)); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	// The owner is the issue's creator, so they DO have a subscription — but
	// it must still carry only the creator reason, never 'mention'.
	if hasReason(watcherReasons(t, pool, issue.ID, owner), "mention") {
		t.Error("editing a comment added a 'mention' subscription reason")
	}

	// Route 5 — PATCHING the issue text so that a new mention appears.
	newNotes := "cc @" + carolName
	if _, err := store.PatchIssue(ctx, issue.ID,
		PatchIssueInput{ExpectedVersion: issue.Version, Notes: &newNotes},
		HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}
	if n := countWatcherRows(t, pool, issue.ID, carol); n != 0 {
		t.Errorf("mention added by a patch created %d subscription rows, want 0", n)
	}
	if got := countNotifications(t, pool, carol, pid, "issue.mention"); got != 1 {
		t.Errorf("the patch mention must still be delivered: got %d, want 1", got)
	}
}

// TestNotificationPaths_LegitimateSubscriptionsSurvive — the other half of
// row 16. Narrowing auto-subscription must not touch the three kinds that
// stay: creating, being assigned, and asking for it by hand — nor the sticky
// unwatch that overrides all of them.
func TestNotificationPaths_LegitimateSubscriptionsSurvive(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	assignee := seedUser(t, pool)
	assigneeName := usernameFor(t, pool, assignee)
	manual := seedUser(t, pool)
	quitter := seedUser(t, pool)
	quitterName := usernameFor(t, pool, quitter)

	pid, prefix := seedProject(t, pool, owner)
	for _, u := range []uuid.UUID{assignee, manual, quitter} {
		addProjectMember(t, pool, pid, u, owner)
	}

	store := NewStore(pool)
	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "subscriptions that stay", Type: "task", Priority: 2},
		owner, HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Creator.
	if !hasReason(watcherReasons(t, pool, issue.ID, owner), "creator") {
		t.Error("the creator's auto-subscription is gone")
	}

	// Assignee.
	if _, err := store.PatchIssue(ctx, issue.ID,
		PatchIssueInput{ExpectedVersion: issue.Version, Assignee: ptrStr(assigneeName)},
		HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("PatchIssue(assign): %v", err)
	}
	if !hasReason(watcherReasons(t, pool, issue.ID, assignee), "assignee") {
		t.Error("the assignee's auto-subscription is gone")
	}

	// Manual — an explicit watch keeps working exactly as before.
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual'], now())`, issue.ID, manual); err != nil {
		t.Fatalf("seed manual watch: %v", err)
	}
	if !hasReason(watcherReasons(t, pool, issue.ID, manual), "manual") {
		t.Error("the manual subscription is gone")
	}

	// An explicit unwatch beats a later auto-add: assigning the task to
	// someone who unsubscribed by hand must not re-subscribe them. Since
	// sk-622.21 the explicit unwatch deletes the subscription and records an
	// opt-out (US-1); the automatic subscription reads the opt-out (INV-2).
	notifStore := notifications.NewStore(pool)
	if err := notifStore.AddIssueWatch(ctx, issue.ID, quitter, false); err != nil {
		t.Fatalf("watch before the unwatch: %v", err)
	}
	if err := notifStore.RemoveIssueWatch(ctx, issue.ID, quitter); err != nil {
		t.Fatalf("explicit unwatch: %v", err)
	}
	fresh, err := store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if _, err := store.PatchIssue(ctx, issue.ID,
		PatchIssueInput{ExpectedVersion: fresh.Version, Assignee: ptrStr(quitterName)},
		HumanActor(owner, ownerName)); err != nil {
		t.Fatalf("PatchIssue(reassign): %v", err)
	}
	if n := countWatcherRows(t, pool, issue.ID, quitter); n != 0 {
		t.Errorf("an auto-add slipped past an explicit unwatch: reasons = %v", watcherReasons(t, pool, issue.ID, quitter))
	}
	var optedOut bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2)`,
		issue.ID, quitter).Scan(&optedOut); err != nil {
		t.Fatalf("read opt-out: %v", err)
	}
	if !optedOut {
		t.Error("the explicit unwatch's opt-out was cleared by an auto-add")
	}
}
