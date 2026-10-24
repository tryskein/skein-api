package notifications_test

// signal_recipients_test.go — sk-622.21, test matrix row 34 (T016; US-11,
// "Personal signal matrix": "Feed notification written", "Read /
// unread / archive / read all / read by issue" — REST and MCP —
// and "Subscribe / unsubscribe / 'remove' / bulk unsubscribe").
//
// Subject: the personal signal the real write paths publish after commit,
// observed on `skein.user.*.signal` through an in-process NATS server.
//
// Reference: spec sk-622.21 "Personal signal matrix" — the recipient of a
// feed notification gets `watching` when the row has a Watching card and
// `mentions` when it is a mention; the owner of the rows a read / archive
// touched gets `watching` and/or `mentions` by those rows; the user whose
// subscription or cards changed gets `watching`. Anybody else gets nothing.
//
// Absence is judged after a SENTINEL transaction whose message, published on
// the same connection after the action's, proves the action's messages are in.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/notifications"
	"skein-api/internal/signals"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------- NATS sink

type sigMsg struct {
	user  uuid.UUID
	areas []string
}

type sigSink struct {
	mu     sync.Mutex
	msgs   []sigMsg
	notify chan struct{}
}

func startSigSink(t *testing.T) *sigSink {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.NoLog = true
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	pub, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(pub.Close)
	sub, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	t.Cleanup(sub.Close)

	s := &sigSink{notify: make(chan struct{}, 1)}
	if _, err := sub.Subscribe(signals.SignalSubjectPattern, func(m *nats.Msg) {
		uid, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(m.Subject, "skein.user."), ".signal"))
		if err != nil {
			t.Errorf("subject %q carries no user id: %v", m.Subject, err)
			return
		}
		var body struct {
			Areas []string `json:"areas"`
		}
		if err := json.Unmarshal(m.Data, &body); err != nil {
			t.Errorf("signal body %s: %v", m.Data, err)
			return
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, sigMsg{user: uid, areas: body.Areas})
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := sub.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	signals.Configure(pub)
	t.Cleanup(func() { signals.Configure(nil) })
	return s
}

func (s *sigSink) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// settle commits a sentinel and returns the messages received since from,
// once the sentinel's own message is in (sentinel excluded).
func (s *sigSink) settle(t *testing.T, pool *pgxpool.Pool, from int) []sigMsg {
	t.Helper()
	sentinel := uuid.New()
	if err := signals.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		signals.Add(tx, sentinel, signals.AreaInbox)
		_, err := tx.Exec(context.Background(), `SELECT 1`)
		return err
	}); err != nil {
		t.Fatalf("sentinel transaction: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		for i := from; i < len(s.msgs); i++ {
			if s.msgs[i].user == sentinel {
				out := append([]sigMsg(nil), s.msgs[from:i]...)
				s.mu.Unlock()
				return out
			}
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-deadline:
			t.Fatal("sentinel signal never arrived")
		}
	}
}

func union(msgs []sigMsg) map[uuid.UUID]map[string]bool {
	out := make(map[uuid.UUID]map[string]bool)
	for _, m := range msgs {
		if out[m.user] == nil {
			out[m.user] = make(map[string]bool)
		}
		for _, a := range m.areas {
			out[m.user][a] = true
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- fixture

// world: the project owner acts, the reader owns notifications and watches
// the issue, the mentioned user is named in a comment, the bystander is a
// member with nothing to do with the action.
type world struct {
	pool   *pgxpool.Pool
	store  *notifications.Store
	pid    uuid.UUID
	issue  string
	ids    map[string]uuid.UUID
	names  map[string]string
	prefix string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	w := &world{pool: pool, store: notifications.NewStore(pool), ids: map[string]uuid.UUID{}, names: map[string]string{}}
	for _, p := range []string{"owner", "reader", "mentioned", "bystander"} {
		id, name := testsupport.SeedUser(t, pool, "")
		w.ids[p], w.names[p] = id, name
	}
	w.pid, w.prefix = testsupport.SeedProject(t, pool, w.ids["owner"], "signtf")
	// The project's events — and with them their notifications, which owe
	// pushes to the shared push queue — go with the fixture, so they do not
	// sit in the queue ahead of other packages' rows.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, w.pid)
	})
	for _, p := range []string{"reader", "mentioned", "bystander"} {
		testsupport.AddProjectMember(t, pool, w.pid, w.ids[p], "member")
	}
	w.issue = testsupport.SeedIssue(t, pool, w.pid, w.ids["owner"], w.prefix+"-1", "signal issue", "open", 2)
	return w
}

// seedRow writes one feed notification of eventType for the reader, with a
// Watching card on the issue when carded.
func (w *world) seedRow(t *testing.T, eventType string, carded bool) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID int64
	if err := w.pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
VALUES (now(), $1, 'issue', $2, $3, 'tester', $4, 'human', '{}'::jsonb)
RETURNING id`, eventType, w.issue, w.pid, w.ids["owner"]).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, eventID) })
	var card any
	if carded {
		card = w.issue
	}
	var id int64
	if err := w.pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel, card_issue_id, card_resolved)
VALUES ($1, $2, $3, $4, now(), true, $5, true)
RETURNING id`, w.ids["reader"], eventID, eventType, w.pid, card).Scan(&id); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return id
}

