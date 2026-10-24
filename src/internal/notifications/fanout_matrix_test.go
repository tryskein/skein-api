package notifications

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fanout_matrix_test.go — sk-303.13 / T009, coverage-matrix rows 9, 10, 13 and 14:
// the DB regression on what the fan-out actually materialises.
//
// Everything here is asserted BY QUERYING THE DATA, not by reading a screen —
// SC-001 and SC-013 both say so in as many words. The unit gate (row 8)
// proves the decision; this proves that the decision is what lands in the
// table, together with the two things only a database can show: that a
// recipient found by two paths gets ONE row, and that a failure to read the
// preferences leaves the user's own operation committed.

// notifRow is the part of a notifications row this matrix cares about.
type notifRow struct {
	found       bool
	reason      *string
	feedChannel bool
	pushState   string
	pushOutcome *string
	hasDeadline bool
	hasNextAt   bool
}

func readNotifRow(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, eventID int64) notifRow {
	t.Helper()
	var r notifRow
	err := pool.QueryRow(context.Background(), `
SELECT reason, feed_channel, push_state, push_outcome,
       push_deadline_at IS NOT NULL, push_next_attempt_at IS NOT NULL
  FROM notifications
 WHERE user_id = $1 AND event_id = $2`, userID, eventID).
		Scan(&r.reason, &r.feedChannel, &r.pushState, &r.pushOutcome, &r.hasDeadline, &r.hasNextAt)
	if err == pgx.ErrNoRows {
		return notifRow{}
	}
	if err != nil {
		t.Fatalf("read notification row: %v", err)
	}
	r.found = true
	return r
}

// fanOutCandidatesFor runs the recipient-selection query alone, without the
// composition and the INSERT behind it.
//
// It exists because the two dedup mechanisms of the write path are NOT equally
// observable through the notifications table: candidate selection merges a
// person found by several arms (bool_or over the UNION), and the INSERT then
// refuses a second row for the same (recipient, event) anyway. A row count of
// 1 is therefore true whichever of the two is working, which is exactly how a
// "one row for two paths" assertion can stay green with the merge deleted
// (sk-338, verified by mutation). Reading the candidates directly is what
// makes the merge falsifiable.
func fanOutCandidatesFor(t *testing.T, pool *pgxpool.Pool, in FanOutInput) []fanOutCandidate {
	t.Helper()
	cands, err := selectFanOutCandidates(context.Background(), pool, in)
	if err != nil {
		t.Fatalf("selectFanOutCandidates: %v", err)
	}
	return cands
}

