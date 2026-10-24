package notifications

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/authz"
)

// fanout_compose_test.go — sk-303.13, coverage-matrix row 8: the UNIT gate
// on the fan-out's decision, with no database anywhere in it.
//
// What is provable here and what is not, stated once so the split is not
// mistaken for a hole:
//
//   - The COMPOSITION — "a channel = the recipient has this channel of the
//     reason on AND the recipient themselves is in that channel's audience" — is a pure function
//     of (event, relation, preferences). Every claim US-2 makes about the four
//     channel states, about the two audiences of one reason differing, and
//     about the subscription/role union is tested against Compose directly.
//
//   - The RECIPIENT SELECTION gates — membership in the project, self-action
//     exclusion, sticky unwatch, agent-action suppression — decide who is a
//     CANDIDATE, and they live in SQL because that is where the pools are.
//     They are proven behaviourally by the DB matrix (T009, rows 9–10). What
//     this file adds for them is a SHAPE check on the selection query: each
//     gate is present where the spec says it applies and ABSENT where the spec
//     says it does not (the role arm carries no agent-action suppression at
//     all, R-14 / INV-8; since sk-343 the subscription arms carry it with a
//     personal-addressee exemption). A behavioural test cannot catch a gate
//     quietly dropped from one arm of a four-arm UNION, nor an exemption
//     written against the wrong arm's alias; this can.

// fixedPrefs is a preference lookup that answers the same pair for every
// reason — the tests that use it are about the audience half of the rule.
func fixedPrefs(feed, push bool) func(ReasonKey) ReasonChannelState {
	return func(ReasonKey) ReasonChannelState {
		return ReasonChannelState{Feed: feed, Push: push}
	}
}

// TestCompose_FourChannelStates walks the state table of US-2 verbatim. The
// reason is "Mention via @" because both of its channels are applicable
// and both address the same audience, so the table isolates the PREFERENCE
// half of the rule.
func TestCompose_FourChannelStates(t *testing.T) {
	ev := EventFacts{Type: "comment.mention"}
	rc := RecipientRelation{IsMentioned: true}

	cases := []struct {
		name                        string
		feedPref, pushPref          bool
		wantRow, wantFeed, wantPush bool
		wantReason                  ReasonKey
		explanation                 string
	}{
		{"off/off", false, false, false, false, false, ReasonKeyMention,
			"no row is created — INV-1"},
		{"on/off", true, false, true, true, false, ReasonKeyMention,
			"all feeds, counters, live updates, agent replies"},
		{"off/on", false, true, true, false, true, ReasonKeyMention,
			"push delivery only: no feed shows it"},
		{"on/on", true, true, true, true, true, ReasonKeyMention,
			"everything, and the push goes out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Compose(ev, rc, fixedPrefs(tc.feedPref, tc.pushPref))
			if m.Materialises() != tc.wantRow {
				t.Fatalf("Materialises() = %v, want %v (%s)", m.Materialises(), tc.wantRow, tc.explanation)
			}
			if m.Feed != tc.wantFeed || m.Push != tc.wantPush {
				t.Errorf("feed/push = %v/%v, want %v/%v (%s)", m.Feed, m.Push, tc.wantFeed, tc.wantPush, tc.explanation)
			}
			if m.Outcome != OutcomeReason || m.Reason != tc.wantReason {
				t.Errorf("outcome/reason = %s/%s, want %s/%s", m.Outcome, m.Reason, OutcomeReason, tc.wantReason)
			}
		})
	}
}

