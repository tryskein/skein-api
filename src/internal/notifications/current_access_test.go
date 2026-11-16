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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
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

// ----------------- sk-1007 US-1: the marks change only the notification set -----------------
//
// Reference — spec sk-1007 US-1 and SC-005, written before the code: a
// change of state of notification rows acts only on the reader's set (his
// rows of no project or of a project he sees now, inside the key's scope);
// the number it returns is the number of rows of that set it changed; a row
// outside the set is not changed and answers as a row that does not exist.
// Access returned: rows hidden while marks ran are visible again and
// unread.
//
// Corpus (markEnv): A of project P is watched with its subtree. Two rows of
// P and two rows of Q are about A and carded to A (the Q rows stand for
// events of a descendant in Q; they share the entity with the P rows so that
// neither the card nor the entity filter alone can leave them out). Q lives
// in a space of its own, so the reader can lose it through the production
// path of leaving that space.

// markEnv is the corpus above.
type markEnv struct {
	pool         *pgxpool.Pool
	store        *notifications.Store
	owner        uuid.UUID
	reader       uuid.UUID
	readerName   string
	p, q         uuid.UUID
	qSpace       uuid.UUID
	a            string
	pRows, qRows []int64
}

// newMarkEnv seeds the corpus; read writes the four rows read (for unread).
func newMarkEnv(t *testing.T, read bool) *markEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	reader, readerName := testsupport.SeedUser(t, pool, "user")
	qSpace := testsupport.SeedSpace(t, pool, owner)
	p, pPrefix := testsupport.SeedProject(t, pool, owner, "mkp")
	q, _ := testsupport.SeedProjectInSpace(t, pool, owner, qSpace, "mkq")
	testsupport.AddProjectMember(t, pool, p, reader, "member")
	testsupport.AddProjectMember(t, pool, q, reader, "member")
	a := testsupport.SeedIssue(t, pool, p, owner, pPrefix+"-1", "marked", "open", 2)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, scope) VALUES ($1, $2, ARRAY['manual']::text[], 'subtree')`,
		a, reader); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	e := &markEnv{pool: pool, store: notifications.NewStore(pool), owner: owner, reader: reader,
		readerName: readerName, p: p, q: q, qSpace: qSpace, a: a}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id = $1`, reader)
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = ANY($1::uuid[])`, []uuid.UUID{p, q})
	})
	for i := 0; i < 2; i++ {
		e.pRows = append(e.pRows, e.row(t, p, read))
		e.qRows = append(e.qRows, e.row(t, q, read))
	}
	return e
}

// row writes an event about A in project and the reader's feed row of it,
// carded to A.
func (e *markEnv) row(t *testing.T, project uuid.UUID, read bool) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
    VALUES (now(), 'issue.status_changed', 'issue', $3, $2, 'tester', $4, 'human', '{}'::jsonb)
    RETURNING id
)
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_issue_id, card_resolved, read_at)
SELECT $1, ev.id, 'issue.status_changed', $2, true, $3, true, CASE WHEN $5 THEN now() END FROM ev
RETURNING id`, e.reader, project, e.a, e.owner, read).Scan(&id); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	return id
}