func countRowsForEvent(t *testing.T, pool *pgxpool.Pool, eventID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE event_id = $1`, eventID).Scan(&n); err != nil {
		t.Fatalf("count rows for event: %v", err)
	}
	return n
}

// statusChangeFanOut drives ONE issue.status_changed through the production
// write path and returns the event id. issue.status_changed is the event this
// file leans on because "Delegated issue status" is the reason whose two
// channels have DIFFERENT audiences — the feed goes to subscribers, the push
// goes to the delegating creator — which is exactly what the matrix is about.
func statusChangeFanOut(
	t *testing.T, pool *pgxpool.Pool, issueID string, projectID uuid.UUID,
	in FanOutInput,
) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	eid := addEvent(t, tx, "issue.status_changed", "issue", issueID, projectID, in.ActorUserID, "human")
	in.EventID = eid
	in.EventType = "issue.status_changed"
	in.EntityType = "issue"
	in.IssueID = issueID
	in.ProjectID = projectID
	if in.ActorType == "" {
		in.ActorType = "human"
	}
	if _, err := FanOut(ctx, tx, in); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return eid
}

// TestFanOutMatrix_ChannelStatesProduceZeroOrOneRow — row 9. The four
// states of a channel pair, checked in the table: no row at all when both are
// off, and exactly ONE row otherwise, carrying its reason and both channel
// facts.
func TestFanOutMatrix_ChannelStatesProduceZeroOrOneRow(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)
	addMember(t, pool, projectID, assignee, actor)

	issueID := "fmch-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// The creator watches the issue, so they are BOTH a subscriber (feed
	// audience) and the delegating creator (push audience) — the one relation
	// on which all four states are observable for a single person.
	tx0, _ := pool.Begin(ctx)
	if err := AddIssueWatcher(ctx, tx0, issueID, creator, ReasonCreator); err != nil {
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	_ = tx0.Commit(ctx)

	cases := []struct {
		name             string
		feed, push       bool
		wantRow          bool
		wantFeed         bool
		wantPushState    string
		wantPushSchedule bool
	}{
		{"off/off — no row", false, false, false, false, "", false},
		{"on/off — feed only", true, false, true, true, "none", false},
		{"off/on — push only", false, true, true, false, "pending", true},
		{"on/on — both", true, true, true, true, "pending", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, tc.feed, tc.push)
			eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
				ActorUserID:    actor,
				CreatorUserID:  creator,
				AssigneeUserID: assignee,
			})

			row := readNotifRow(t, pool, creator, eid)
			if row.found != tc.wantRow {
				t.Fatalf("row exists = %v, want %v (SC-001: verified by a query against the data)", row.found, tc.wantRow)
			}
			if !tc.wantRow {
				return
			}
			if row.reason == nil || *row.reason != string(ReasonKeyDelegatedStatus) {
				t.Errorf("reason = %v, want %s — the row must know what it is about", row.reason, ReasonKeyDelegatedStatus)
			}
			if row.feedChannel != tc.wantFeed {
				t.Errorf("feed_channel = %v, want %v", row.feedChannel, tc.wantFeed)
			}
			if row.pushState != tc.wantPushState {
				t.Errorf("push_state = %q, want %q", row.pushState, tc.wantPushState)
			}
			if row.hasDeadline != tc.wantPushSchedule || row.hasNextAt != tc.wantPushSchedule {
				t.Errorf("push schedule (deadline=%v, next=%v), want both %v — a pending push without a deadline never becomes final",
					row.hasDeadline, row.hasNextAt, tc.wantPushSchedule)
			}
			if row.pushOutcome != nil {
				t.Errorf("push_outcome = %v on a freshly created row, want NULL", row.pushOutcome)
			}
			// Exactly one row for this recipient — two channels are two facts
			// on one row, never two rows.
			var mine int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
				creator, eid).Scan(&mine); err != nil {
				t.Fatalf("count: %v", err)
			}
			if mine != 1 {
				t.Errorf("rows for one recipient = %d, want 1", mine)
			}
		})
	}
}

// TestFanOutMatrix_PushOnlyRowCarriesNoFeedOutcome — row 9, the push-only
// state. The row exists and is owed to a device, and it is NOT part of the
// feed: no feed_channel, and no NATS/live outcome stamped on it (published_at
// stays NULL — that column means the NATS outcome and nothing else).
//
// sk-338 changed the SETUP, not the claim. The creator now WATCHES the issue,
// so they are inside the feed channel's audience (subscribers) and the only
// thing keeping feed_channel false is the switched-off feed preference. Before
// that the absence of the feed was over-determined — the creator was not a
// subscriber either — and the assertion survived removing the preference half
// of the composition rule («st.Feed &&») as readily as it survived removing
// the audience half, i.e. it attributed the missing feed to nothing at all.
func TestFanOutMatrix_PushOnlyRowCarriesNoFeedOutcome(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)
	addMember(t, pool, projectID, assignee, actor)

	issueID := "fmpo-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// In the feed AUDIENCE: only the preference can hold the feed back now.
	tx0, _ := pool.Begin(ctx)
	if err := AddIssueWatcher(ctx, tx0, issueID, creator, ReasonCreator); err != nil {
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	_ = tx0.Commit(ctx)

	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, false, true)
	eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
		ActorUserID:    actor,
		CreatorUserID:  creator,
		AssigneeUserID: assignee,
	})

	row := readNotifRow(t, pool, creator, eid)
	if !row.found || row.feedChannel || row.pushState != "pending" {
		t.Fatalf("push-only row = %+v, want found with feed_channel=false, push_state=pending", row)
	}
	var publishedAt *string
	if err := pool.QueryRow(ctx,
		`SELECT published_at::text FROM notifications WHERE user_id = $1 AND event_id = $2`,
		creator, eid).Scan(&publishedAt); err != nil {
		t.Fatalf("read published_at: %v", err)
	}
	if publishedAt != nil {
		t.Errorf("published_at = %v on a push-only row, want NULL — that column is the NATS/live outcome", *publishedAt)
	}
	// And the row is invisible to a feed-filtered read.
	var feedVisible int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2 AND feed_channel`,
		creator, eid).Scan(&feedVisible); err != nil {
		t.Fatalf("count feed rows: %v", err)
	}
	if feedVisible != 0 {
		t.Errorf("feed-filtered read returned %d rows for a push-only notification, want 0 (SC-013)", feedVisible)
	}
}

