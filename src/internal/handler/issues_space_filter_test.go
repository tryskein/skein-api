package handler

// issues_space_filter_test.go — sk-674.18, the test delivery of rows 140
// and 141 (T032, US-3, US-13, the `outboxMode` invariant of CLAUDE.md). The
// references are the acceptance of US-3 / US-13 and the invariant as the
// spec words them, on the fixture legend of testsupport.SeedTenancy,
// written before the code:
//
//   - 140: on the place «space» the counters of that space's project rows —
//     the plain `{metric}Count` triple and the `myOutbox{Metric}Count`
//     triple — agree with the lists the place shows under `space=`: the
//     open and in-progress issues of /issues, and /me/created-by (the
//     outbox lens). The fixture has no deferred and no standing issue, so
//     open + inProgress + blocked of a row counts exactly its open and
//     in-progress issues (blocked is their disjoint complement, sk-423).
//   - 141: an issue's space is the space of its project, not of its id
//     prefix: an issue of B.space numbered with A.space's prefix carries
//     B's spaceId in the lists and the queues (/me/assigned-to, the Inbox
//     row, the Watching card).
//
// TestIsolationMatrix_SpaceFilter (T032's own cut of rows 23/24) holds the
// open-outbox agreement and the card; these rows hold the full counter
// triples against the lists and the queues.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"skein-api/internal/testsupport"
)

// spaceCounters sums the counters of the project rows of one space as the
// caller sees them.
type spaceCounters struct{ plain, outbox int }

func (w *isoWorld) spaceCounters(t *testing.T, a testsupport.Actor, short string) spaceCounters {
	t.Helper()
	r := w.get(a, "/api/v1/projects?limit=100")
	if r.status != http.StatusOK {
		t.Fatalf("GET projects: %d %s", r.status, r.body)
	}
	var res struct {
		Items []struct {
			SpaceID                 string `json:"spaceId"`
			OpenCount               int    `json:"openCount"`
			InProgressCount         int    `json:"inProgressCount"`
			BlockedCount            int    `json:"blockedCount"`
			MyOutboxOpenCount       int    `json:"myOutboxOpenCount"`
			MyOutboxInProgressCount int    `json:"myOutboxInProgressCount"`
			MyOutboxBlockedCount    int    `json:"myOutboxBlockedCount"`
		} `json:"items"`
		HasMore bool `json:"hasMore"`
	}
	decodeInto(t, r, &res)
	if res.HasMore {
		t.Fatalf("the project list spans more than one page")
	}
	var c spaceCounters
	for _, it := range res.Items {
		if it.SpaceID != short {
			continue
		}
		c.plain += it.OpenCount + it.InProgressCount + it.BlockedCount
		c.outbox += it.MyOutboxOpenCount + it.MyOutboxInProgressCount + it.MyOutboxBlockedCount
	}
	return c
}

// TestIssuesSpaceFilter_CountersAgreeWithLists — row 140.
func TestIssuesSpaceFilter_CountersAgreeWithLists(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	caller := testsupport.ActorBoth
	me := w.f.User(t, caller)
	worker := w.f.User(t, testsupport.ActorOnlyA)
	workerB := w.f.User(t, testsupport.ActorOnlyB)

	// Per space: issues on me, issues I delegated (open and in progress).
	for i, c := range []struct {
		key      string
		delegate string
	}{{keyASpace, worker.Username}, {keyAMembers, worker.Username}, {keyBSpace, workerB.Username}} {
		p := w.byKey[c.key]
		base := 300 + 10*i
		ids := []string{fmt.Sprintf("%s-%d", p.IDPrefix, base), fmt.Sprintf("%s-%d", p.IDPrefix, base+1),
			fmt.Sprintf("%s-%d", p.IDPrefix, base+2)}
		testsupport.SeedIssue(t, w.pool, p.ID, p.OwnerID, ids[0], "on me "+w.word, "open", 2)
		testsupport.SetAssignee(t, w.pool, ids[0], me.Username)
		testsupport.SeedIssue(t, w.pool, p.ID, me.ID, ids[1], "delegated open "+w.word, "open", 2)
		testsupport.SetAssignee(t, w.pool, ids[1], c.delegate)
		testsupport.SeedIssue(t, w.pool, p.ID, me.ID, ids[2], "delegated in progress "+w.word, "in_progress", 2)
		testsupport.SetAssignee(t, w.pool, ids[2], c.delegate)
		p.IssueIDs = append(p.IssueIDs, ids...)
	}

	for name, id := range map[string]any{"A": w.f.SpaceA, "B": w.f.SpaceB} {
		var short string
		if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&short); err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			got := w.spaceCounters(t, caller, short)
			listed := 0
			for _, status := range []string{"open", "in_progress"} {
				listed += len(w.spaceRows(t, caller, "/api/v1/issues?limit=50&status="+status+"&space="+short))
			}
			if got.plain != listed {
				t.Errorf("space %s: open+inProgress+blocked of the rows = %d, the list under space= = %d", name, got.plain, listed)
			}
			outbox := len(w.spaceRows(t, caller, "/api/v1/me/created-by?limit=50&space="+short))
			if got.outbox != outbox {
				t.Errorf("space %s: myOutbox open+inProgress+blocked = %d, /me/created-by?space= = %d", name, got.outbox, outbox)
			}
			if got.plain == 0 || got.outbox == 0 {
				t.Errorf("space %s: counters %+v — the fixture proves nothing at zero", name, got)
			}
		})
	}
}