// TestCompose_ToggleAloneIsNotEnough is the half of the rule that is easiest
// to lose: "One switched-on toggle is not enough". Both channels of
// "Issue assignment" are switched on, but a recipient who merely watches the
// issue is not in the PUSH audience of that reason (which is the new
// assignee), so the push half stays false.
func TestCompose_ToggleAloneIsNotEnough(t *testing.T) {
	ev := EventFacts{Type: "issue.updated", ChangedFields: []string{"assignee"}}

	watcher := Compose(ev, RecipientRelation{IsSubscriber: true}, fixedPrefs(true, true))
	if watcher.Reason != ReasonKeyAssignment {
		t.Fatalf("reason = %s, want %s", watcher.Reason, ReasonKeyAssignment)
	}
	if !watcher.Feed {
		t.Error("a subscriber is in the FEED audience of Issue assignment; want feed=true")
	}
	if watcher.Push {
		t.Error("a subscriber is NOT the new assignee; want push=false even with the toggle on")
	}
}

// TestCompose_StickyUnwatchMutesFeedNotRolePush is "Acceptance (composition,
// case a)" expressed at the level the rule actually lives on. Sticky unwatch
// means "found by the role path only", i.e. IsSubscriber=false — and that
// alone must decide it: the feed channel of "Issue assignment" addresses
// subscribers, its push channel addresses the new assignee.
func TestCompose_StickyUnwatchMutesFeedNotRolePush(t *testing.T) {
	ev := EventFacts{Type: "issue.updated", ChangedFields: []string{"assignee"}}

	m := Compose(ev, RecipientRelation{IsNewAssignee: true, IsSubscriber: false}, fixedPrefs(true, true))
	if !m.Materialises() {
		t.Fatal("the personal push survives a sticky unwatch; want a row")
	}
	if m.Feed {
		t.Error("sticky unwatch excludes the subscription membership of the feed channel; want feed=false")
	}
	if !m.Push {
		t.Error("sticky unwatch must NOT mute the role push (R-14); want push=true")
	}
}

// TestCompose_UnionOfBothPathsIsOneRow is "case b": the same person is BOTH
// a legitimate subscriber and the new assignee. The answer is ONE membership
// belonging to both channels — not two rows, not two notifications.
func TestCompose_UnionOfBothPathsIsOneRow(t *testing.T) {
	ev := EventFacts{Type: "issue.updated", ChangedFields: []string{"assignee"}}

	m := Compose(ev, RecipientRelation{IsNewAssignee: true, IsSubscriber: true}, fixedPrefs(true, true))
	if !m.Feed || !m.Push {
		t.Fatalf("feed/push = %v/%v, want true/true (one row belonging to both channels)", m.Feed, m.Push)
	}
	if m.Reason != ReasonKeyAssignment {
		t.Errorf("reason = %s, want %s (the more specific reason wins)", m.Reason, ReasonKeyAssignment)
	}
}

// TestCompose_ChannelAudiencesDifferPerReason pins R-12 / INV-10 on the reason
// whose two audiences differ the most: "Delegated issue status" goes to
// every subscriber in the feed but pushes ONLY the creator, and only when the
// task is really delegated — the creator is neither the assignee nor the actor.
func TestCompose_ChannelAudiencesDifferPerReason(t *testing.T) {
	ev := EventFacts{Type: "issue.status_changed"}
	prefs := fixedPrefs(true, true)

	t.Run("subscriber with no role — feed only", func(t *testing.T) {
		m := Compose(ev, RecipientRelation{IsSubscriber: true}, prefs)
		if !m.Feed || m.Push {
			t.Errorf("feed/push = %v/%v, want true/false", m.Feed, m.Push)
		}
	})
	t.Run("creator of a delegated issue — both feed and push", func(t *testing.T) {
		m := Compose(ev, RecipientRelation{IsSubscriber: true, IsCreator: true}, prefs)
		if !m.Feed || !m.Push {
			t.Errorf("feed/push = %v/%v, want true/true", m.Feed, m.Push)
		}
	})
	t.Run("creator who is also the assignee — no delegation, no push", func(t *testing.T) {
		m := Compose(ev, RecipientRelation{IsSubscriber: true, IsCreator: true, IsAssignee: true}, prefs)
		if !m.Feed || m.Push {
			t.Errorf("feed/push = %v/%v, want true/false", m.Feed, m.Push)
		}
	})
	t.Run("creator who is also the actor — own action is not pushed", func(t *testing.T) {
		m := Compose(ev, RecipientRelation{IsSubscriber: true, IsCreator: true, IsActor: true}, prefs)
		if m.Push {
			t.Error("a creator acting on their own task has been delegated nothing; want push=false")
		}
	})
	t.Run("creator without a subscription — push yes, feed no", func(t *testing.T) {
		m := Compose(ev, RecipientRelation{IsCreator: true}, prefs)
		if m.Feed || !m.Push {
			t.Errorf("feed/push = %v/%v, want false/true", m.Feed, m.Push)
		}
	})
}

