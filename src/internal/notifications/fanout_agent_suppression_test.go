package notifications

// fanout_agent_suppression_test.go — sk-335.
//
// The "do not notify me about agent actions" setting got a new meaning:
// actions of ANY agents are suppressed, not only one's own. The old meaning
// was unreachable by construction — the setting's branch sat under the
// self-action exclusion and demanded the opposite condition, so it removed
// no row whether the setting was on or off, and the toggle in the settings
// form was visible to the person the whole time.
//
// The file checks exactly four acceptance points of the issue and nothing
// beyond:
//
//	1. An event of SOMEONE ELSE'S agent on a subscription: setting on —
//	   no row; off — there is one.
//	2. The same event where the recipient is addressed BY ROLE (new
//	   assignee, creator of a delegated issue, mentioned) — delivered
//	   either way.
//	3. A HUMAN's action with the setting on — delivered as before.
//	4. Own actions still do not notify — that is a separate rule, and it
//	   must not have vanished with the fix.
//
// Why suppression cannot touch a personal call — by mechanics, not by
// promise: suppression drops the recipient from the SUBSCRIPTION arms, and
// they still pass the role arm, only now as `is_subscriber = false`. A
// channel with the subscribers audience cannot claim such a row; a channel
// with a personal audience can. Sticky unwatch works exactly the same way.
//
// # sk-343 — CHANGE OF REQUIREMENT, not a repair
//
// The mechanics of the paragraph above worked as described, and that is
// exactly what turned out to be the wrong rule. A recipient the event
// addresses PERSONALLY BY ROLE arrived only on the role arm
// (`is_subscriber = false`) and therefore lost the feed: the feed audience
// of "Assignment", "Acceptance" and "Delegated issue status" is
// subscribers. A creator whose delegated issue was closed by someone else's
// agent did not see it in the feed at all, and with that reason's push
// channel off they got no row whatsoever.
//
// The new rule: suppression is NOT applied to a recipient who is in the
// event's personal-addressee list (`$6::uuid[]` — the same array that feeds
// the role arm), and it continues to apply to every other subscriber. So
// the "delegating creator" subcase below asserts the OPPOSITE of what it
// asserted before sk-343, and
// `TestAgentSuppression_PersonalAddresseeKeepsTheFeed` stitches on the
// second half of the requirement: an ordinary subscriber of the same event
// still gets nothing.
//
// The fifth addressed reason — reminder — is deliberately not in this file:
// its row is written by the reminder scheduler
// (`internal/reminders/scheduler_store.go`) with a direct INSERT, bypassing
// the fan-out, so none of the edited query's predicates reach it. Asserting
// that with a test here would mean testing a foreign package.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// setAgentSuppression writes the caller's `suppress_agent_actions` flag and
// removes the row again afterwards, so a test that leaves it ON cannot leak
// into the next one.
func setAgentSuppression(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, on bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE
   SET suppress_agent_actions = EXCLUDED.suppress_agent_actions,
       updated_at             = now()`, userID, on); err != nil {
		t.Fatalf("set suppress_agent_actions=%v for %s: %v", on, userID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_notification_prefs WHERE user_id = $1`, userID)
	})
}

// issueCreatedFanOut drives ONE issue.created through the production write
// path. It is the event that produces the "Issue assignment" reason for the
// person this event made the assignee, without any payload plumbing.
func issueCreatedFanOut(
	t *testing.T, pool *pgxpool.Pool, issueID string, projectID uuid.UUID,
	in FanOutInput,
) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if in.ActorType == "" {
		in.ActorType = "human"
	}
	eid := addEvent(t, tx, "issue.created", "issue", issueID, projectID, in.ActorUserID, in.ActorType)
	in.EventID = eid
	in.EventType = "issue.created"
	in.EntityType = "issue"
	in.IssueID = issueID
	in.ProjectID = projectID
	if _, err := FanOut(ctx, tx, in); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return eid
}