// TestIssuesSpaceFilter_SpaceByProjectInQueues — row 141.
func TestIssuesSpaceFilter_SpaceByProjectInQueues(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	reader := testsupport.ActorBoth
	me := w.f.User(t, reader)
	a, b := w.byKey[keyASpace], w.byKey[keyBSpace]
	foreign := a.IDPrefix + "-88" // A's prefix, B's project
	testsupport.SeedIssue(t, w.pool, b.ID, b.OwnerID, foreign, "foreign prefix "+w.word, "open", 2)
	testsupport.SetAssignee(t, w.pool, foreign, me.Username)
	b.IssueIDs = append(b.IssueIDs, foreign)
	var bShort, aShort string
	if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, w.f.SpaceB).Scan(&bShort); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, w.f.SpaceA).Scan(&aShort); err != nil {
		t.Fatal(err)
	}
	var ev int64
	if err := w.pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id, payload)
VALUES (now(), 'issue.status_changed', 'space-filter', $1, 'human', 'issue', $2, $3, '{}'::jsonb) RETURNING id`,
		b.OwnerID, foreign, b.ID).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_issue_id)
VALUES ($1, $2, 'issue.status_changed', $3, true, $4)`, me.ID, ev, b.ID, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since) VALUES ($1, $2, ARRAY['manual']::text[], now())
ON CONFLICT DO NOTHING`, foreign, me.ID); err != nil {
		t.Fatal(err)
	}

	// spaceOfRow finds the row naming the issue in a list body and returns
	// its spaceId.
	spaceOfRow := func(t *testing.T, body []byte, listKey string) (string, bool) {
		t.Helper()
		var res map[string]json.RawMessage
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(res[listKey], &rows); err != nil {
			t.Fatalf("decode %s: %v", listKey, err)
		}
		for _, raw := range rows {
			if !bytes.Contains(raw, []byte(`"`+foreign+`"`)) {
				continue
			}
			var row struct {
				SpaceID string `json:"spaceId"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			return row.SpaceID, true
		}
		return "", false
	}
	for _, c := range []struct{ name, path, listKey string }{
		{"assigned-to list", "/api/v1/me/assigned-to?limit=100", "items"},
		{"all issues list", "/api/v1/issues?limit=100&q=" + w.word, "items"},
		{"inbox row", "/api/v1/notifications?scope=all&limit=100", "items"},
		{"watching card", "/api/v1/me/watching", "items"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := w.get(reader, c.path)
			if r.status != http.StatusOK {
				t.Fatalf("%s: %d %s", c.path, r.status, r.body)
			}
			got, found := spaceOfRow(t, r.body, c.listKey)
			if !found {
				t.Fatalf("%s does not list the issue %s: %s", c.name, foreign, r.body)
			}
			if got != bShort {
				t.Errorf("%s: spaceId %q, want B's %q (its project), not A's %q (its prefix)", c.name, got, bShort, aShort)
			}
		})
	}
}