// TestCompose_OwnAgentSuppressionDoesNotTouchMention is the pure half of
// INV-8: the mention audience is "the mentioned person" and nothing about the actor
// enters it. "@owner, take a look" from the owner's own agent still composes
// into both channels — that call is the whole point of the reason.
func TestCompose_OwnAgentSuppressionDoesNotTouchMention(t *testing.T) {
	ev := EventFacts{Type: "comment.mention"}
	m := Compose(ev, RecipientRelation{IsMentioned: true, IsActor: true}, fixedPrefs(true, true))
	if !m.Feed || !m.Push {
		t.Errorf("feed/push = %v/%v, want true/true — the mention audience does not read the actor", m.Feed, m.Push)
	}
}

// TestCompose_InapplicableChannelStaysOff — a channel the reason does not have
// cannot be switched on, whatever storage says. "Reminder" has no feed;
// "New issue", "Comment", "Attachment", "Moved in the tree", "Other
// issue edits" and "Automatic unclaim" have no push.
func TestCompose_InapplicableChannelStaysOff(t *testing.T) {
	cases := []struct {
		name string
		ev   EventFacts
		rc   RecipientRelation
		key  ReasonKey
	}{
		{"reminder — no feed", EventFacts{Type: "reminder.rang"}, RecipientRelation{IsReminderOwner: true}, ReasonKeyReminder},
		{"new issue — no push", EventFacts{Type: "issue.created"}, RecipientRelation{IsSubscriber: true}, ReasonKeyIssueCreated},
		{"comment — no push", EventFacts{Type: "comment.created"}, RecipientRelation{IsSubscriber: true}, ReasonKeyComment},
		{"attachment — no push", EventFacts{Type: "attachment.added"}, RecipientRelation{IsSubscriber: true}, ReasonKeyAttachment},
		{"reparent — no push", EventFacts{Type: "issue.reparented"}, RecipientRelation{IsSubscriber: true}, ReasonKeyReparent},
		{"other edits — no push", EventFacts{Type: "issue.updated", ChangedFields: []string{"priority"}}, RecipientRelation{IsSubscriber: true}, ReasonKeyOtherEdits},
		{"auto-unclaim — no push", EventFacts{Type: "issue.unclaimed", StaleLockRelease: true}, RecipientRelation{IsSubscriber: true}, ReasonKeyAutoUnclaim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Both toggles ON — the assertion is that applicability wins.
			m := Compose(tc.ev, tc.rc, fixedPrefs(true, true))
			if m.Reason != tc.key {
				t.Fatalf("reason = %s, want %s", m.Reason, tc.key)
			}
			r, _ := LookupReason(tc.key)
			if !r.Feed.Applicable && m.Feed {
				t.Error("feed channel is not applicable but composed as on")
			}
			if !r.Push.Applicable && m.Push {
				t.Error("push channel is not applicable but composed as on")
			}
			if !m.Materialises() {
				t.Error("the applicable channel is on, so a row is due")
			}
		})
	}
}