// TestAgentSuppression_SubscriptionPath_AnyAgentNotOnlyMine — acceptance 1.
//
// A subscriber related to the issue by nothing except the subscription. The
// actor is SOMEONE ELSE'S agent — exactly the case the old branch did not
// touch: it required the actor to coincide with the recipient, and the
// self-action exclusion one line above had already dropped such rows.
//
// Both states of the setting are checked on ONE configuration: the same
// subscriber, the same event, the only difference is the setting. Checking
// one state would prove only that a row happens (or does not), not that its
// presence is governed by the setting.
func TestAgentSuppression_SubscriptionPath_AnyAgentNotOnlyMine(t *testing.T) {
	pool := newTestPool(t)

	owner := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, creator, owner)
	addMember(t, pool, projectID, assignee, owner)
	addMember(t, pool, projectID, subscriber, owner)

	issueID := "sk335a-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// A plain watcher: not the creator, not the assignee, not the actor — so
	// the ONLY path that can reach them is the subscription one.
	watchIssue(t, pool, issueID, subscriber, "manual")
	enableReasonChannels(t, pool, subscriber, ReasonKeyDelegatedStatus, true, true)

	cases := []struct {
		name     string
		suppress bool
		wantRow  bool
	}{
		{"setting off — there is a row", false, true},
		{"setting on — there is no row", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setAgentSuppression(t, pool, subscriber, tc.suppress)

			// `owner`'s agent moves the task — somebody ELSE's agent as far as
			// the subscriber is concerned.
			eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
				ActorUserID:    owner,
				ActorType:      "agent",
				CreatorUserID:  creator,
				AssigneeUserID: assignee,
			})
			if got := readNotifRow(t, pool, subscriber, eid).found; got != tc.wantRow {
				t.Errorf("row exists = %v, want %v (suppress_agent_actions=%v, actor is another owner's agent)",
					got, tc.wantRow, tc.suppress)
			}
		})
	}
}

// TestAgentSuppression_NeverBindsTheRolePath — acceptance 2.
//
// A recipient the event reaches BY ROLE keeps the signal with the setting
// on. The requirement is hard: otherwise the person would stop learning that
// an issue was assigned to them or that they were mentioned — and in this
// system almost everything is done by agents, so turning the setting on
// would leave them with no notifications at all. For mentions this is also
// INV-8 of the spec.
//
// Three subcases — three different channel audiences: new assignee, creator
// of a delegated issue, mentioned. In all three the setting is on and the actor is an agent.
func TestAgentSuppression_NeverBindsTheRolePath(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)

	t.Run("new assignee gets the assignment push", func(t *testing.T) {
		recipient := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, recipient, owner)
		setAgentSuppression(t, pool, recipient, true)
		enableReasonChannels(t, pool, recipient, ReasonKeyAssignment, true, true)

		issueID := "sk335b-1"
		seedTestIssue(t, pool, projectID, owner, issueID, 2)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

		eid := issueCreatedFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID:       owner,
			ActorType:         "agent",
			CreatorUserID:     owner,
			AssigneeUserID:    recipient,
			NewAssigneeUserID: recipient,
		})
		row := readNotifRow(t, pool, recipient, eid)
		if !row.found {
			t.Fatal("the new assignee has no row — suppression reached the role path")
		}
		if row.reason == nil || *row.reason != string(ReasonKeyAssignment) {
			t.Errorf("reason = %v, want %s", row.reason, ReasonKeyAssignment)
		}
		if row.pushState != "pending" {
			t.Errorf("pushState = %q, want \"pending\" — the assignment is personally addressed", row.pushState)
		}
	})

	// ⚠ INVERTED CHECK (sk-343) — this is a CHANGE OF REQUIREMENT, not a
	// repair of a bug. Before sk-343 the subcase was named "gets push but not
	// feed" and asserted `row.feedChannel == false`. The assertion was true
	// and the code did exactly what it claimed: suppression struck the
	// recipient from the subscription arms, and the feed of "Delegated
	// issue status" has the subscribers audience, so no feed remained.
	//
	// What was wrong is the rule itself. A creator whose issue was closed by
	// someone else's agent did not see it in the feed, and with that
	// reason's push channel off they got NOTHING AT ALL about the fate of
	// the delegated issue. The product owner changed the requirement:
	// suppression is not applied to a recipient the event addresses
	// personally by role. So here the opposite is now asserted — both feed
	// and push.
	t.Run("delegating creator gets both push and feed", func(t *testing.T) {
		recipient := seedTestUser(t, pool, "user")
		assignee := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, recipient, owner)
		addMember(t, pool, projectID, assignee, owner)
		setAgentSuppression(t, pool, recipient, true)
		enableReasonChannels(t, pool, recipient, ReasonKeyDelegatedStatus, true, true)

		issueID := "sk335b-2"
		seedTestIssue(t, pool, projectID, recipient, issueID, 2)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

		// The creator ALSO watches the issue, so both paths find them. This is
		// the sharp case: before sk-343 the subscription copy was what the
		// setting silenced; now the role exemption keeps it, so the single row
		// belongs to both channels.
		watchIssue(t, pool, issueID, recipient, "creator")

		eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID:    owner,
			ActorType:      "agent",
			CreatorUserID:  recipient,
			AssigneeUserID: assignee,
		})
		row := readNotifRow(t, pool, recipient, eid)
		if !row.found {
			t.Fatal("the delegating creator has no row — suppression reached the role path")
		}
		if row.pushState != "pending" {
			t.Errorf("pushState = %q, want \"pending\" — the creator's push must not be suppressed", row.pushState)
		}
		if !row.feedChannel {
			t.Error("the creator has no feed: sk-343 requires that suppression " +
				"not apply to a recipient addressed personally by role")
		}
	})

	t.Run("the mentioned person gets the mention", func(t *testing.T) {
		recipient := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, recipient, owner)
		setAgentSuppression(t, pool, recipient, true)
		enableReasonChannels(t, pool, recipient, ReasonKeyMention, true, true)

		issueID := "sk335b-3"
		seedTestIssue(t, pool, projectID, owner, issueID, 2)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		eid := addEvent(t, tx, "issue.mention", "issue", issueID, projectID, owner, "agent")
		if err := WriteMentionNotification(ctx, tx, eid, recipient, "issue.mention", projectID); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("WriteMentionNotification: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		row := readNotifRow(t, pool, recipient, eid)
		if !row.found {
			t.Fatal("the mentioned person has no row — suppression reached the mention channel (INV-8)")
		}
		if row.reason == nil || *row.reason != string(ReasonKeyMention) {
			t.Errorf("reason = %v, want %s", row.reason, ReasonKeyMention)
		}
	})
}

