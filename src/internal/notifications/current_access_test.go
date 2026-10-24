package notifications_test

// current_access_test.go — sk-674.18, the test delivery of row 128 (T029,
// INV-11, SC-010). Reference — the invariant and the success criterion as
// the spec words them, written before the code:
//
//   - INV-11: a notification, a watch, a mention, a reminder is given out
//     (as a row, a number or a text) only while the recipient has access to
//     the issue's project NOW;
//   - SC-010: after leaving a space the person sees no notification, watch,
//     mention, issue or project of that space and is the assignee of no
//     unclosed issue in it.
//
// Fixture: testsupport.SeedTenancy. both is a participant of A and B and a
// member of A.members; he holds, in A.space, A.members and B.space, a feed
// notification carded for Watching, a mention, a watch and a reminder, and
// is the unaccepted assignee of one open issue in each. Before leaving A
// every row is given out (the positive control); after he leaves space A
// through the production path (spaces.Service.Leave) the rows of A are
// given out by no surface — Inbox, Mentions, Watching, reminders — and
// counted by no badge of notification-counts, while B's stay.
//
// Row 29 of the isolation matrix holds the static cut (projects an actor
// never saw); this row holds the transition: the same person, the same
// rows, before and after the loss.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

func TestCurrentAccess_LeavingSpaceEmptiesItsQueues(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	f := testsupport.SeedTenancy(t, pool)
	leaver := f.User(t, testsupport.ActorBoth)
	lost := []testsupport.TenancyProject{f.ASpace, f.AMembers}
	kept := f.BSpace
	all := append(append([]testsupport.TenancyProject{}, lost...), kept)
	t.Cleanup(func() {
		for _, p := range all {
			_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE project_id = $1`, p.ID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, p.ID)
		}
	})

	for _, p := range all {
		for _, typ := range []string{"issue.status_changed", "issue.mention"} {
			var ev int64
			if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id, payload)
VALUES (now(), $1, 'current-access', $2, 'human', 'issue', $3, $4, '{}'::jsonb) RETURNING id`,
				typ, p.OwnerID, p.IssueID, p.ID).Scan(&ev); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_issue_id)
VALUES ($1, $2, $3, $4, true, CASE WHEN $3 = 'issue.status_changed' THEN $5 END)`,
				leaver.ID, ev, typ, p.ID, p.IssueID); err != nil {
				t.Fatalf("seed notification: %v", err)
			}
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual']::text[], now()) ON CONFLICT DO NOTHING`, p.IssueID, leaver.ID); err != nil {
			t.Fatalf("seed watch: %v", err)
		}
		testsupport.SeedReminder(t, pool, p.IssueID, leaver.ID)
		testsupport.SetAssignee(t, pool, p.IssueID, leaver.Username)
	}

	issuesStore := issues.NewStore(pool)
	r := chi.NewRouter()
	r.Use(handler.ParseUserHeaders)
	r.Use(handler.RequireUser)
	notifH := handler.NewNotificationsHandler(pool)
	meH := handler.NewMeHandler(pool, issuesStore, notifications.NewStore(pool))
	remH := handler.NewRemindersHandler(pool, reminders.NewStore(pool), issuesStore, projects.NewStore(pool))
	r.Get("/api/v1/notifications", notifH.ListNotifications)
	r.Get("/api/v1/me/watching", meH.GetWatching)
	r.Get("/api/v1/me/notification-counts", meH.NotificationCounts)
	r.Get("/api/v1/reminders", remH.List)
	get := func(t *testing.T, path string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(auth.HeaderUserID, leaver.ID.String())
		req.Header.Set(auth.HeaderUsername, leaver.Username)
		req.Header.Set(auth.HeaderUserRole, leaver.Role)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	surfaces := map[string]string{
		"inbox":     "/api/v1/notifications?scope=all&limit=100",
		"mentions":  "/api/v1/notifications?scope=mentions&limit=100",
		"watching":  "/api/v1/me/watching",
		"reminders": "/api/v1/reminders?state=all",
	}
	type counts struct {
		Inbox    int `json:"inbox"`
		Mentions int `json:"mentions"`
		Watching int `json:"watching"`
	}
	readCounts := func(t *testing.T) counts {
		t.Helper()
		var c counts
		if err := json.Unmarshal(get(t, "/api/v1/me/notification-counts"), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	tokens := func(p testsupport.TenancyProject) []string {
		return []string{`"` + p.IssueID + `"`, p.ID.String(), p.Name}
	}

	// Positive control: every row is given out while he has access.
	for name, path := range surfaces {
		body := get(t, path)
		for _, p := range all {
			if !bytes.Contains(body, []byte(`"`+p.IssueID+`"`)) {
				t.Fatalf("before leaving, %s misses the row of %s — the fixture proves nothing", name, p.Key())
			}
		}
	}
	before := readCounts(t)
	if before.Mentions < len(all) || before.Watching < len(all) || before.Inbox < len(all) {
		t.Fatalf("before leaving, the badges %+v count fewer than the %d seeded projects", before, len(all))
	}

	svc := spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll})
	var short string
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, f.SpaceA).Scan(&short); err != nil {
		t.Fatal(err)
	}
	if err := svc.Leave(ctx, spaces.Caller{
		UserID: leaver.ID, Username: leaver.Username,
		Actor: spaces.Actor{UserID: leaver.ID, Username: leaver.Username, Type: "human"},
	}, short); err != nil {
		t.Fatalf("leave space A: %v", err)
	}

	for name, path := range surfaces {
		t.Run(name, func(t *testing.T) {
			body := get(t, path)
			for _, p := range lost {
				for _, tok := range tokens(p) {
					if bytes.Contains(body, []byte(tok)) {
						t.Errorf("after leaving, %s gives out %q of %s", name, tok, p.Key())
					}
				}
			}
			if !bytes.Contains(body, []byte(`"`+kept.IssueID+`"`)) {
				t.Errorf("after leaving A, %s lost the row of B (control)", name)
			}
		})
	}
	t.Run("notification-counts", func(t *testing.T) {
		after := readCounts(t)
		want := counts{
			Inbox:    before.Inbox - len(lost),
			Mentions: before.Mentions - len(lost),
			Watching: before.Watching - len(lost),
		}
		if after != want {
			t.Errorf("badges after leaving %+v, want %+v (before %+v minus the %d projects of A)", after, want, before, len(lost))
		}
	})
	t.Run("assignee of no unclosed issue of the space", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
 WHERE p.space_id = $1 AND i.status <> 'closed' AND lower(i.assignee) = lower($2)`,
			f.SpaceA, leaver.Username).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("after leaving, %d unclosed issue(s) of space A still name him as assignee", n)
		}
	})
}