// TestFanOutMatrix_SubscriptionAndRolePaths — row 10. The three
// interesting relations of one person to one event, each producing a
// different row (or none).
func TestFanOutMatrix_SubscriptionAndRolePaths(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)
	addMember(t, pool, projectID, assignee, actor)

	issueID := "fmsr-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })
	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, true, true)

	setWatch := func(reasons string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, $3::text[], now())
ON CONFLICT (issue_id, user_id) DO UPDATE SET reasons = EXCLUDED.reasons`,
			issueID, creator, reasons); err != nil {
			t.Fatalf("set watch: %v", err)
		}
	}

	t.Run("role without a subscription — push yes, feed no", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, creator); err != nil {
			t.Fatalf("clear watch: %v", err)
		}
		eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID: actor, CreatorUserID: creator, AssigneeUserID: assignee,
		})
		row := readNotifRow(t, pool, creator, eid)
		if !row.found || row.feedChannel || row.pushState != "pending" {
			t.Errorf("role-only row = %+v, want push-only", row)
		}
	})

	// sk-622.21 (T025): an explicit unwatch deletes the subscription and
	// records an opt-out; the fan-out does not read opt-outs (INV-2), so the
	// subscription path finds nobody and the role path still reaches them.
	t.Run("explicit unwatch mutes the feed and does not mute the role push", func(t *testing.T) {
		setWatch(`{creator}`)
		if err := NewStore(pool).RemoveIssueWatch(ctx, issueID, creator); err != nil {
			t.Fatalf("explicit unwatch: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`, issueID, creator)
		})
		eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID: actor, CreatorUserID: creator, AssigneeUserID: assignee,
		})
		row := readNotifRow(t, pool, creator, eid)
		if !row.found {
			t.Fatal("an explicit unwatch must not swallow the personal push (R-14)")
		}
		if row.feedChannel {
			t.Error("feed_channel = true after an explicit unwatch, want false")
		}
		if row.pushState != "pending" {
			t.Errorf("push_state = %q, want pending", row.pushState)
		}
	})

	t.Run("both paths — one row belonging to both channels", func(t *testing.T) {
		setWatch(`{creator}`)

		// The union happens in CANDIDATE SELECTION, and that is where it has
		// to be asserted (sk-338). The row count below cannot see it: a second
		// candidate for the same person would still collapse into one row at
		// the INSERT (ON CONFLICT DO NOTHING), so «rows = 1» stays true with
		// the bool_or/GROUP BY dedup deleted — verified by mutation. What only
		// the dedup can produce is ONE candidate carrying is_subscriber=true,
		// i.e. the two paths merged BEFORE composition, which is what lets the
		// single row belong to both channels.
		mine, subscriber := 0, false
		for _, c := range fanOutCandidatesFor(t, pool, FanOutInput{
			EventID: 1, EventType: "issue.status_changed", EntityType: "issue",
			IssueID: issueID, ProjectID: projectID,
			ActorUserID: actor, ActorType: "human",
			CreatorUserID: creator, AssigneeUserID: assignee,
		}) {
			if c.userID == creator {
				mine++
				subscriber = subscriber || c.isSubscriber
			}
		}
		if mine != 1 {
			t.Errorf("candidates for the recipient found by BOTH paths = %d, want 1 — "+
				"the two paths must merge into one candidate, not two", mine)
		}
		if !subscriber {
			t.Error("the merged candidate is not marked is_subscriber: bool_or over the two " +
				"paths is what lets one row belong to the subscriber-audience channel")
		}

		eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID: actor, CreatorUserID: creator, AssigneeUserID: assignee,
		})
		row := readNotifRow(t, pool, creator, eid)
		if !row.found || !row.feedChannel || row.pushState != "pending" {
			t.Errorf("union row = %+v, want feed+push on ONE row", row)
		}
		if got := countRowsForEvent(t, pool, eid); got != 1 {
			t.Errorf("rows for the event = %d, want 1 (no duplicate for the second path)", got)
		}
	})

	t.Run("self-action — no row", func(t *testing.T) {
		setWatch(`{creator}`)
		// The creator changes the status themselves: they are the actor.
		eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID: creator, CreatorUserID: creator, AssigneeUserID: assignee,
		})
		if row := readNotifRow(t, pool, creator, eid); row.found {
			t.Errorf("actor got a row about their own action: %+v", row)
		}
	})
}

