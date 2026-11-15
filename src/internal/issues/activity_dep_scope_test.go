package issues

import (
	"context"
	"testing"

	"skein-api/internal/testsupport"
)

// TestListIssueActivity_DepEventsMatchTheExactIssueID asserts that the feed
// of one issue carries the link events naming it on either side and no link
// event of another issue — including an issue whose id differs only where
// this one has '_', a character the id alphabet allows (validate.go
// customSuffixRegex) and LIKE reads as a wildcard.
func TestListIssueActivity_DepEventsMatchTheExactIssueID(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "")
	projectID, prefix := testsupport.SeedProject(t, pool, owner, "actdep")

	target := testsupport.SeedIssue(t, pool, projectID, owner, prefix+"-a_b", "target", "open", 2)
	lookalike := testsupport.SeedIssue(t, pool, projectID, owner, prefix+"-axb", "lookalike", "open", 2)
	other := testsupport.SeedIssue(t, pool, projectID, owner, prefix+"-z", "other", "open", 2)

	insertDepEvent := func(entityID string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
VALUES (now(), 'dependency.created', 'dep', $1, $2, 'tester', $3, 'human', '{}'::jsonb)
RETURNING id`, entityID, projectID, owner).Scan(&id); err != nil {
			t.Fatalf("insert dep event %s: %v", entityID, err)
		}
		return id
	}
	fromTarget := insertDepEvent(target + "::" + other + "::blocks")
	toTarget := insertDepEvent(other + "::" + target + "::related")
	fromLookalike := insertDepEvent(lookalike + "::" + other + "::blocks")
	toLookalike := insertDepEvent(other + "::" + lookalike + "::related")

	entries, _, err := NewStore(pool).ListIssueActivity(ctx, projectID, target, []string{"dependency.created"}, nil, 50)
	if err != nil {
		t.Fatalf("ListIssueActivity(%s): %v", target, err)
	}
	got := map[int64]bool{}
	for _, e := range entries {
		got[e.ID] = true
	}
	for _, want := range []int64{fromTarget, toTarget} {
		if !got[want] {
			t.Errorf("ListIssueActivity(%s) lacks its link event %d; got %v", target, want, got)
		}
	}
	for _, foreign := range []int64{fromLookalike, toLookalike} {
		if got[foreign] {
			t.Errorf("ListIssueActivity(%s) carries link event %d of %s", target, foreign, lookalike)
		}
	}
	if len(entries) != 2 {
		t.Errorf("ListIssueActivity(%s) = %d entries, want 2", target, len(entries))
	}
}
