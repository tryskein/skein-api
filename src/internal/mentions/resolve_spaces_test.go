package mentions_test

// resolve_spaces_test.go — sk-674.18, the test delivery of row 123 (T027,
// E-21): an @mention of a person who does not see the issue's project
// notifies nobody, and the text of the mention is kept exactly as typed.
//
// Reference — E-21 as the spec words it, on the fixture legend
// (testsupport.SeedTenancy), written before the code: A.members is a
// members-visibility project of space A; onlyA is its member and sees it;
// participantNoMembers is a participant of space A without membership and does not see it;
// onlyB is in space B only and does not see it.
//
// Row 34 (resolve_isolation_test.go, T005) holds the per-actor notification
// sweep of the comment path; this row holds the description path (the
// issue.mention notification) and the preservation of the typed text on
// both paths.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

func TestMentionSpaces_NonSeeingPersonTextKeptNoNotification(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	f := testsupport.SeedTenancy(t, pool)
	store := issues.NewStore(pool)
	p := f.AMembers
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, p.ID)
	})

	sees := f.User(t, testsupport.ActorOnlyA)
	noMembership := f.User(t, testsupport.ActorParticipantNoMembers)
	otherSpace := f.User(t, testsupport.ActorOnlyB)
	owner := f.User(t, testsupport.ActorProjectOwner)
	if owner.ID != p.OwnerID {
		t.Fatalf("fixture: A.members is owned by %s, not by projectOwner", p.OwnerID)
	}
	text := "Ping @" + noMembership.Username + ", @" + otherSpace.Username + " and @" + sees.Username + " — as typed."

	notified := func(t *testing.T, eventType, entityID string) map[uuid.UUID]bool {
		t.Helper()
		rows, err := pool.Query(ctx, `
SELECT n.user_id FROM notifications n JOIN events e ON e.id = n.event_id
 WHERE n.type = $1 AND e.entity_id = $2`, eventType, entityID)
		if err != nil {
			t.Fatalf("read %s notifications: %v", eventType, err)
		}
		defer rows.Close()
		out := map[uuid.UUID]bool{}
		for rows.Next() {
			var uid uuid.UUID
			if err := rows.Scan(&uid); err != nil {
				t.Fatal(err)
			}
			out[uid] = true
		}
		return out
	}
	assertRecipients := func(t *testing.T, got map[uuid.UUID]bool) {
		t.Helper()
		if !got[sees.ID] {
			t.Errorf("the person who sees the project is not notified (positive control)")
		}
		if got[noMembership.ID] {
			t.Errorf("a participant of the space who does not see the project is notified")
		}
		if got[otherSpace.ID] {
			t.Errorf("a person of another space is notified")
		}
	}

	t.Run("issue description", func(t *testing.T) {
		var version int
		if err := pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, p.IssueID).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PatchIssue(ctx, p.IssueID, issues.PatchIssueInput{
			ExpectedVersion: version,
			Description:     &text,
		}, issues.HumanActor(owner.ID, owner.Username)); err != nil {
			t.Fatalf("PatchIssue: %v", err)
		}
		var stored string
		if err := pool.QueryRow(ctx, `SELECT description FROM issues WHERE id = $1`, p.IssueID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != text {
			t.Errorf("description stored %q, want it exactly as typed %q", stored, text)
		}
		assertRecipients(t, notified(t, "issue.mention", p.IssueID))
	})

	t.Run("comment", func(t *testing.T) {
		c, err := store.AddComment(ctx, p.IssueID, text, owner.ID, owner.Username,
			issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
			issues.HumanActor(owner.ID, owner.Username))
		if err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		var stored string
		if err := pool.QueryRow(ctx, `SELECT body FROM comments WHERE id = $1`, c.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != text || c.Body != text {
			t.Errorf("comment body stored %q / returned %q, want it exactly as typed %q", stored, c.Body, text)
		}
		assertRecipients(t, notified(t, "comment.mention", c.ID.String()))
	})
}