// TestFanOutMatrix_NonMemberAssigneeGetsNothing — row 10 and the US-2
// acceptance in as many words: "an administrator assigned an issue to a person
// not in the project → the assignee gets neither a row nor a push". The
// assignee is resolved by handle, not by project roster, so this gate is the
// only thing standing between a private issue title and a stranger.
func TestFanOutMatrix_NonMemberAssigneeGetsNothing(t *testing.T) {
	pool := newTestPool(t)

	admin := seedTestUser(t, pool, "admin")
	outsider := seedTestUser(t, pool, "user")
	member := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, admin)
	addMember(t, pool, projectID, member, admin)

	issueID := "fmnm-1"
	seedTestIssue(t, pool, projectID, member, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// Both channels of "Issue assignment" on for the outsider — the refusal
	// must come from membership, not from a preference.
	enableReasonChannels(t, pool, outsider, ReasonKeyAssignment, true, true)

	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	eid := addEvent(t, tx, "issue.updated", "issue", issueID, projectID, admin, "human")
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:           eid,
		EventType:         "issue.updated",
		EntityType:        "issue",
		IssueID:           issueID,
		ProjectID:         projectID,
		ActorUserID:       admin,
		ActorType:         "human",
		CreatorUserID:     member,
		AssigneeUserID:    outsider,
		NewAssigneeUserID: outsider,
		Payload:           []byte(`{"changes":[{"field":"assignee"}]}`),
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if row := readNotifRow(t, pool, outsider, eid); row.found {
		t.Errorf("a non-member assignee got a row: %+v — the issue title would leak out of the project", row)
	}
}

// TestFanOutMatrix_RoleRecipientWhoIsTheActorGetsNothing — row 10,
// "addressee coincides with the actor — yes, there is no row" on the ROLE path.
//
// sk-338 added it. The role arm's actor exclusion had no behavioural test at
// all: the only proof it existed was the literal-shape check on the selection
// query, and every DB test that looked like it covered self-action was in fact
// covered by the SUBSCRIPTION arm's exclusion one line above. Mutation showed
// it — deleting `$4::uuid IS NULL OR u.id <> $4::uuid` from the role CTE left
// the entire behavioural matrix green.
//
// The case that makes it observable is self-assignment: a person creates an
// issue with themselves as the assignee, so this event's role recipient IS its
// actor. The push audience of "Issue assignment" is the new assignee and does
// not read the actor, so nothing downstream of selection would hold the push
// back — the exclusion is the whole of it.
func TestFanOutMatrix_RoleRecipientWhoIsTheActorGetsNothing(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	self := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, self)

	issueID := "fmra-1"
	seedTestIssue(t, pool, projectID, self, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// Both channels of "Issue assignment" on: the silence must come from the
	// actor exclusion, not from a preference.
	enableReasonChannels(t, pool, self, ReasonKeyAssignment, true, true)

	tx, _ := pool.Begin(ctx)
	eid := addEvent(t, tx, "issue.created", "issue", issueID, projectID, self, "human")
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:           eid,
		EventType:         "issue.created",
		EntityType:        "issue",
		IssueID:           issueID,
		ProjectID:         projectID,
		ActorUserID:       self,
		ActorType:         "human",
		CreatorUserID:     self,
		AssigneeUserID:    self,
		NewAssigneeUserID: self,
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if row := readNotifRow(t, pool, self, eid); row.found {
		t.Errorf("the actor was notified that they assigned the task to themselves: %+v — "+
			"the role arm must exclude the actor", row)
	}
}

