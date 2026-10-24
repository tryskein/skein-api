package handler

// sk-303.13 / T017 — contract C2 smoke: the server-side Outbox surface is
// not served any more.
//
// The row-33 corpus (sk-303.13.2.4.7) owns the full removed-contract class —
// router coverage, adjacent counters unchanged on fixed data, the wording of
// the Watching description. What lives here is the smoke T017's own
// acceptance asks for: the removed slice answers as an unknown slice, and the
// counters response has no Outbox field.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"skein-api/internal/auth"
)

// TestOutboxScopeIsNotServed: `scope=outbox` is now an unknown scope, so it
// is a 400 like any other typo — not an empty 200, which would let a client
// believe the slice still exists and is simply empty.
func TestOutboxScopeIsNotServed(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewNotificationsHandler(pool)

	// One unread feed row, so the "surviving slices still work" half below
	// asserts CONTENT and not merely a 200 — a slice that answers 200 with
	// an empty page is broken in a way a status code cannot show.
	seedMeIssue(t, pool, pid, other, prefix+"-1", "", nil)
	ev := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeNotification(t, pool, user, ev, "issue.status_changed", pid)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications?scope=outbox", nil)
	req = req.WithContext(auth.WithUserRole(auth.WithUserID(req.Context(), user), "user"))
	rec := httptest.NewRecorder()
	h.ListNotifications(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /notifications?scope=outbox = %d, want 400 (the slice is not served)", rec.Code)
	}
	if body := strings.ToLower(rec.Body.String()); strings.Contains(body, "outbox") {
		t.Errorf("the rejection still advertises the removed slice: %s", body)
	}

	// The surviving slices keep working — the removal is one slice, not the
	// endpoint. wantRow says whether the seeded non-mention unread row
	// belongs to that slice.
	for _, tc := range []struct {
		scope   string
		wantRow bool
	}{
		{"unread", true},
		{"all", true},
		{"archived", false},
		{"mentions", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications?scope="+tc.scope, nil)
		req = req.WithContext(auth.WithUserRole(auth.WithUserID(req.Context(), user), "user"))
		rec := httptest.NewRecorder()
		h.ListNotifications(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /notifications?scope=%s = %d, want 200", tc.scope, rec.Code)
			continue
		}
		var body struct {
			Items []struct {
				ID int64 `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("scope=%s: decode: %v", tc.scope, err)
			continue
		}
		if got := len(body.Items) > 0; got != tc.wantRow {
			t.Errorf("scope=%s: row present = %v, want %v (body=%s)", tc.scope, got, tc.wantRow, rec.Body.String())
		}
	}
}

// TestNotificationCountsHasNoOutboxField: the counters response no longer
// carries the field. Absent, not zero — a zero would keep the contract alive
// and let a header render a permanently empty badge.
func TestNotificationCountsHasNoOutboxField(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, otherName := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// The exact fixture the removed counter existed for: an issue the caller
	// authored and delegated, with an unread feed notification on it.
	seedMeIssue(t, pool, pid, user, prefix+"-1", otherName, nil)
	ev := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", prefix+"-1", other)
	seedMeNotification(t, pool, user, ev, "issue.status_changed", pid)

	res, body := callCounts(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if _, present := body["outbox"]; present {
		t.Fatalf("counts response still carries `outbox`: %v", body)
	}
	// The row itself was NOT deleted — it still exists and still feeds the
	// other surfaces (R-9: "only the way of looking is removed").
	var live int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		user, ev).Scan(&live); err != nil {
		t.Fatalf("count notification rows: %v", err)
	}
	if live != 1 {
		t.Errorf("notification rows for the event = %d, want 1 — removing the surface must not delete the records", live)
	}
}

// TestAdjacentCountersUnchangedByOutboxRemoval (SC-012, row 33): "on the same
// data, when the Outbox slice is gone and nothing else has changed,
// mentions and watching still count the previous values".
//
// "Prior values" cannot be read from a version of the code that no longer
// exists, so the property is expressed the way it is actually decidable: the
// fixture contains rows of exactly the class the removed counter owned — a
// non-mention notification on an issue the caller AUTHORED and DELEGATED and
// does NOT subscribe to — and those rows must contribute ZERO to both
// surviving counters. If either counter had ever been entangled with the
// authorship predicate, this is where it would show.
//
// The other two classes are seeded beside them so the assertions are about
// specific numbers rather than about zero: one mention, one subscription with
// activity.
func TestAdjacentCountersUnchangedByOutboxRemoval(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	other, otherName := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := NewMeHandler(pool, nil, nil)

	// (a) THE OUTBOX CLASS: authored by the caller, delegated to somebody
	// else, not subscribed to. Two rows, so a leak would be visible as a
	// number and not as an off-by-one.
	authoredDelegated := prefix + "-1"
	seedMeIssue(t, pool, pid, user, authoredDelegated, otherName, nil)
	for _, typ := range []string{"issue.status_changed", "issue.closed"} {
		ev := seedMeEvent(t, pool, pid, typ, "issue", authoredDelegated, other)
		seedMeNotification(t, pool, user, ev, typ, pid)
	}

	// (b) THE MENTION CLASS: one mention, on somebody else's issue.
	mentioned := prefix + "-2"
	seedMeIssue(t, pool, pid, other, mentioned, "", nil)
	evM := seedMeEvent(t, pool, pid, "issue.mention", "issue", mentioned, other)
	seedMeNotification(t, pool, user, evM, "issue.mention", pid)

	// (c) THE SUBSCRIPTION CLASS: one subscribed issue with one event.
	watched := prefix + "-3"
	seedMeIssue(t, pool, pid, other, watched, "", nil)
	seedWatcher(t, pool, watched, user, []string{"manual"})
	evW := seedMeEvent(t, pool, pid, "issue.status_changed", "issue", watched, other)
	// Written to a subscriber, so it carries the Watching card the fan-out
	// gives it (sk-622.21 US-4); since R2 the badge reads that card.
	seedMeCardNotification(t, pool, user, evW, "issue.status_changed", pid, watched)

	_, body := callCounts(t, h, user)
	if got := mustInt(t, body, "mentions"); got != 1 {
		t.Errorf("mentions = %d, want 1 — only the mention row counts; the two authored-and-delegated rows are nobody's business now", got)
	}
	if got := mustInt(t, body, "watching"); got != 1 {
		t.Errorf("watching = %d, want 1 — only the subscribed issue counts; authorship is not a subscription", got)
	}
	if _, present := body["outbox"]; present {
		t.Errorf("counts response still carries `outbox`: %v", body)
	}

	// The two authored-and-delegated rows are still THERE — the surface went,
	// the records did not (R-9). Without this the test above would also pass
	// on an implementation that simply stopped creating them.
	var live int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*)
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1 AND e.entity_id = $2`, user, authoredDelegated).Scan(&live); err != nil {
		t.Fatalf("count live rows: %v", err)
	}
	if live != 2 {
		t.Errorf("notifications on the authored-and-delegated issue = %d, want 2 — removing the surface must not delete the records", live)
	}
}

// TestWatchingCounterDescriptionMatchesTheRule (US-9, row 33): "the watching
// counter's description in the server code … matches the actual rule
// ("there is a subscription") and asserts nothing about issue authorship".
//
// This is the one place where a text check is the right instrument, because
// the artifact under test IS the text: after the Outbox surface is gone,
// Watching is where a delegator sees activity on the tasks they filed, and a
// description still saying "issues the caller did NOT author" misleads harder
// than it did before — it tells a reader the counter excludes exactly the
// case it now covers.
//
// The check is deliberately narrow: one required phrase (the rule) and a
// short closed list of authorship claims the old wording used. It is not a
// style checker.
func TestWatchingCounterDescriptionMatchesTheRule(t *testing.T) {
	src, err := os.ReadFile("me.go")
	if err != nil {
		t.Fatalf("read me.go: %v", err)
	}
	doc := docCommentAbove(t, string(src), "\tWatching int `json:\"watching\"`")

	if !strings.Contains(doc, "SUBSCRIPTIONS") && !strings.Contains(strings.ToLower(doc), "subscription") {
		t.Errorf("the Watching field's description does not state the rule (\"there is a subscription\"):\n%s", doc)
	}
	for _, claim := range []string{
		"did NOT author",
		"did not author",
		"non-self-authored",
		"issues the caller authored",
	} {
		if strings.Contains(doc, claim) {
			t.Errorf("the Watching field's description still makes an authorship claim (%q); the rule is subscription, not authorship:\n%s", claim, doc)
		}
	}
}

// docCommentAbove returns the contiguous `//` comment block immediately above
// the first line containing anchor.
func docCommentAbove(t *testing.T, src, anchor string) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	idx := -1
	for i, l := range lines {
		if strings.Contains(l, anchor) {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("anchor %q not found — the field was renamed or moved; update this test with it", anchor)
	}
	var block []string
	for i := idx - 1; i >= 0; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "//") {
			break
		}
		block = append([]string{lines[i]}, block...)
	}
	if len(block) == 0 {
		t.Fatalf("the field at %q has no doc comment at all", anchor)
	}
	return strings.Join(block, "\n")
}