// TestCompose_NoReasonForRecipient — the event delivers, but not to THIS
// person. Distinct from audit-only: the event has a delivery path, this
// recipient is simply in none of its audiences.
func TestCompose_NoReasonForRecipient(t *testing.T) {
	m := Compose(EventFacts{Type: "reminder.rang"}, RecipientRelation{IsSubscriber: true}, fixedPrefs(true, true))
	if m.Outcome != OutcomeNoReasonForRecipient {
		t.Fatalf("outcome = %s, want %s", m.Outcome, OutcomeNoReasonForRecipient)
	}
	if m.Materialises() {
		t.Error("a recipient in no audience of the event gets no row")
	}
}

// TestEventDeliveryOutcome_GatesTheFanOut covers the pre-selection probe: what
// the fan-out decides about an event BEFORE it knows a single recipient. The
// two negative answers must stay distinguishable — audit-only is a decision,
// unmapped is a configuration error that has to be observable.
func TestEventDeliveryOutcome_GatesTheFanOut(t *testing.T) {
	cases := []struct {
		name string
		ev   EventFacts
		want Outcome
	}{
		{"claim — journal only", EventFacts{Type: "issue.claimed"}, OutcomeAuditOnly},
		{"close — journal only", EventFacts{Type: "issue.closed"}, OutcomeAuditOnly},
		{"archival — journal only", EventFacts{Type: "issue.archived"}, OutcomeAuditOnly},
		{"dependency created — journal only", EventFacts{Type: "dependency.created"}, OutcomeAuditOnly},
		{"dependency deleted — journal only", EventFacts{Type: "dependency.deleted"}, OutcomeAuditOnly},
		{"edit of an undeliverable field", EventFacts{Type: "issue.updated", ChangedFields: []string{"title", "description"}}, OutcomeAuditOnly},
		{"automatic acceptance", EventFacts{Type: "issue.accepted"}, OutcomeAuditOnly},
		{"human unclaim", EventFacts{Type: "issue.unclaimed"}, OutcomeAuditOnly},
		{"priority change — has delivery", EventFacts{Type: "issue.updated", ChangedFields: []string{"priority"}}, OutcomeReason},
		{"assignee change — has delivery", EventFacts{Type: "issue.updated", ChangedFields: []string{"assignee"}}, OutcomeReason},
		{"explicit acceptance — has delivery", EventFacts{Type: "issue.accepted", ExplicitAccept: true}, OutcomeReason},
		{"lock release by the reaper — has delivery", EventFacts{Type: "issue.unclaimed", StaleLockRelease: true}, OutcomeReason},
		{"status change — has delivery", EventFacts{Type: "issue.status_changed"}, OutcomeReason},
		{"unknown type — configuration error", EventFacts{Type: "issue.teleported"}, OutcomeUnmapped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EventDeliveryOutcome(tc.ev); got != tc.want {
				t.Errorf("EventDeliveryOutcome(%s) = %s, want %s", tc.ev.Type, got, tc.want)
			}
		})
	}
}

// TestRoleCandidates_DedupAndEmpty — the role path asks about the new assignee
// and the creator, never about the same person twice, and never about a
// missing one.
func TestRoleCandidates_DedupAndEmpty(t *testing.T) {
	a := uuid.Must(uuid.NewRandom())
	b := uuid.Must(uuid.NewRandom())

	if got := (FanOutInput{}).roleCandidates(); len(got) != 0 {
		t.Errorf("no role context → %v, want empty", got)
	}
	if got := (FanOutInput{NewAssigneeUserID: a, CreatorUserID: b}).roleCandidates(); len(got) != 2 {
		t.Errorf("two distinct roles → %v, want 2 candidates", got)
	}
	if got := (FanOutInput{NewAssigneeUserID: a, CreatorUserID: a}).roleCandidates(); len(got) != 1 {
		t.Errorf("creator assigned the task to themselves → %v, want 1 candidate", got)
	}
	if got := (FanOutInput{CreatorUserID: a}).roleCandidates(); len(got) != 1 || got[0] != a {
		t.Errorf("creator only → %v, want [%s]", got, a)
	}
}