// TestFanOutMatrix_InternalAdminBypassesMembership — row 10. The
// watch-everything admin arm is what lets an internal administrator receive
// events from a project they are not a member of.
//
// Its companion guard, `AND NOT u.is_external`, cannot be exercised through
// this path because the database refuses the state it protects against: the
// users_external_not_admin CHECK (migration 000033) makes "external AND admin"
// unrepresentable. The test therefore asserts BOTH halves — the bypass works,
// and the state the SQL guard mirrors is genuinely impossible to reach — so
// the guard is documented as a mirror of a schema invariant rather than
// silently believed to be reachable.
func TestFanOutMatrix_InternalAdminBypassesMembership(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	internalAdmin := seedTestUser(t, pool, "admin")
	projectID := seedTestProject(t, pool, owner)

	issueID := "fmad-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// The admin is NOT a project member; they watch the issue and have the
	// reason on.
	tx, _ := pool.Begin(ctx)
	if err := AddIssueWatcher(ctx, tx, issueID, internalAdmin, ReasonManual); err != nil {
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	_ = tx.Commit(ctx)
	enableReasonChannels(t, pool, internalAdmin, ReasonKeyDelegatedStatus, true, false)

	eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
		ActorUserID: owner, CreatorUserID: owner,
	})
	if row := readNotifRow(t, pool, internalAdmin, eid); !row.found {
		t.Error("internal admin watching the issue got no row — the admin bypass is gone")
	}

	// The state the `NOT u.is_external` half guards against is unrepresentable.
	if _, err := pool.Exec(ctx,
		`UPDATE users SET is_external = true WHERE id = $1`, internalAdmin); err == nil {
		t.Error("an external admin was accepted by the schema; the fan-out guard is no longer a mirror of a DB invariant")
	}
}

// TestFanOutMatrix_AgentSuppressionBindsOnlyTheSubscriptionPath — row 10.
// The suppression is a SUBSCRIPTION-path filter. A recipient reached because
// the event concerns them PERSONALLY keeps their signal with the toggle on.
//
// sk-335 sharpened what that claim rests on. The toggle used to be read as «do
// not tell me what MY agents did», which made this case trivially safe: the
// actor here is somebody else's agent, so the setting was not about them
// anyway — and, as it turned out, the setting was about nobody at all, since
// its branch could never fire. Now the setting covers EVERY agent, this actor
// included, and the personal push survives for the real reason: suppression
// removes the recipient from the subscription pools, and the role arm delivers
// them regardless.
func TestFanOutMatrix_AgentSuppressionBindsOnlyTheSubscriptionPath(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	otherOwner := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, otherOwner)
	addMember(t, pool, projectID, creator, otherOwner)
	addMember(t, pool, projectID, assignee, otherOwner)

	issueID := "fmag-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions, updated_at)