// state reads whether the row is read and archived.
func (e *markEnv) state(t *testing.T, id int64) (read, archived bool) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT read_at IS NOT NULL, archived_at IS NOT NULL FROM notifications WHERE id = $1`, id).Scan(&read, &archived); err != nil {
		t.Fatalf("read state of %d: %v", id, err)
	}
	return read, archived
}

// removeFromQ takes the reader out of Q's members.
func (e *markEnv) removeFromQ(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, e.q, e.reader); err != nil {
		t.Fatalf("remove from Q: %v", err)
	}
}

// leaveQSpace makes the reader leave Q's space through spaces.Service.Leave.
func (e *markEnv) leaveQSpace(t *testing.T) {
	t.Helper()
	var short string
	if err := e.pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, e.qSpace).Scan(&short); err != nil {
		t.Fatal(err)
	}
	svc := spaces.NewService(e.pool, spaces.Options{Creation: spaces.CreationAll})
	if err := svc.Leave(context.Background(), spaces.Caller{
		UserID: e.reader, Username: e.readerName,
		Actor: spaces.Actor{UserID: e.reader, Username: e.readerName, Type: "human"},
	}, short); err != nil {
		t.Fatalf("leave Q's space: %v", err)
	}
}

// keyOnP is the reader's request context authenticated by a key whose scope
// is projects=[P]: Q stays visible to him, outside the key.
func (e *markEnv) keyOnP() context.Context {
	ctx := auth.WithUserRole(auth.WithUserID(context.Background(), e.reader), "user")
	ctx = auth.WithAPIKeyID(ctx, uuid.New())
	ctx = auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelManage,
		Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{e.p}}})
	return authz.WithRequestCache(ctx)
}

// markCauses are the two causes of a row outside the set (No. 10): the
// reader does not see Q now — removed from it, or gone from its space — or
// Q lies outside the key's scope.
var markCauses = []struct {
	name string
	lose func(t *testing.T, e *markEnv) context.Context
}{
	{"removed from Q", func(t *testing.T, e *markEnv) context.Context { e.removeFromQ(t); return context.Background() }},
	{"left the space of Q", func(t *testing.T, e *markEnv) context.Context { e.leaveQSpace(t); return context.Background() }},
	{"key scope projects=[P]", func(_ *testing.T, e *markEnv) context.Context { return e.keyOnP() }},
}

// TestCurrentAccess_BulkMarksChangeOnlyTheSet — sk-1007 No. 10, SC-005: read
// the card, read all and read by entity mark and count the two rows of P
// and leave the rows of Q unread.
func TestCurrentAccess_BulkMarksChangeOnlyTheSet(t *testing.T) {
	marks := []struct {
		name string
		run  func(ctx context.Context, e *markEnv) (int64, error)
	}{
		{"MarkReadCard", func(ctx context.Context, e *markEnv) (int64, error) {
			r, err := e.store.MarkReadCard(ctx, e.reader, e.a)
			return int64(r.Marked), err
		}},
		{"MarkAllRead", func(ctx context.Context, e *markEnv) (int64, error) {
			return e.store.MarkAllRead(ctx, notifications.MarkAllReadArgs{UserID: e.reader})
		}},
		{"MarkReadByEntity", func(ctx context.Context, e *markEnv) (int64, error) {
			return e.store.MarkReadByEntity(ctx, e.reader, "issue", e.a)
		}},
	}
	for _, cause := range markCauses {
		for _, m := range marks {
			t.Run(cause.name+"/"+m.name, func(t *testing.T) {
				e := newMarkEnv(t, false)
				ctx := cause.lose(t, e)
				n, err := m.run(ctx, e)
				if err != nil {
					t.Fatalf("%s: %v", m.name, err)
				}
				if n != 2 {
					t.Errorf("%s returned %d, want 2 (the rows of P)", m.name, n)
				}
				for _, id := range e.pRows {
					if read, _ := e.state(t, id); !read {
						t.Errorf("row %d of P is unread after %s (control)", id, m.name)
					}
				}
				for _, id := range e.qRows {
					if read, archived := e.state(t, id); read || archived {
						t.Errorf("row %d of Q changed by %s: read %v archived %v, want unread, not archived", id, m.name, read, archived)
					}
				}
			})
		}
	}
}

// TestCurrentAccess_SingleMarksTreatARowOutsideTheSetAsMissing — sk-1007
// No. 10: mark read, mark unread and archive of a row of Q answer
// ErrNotFound, like an id that does not exist, and leave it unchanged; the
// same mark of a row of P succeeds (control).
func TestCurrentAccess_SingleMarksTreatARowOutsideTheSetAsMissing(t *testing.T) {
	const missing = int64(1) << 62
	marks := []struct {
		name string
		read bool // the corpus is seeded read (for MarkUnread)
		run  func(ctx context.Context, e *markEnv, id int64) error
		// changed reports whether the mark moved the row's state.
		changed func(read, archived bool) bool
	}{
		{"MarkRead", false, func(ctx context.Context, e *markEnv, id int64) error { return e.store.MarkRead(ctx, id, e.reader) },
			func(read, _ bool) bool { return read }},
		{"MarkUnread", true, func(ctx context.Context, e *markEnv, id int64) error { return e.store.MarkUnread(ctx, id, e.reader) },
			func(read, _ bool) bool { return !read }},
		{"Archive", false, func(ctx context.Context, e *markEnv, id int64) error { return e.store.Archive(ctx, id, e.reader) },
			func(read, archived bool) bool { return read || archived }},
	}
	for _, cause := range markCauses {
		for _, m := range marks {
			t.Run(cause.name+"/"+m.name, func(t *testing.T) {
				e := newMarkEnv(t, m.read)
				ctx := cause.lose(t, e)
				if err := m.run(ctx, e, missing); !errors.Is(err, notifications.ErrNotFound) {
					t.Fatalf("%s of a missing id = %v, want ErrNotFound (the reference answer)", m.name, err)
				}
				q := e.qRows[0]
				if err := m.run(ctx, e, q); !errors.Is(err, notifications.ErrNotFound) {
					t.Errorf("%s of row %d of Q = %v, want ErrNotFound like a missing id", m.name, q, err)
				}
				if read, archived := e.state(t, q); m.changed(read, archived) {
					t.Errorf("row %d of Q changed by %s: read %v archived %v", q, m.name, read, archived)
				}
				p := e.pRows[0]
				if err := m.run(ctx, e, p); err != nil {
					t.Fatalf("%s of row %d of P = %v, want nil (control)", m.name, p, err)
				}
				if read, archived := e.state(t, p); !m.changed(read, archived) {
					t.Errorf("row %d of P not changed by %s (control)", p, m.name)
				}
			})
		}
	}
}

// TestCurrentAccess_RowsReadWithoutAccessComeBackUnread — sk-1007 No. 11,
// US-1 "access returned": without access to Q the reader reads the card A
// and reads all; when access is returned the rows of Q are on the card
// again, counted and in its timeline, and still unread.
func TestCurrentAccess_RowsReadWithoutAccessComeBackUnread(t *testing.T) {
	for _, loss := range []struct {
		name string
		lose func(e *markEnv, t *testing.T)
	}{
		{"removed from Q", (*markEnv).removeFromQ},
		{"left the space of Q", (*markEnv).leaveQSpace},
	} {
		t.Run(loss.name, func(t *testing.T) {
			e := newMarkEnv(t, false)
			ctx := context.Background()
			loss.lose(e, t)
			if _, err := e.store.MarkReadCard(ctx, e.reader, e.a); err != nil {
				t.Fatalf("MarkReadCard: %v", err)
			}
			if _, err := e.store.MarkAllRead(ctx, notifications.MarkAllReadArgs{UserID: e.reader}); err != nil {
				t.Fatalf("MarkAllRead: %v", err)
			}

			testsupport.AddProjectMember(t, e.pool, e.q, e.reader, "member")

			for _, id := range e.qRows {
				if read, _ := e.state(t, id); read {
					t.Errorf("row %d of Q is read after access returned, want unread", id)
				}
			}
			res, err := e.store.ListWatchingFeed(ctx, e.reader, notifications.WatchingStateAll)
			if err != nil {
				t.Fatalf("ListWatchingFeed: %v", err)
			}
			if len(res.Rows) != 1 || res.Rows[0].IssueID != e.a || res.Rows[0].UnreadEventsCount != 2 || res.UnreadTotal != 2 {
				t.Errorf("column = %+v (unreadTotal %d), want card %s with the 2 unread rows of Q", res.Rows, res.UnreadTotal, e.a)
			}
			recent, err := e.store.HydrateWatchingRecentEvents(ctx, e.reader, []string{e.a})
			if err != nil {
				t.Fatalf("HydrateWatchingRecentEvents: %v", err)
			}
			seen := map[int64]bool{}
			for _, n := range recent[e.a] {
				seen[n.ID] = true
			}
			for _, id := range e.qRows {
				if !seen[id] {
					t.Errorf("row %d of Q missing from the timeline of %s after access returned", id, e.a)
				}
			}
		})
	}
}
