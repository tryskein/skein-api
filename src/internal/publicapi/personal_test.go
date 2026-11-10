package publicapi_test

// personal_test.go — the operations of the caller's own state of the public
// API (sk-878.3.1 T027, row 98; SC-022, US-3 «write acceptance»): every
// effect of row 29 through REST, createWatchFilter with a project outside
// the scope — 422, listWatching paged per collection, and parity of the
// personal operations with MCP. The reference is the spec: «a write by a
// key changes only objects inside its scope: read all leaves Q's
// notifications; a filter over Q or over any project is neither listed nor
// deleted; a filter only over projects inside the scope, any project only
// with scope all; reminders on Q are neither listed nor counted, snoozing
// and dismissing them answer as a missing one; reading a Watching card with
// its subtree marks only the events of issues inside the scope». Each case
// compares Q's state before and after, and the answer with the one for a
// really missing id.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

// personalWorld is the owner of P and Q of one space, an issue in each.
type personalWorld struct {
	*writeWorld
	q              uuid.UUID
	pIssue, qIssue string
}

func seedPersonalWorld(t *testing.T) (*resourceRig, *personalWorld) {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	q, qPrefix := seedProject(t, pool, w.owner, w.space, "pso")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, q)
	})
	pw := &personalWorld{writeWorld: w, q: q, pIssue: w.issue("inside")}
	pw.qIssue = testsupport.SeedIssue(t, pool, q, w.owner, qPrefix+"-1", "outside", "open", 2)
	return rg, pw
}

// notification seeds an unread notification of the owner about issue of
// project, carded to card when card is not "".
func (w *personalWorld) notification(project uuid.UUID, issue, card string) int64 {
	w.t.Helper()
	var event, id int64
	if err := w.pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
VALUES (now(), 'issue.updated', 'issue', $1, $2, 'tester', $3, 'human', '{}'::jsonb) RETURNING id`, issue, project, w.owner).Scan(&event); err != nil {
		w.t.Fatalf("seed event: %v", err)
	}
	if err := w.pool.QueryRow(context.Background(), `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel)
VALUES ($1, $2, 'issue.updated', $3, now(), true) RETURNING id`, w.owner, event, project).Scan(&id); err != nil {
		w.t.Fatalf("seed notification: %v", err)
	}
	if card != "" {
		w.exec(`UPDATE notifications SET card_issue_id = $2, card_resolved = true WHERE id = $1`, id, card)
	}
	return id
}

func (w *personalWorld) read(id int64) bool {
	w.t.Helper()
	var read bool
	if err := w.pool.QueryRow(context.Background(), `SELECT read_at IS NOT NULL FROM notifications WHERE id = $1`, id).Scan(&read); err != nil {
		w.t.Fatalf("read state of %d: %v", id, err)
	}
	return read
}

// filter seeds a subscription filter of the owner.
func (w *personalWorld) filter(name string, f notifications.CustomWatcherFilter) string {
	w.t.Helper()
	raw, _ := json.Marshal(f)
	id := uuid.New()
	w.exec(`INSERT INTO custom_watchers (id, user_id, name, filter) VALUES ($1, $2, $3, $4::jsonb)`, id, w.owner, name+" "+uuid.NewString()[:6], raw)
	return id.String()
}

func (w *personalWorld) filterExists(id string) bool {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM custom_watchers WHERE id = $1`, id).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n == 1
}

// wantSameAnswer asserts that got answered status and code, and exactly as
// the request for a missing id did.
func wantSameAnswer(t *testing.T, what string, got, missing restCall, status int, code string) {
	t.Helper()
	if got.status != status || got.code() != code {
		t.Errorf("%s: %d %s, want %d %s", what, got.status, got.body, status, code)
	}
	if got.status != missing.status || got.code() != missing.code() {
		t.Errorf("%s: %d %s; a missing id: %d %s — must be the same", what, got.status, got.code(), missing.status, missing.code())
	}
}