VALUES ($1, true, now())
ON CONFLICT (user_id) DO UPDATE SET suppress_agent_actions = true`, creator); err != nil {
		t.Fatalf("set suppression: %v", err)
	}
	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, true, true)

	// Somebody ELSE's agent moves the task. Under sk-335 the setting DOES
	// cover that agent — and the personal push must still arrive, because the
	// creator is reached by the role arm, not by a subscription.
	eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
		ActorUserID:    otherOwner,
		ActorType:      "agent",
		CreatorUserID:  creator,
		AssigneeUserID: assignee,
	})
	row := readNotifRow(t, pool, creator, eid)
	if !row.found || row.pushState != "pending" {
		t.Errorf("row = %+v, want a pending push — suppression must not bind another owner's agent", row)
	}
}

// shadowReasonPrefs makes the reason-preferences table unreadable FOR THIS
// TRANSACTION ONLY, by creating a temporary table of the same name with
// incompatible columns. pg_temp precedes the public schema in search_path, so
// the fan-out's preferences SELECT resolves to the shadow and fails; ON COMMIT
// DROP removes it again, so nothing leaks into the next test.
//
// This is how "settings unavailable" is produced deterministically without
// touching the real schema or racing another test.
func shadowReasonPrefs(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx,
		`CREATE TEMPORARY TABLE user_notification_reason_prefs (shadow int) ON COMMIT DROP`); err != nil {
		t.Fatalf("shadow prefs table: %v", err)
	}
}

// TestFanOutMatrix_PrefsUnavailable_ParentOperationSurvives — row 13 and
// SC-019. The rule has three halves and all three are asserted: no row is
// created, the fan-out does not report an error, and the parent transaction
// still commits its own work.
func TestFanOutMatrix_PrefsUnavailable_ParentOperationSurvives(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)

	issueID := "fmpf-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	shadowReasonPrefs(t, ctx, tx)

	eid := addEvent(t, tx, "issue.status_changed", "issue", issueID, projectID, actor, "human")
	inserted, err := FanOut(ctx, tx, FanOutInput{
		EventID:       eid,
		EventType:     "issue.status_changed",
		EntityType:    "issue",
		IssueID:       issueID,
		ProjectID:     projectID,
		ActorUserID:   actor,
		ActorType:     "human",
		CreatorUserID: creator,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("a preferences outage must not fail the fan-out: %v", err)
	}
	if inserted != 0 {
		t.Errorf("inserted %d rows with the preferences unreadable, want 0 — \"create just in case\" is not available", inserted)
	}

	// The parent operation continues in the SAME transaction and commits.
	// Before the savepoint this was impossible: the failed statement aborted
	// the whole transaction and took the user's work with it.
	if _, err := tx.Exec(ctx,
		`UPDATE issues SET title = 'parent work survived' WHERE id = $1`, issueID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("parent transaction was poisoned by the preferences outage: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("parent commit failed: %v", err)
	}

	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM issues WHERE id = $1`, issueID).Scan(&title); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if title != "parent work survived" {
		t.Errorf("issue title = %q — the parent operation did not commit", title)
	}
	if got := countRowsForEvent(t, pool, eid); got != 0 {
		t.Errorf("notifications for the event = %d, want 0", got)
	}
}

// TestFanOutMatrix_OtherDBErrorsStayErrors — row 13, the other half. A
// preferences outage is swallowed on purpose; every other database failure
// must still reach the caller, because it says nothing about the user's
// intent having been honoured. Here the event id does not exist, so the
// INSERT violates the foreign key.
func TestFanOutMatrix_OtherDBErrorsStayErrors(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)

	issueID := "fmer-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })
	// Both channels on, so the composition really produces a row and the
	// INSERT is really reached — otherwise the test would pass on an empty
	// write path and prove nothing.
	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, true, true)

	tx, _ := pool.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()

	const noSuchEvent = int64(1 << 62)
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:       noSuchEvent,
		EventType:     "issue.status_changed",
		EntityType:    "issue",
		IssueID:       issueID,
		ProjectID:     projectID,
		ActorUserID:   actor,
		ActorType:     "human",
		CreatorUserID: creator,
	}); err == nil {
		t.Fatal("a foreign-key violation was swallowed; only a preferences outage is")
	}
}