// TestRelationOf_ReadsTheRoleContext — the relation handed to the classifier
// is built from the event's role context plus the one fact the query supplies.
func TestRelationOf_ReadsTheRoleContext(t *testing.T) {
	assignee := uuid.Must(uuid.NewRandom())
	creator := uuid.Must(uuid.NewRandom())
	actor := uuid.Must(uuid.NewRandom())
	stranger := uuid.Must(uuid.NewRandom())

	in := FanOutInput{
		NewAssigneeUserID: assignee,
		AssigneeUserID:    assignee,
		CreatorUserID:     creator,
		ActorUserID:       actor,
	}

	if rc := in.relationOf(assignee, true); !rc.IsNewAssignee || !rc.IsAssignee || rc.IsCreator || rc.IsActor || !rc.IsSubscriber {
		t.Errorf("assignee relation = %+v", rc)
	}
	if rc := in.relationOf(creator, false); !rc.IsCreator || rc.IsNewAssignee || rc.IsSubscriber {
		t.Errorf("creator relation = %+v", rc)
	}
	if rc := in.relationOf(actor, true); !rc.IsActor || rc.IsCreator {
		t.Errorf("actor relation = %+v", rc)
	}
	if rc := (in.relationOf(stranger, false)); rc != (RecipientRelation{}) {
		t.Errorf("stranger relation = %+v, want the zero value", rc)
	}
	// A zero role id must never match a real user: an unset creator would
	// otherwise make everybody the creator.
	empty := FanOutInput{}
	if rc := empty.relationOf(uuid.Nil, false); rc.IsCreator || rc.IsAssignee || rc.IsNewAssignee || rc.IsActor {
		t.Errorf("unset role context matched uuid.Nil: %+v", rc)
	}
}