// TestAgentSuppression_HumanActionStillDelivers — acceptance 3.
//
// The setting names the KIND OF ACTOR, not "everything". A human's action
// with the setting on must still arrive on a subscription as before:
// suppression that also silences people is not this setting, it is a broken
// subscription.
func TestAgentSuppression_HumanActionStillDelivers(t *testing.T) {
	pool := newTestPool(t)

	owner := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, creator, owner)
	addMember(t, pool, projectID, assignee, owner)
	addMember(t, pool, projectID, subscriber, owner)

	issueID := "sk335c-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	watchIssue(t, pool, issueID, subscriber, "manual")
	enableReasonChannels(t, pool, subscriber, ReasonKeyDelegatedStatus, true, false)
	setAgentSuppression(t, pool, subscriber, true)

	eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
		ActorUserID:    owner,
		ActorType:      "human",
		CreatorUserID:  creator,
		AssigneeUserID: assignee,
	})
	row := readNotifRow(t, pool, subscriber, eid)
	if !row.found || !row.feedChannel {
		t.Errorf("row = %+v, want a feed row — the setting is about agents, not people", row)
	}
}

// TestAgentSuppression_SelfActionStillExcluded — acceptance 4.
//
// Self-action exclusion is a SEPARATE rule, living one line above
// suppression. The sk-335 fix removed the "recipient == actor" conjunct from
// the suppression branch; the conjunct was there in error, but the rule
// itself could not be removed. The setting is OFF in every subcase: that is
// how it is visible that the silence is produced by self-action exclusion,
// not by suppression.
func TestAgentSuppression_SelfActionStillExcluded(t *testing.T) {
	pool := newTestPool(t)

	owner := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	addMember(t, pool, projectID, creator, owner)
	addMember(t, pool, projectID, assignee, owner)
	addMember(t, pool, projectID, subscriber, owner)

	issueID := "sk335d-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	watchIssue(t, pool, issueID, subscriber, "manual")
	enableReasonChannels(t, pool, subscriber, ReasonKeyDelegatedStatus, true, true)
	setAgentSuppression(t, pool, subscriber, false)

	for _, actorType := range []string{"human", "agent"} {
		t.Run("actor is the recipient themselves, actor_type="+actorType, func(t *testing.T) {
			eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
				ActorUserID:    subscriber,
				ActorType:      actorType,
				CreatorUserID:  creator,
				AssigneeUserID: assignee,
			})
			if n := countNotif(t, pool, subscriber, eid); n != 0 {
				t.Errorf("own action (%s) produced %d rows, want 0 — "+
					"self-action exclusion vanished with the fix", actorType, n)
			}
		})
	}
}

