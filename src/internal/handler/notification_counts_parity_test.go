package handler

// notification_counts_parity_test.go — sk-622.21, T024, matrix row No. 48
// (US-16 «equal to the reference», R7).
//
// GET /me/notification-counts, computed by one statement over
// issues.assignee_user_id, must answer what the previous queries answered.
// The reference is computed by hand from the old rules — assignee.LeftJoin
// (username, e-mail or id, any case, deactivated users included) for Inbox
// and the lenses, review.UnreviewedPredicate (the assignee TEXT against the
// author's username) for the unreviewed set — and written down as literals
// next to the corpus below. The corpus includes the two directory changes
// that move a resolution: an account created for an e-mail that tasks were
// already assigned to (late binding), and an e-mail change at SSO login,
// both through the product's provisioner (auth.Provisioner).

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

// parityExec runs one fixture statement.
func parityExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture %q: %v", strings.SplitN(strings.TrimSpace(sql), "\n", 2)[0], err)
	}
}

// TestNotificationCountsParity_AssigneeFormsAndDirectoryChanges — No. 48.
func TestNotificationCountsParity_AssigneeFormsAndDirectoryChanges(t *testing.T) {
	pool := newMeTestPool(t)
	ctx := context.Background()

	// The caller is provisioned mid-test; this cleanup is registered first
	// so it runs last, after the project cleanup removed the caller's issues.
	var caller uuid.UUID
	t.Cleanup(func() {
		if caller != uuid.Nil {
			_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, caller)
		}
	})

	other, otherName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, other, "cpar")
	tag := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	emailBefore := "cpar-" + tag + "-old@test.local"
	emailAfter := "cpar-" + tag + "-new@test.local"
	username := "cpar" + tag
	sub := "cpar-sub-" + tag

	issue := func(n int, creator uuid.UUID, assignee, status string) string {
		id := prefix + "-" + itoa(n)
		testsupport.SeedIssue(t, pool, pid, creator, id, "parity", status, 2)
		if assignee != "" {
			testsupport.SetAssignee(t, pool, id, assignee)
		}
		return id
	}

	// Late binding: assigned to an e-mail nobody owns yet.
	issue(1, other, emailBefore, "open")

	prov := auth.NewProvisioner(pool, true, "user", "")
	res, err := prov.Provision(ctx, auth.ProvisionInput{Sub: sub, Email: emailBefore, Username: username})
	if err != nil {
		t.Fatalf("provision the caller: %v", err)
	}
	caller = res.UserID
	testsupport.AddProjectMember(t, pool, pid, caller, "member")
	h := NewMeHandler(pool, nil, notifications.NewStore(pool))

	_, body := callCounts(t, h, caller)
	if got := mustInt(t, body, "inbox"); got != 1 {
		t.Fatalf("after the account was created for the e-mail: inbox = %d, want 1 — the task assigned to that e-mail before registration became the caller's", got)
	}

	// The rest of the corpus, written while the caller's e-mail is the old
	// one; the e-mail changes at an SSO login afterwards.
	//
	// Inbox (created by other, open, unaccepted, resolves to the caller):
	issue(2, other, username, "open")                         // username
	issue(3, other, strings.ToUpper(username), "open")        // username, upper case
	issue(4, other, emailAfter, "open")                       // the NEW e-mail — nobody's until the SSO change
	issue(5, other, strings.ToUpper(caller.String()), "open") // uuid, upper case
	// #1 is assigned to the OLD e-mail: after the change it resolves to nobody.

	// On me (created by other, accepted, active):
	acc := issue(6, other, emailAfter, "in_progress")
	testsupport.Accept(t, pool, acc, caller, true)
	acc = issue(7, other, caller.String(), "open")
	testsupport.Accept(t, pool, acc, caller, true)
	self := issue(8, caller, username, "open") // self-assigned: neither Inbox nor On me
	testsupport.Accept(t, pool, self, caller, true)

	// Delegated (active, assigned to somebody who is not the caller):
	issue(9, caller, otherName, "open")                                 // created by the caller
	issue(10, caller, strings.ToUpper(otherName+"@test.local"), "open") // created by the caller, e-mail form
	issue(11, caller, "nobody-"+tag+"@nowhere.test", "open")            // unresolved text: not the caller
	issue(12, caller, emailAfter, "open")                               // the caller's own e-mail: not delegated
	prev := issue(13, other, otherName, "open")                         // caller is a previous assignee
	parityExec(t, pool, `UPDATE issues SET previous_assignees = ARRAY[$2::uuid] WHERE id = $1`, prev, caller)
	dism := issue(14, other, otherName, "open") // … and dismissed it
	parityExec(t, pool, `UPDATE issues SET previous_assignees = ARRAY[$2::uuid], dismissed_by = ARRAY[$2::uuid] WHERE id = $1`, dism, caller)

	// Unreviewed (closed, created by the caller, review required, the assignee
	// TEXT is not the caller's username — R7):
	for n, a := range map[int]string{
		15: otherName,                 // in the set
		16: emailAfter,                // the author's e-mail: stays in the set
		17: caller.String(),           // the author's uuid: stays in the set
		18: strings.ToUpper(username), // the author's username in another case: out
	} {
		id := issue(n, caller, a, "open")
		closeMeIssue(t, pool, id)
		parityExec(t, pool, `UPDATE issues SET review_required = true, reviewed_at = NULL WHERE id = $1`, id)
	}

	// Mentions and Watching: an unread mention 100 days old (older than the
	// Watching window and the default retention) and one carded notification
	// a day old.
	for _, row := range []struct {
		ntype string
		card  any
		days  int
	}{
		{"issue.mention", nil, 100},
		{"issue.status_changed", prefix + "-2", 1},
	} {
		parityExec(t, pool, `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                        actor, actor_user_id, actor_type, payload)
    VALUES (now() - make_interval(days => $5), $3, 'issue', $6, $2, 'tester', $7, 'human', '{}'::jsonb)
    RETURNING id, occurred_at
)
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_issue_id, card_resolved, created_at)
SELECT $1, ev.id, $3, $2, true, $4, true, ev.occurred_at FROM ev`,
			caller, pid, row.ntype, row.card, row.days, prefix+"-2", other)
	}

	// The e-mail changes at an SSO login (same sub, new e-mail).
	if _, err := prov.Provision(ctx, auth.ProvisionInput{Sub: sub, Email: emailAfter, Username: username}); err != nil {
		t.Fatalf("SSO login with the new e-mail: %v", err)
	}

	_, body = callCounts(t, h, caller)
	want := map[string]int{
		"inbox":                    4, // #2 #3 #4 #5; #1 (old e-mail) dropped out
		"onMeOpenCount":            2, // #6 #7
		"delegatedOpenCount":       4, // #9 #10 #11 #13
		"mentions":                 1, // the 100-day mention
		"watching":                 1, // the carded row; the mention has no card
		"delegatedUnreviewedCount": 3, // #15 #16 #17
	}
	for key, w := range want {
		if got := mustInt(t, body, key); got != w {
			t.Errorf("%s = %d, want %d; body=%v", key, got, w, body)
		}
	}
	if body["reviewMechanicEnabled"] != true {
		t.Errorf("reviewMechanicEnabled = %v, want true (the default)", body["reviewMechanicEnabled"])
	}
}