// TestFanOutCandidateSQL_GatesArePresentPerPath is the shape check on the
// recipient-selection query. A behavioural test drives one arm at a time and
// cannot notice a gate dropped from a sibling arm of the UNION; this notices.
//
// The claims are exactly the two matrices of US-2, as amended by sk-335. On
// the SUBSCRIPTION path every gate is present; on the ROLE path the matrix is
// different and CLOSED — the actor exclusion and the membership recheck apply,
// and the subscription machinery (sticky unwatch, the type whitelist) and the
// agent-action suppression do not.
func TestFanOutCandidateSQL_GatesArePresentPerPath(t *testing.T) {
	sql := selectFanOutCandidateSQL

	// Sticky unwatch on the project arm only. The issue-watcher arms carry no
	// tombstone filter since R2 (sk-622.21 T025, decision R16): the CHECK of
	// migration 000062 forbids tombstone rows in issue_watchers, and an
	// explicit unwatch deletes the subscription.
	if got := strings.Count(sql, `NOT 'unwatched' = ANY(iw.reasons)`); got != 0 {
		t.Errorf("tombstone predicate on issue arms = %d, want 0 (R2: issue_watchers holds live subscriptions only)", got)
	}
	if !strings.Contains(sql, `NOT 'unwatched' = ANY(pw.reasons)`) {
		t.Error("sticky-unwatch predicate missing on the project-watcher arm")
	}
	// Self-action exclusion on every arm. sk-338: only the issue arms had any
	// behavioural cover for it, so deleting the predicate from the project or
	// the custom arm used to leave the WHOLE suite green, this shape check
	// included. It is pinned per alias here for the same reason the
	// suppression predicate is: a four-arm UNION hides the loss of one arm
	// from any behavioural test that drives a different arm.
	if got := strings.Count(sql, `($4::uuid IS NULL OR iw.user_id <> $4::uuid)`); got != 2 {
		t.Errorf("self-action exclusion on issue arms = %d, want 2 (exact + subtree)", got)
	}
	if !strings.Contains(sql, `($4::uuid IS NULL OR pw.user_id <> $4::uuid)`) {
		t.Error("self-action exclusion missing on the project-watcher arm")
	}
	if !strings.Contains(sql, `($4::uuid IS NULL OR cw.user_id <> $4::uuid)`) {
		t.Error("self-action exclusion missing on the custom-watcher arm")
	}
	// Agent-action suppression on the four SUBSCRIPTION arms only. sk-343
	// changed WHOM the predicate may reach, not where it lives: the column is
	// still read exactly once per subscription arm and never on the role arm.
	if got := strings.Count(sql, "suppress_agent_actions"); got != 4 {
		t.Errorf("agent-action suppression occurrences = %d, want 4 (one per subscription arm)", got)
	}
	// sk-335: the suppression branch must NOT re-test «the recipient is the
	// actor». That conjunct is what made the whole branch dead — the
	// self-action exclusion one line above admits only rows where the
	// recipient is NOT the actor, so a branch demanding the opposite removed
	// nothing on either setting. A behavioural test cannot catch it coming
	// back: the branch would simply go quiet again, exactly as it was for
	// releases, and every behavioural assertion would stay green. So the
	// predicate is pinned literally, as a whole, once per arm.
	for _, tbl := range []string{"iw", "pw", "cw"} {
		if strings.Contains(sql, "AND "+tbl+".user_id = $4::uuid\n       )") {
			t.Errorf("suppression branch on the %s arm re-tests recipient == actor again: "+
				"that conjunct is always false after the self-action exclusion and kills the branch", tbl)
		}
	}
	// sk-343 — CHANGE OF REQUIREMENT, pinned literally per arm.
	//
	// The predicate grew a THIRD conjunct: the recipient must not be one of
	// the event's personal addressees ($6 — the same array that feeds the role
	// arm). Its aliases differ per arm, which is exactly why it is pinned per
	// alias rather than counted: a single count would stay at 4 if one arm's
	// exemption were written against the WRONG alias, and the resulting
	// cross-arm predicate would silently exempt (or suppress) the wrong
	// person. Two of the four arms are the issue-watcher pair, so `iw` is
	// expected twice and the other two aliases once each.
	for _, tc := range []struct {
		alias string
		want  int
	}{{"iw", 2}, {"pw", 1}, {"cw", 1}} {
		predicate := `AND NOT (
           COALESCE(p.suppress_agent_actions, true) = true
           AND $5::text = 'agent'
           AND NOT (` + tc.alias + `.user_id = ANY($6::uuid[]))
       )`
		if got := strings.Count(sql, predicate); got != tc.want {
			t.Errorf("exact suppression predicate on the %s arm(s) = %d occurrences, want %d — "+
				"the branch must read the stored flag, the actor KIND and the personal-addressee "+
				"exemption, and nothing else", tc.alias, got, tc.want)
		}
	}
	// And the exemption must not leak onto the ROLE arm's own membership test:
	// $6 is that arm's INPUT there, not an exemption. Counted globally so a
	// stray fifth copy anywhere is caught.
	if got := strings.Count(sql, `AND NOT (`+`u.user_id = ANY($6::uuid[]))`); got != 0 {
		t.Errorf("role arm grew a personal-addressee exemption (%d occurrences): "+
			"there it is the selection input, and exempting it would select nobody", got)
	}
	// The project type whitelist belongs to the subscription path.
	if !strings.Contains(sql, `pw.event_types IS NULL OR $1::text = ANY(pw.event_types)`) {
		t.Error("project-watcher event_types whitelist missing")
	}
	// The recipient filter (authz: active AND sees the project — the access
	// relation, which carries the internal-admin reach and its external
	// ceiling) stands on the three subscription arms that recheck access.
	// sk-674.18 T010 replaced the hand-written "membership OR internal
	// admin" recheck this check used to count (4: these three arms + the
	// role arm, asserted below); the paths guarded are the same.
	for _, tc := range []struct {
		user string
		want int
	}{{"iw.user_id", 2}, {"cw.user_id", 1}} {
		if got := strings.Count(sql, authz.RecipientEligibleExpr(tc.user, "$2::uuid")); got != tc.want {
			t.Errorf("recipient filter on the %s arm(s) = %d occurrences, want %d", tc.user, got, tc.want)
		}
	}

	role, ok := cutRoleRecipientsCTE(sql)
	if !ok {
		t.Fatal("role_recipients CTE not found in the selection query")
	}
	if !strings.Contains(role, `$4::uuid IS NULL OR u.id <> $4::uuid`) {
		t.Error("role path must exclude the actor: \"addressee coincides with the actor — yes, there is no row\"")
	}
	if !strings.Contains(role, authz.RecipientEligibleExpr("u.id", "$2::uuid")) {
		t.Error("role path must pass the recipient filter (authz: active and sees the project)")
	}
	if strings.Contains(role, "suppress_agent_actions") {
		t.Error("agent-action suppression must NOT bind the role path (sk-335): a person " +
			"addressed personally — assignment, acceptance, delegated status — keeps their signal")
	}
	if strings.Contains(role, "unwatched") || strings.Contains(role, "event_types") {
		t.Error("subscription machinery (sticky unwatch, type whitelist) must not bind the role path (R-14)")
	}
}