// TestPersonal_WritesChangeOnlyInsideTheScope — row 98 (SC-022, US-3). Each
// effect of row 29 through the REST operations of a key of scope P. Red when
// an effect touches Q.
func TestPersonal_WritesChangeOnlyInsideTheScope(t *testing.T) {
	rg, w := seedPersonalWorld(t)
	kp := projectsKey(t, w.pool, w.owner, apikeys.LevelWrite, w.project)
	kall := w.key(w.owner, apikeys.LevelWrite)

	t.Run("read all leaves Q's notifications unread", func(t *testing.T) {
		inP, inQ := w.notification(w.project, w.pIssue, ""), w.notification(w.q, w.qIssue, "")
		wantOK(t, "markAllNotificationsRead", rg.rest(kp, http.MethodPost, "/api/v1/notifications/read-all", nil, nil), http.StatusOK)
		if !w.read(inP) {
			t.Errorf("P's notification is still unread")
		}
		if w.read(inQ) {
			t.Errorf("read all by a key of scope P marked Q's notification read")
		}
	})

	t.Run("reading a card with its subtree leaves Q's events unread", func(t *testing.T) {
		child := testsupport.SeedIssue(t, w.pool, w.q, w.owner, w.pIssue+".1", "a child in Q", "open", 2)
		own, fromQ := w.notification(w.project, w.pIssue, w.pIssue), w.notification(w.q, child, w.pIssue)
		wantOK(t, "markWatchingCardRead", rg.rest(kp, http.MethodPost, "/api/v1/me/watching/issues/"+w.pIssue+"/read", nil, nil), http.StatusOK)
		if !w.read(own) {
			t.Errorf("the card's own event is still unread")
		}
		if w.read(fromQ) {
			t.Errorf("reading the card with scope P marked an event of Q's issue read")
		}
	})

	t.Run("subscription filters", func(t *testing.T) {
		inP := w.filter("over P", notifications.CustomWatcherFilter{Projects: []string{w.project.String()}})
		overQ := w.filter("over Q", notifications.CustomWatcherFilter{Projects: []string{w.q.String()}})
		anyProject := w.filter("over any project", notifications.CustomWatcherFilter{})

		list := rg.rest(kp, http.MethodGet, "/api/v1/me/watch-filters", nil, nil)
		wantOK(t, "listWatchFilters", list, http.StatusOK)
		if got := ids(t, list.json(t)["items"], "id"); len(got) != 1 || got[0] != inP {
			t.Errorf("listWatchFilters with scope P = %v, want only %s", got, inP)
		}
		missing := rg.rest(kp, http.MethodDelete, "/api/v1/me/watch-filters/"+uuid.NewString(), nil, nil)
		for name, id := range map[string]string{"over Q": overQ, "over any project": anyProject} {
			got := rg.rest(kp, http.MethodDelete, "/api/v1/me/watch-filters/"+id, nil, nil)
			// The filter of the path: 404 watch_filter_not_found (decision of
			// the lead 01a1c6d3), as for a missing one.
			wantSameAnswer(t, "deleteWatchFilter "+name, got, missing, http.StatusNotFound, "watch_filter_not_found")
			if !w.filterExists(id) {
				t.Errorf("filter %s was deleted by a key of scope P", name)
			}
		}

		createQ := rg.rest(kp, http.MethodPost, "/api/v1/me/watch-filters", map[string]any{"name": "new over Q",
			"filter": map[string]any{"projects": []string{w.q.String()}}}, nil)
		createMissing := rg.rest(kp, http.MethodPost, "/api/v1/me/watch-filters", map[string]any{"name": "new over nothing",
			"filter": map[string]any{"projects": []string{uuid.NewString()}}}, nil)
		wantSameAnswer(t, "createWatchFilter over Q", createQ, createMissing, http.StatusUnprocessableEntity, "project_not_found")
		if c := rg.rest(kp, http.MethodPost, "/api/v1/me/watch-filters", map[string]any{"name": "new over any",
			"filter": map[string]any{}}, nil); c.status != http.StatusForbidden || c.code() != "forbidden" {
			t.Errorf("createWatchFilter over any project with scope P: %d %s, want 403 forbidden", c.status, c.body)
		}
		created := rg.rest(kall, http.MethodPost, "/api/v1/me/watch-filters", map[string]any{"name": "new over any, scope all " + uuid.NewString()[:6],
			"filter": map[string]any{}}, nil)
		wantOK(t, "createWatchFilter over any project with scope all", created, http.StatusCreated)
		var n int
		if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM custom_watchers WHERE user_id = $1 AND name IN ('new over Q', 'new over any', 'new over nothing')`,
			w.owner).Scan(&n); err != nil || n != 0 {
			t.Errorf("refused filters written: %d (%v)", n, err)
		}
	})

	t.Run("reminders on Q are as missing ones", func(t *testing.T) {
		inP, inQ := testsupport.SeedReminder(t, w.pool, w.pIssue, w.owner), testsupport.SeedReminder(t, w.pool, w.qIssue, w.owner)
		w.exec(`UPDATE reminders SET remind_at = now() - interval '1 minute' WHERE id = ANY($1)`, []uuid.UUID{inP, inQ})
		list := rg.rest(kp, http.MethodGet, "/api/v1/reminders", nil, nil)
		wantOK(t, "listReminders", list, http.StatusOK)
		body := list.json(t)
		if got := ids(t, body["items"], "reminderId"); len(got) != 1 || got[0] != inP.String() {
			t.Errorf("listReminders with scope P = %v, want only %s", got, inP)
		}
		if n, _ := body["ringingTaskCount"].(float64); n != 1 {
			t.Errorf("ringingTaskCount with scope P = %v, want 1", body["ringingTaskCount"])
		}
		state := func() (at string, snoozes int) {
			if err := w.pool.QueryRow(context.Background(), `SELECT remind_at::text, snooze_count FROM reminders WHERE id = $1`, inQ).Scan(&at, &snoozes); err != nil {
				t.Fatalf("Q's reminder is gone: %v", err)
			}
			return at, snoozes
		}
		beforeAt, beforeSnoozes := state()
		missing := uuid.NewString()
		snooze := func(id string) restCall {
			return rg.rest(kp, http.MethodPost, "/api/v1/reminders/"+id+"/snooze", map[string]any{"by": "1h"}, nil)
		}
		dismiss := func(id string) restCall { return rg.rest(kp, http.MethodDelete, "/api/v1/reminders/"+id, nil, nil) }
		wantSameAnswer(t, "snoozeReminder of Q's", snooze(inQ.String()), snooze(missing), http.StatusNotFound, "reminder_not_found")
		wantSameAnswer(t, "dismissReminder of Q's", dismiss(inQ.String()), dismiss(missing), http.StatusNotFound, "reminder_not_found")
		if at, snoozes := state(); at != beforeAt || snoozes != beforeSnoozes {
			t.Errorf("Q's reminder changed: remind_at %s → %s, snoozes %d → %d", beforeAt, at, beforeSnoozes, snoozes)
		}
	})
}

// TestPersonal_ListWatchingPagesEachCollection — row 98. listWatching pages
// issues and projects each by its own cursor: limit 1 over two watched
// issues and two watched projects gives one item of each with hasMore and
// a cursor, the cursor of a collection continues that collection only, and
// the pages together are every subscription once. A cursor of the other
// collection's shape is accepted, a foreign one is 400. Red when the
// collections share a cursor or a page repeats or loses a row.
func TestPersonal_ListWatchingPagesEachCollection(t *testing.T) {
	rg, w := seedPersonalWorld(t)
	key := w.key(w.owner, apikeys.LevelRead)
	issues := []string{w.issue("watched a"), w.issue("watched b")}
	for i, id := range issues {
		w.exec(`INSERT INTO issue_watchers (issue_id, user_id, reasons, since) VALUES ($1, $2, ARRAY['manual'], now() - make_interval(mins => $3))
ON CONFLICT (issue_id, user_id) DO UPDATE SET reasons = EXCLUDED.reasons, since = EXCLUDED.since`, id, w.owner, i)
	}
	for i, p := range []uuid.UUID{w.project, w.q} {
		w.exec(`INSERT INTO project_watchers (project_id, user_id, reasons, since) VALUES ($1, $2, ARRAY['manual'], now() - make_interval(mins => $3))
ON CONFLICT (project_id, user_id) DO UPDATE SET reasons = EXCLUDED.reasons, since = EXCLUDED.since`, p, w.owner, i)
	}
	// The owner may watch other issues of the shared fixtures: only the
	// seeded ones are asked about.
	page := func(query string) map[string]any {
		t.Helper()
		c := rg.rest(key, http.MethodGet, "/api/v1/me/watching?"+query, nil, nil)
		wantOK(t, "listWatching "+query, c, http.StatusOK)
		return c.json(t)
	}
	collect := func(name, key string) []string {
		t.Helper()
		out, cursor := []string{}, ""
		for i := 0; i < 20; i++ {
			q := "limit=1"
			if cursor != "" {
				q += "&" + name + "Cursor=" + cursor
			}
			coll := page(q)[name].(map[string]any)
			items, _ := coll["items"].([]any)
			if len(items) > 1 {
				t.Fatalf("%s: a page of %d items at limit 1", name, len(items))
			}
			for _, it := range items {
				out = append(out, it.(map[string]any)[key].(string))
			}
			next, _ := coll["nextCursor"].(string)
			if more, _ := coll["hasMore"].(bool); !more {
				if next != "" {
					t.Errorf("%s: the last page carries a cursor", name)
				}
				return out
			}
			if next == "" {
				t.Fatalf("%s: hasMore without a cursor", name)
			}
			cursor = next
		}
		t.Fatalf("%s: no last page", name)
		return nil
	}
	gotIssues, gotProjects := collect("issues", "issueId"), collect("projects", "projectId")
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"issues", gotIssues, issues},
		{"projects", gotProjects, []string{w.project.String(), w.q.String()}},
	} {
		seen := map[string]int{}
		for _, id := range c.got {
			seen[id]++
		}
		for _, id := range c.want {
			if seen[id] != 1 {
				t.Errorf("%s: %s listed %d times over the pages %v, want once", c.name, id, seen[id], c.got)
			}
		}
	}
	// The first page's issues cursor does not move the projects.
	first := page("limit=1")
	issuesCursor, _ := first["issues"].(map[string]any)["nextCursor"].(string)
	second := page("limit=1&issuesCursor=" + issuesCursor)
	p1 := ids(t, first["projects"].(map[string]any)["items"], "projectId")
	p2 := ids(t, second["projects"].(map[string]any)["items"], "projectId")
	if len(p1) != 1 || len(p2) != 1 || p1[0] != p2[0] {
		t.Errorf("the issues cursor moved the projects: %v then %v", p1, p2)
	}
	if c := rg.rest(key, http.MethodGet, "/api/v1/me/watching?issuesCursor=not-a-cursor", nil, nil); c.status != http.StatusBadRequest || c.code() != "validation_error" {
		t.Errorf("a foreign cursor: %d %s, want 400 validation_error", c.status, c.body)
	}
}

// TestPersonal_ParityWithMCP — row 98 (INV-4). The personal operations asked
// with the same key on REST and on MCP give the same code for the success
// and for the refusals. Red when a code differs from MCP.
func TestPersonal_ParityWithMCP(t *testing.T) {
	rg, w := seedPersonalWorld(t)
	key := w.key(w.owner, apikeys.LevelWrite)
	missingIssue := w.prefix + "-99999"
	for _, c := range []struct {
		name string
		rest func() restCall
		mcp  func() toolAnswer
		want string
	}{
		{"markNotificationRead", func() restCall {
			return rg.rest(key, http.MethodPost, "/api/v1/notifications/"+strconv.FormatInt(w.notification(w.project, w.pIssue, ""), 10)+"/read", nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_notifications", map[string]any{"action": "mark_read", "notificationId": w.notification(w.project, w.pIssue, "")})
		}, ""},
		{"archiveNotification", func() restCall {
			return rg.rest(key, http.MethodPost, "/api/v1/notifications/"+strconv.FormatInt(w.notification(w.project, w.pIssue, ""), 10)+"/archive", nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_notifications", map[string]any{"action": "archive", "notificationId": w.notification(w.project, w.pIssue, "")})
		}, ""},
		{"markIssueNotificationsRead", func() restCall {
			return rg.rest(key, http.MethodPost, issuePath(w.pIssue, "/notifications/read"), nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_notifications", map[string]any{"action": "mark_read_issue", "issueId": w.pIssue})
		}, ""},
		{"watchIssue", func() restCall {
			return rg.rest(key, http.MethodPut, issuePath(w.issue("watch"), "/watch"), map[string]any{"subtree": true}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_watchers", map[string]any{"action": "watch", "issueId": w.issue("watch"), "subtree": true})
		}, ""},
		{"watchIssue of a missing issue", func() restCall {
			return rg.rest(key, http.MethodPut, issuePath(missingIssue, "/watch"), nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_watchers", map[string]any{"action": "watch", "issueId": missingIssue})
		}, "issue_not_found"},
		{"unwatchIssue", func() restCall {
			return rg.rest(key, http.MethodDelete, issuePath(w.issue("unwatch"), "/watch"), nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_watchers", map[string]any{"action": "unwatch", "issueId": w.issue("unwatch")})
		}, ""},
		{"watchProject with an undeliverable event type", func() restCall {
			return rg.rest(key, http.MethodPut, "/api/v1/projects/"+w.project.String()+"/watch", map[string]any{"eventTypes": []string{"nonsense"}}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_watchers", map[string]any{"action": "project_watch", "project": w.project.String(), "eventTypes": []any{"nonsense"}})
		}, "validation_error"},
		{"watchProject", func() restCall {
			return rg.rest(key, http.MethodPut, "/api/v1/projects/"+w.project.String()+"/watch", nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_watchers", map[string]any{"action": "project_watch", "project": w.q.String()})
		}, ""},
		{"unwatchProject", func() restCall {
			return rg.rest(key, http.MethodDelete, "/api/v1/projects/"+w.project.String()+"/watch", nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_watchers", map[string]any{"action": "project_unwatch", "project": w.q.String()})
		}, ""},
		{"createReminder", func() restCall {
			return rg.rest(key, http.MethodPost, issuePath(w.issue("remind"), "/reminders"), map[string]any{"remindAt": "2099-01-01T00:00:00Z"}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_remind", map[string]any{"action": "set", "issueId": w.issue("remind"), "remindAt": "2099-01-01T00:00:00Z"})
		}, ""},
		{"createReminder with a bad time", func() restCall {
			return rg.rest(key, http.MethodPost, issuePath(w.pIssue, "/reminders"), map[string]any{"remindAt": "tomorrow"}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_remind", map[string]any{"action": "set", "issueId": w.pIssue, "remindAt": "tomorrow"})
		}, "validation_error"},
		{"snoozeReminder", func() restCall {
			return rg.rest(key, http.MethodPost, "/api/v1/reminders/"+testsupport.SeedReminder(t, w.pool, w.issue("snooze"), w.owner).String()+"/snooze", map[string]any{"by": "1h"}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_remind", map[string]any{"action": "snooze", "reminderId": testsupport.SeedReminder(t, w.pool, w.issue("snooze"), w.owner).String(), "by": "1h"})
		}, ""},
		{"dismissReminder", func() restCall {
			return rg.rest(key, http.MethodDelete, "/api/v1/reminders/"+testsupport.SeedReminder(t, w.pool, w.issue("dismiss"), w.owner).String(), nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_remind", map[string]any{"action": "dismiss", "reminderId": testsupport.SeedReminder(t, w.pool, w.issue("dismiss"), w.owner).String()})
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rest, mcp := c.rest(), c.mcp()
			parity(t, c.name, rest, mcp, c.want)
			if c.want == "" && (rest.status < 200 || rest.status > 299) {
				t.Errorf("REST answered %d %s", rest.status, rest.body)
			}
		})
	}

	t.Run("listNotifications and the inbox", func(t *testing.T) {
		n := w.notification(w.project, w.pIssue, "")
		got := rg.rest(key, http.MethodGet, "/api/v1/notifications?scope=unread&limit=100", nil, nil)
		wantOK(t, "listNotifications", got, http.StatusOK)
		ref := rg.tool(key, "skein_notifications", map[string]any{"action": "inbox", "scope": "unread", "limit": 100})
		has := func(items any) bool {
			list, _ := items.([]any)
			for _, it := range list {
				if id, _ := it.(map[string]any)["id"].(float64); int64(id) == n {
					return true
				}
			}
			return false
		}
		if !has(got.json(t)["items"]) || !has(ref.json(t)["items"]) {
			t.Errorf("the unread notification %d: REST lists it %v, MCP %v", n, has(got.json(t)["items"]), has(ref.json(t)["items"]))
		}
		if c := rg.rest(key, http.MethodGet, "/api/v1/notifications?limit=101", nil, nil); c.status != http.StatusBadRequest {
			t.Errorf("limit 101: %d, want 400 (no clamping)", c.status)
		}
	})
}

// TestPersonal_ObjectsOfThePath — decision of the lead 01a1c6d3 (spec
// «Codes»: an object of the path that does not exist or is outside the
// key's scope — 404). A notification and a filter of the path that are
// missing, outside the key's scope or malformed answer one 404 with their
// own code, and nothing changes; MCP keeps validation_error for them. Red
// when one of them answers anything else or the codes differ.
func TestPersonal_ObjectsOfThePath(t *testing.T) {
	rg, w := seedPersonalWorld(t)
	kp := projectsKey(t, w.pool, w.owner, apikeys.LevelWrite, w.project)
	inQ := w.notification(w.q, w.qIssue, "")
	overQ := w.filter("over Q", notifications.CustomWatcherFilter{Projects: []string{w.q.String()}})
	for _, c := range []struct {
		name, method, path, code string
	}{
		{"markNotificationRead outside the scope", http.MethodPost, "/api/v1/notifications/" + strconv.FormatInt(inQ, 10) + "/read", "notification_not_found"},
		{"markNotificationRead of a missing one", http.MethodPost, "/api/v1/notifications/999999999/read", "notification_not_found"},
		{"markNotificationRead of a malformed id", http.MethodPost, "/api/v1/notifications/abc/read", "notification_not_found"},
		{"archiveNotification outside the scope", http.MethodPost, "/api/v1/notifications/" + strconv.FormatInt(inQ, 10) + "/archive", "notification_not_found"},
		{"archiveNotification of a missing one", http.MethodPost, "/api/v1/notifications/999999999/archive", "notification_not_found"},
		{"deleteWatchFilter outside the scope", http.MethodDelete, "/api/v1/me/watch-filters/" + overQ, "watch_filter_not_found"},
		{"deleteWatchFilter of a missing one", http.MethodDelete, "/api/v1/me/watch-filters/" + uuid.NewString(), "watch_filter_not_found"},
		{"deleteWatchFilter of a malformed id", http.MethodDelete, "/api/v1/me/watch-filters/abc", "watch_filter_not_found"},
	} {
		if got := rg.rest(kp, c.method, c.path, nil, nil); got.status != http.StatusNotFound || got.code() != c.code {
			t.Errorf("%s: %d %s, want 404 %s", c.name, got.status, got.body, c.code)
		}
	}
	if w.read(inQ) || !w.filterExists(overQ) {
		t.Errorf("a refused request changed Q's state: read %v, filter kept %v", w.read(inQ), w.filterExists(overQ))
	}
	if ref := rg.tool(kp, "skein_notifications", map[string]any{"action": "mark_read", "notificationId": 999999999}); ref.code != "validation_error" {
		t.Errorf("MCP mark_read of a missing notification = %q, want validation_error (unchanged)", ref.code)
	}
}