// rest calls one notifications REST route as the reader.
func (w *world) rest(t *testing.T, method, path string, body string) {
	t.Helper()
	h := handler.NewNotificationsHandler(w.pool)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			ctx := auth.WithUserID(req.Context(), w.ids["reader"])
			ctx = auth.WithUsername(ctx, w.names["reader"])
			ctx = auth.WithUserRole(ctx, "user")
			next.ServeHTTP(rw, req.WithContext(ctx))
		})
	})
	r.Post("/notifications/mark-all-read", h.MarkAllRead)
	r.Post("/notifications/mark-read-by-entity", h.MarkReadByEntity)
	r.Post("/notifications/{id}/read", h.MarkRead)
	r.Post("/notifications/{id}/unread", h.MarkUnread)
	r.Post("/notifications/{id}/archive", h.Archive)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	if rec.Code >= 300 {
		t.Fatalf("%s %s → %d %s", method, path, rec.Code, rec.Body.String())
	}
}

// mcp calls skein_notifications as the reader through a real MCP server.
func (w *world) mcp(t *testing.T, args map[string]any) {
	t.Helper()
	s := server.NewMCPServer("signal-test", "1", server.WithToolCapabilities(false))
	tools.RegisterNotifications(s, w.pool, w.store)
	ctx := auth.WithUserID(context.Background(), w.ids["reader"])
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithSessionID(ctx, "mcp-session-"+uuid.NewString())
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "skein_notifications", "arguments": args},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := json.Marshal(s.HandleMessage(ctx, msg))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var resp struct {
		Error  any `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if resp.Error != nil || resp.Result.IsError {
		t.Fatalf("skein_notifications %v failed: %s", args, out)
	}
}

func (w *world) comment(t *testing.T, body string) {
	t.Helper()
	st := issues.NewStore(w.pool)
	if _, err := st.AddComment(t.Context(), w.issue, body, w.ids["owner"], w.names["owner"], "human", "human",
		issues.HumanActor(w.ids["owner"], w.names["owner"])); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
}

func (w *world) watch(t *testing.T, who string) {
	t.Helper()
	if err := w.store.AddIssueWatch(t.Context(), w.issue, w.ids[who], false); err != nil {
		t.Fatalf("watch: %v", err)
	}
}

// autoWatch places an automatic subscription (creator, assignee, …) the way
// the issue write paths do: AddIssueWatcher inside a collecting transaction.
func (w *world) autoWatch(t *testing.T, who, reason string) {
	t.Helper()
	if err := signals.BeginTxFunc(t.Context(), w.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		return notifications.AddIssueWatcher(t.Context(), tx, w.issue, w.ids[who], reason)
	}); err != nil {
		t.Fatalf("auto watch (%s): %v", reason, err)
	}
}

// ---------------------------------------------------------------- table

type row struct {
	name   string
	setup  func(t *testing.T, w *world) int64
	act    func(t *testing.T, w *world, n int64)
	want   map[string][]string
	silent []string
}

// TestSignalRecipients_NotificationsAndSubscriptions — matrix row 34.
func TestSignalRecipients_NotificationsAndSubscriptions(t *testing.T) {
	carded := func(t *testing.T, w *world) int64 { return w.seedRow(t, "comment.created", true) }
	mention := func(t *testing.T, w *world) int64 { return w.seedRow(t, "comment.mention", false) }
	path := func(n int64, op string) string { return "/notifications/" + strconv.FormatInt(n, 10) + "/" + op }

	rows := []row{
		{
			name: "Feed notification written: card",
			setup: func(t *testing.T, w *world) int64 {
				w.watch(t, "reader")
				// The "Comment" reason's feed is off by default; the
				// reader switches it on so a feed row is written.
				if _, err := w.pool.Exec(t.Context(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled, updated_at)
VALUES ($1, $2, true, false, now())`, w.ids["reader"], string(notifications.ReasonKeyComment)); err != nil {
					t.Fatalf("enable comment feed: %v", err)
				}
				return 0
			},
			act:    func(t *testing.T, w *world, _ int64) { w.comment(t, "a plain comment") },
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name: "Feed notification written: mention",
			act: func(t *testing.T, w *world, _ int64) {
				w.comment(t, "look here @"+w.names["mentioned"])
			},
			want:   map[string][]string{"mentioned": {"mentions"}},
			silent: []string{"reader", "bystander", "owner"},
		},
		{
			name:   "Read (REST)",
			setup:  carded,
			act:    func(t *testing.T, w *world, n int64) { w.rest(t, http.MethodPost, path(n, "read"), "") },
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name:   "Mention read (REST)",
			setup:  mention,
			act:    func(t *testing.T, w *world, n int64) { w.rest(t, http.MethodPost, path(n, "read"), "") },
			want:   map[string][]string{"reader": {"mentions"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name: "Unread (REST)",
			setup: func(t *testing.T, w *world) int64 {
				n := carded(t, w)
				if err := w.store.MarkRead(t.Context(), n, w.ids["reader"]); err != nil {
					t.Fatalf("MarkRead: %v", err)
				}
				return n
			},
			act:    func(t *testing.T, w *world, n int64) { w.rest(t, http.MethodPost, path(n, "unread"), "") },
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			// Found in review: a repeated mark that moves
			// nothing answers as before (rest() fails on a non-2xx) and
			// signals nothing.
			name: "Repeated read (REST) — no change",
			setup: func(t *testing.T, w *world) int64 {
				n := carded(t, w)
				if err := w.store.MarkRead(t.Context(), n, w.ids["reader"]); err != nil {
					t.Fatalf("MarkRead: %v", err)
				}
				return n
			},
			act:    func(t *testing.T, w *world, n int64) { w.rest(t, http.MethodPost, path(n, "read"), "") },
			silent: []string{"reader", "mentioned", "bystander", "owner"},
		},
		{
			name:   "Unread of already unread (REST) — no change",
			setup:  carded,
			act:    func(t *testing.T, w *world, n int64) { w.rest(t, http.MethodPost, path(n, "unread"), "") },
			silent: []string{"reader", "mentioned", "bystander", "owner"},
		},
		{
			name: "Repeated archive (MCP) — no change",
			setup: func(t *testing.T, w *world) int64 {
				n := mention(t, w)
				if err := w.store.Archive(t.Context(), n, w.ids["reader"]); err != nil {
					t.Fatalf("Archive: %v", err)
				}
				return n
			},
			act: func(t *testing.T, w *world, n int64) {
				w.mcp(t, map[string]any{"action": "archive", "notificationId": float64(n)})
			},
			silent: []string{"reader", "mentioned", "bystander", "owner"},
		},
		{
			name:   "Archive (REST)",
			setup:  carded,
			act:    func(t *testing.T, w *world, n int64) { w.rest(t, http.MethodPost, path(n, "archive"), "") },
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name: "Read all (REST)",
			setup: func(t *testing.T, w *world) int64 {
				carded(t, w)
				return mention(t, w)
			},
			act: func(t *testing.T, w *world, _ int64) {
				w.rest(t, http.MethodPost, "/notifications/mark-all-read", "{}")
			},
			want:   map[string][]string{"reader": {"watching", "mentions"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name:  "Read by issue (REST)",
			setup: carded,
			act: func(t *testing.T, w *world, _ int64) {
				w.rest(t, http.MethodPost, "/notifications/mark-read-by-entity",
					`{"entityType":"issue","entityId":"`+w.issue+`"}`)
			},
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name:  "Read (MCP)",
			setup: carded,
			act: func(t *testing.T, w *world, n int64) {
				w.mcp(t, map[string]any{"action": "mark_read", "notificationId": float64(n)})
			},
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name:  "Archive (MCP)",
			setup: mention,
			act: func(t *testing.T, w *world, n int64) {
				w.mcp(t, map[string]any{"action": "archive", "notificationId": float64(n)})
			},
			want:   map[string][]string{"reader": {"mentions"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name:  "Read by issue (MCP)",
			setup: carded,
			act: func(t *testing.T, w *world, _ int64) {
				w.mcp(t, map[string]any{"action": "mark_read_issue", "issueId": w.issue})
			},
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name:   "Read all (MCP)",
			setup:  mention,
			act:    func(t *testing.T, w *world, _ int64) { w.mcp(t, map[string]any{"action": "mark_read_all"}) },
			want:   map[string][]string{"reader": {"mentions"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name:   "Subscribe",
			act:    func(t *testing.T, w *world, _ int64) { w.watch(t, "reader") },
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			// Found in review: a repeat that changes no
			// reason, since or scope is no change and no signal.
			name: "Repeated manual subscription — no change",
			setup: func(t *testing.T, w *world) int64 {
				w.watch(t, "reader")
				return 0
			},
			act:    func(t *testing.T, w *world, _ int64) { w.watch(t, "reader") },
			silent: []string{"reader", "mentioned", "bystander", "owner"},
		},
		{
			name: "Repeated auto-subscription by the same reason — no change",
			setup: func(t *testing.T, w *world) int64 {
				w.autoWatch(t, "reader", notifications.ReasonAssignee)
				return 0
			},
			act:    func(t *testing.T, w *world, _ int64) { w.autoWatch(t, "reader", notifications.ReasonAssignee) },
			silent: []string{"reader", "mentioned", "bystander", "owner"},
		},
		{
			name: "Auto-subscription by a new reason — change",
			setup: func(t *testing.T, w *world) int64 {
				w.autoWatch(t, "reader", notifications.ReasonAssignee)
				return 0
			},
			act:    func(t *testing.T, w *world, _ int64) { w.autoWatch(t, "reader", notifications.ReasonCreator) },
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name: "Widening the subscription scope to the subtree — change",
			setup: func(t *testing.T, w *world) int64 {
				w.watch(t, "reader")
				return 0
			},
			act: func(t *testing.T, w *world, _ int64) {
				if err := w.store.AddIssueWatch(t.Context(), w.issue, w.ids["reader"], true); err != nil {
					t.Fatalf("subtree watch: %v", err)
				}
			},
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name: "Unsubscribe",
			setup: func(t *testing.T, w *world) int64 {
				w.watch(t, "reader")
				return 0
			},
			act: func(t *testing.T, w *world, _ int64) {
				if err := w.store.RemoveIssueWatch(t.Context(), w.issue, w.ids["reader"]); err != nil {
					t.Fatalf("RemoveIssueWatch: %v", err)
				}
			},
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			// "Remove" historical card: no subscription, only a card.
			name:  "Remove the card",
			setup: carded,
			act: func(t *testing.T, w *world, _ int64) {
				if err := w.store.RemoveIssueWatch(t.Context(), w.issue, w.ids["reader"]); err != nil {
					t.Fatalf("RemoveIssueWatch: %v", err)
				}
			},
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
		{
			name: "Bulk unsubscribe",
			setup: func(t *testing.T, w *world) int64 {
				w.watch(t, "reader")
				return 0
			},
			act: func(t *testing.T, w *world, _ int64) {
				if _, err := w.store.BulkUnwatch(t.Context(), w.ids["reader"], notifications.BulkUnwatchInput{IssueIDs: []string{w.issue}}); err != nil {
					t.Fatalf("BulkUnwatch: %v", err)
				}
			},
			want:   map[string][]string{"reader": {"watching"}},
			silent: []string{"mentioned", "bystander", "owner"},
		},
	}

	for _, rw := range rows {
		t.Run(rw.name, func(t *testing.T) {
			w := newWorld(t)
			sink := startSigSink(t)
			var n int64
			if rw.setup != nil {
				n = rw.setup(t, w)
			}
			sink.settle(t, w.pool, sink.mark())
			from := sink.mark()
			rw.act(t, w, n)
			got := union(sink.settle(t, w.pool, from))

			for who, areas := range rw.want {
				for _, a := range areas {
					if !got[w.ids[who]][a] {
						t.Errorf("%s: got areas %v, want %q among them", who, keys(got[w.ids[who]]), a)
					}
				}
			}
			for _, who := range rw.silent {
				if g := got[w.ids[who]]; len(g) != 0 {
					t.Errorf("%s is not an addressee of this row, got areas %v", who, keys(g))
				}
			}
		})
	}
}