// cutRoleRecipientsCTE returns the body of the role_recipients CTE. The CTE
// closes on the first line that is exactly ")", which is how the query is
// written and formatted.
func cutRoleRecipientsCTE(sql string) (string, bool) {
	const head = "role_recipients AS ("
	i := strings.Index(sql, head)
	if i < 0 {
		return "", false
	}
	rest := sql[i+len(head):]
	j := strings.Index(rest, "\n)")
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// TestInsertFanOutRowsSQL_WritesTheChannelStateAtomically — the row must carry
// its reason and both channel facts in the SAME insert. A row that exists
// without them would be a notification nobody can classify afterwards, and a
// push_state written in a second statement would leave a window in which a
// crash loses the push.
func TestInsertFanOutRowsSQL_WritesTheChannelStateAtomically(t *testing.T) {
	sql := insertFanOutRowsSQL
	for _, col := range []string{"reason", "feed_channel", "push_state", "push_next_attempt_at", "push_deadline_at"} {
		if !strings.Contains(sql, col) {
			t.Errorf("insert does not write %s", col)
		}
	}
	if !strings.Contains(sql, "ON CONFLICT (user_id, event_id) DO NOTHING") {
		t.Error("insert must stay idempotent per (recipient, event)")
	}
	if strings.Count(sql, "INSERT INTO notifications") != 1 {
		t.Error("one statement, one insert — two rows for two channels is exactly what the model forbids")
	}
	// unnest of the four parallel arrays is what makes the write set-based:
	// one statement whatever the size of the pool.
	if !strings.Contains(sql, "unnest($4::uuid[], $5::text[], $6::boolean[], $7::boolean[])") {
		t.Error("insert must be set-based over the composed arrays")
	}
}

// TestDefaultReasonState_MissingRowIsTheNarrowDefault — an absent preference
// row means "the narrow default is enough", never "everything is on". Getting this wrong
// would make a freshly created account louder than a migrated one.
func TestDefaultReasonState_MissingRowIsTheNarrowDefault(t *testing.T) {
	states := reasonStates{}
	lookup := states.lookup(uuid.Must(uuid.NewRandom()))

	for _, d := range DefaultChannelStates() {
		got := lookup(d.Key)
		if got.Feed != d.Feed || got.Push != d.Push {
			t.Errorf("%s: lookup with no row = %+v, want feed=%v push=%v", d.Key, got, d.Feed, d.Push)
		}
	}
	if got := lookup(ReasonKey("no_such_reason")); got.Feed || got.Push {
		t.Errorf("unknown reason = %+v, want both channels off", got)
	}
}