// TestFanOutMatrix_MentionIsAddressedAndFiltered — row 14. A mention
// reaches a person who watches nothing, obeys the same four channel states as
// everything else, and creates no subscription on the way.
func TestFanOutMatrix_MentionIsAddressedAndFiltered(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	author := seedTestUser(t, pool, "user")
	mentioned := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, author)
	addMember(t, pool, projectID, mentioned, author)

	issueID := "fmmn-1"
	seedTestIssue(t, pool, projectID, author, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	cases := []struct {
		name          string
		feed, push    bool
		wantRow       bool
		wantFeed      bool
		wantPushState string
	}{
		{"off/off — no row at all", false, false, false, false, ""},
		{"on/off — feed only", true, false, true, true, "none"},
		{"off/on — push only", false, true, true, false, "pending"},
		{"on/on — both", true, true, true, true, "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enableReasonChannels(t, pool, mentioned, ReasonKeyMention, tc.feed, tc.push)

			tx, _ := pool.Begin(ctx)
			eid := addEvent(t, tx, "issue.mention", "issue", issueID, projectID, author, "human")
			if err := WriteMentionNotification(ctx, tx, eid, mentioned, "issue.mention", projectID); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("WriteMentionNotification: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}

			row := readNotifRow(t, pool, mentioned, eid)
			if row.found != tc.wantRow {
				t.Fatalf("row exists = %v, want %v", row.found, tc.wantRow)
			}
			if !tc.wantRow {
				return
			}
			if row.reason == nil || *row.reason != string(ReasonKeyMention) {
				t.Errorf("reason = %v, want %s", row.reason, ReasonKeyMention)
			}
			if row.feedChannel != tc.wantFeed || row.pushState != tc.wantPushState {
				t.Errorf("feed/push = %v/%q, want %v/%q", row.feedChannel, row.pushState, tc.wantFeed, tc.wantPushState)
			}
		})
	}

	// The mentioned user never subscribed to anything — the whole point of
	// "delivered to the addressee regardless of subscriptions" (INV-8).
	var watchers int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		issueID, mentioned).Scan(&watchers); err != nil {
		t.Fatalf("count watchers: %v", err)
	}
	if watchers != 0 {
		t.Errorf("mention created %d subscription rows, want 0 (US-8)", watchers)
	}
}