// TestAgentSuppression_PersonalAddresseeKeepsTheFeed — sk-343.
//
// ⚠ The test pins a NEW REQUIREMENT, not a defect fix: before this issue
// the code honestly did what it claimed, and the expectations below would
// have been red by construction.
//
// The requirement is two-sided, and both sides are checked on ONE event
// where that is possible: a recipient addressed personally by role gets the
// feed despite the checkbox being on; an ordinary subscriber of the same
// event gets nothing. A one-sided check would not tell a point exemption of
// the addressee from exempting the reason as a whole, and exempting the
// reason would put agent noise back into every subscriber's feed — exactly
// what the checkbox exists to prevent.
func TestAgentSuppression_PersonalAddresseeKeepsTheFeed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)

	// Acceptance 1, both halves at once: the creator of a delegated issue and
	// an unrelated subscriber receive THE SAME event from someone else's
	// agent, with the checkbox equally on and the feed channel equally on.
	// All that distinguishes them is personal addressing.
	t.Run("delegated status: creator has feed, subscriber does not", func(t *testing.T) {
		creator := seedTestUser(t, pool, "user")
		assignee := seedTestUser(t, pool, "user")
		bystander := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, creator, owner)
		addMember(t, pool, projectID, assignee, owner)
		addMember(t, pool, projectID, bystander, owner)

		issueID := "sk343a-1"
		seedTestIssue(t, pool, projectID, creator, issueID, 2)
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM issues WHERE id = $1`, issueID) })

		for _, u := range []uuid.UUID{creator, bystander} {
			setAgentSuppression(t, pool, u, true)
			enableReasonChannels(t, pool, u, ReasonKeyDelegatedStatus, true, true)
		}
		watchIssue(t, pool, issueID, creator, "creator")
		watchIssue(t, pool, issueID, bystander, "manual")

		eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID:    owner,
			ActorType:      "agent",
			CreatorUserID:  creator,
			AssigneeUserID: assignee,
		})

		row := readNotifRow(t, pool, creator, eid)
		if !row.found || !row.feedChannel {
			t.Errorf("creator: row = %+v, want a row with feed — the event is addressed to them personally", row)
		}
		if n := countNotif(t, pool, bystander, eid); n != 0 {
			t.Errorf("subscriber got %d rows, want 0 — the addressee must be exempted, not the reason", n)
		}
	})

	// Acceptance 2a: a new assignee on an assignment by an agent. They are
	// subscribed to the issue (in production the assignment itself plants
	// the subscription), so the subscription arm finds them — and that is
	// where the "Assignment" reason's feed comes from.
	t.Run("assignment by an agent: the new assignee sees the feed", func(t *testing.T) {
		recipient := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, recipient, owner)
		setAgentSuppression(t, pool, recipient, true)
		enableReasonChannels(t, pool, recipient, ReasonKeyAssignment, true, true)

		issueID := "sk343b-1"
		seedTestIssue(t, pool, projectID, owner, issueID, 2)
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM issues WHERE id = $1`, issueID) })
		watchIssue(t, pool, issueID, recipient, "assignee")

		eid := issueCreatedFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID:       owner,
			ActorType:         "agent",
			CreatorUserID:     owner,
			AssigneeUserID:    recipient,
			NewAssigneeUserID: recipient,
		})
		row := readNotifRow(t, pool, recipient, eid)
		if !row.found {
			t.Fatal("the new assignee has no row")
		}
		if !row.feedChannel {
			t.Error("the new assignee has no feed: the assignment is addressed to them personally (sk-343)")
		}
		if row.pushState != "pending" {
			t.Errorf("pushState = %q, want \"pending\"", row.pushState)
		}
	})

	// Acceptance 2b: an issue accepted by an agent — the creator. Reason
	// "Acceptance", feed audience is subscribers, push audience is the
	// creator.
	t.Run("acceptance by an agent: the creator sees the feed", func(t *testing.T) {
		creator := seedTestUser(t, pool, "user")
		assignee := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, creator, owner)
		addMember(t, pool, projectID, assignee, owner)
		setAgentSuppression(t, pool, creator, true)
		enableReasonChannels(t, pool, creator, ReasonKeyAccepted, true, true)

		issueID := "sk343c-1"
		seedTestIssue(t, pool, projectID, creator, issueID, 2)
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM issues WHERE id = $1`, issueID) })
		watchIssue(t, pool, issueID, creator, "creator")

		eid := acceptedFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID:    owner,
			ActorType:      "agent",
			CreatorUserID:  creator,
			AssigneeUserID: assignee,
			ExplicitAccept: true,
		})
		row := readNotifRow(t, pool, creator, eid)
		if !row.found {
			t.Fatal("the creator has no row about the acceptance")
		}
		if row.reason == nil || *row.reason != string(ReasonKeyAccepted) {
			t.Errorf("reason = %v, want %s", row.reason, ReasonKeyAccepted)
		}
		if !row.feedChannel {
			t.Error("the creator has no feed on acceptance: the acceptance is addressed to them personally (sk-343)")
		}
	})

	// Acceptance 3: an explicit unwatch is stronger than the checkbox and
	// stays stronger after sk-343. Exemption from suppression does not make
	// the person a subscriber — it only stops striking them from the
	// subscription arms; since sk-622.21 the explicit unwatch deletes the
	// subscription (and records an opt-out the fan-out does not read), so
	// those arms do not find the person at all: no feed, and the personal
	// push still arrives.
	t.Run("explicit unwatch mutes the feed even with the checkbox on", func(t *testing.T) {
		creator := seedTestUser(t, pool, "user")
		assignee := seedTestUser(t, pool, "user")
		addMember(t, pool, projectID, creator, owner)
		addMember(t, pool, projectID, assignee, owner)
		setAgentSuppression(t, pool, creator, true)
		enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, true, true)

		issueID := "sk343d-1"
		seedTestIssue(t, pool, projectID, creator, issueID, 2)
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM issues WHERE id = $1`, issueID) })

		if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, '{creator}'::text[], now())