// TestFanOutMatrix_MentionPrefsUnavailable_ParentSurvives — row 13 on the
// ADDRESSED path. Same rule, same three halves: no row, no error, the comment
// or the edit that carried the mention still commits.
func TestFanOutMatrix_MentionPrefsUnavailable_ParentSurvives(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	author := seedTestUser(t, pool, "user")
	mentioned := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, author)
	addMember(t, pool, projectID, mentioned, author)

	issueID := "fmmp-1"
	seedTestIssue(t, pool, projectID, author, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	shadowReasonPrefs(t, ctx, tx)

	eid := addEvent(t, tx, "issue.mention", "issue", issueID, projectID, author, "human")
	if err := WriteMentionNotification(ctx, tx, eid, mentioned, "issue.mention", projectID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("a preferences outage must not fail the mention write: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE issues SET title = 'mention parent survived' WHERE id = $1`, issueID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("parent transaction was poisoned: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := countRowsForEvent(t, pool, eid); got != 0 {
		t.Errorf("mention rows = %d with preferences unreadable, want 0", got)
	}
	var title string
	_ = pool.QueryRow(ctx, `SELECT title FROM issues WHERE id = $1`, issueID).Scan(&title)
	if title != "mention parent survived" {
		t.Errorf("issue title = %q — the parent operation did not commit", title)
	}
}

// shadowReasonPrefsWithSerializationFailure replaces the preferences table, for
// this transaction only, with a view that answers every read with SQLSTATE
// 40001. That is a CONCURRENCY failure, not an outage of the settings: the
// table is perfectly readable, this run of the transaction just has to be
// repeated. The shadow is created inside the caller's transaction, so its
// rollback removes it.
func shadowReasonPrefsWithSerializationFailure(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
CREATE FUNCTION pg_temp.prefs_serialization_failure()
RETURNS TABLE(user_id uuid, reason text, feed_enabled boolean, push_enabled boolean)
LANGUAGE plpgsql AS $fn$
BEGIN
    RAISE EXCEPTION 'simulated concurrent update' USING ERRCODE = '40001';
END
$fn$`); err != nil {
		t.Fatalf("serialization-failing prefs function: %v", err)
	}
	if _, err := tx.Exec(ctx, `
CREATE TEMPORARY VIEW user_notification_reason_prefs AS
SELECT * FROM pg_temp.prefs_serialization_failure()`); err != nil {
		t.Fatalf("serialization-failing prefs view: %v", err)
	}
}

// TestFanOutMatrix_ConcurrencyFailureReachesTheParent — row 13, the half the
// "settings unavailable" rule was swallowing by mistake.
//
// A savepoint rollback genuinely restores the parent transaction: after a
// 40001 inside it the parent can go on and its COMMIT SUCCEEDS. That is what
// made swallowing this class a real loss rather than a rescue — the comment
// stayed, the notification vanished, and the log and the metric named
// "settings unavailable", a cause that never happened. A concurrency failure
// must go back to the parent so its own retry runs, which is the normal course
// of a serializable transaction and not a failure of the user's operation
// (R-15).
//
// Both halves are asserted: the first attempt reports the error and commits
// nothing, and the retry — with the preferences readable again — writes the
// parent's work AND the notification.
func TestFanOutMatrix_ConcurrencyFailureReachesTheParent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)
	// Channels on, so the retry really has a row to write.
	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, true, true)

	issueID := "fmcf-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	in := func(eventID int64) FanOutInput {
		return FanOutInput{
			EventID:       eventID,
			EventType:     "issue.status_changed",
			EntityType:    "issue",
			IssueID:       issueID,
			ProjectID:     projectID,
			ActorUserID:   actor,
			ActorType:     "human",
			CreatorUserID: creator,
		}
	}

	// --- attempt 1: the preferences read fails with 40001 --------------------
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	shadowReasonPrefsWithSerializationFailure(t, ctx, tx)
	failedEvent := addEvent(t, tx, "issue.status_changed", "issue", issueID, projectID, actor, "human")
	if _, err := tx.Exec(ctx,
		`UPDATE issues SET title = 'parent work' WHERE id = $1`, issueID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("parent work: %v", err)
	}
	if _, err := FanOut(ctx, tx, in(failedEvent)); err == nil {
		_ = tx.Rollback(ctx)
		t.Fatal("a serialization failure was swallowed as settings unavailable — " +
			"the parent would commit its work with the notification silently missing")
	}
	// The parent decides: it aborts and retries. Nothing of the first attempt
	// survives — including the shadow, which was created inside it.
	_ = tx.Rollback(ctx)

	if got := countRowsForEvent(t, pool, failedEvent); got != 0 {
		t.Errorf("notifications from the aborted attempt = %d, want 0", got)
	}

	// --- attempt 2: the parent's retry ---------------------------------------
	retry, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin retry: %v", err)
	}
	retriedEvent := addEvent(t, retry, "issue.status_changed", "issue", issueID, projectID, actor, "human")
	if _, err := retry.Exec(ctx,
		`UPDATE issues SET title = 'parent work' WHERE id = $1`, issueID); err != nil {
		_ = retry.Rollback(ctx)
		t.Fatalf("parent work on retry: %v", err)
	}
	inserted, err := FanOut(ctx, retry, in(retriedEvent))
	if err != nil {
		_ = retry.Rollback(ctx)
		t.Fatalf("retry fan-out: %v", err)
	}
	if inserted != 1 {
		_ = retry.Rollback(ctx)
		t.Fatalf("retry inserted %d rows, want 1", inserted)
	}
	if err := retry.Commit(ctx); err != nil {
		t.Fatalf("retry commit: %v", err)
	}

	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM issues WHERE id = $1`, issueID).Scan(&title); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if title != "parent work" {
		t.Errorf("issue title = %q, want %q — the retry did not commit", title, "parent work")
	}
	if got := countRowsForEvent(t, pool, retriedEvent); got != 1 {
		t.Errorf("notifications after the retry = %d, want 1 — the retry must write BOTH", got)
	}
}