ON CONFLICT (issue_id, user_id) DO UPDATE SET reasons = EXCLUDED.reasons`,
			issueID, creator); err != nil {
			t.Fatalf("seed the creator's subscription: %v", err)
		}
		if err := NewStore(pool).RemoveIssueWatch(ctx, issueID, creator); err != nil {
			t.Fatalf("explicit unwatch: %v", err)
		}

		eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID:    owner,
			ActorType:      "agent",
			CreatorUserID:  creator,
			AssigneeUserID: assignee,
		})
		row := readNotifRow(t, pool, creator, eid)
		if !row.found {
			t.Fatal("the explicit unwatch swallowed the personal push — it mutes the feed, not the addressed path")
		}
		if row.feedChannel {
			t.Error("feed remained after an explicit unwatch: an explicit \"do not show me this issue\" " +
				"outranks the exemption from suppression")
		}
		if row.pushState != "pending" {
			t.Errorf("pushState = %q, want \"pending\"", row.pushState)
		}
	})
}

// acceptedFanOut drives ONE issue.accepted through the production write path.
// The explicit-acceptance flag is the classifier's gate: acceptance stamped
// automatically (claim, self-assign) is journal-only and produces no reason at
// all, so a test about the "Acceptance" reason must set it.
func acceptedFanOut(
	t *testing.T, pool *pgxpool.Pool, issueID string, projectID uuid.UUID,
	in FanOutInput,
) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if in.ActorType == "" {
		in.ActorType = "human"
	}
	eid := addEvent(t, tx, "issue.accepted", "issue", issueID, projectID, in.ActorUserID, in.ActorType)
	in.EventID = eid
	in.EventType = "issue.accepted"
	in.EntityType = "issue"
	in.IssueID = issueID
	in.ProjectID = projectID
	if _, err := FanOut(ctx, tx, in); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return eid
}
