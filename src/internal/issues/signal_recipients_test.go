package issues_test

// signal_recipients_test.go — sk-622.21, test matrix rows 31, 32, 33 (T015;
// US-11, the issue write paths of "Personal signal matrix").
//
// Subject: the personal signal the real issue write paths publish after
// commit, observed on `skein.user.*.signal` through an in-process NATS server
// — the subject skein-web consumes.
//
// Reference: spec sk-622.21 "Personal signal matrix" (roles and areas per
// action), US-11 ("the signal is sent regardless of whether the
// notification was written"; review data stays private), plan K13 (previous assignees =
// previous_assignees minus dismissed_by; subscribers = the issue's watchers
// and the subtree watchers of its canonical ancestors), edge 14.
//
// Each table row is one row of the matrix: an action on a fresh fixture, the
// areas each addressee must have received (`want`), areas an addressee must
// NOT have received (`forbid`), and the people who must have received nothing
// at all (`silent`). "Received" is the union of every message a user got for
// the action. Absence is judged after a SENTINEL: a transaction committed
// after the action names a fresh user, and once its message is in, every
// message the action published is in too (one publisher connection, order
// preserved).

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/issues/lockreap"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/notifications"
	"skein-api/internal/signals"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
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

// mark returns the position the next action's messages start at.
func (s *sigSink) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// settle commits a sentinel transaction and waits for its message, then
// returns every message received since from (sentinel excluded).
func (s *sigSink) settle(t *testing.T, pool *pgxpool.Pool, from int) []sigMsg {
	t.Helper()
	sentinel := uuid.New()
	err := signals.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		signals.Add(tx, sentinel, signals.AreaInbox)
		_, err := tx.Exec(context.Background(), `SELECT 1`)
		return err
	})
	if err != nil {
		t.Fatalf("sentinel transaction: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		for i := from; i < len(s.msgs); i++ {
			if s.msgs[i].user == sentinel {
				out := make([]sigMsg, 0, i-from)
				out = append(out, s.msgs[from:i]...)
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

// union folds messages into user → set of areas, and counts messages per user.
func union(msgs []sigMsg) (map[uuid.UUID]map[string]bool, map[uuid.UUID]int) {
	areas := make(map[uuid.UUID]map[string]bool)
	count := make(map[uuid.UUID]int)
	for _, m := range msgs {
		if areas[m.user] == nil {
			areas[m.user] = make(map[string]bool)
		}
		for _, a := range m.areas {
			areas[m.user][a] = true
		}
		count[m.user]++
	}
	return areas, count
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

// world is one project with named people. Every row builds its own.
type world struct {
	pool    *pgxpool.Pool
	store   *issues.Store
	notif   *notifications.Store
	pid     uuid.UUID
	prefix  string
	ids     map[string]uuid.UUID
	names   map[string]string
	version map[string]int
}

// People of the matrix: author, assignee, other (a new assignee), previous
// (a previous assignee who did not dismiss), dismissed (a previous assignee
// who dismissed), subscriber (a manual watcher), bystander (member with no
// role).
var people = []string{"author", "assignee", "other", "previous", "dismissed", "subscriber", "bystander"}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	w := &world{
		pool:    pool,
		store:   issues.NewStore(pool),
		notif:   notifications.NewStore(pool),
		ids:     map[string]uuid.UUID{},
		names:   map[string]string{},
		version: map[string]int{},
	}
	for _, p := range people {
		id, name := testsupport.SeedUser(t, pool, "")
		w.ids[p], w.names[p] = id, name
	}
	w.pid, w.prefix = testsupport.SeedProject(t, pool, w.ids["author"], "sigrcp")
	// The project's events — and with them their notifications, which owe
	// pushes to the shared push queue — go with the fixture, so they do not
	// sit in the queue ahead of other packages' rows.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, w.pid)
	})
	for _, p := range people[1:] {
		testsupport.AddProjectMember(t, pool, w.pid, w.ids[p], "member")
	}
	return w
}

func (w *world) human(p string) issues.Actor { return issues.HumanActor(w.ids[p], w.names[p]) }

func (w *world) agent(p string) issues.Actor {
	return issues.Actor{UserID: w.ids[p], Username: w.names[p], Role: "agent", SessionID: "mcp-session-" + uuid.NewString()}
}

func ptr[T any](v T) *T { return &v }

// create files an issue by the author, assigned to assignee (empty = none).
func (w *world) create(t *testing.T, assignee string, parent *string) string {
	t.Helper()
	in := issues.CreateIssueInput{Title: "signal " + uuid.NewString()[:8], Type: "task", Priority: 2, ParentID: parent}
	if assignee != "" {
		in.Assignee = ptr(w.names[assignee])
	}
	d, err := w.store.CreateIssue(t.Context(), w.pid, w.prefix, "counter", in, w.ids["author"], w.human("author"))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	return d.ID
}

func (w *world) ver(t *testing.T, id string) int {
	t.Helper()
	var v int
	if err := w.pool.QueryRow(t.Context(), `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version of %s: %v", id, err)
	}
	return v
}

func (w *world) patch(t *testing.T, id string, by issues.Actor, in issues.PatchIssueInput) {
	t.Helper()
	in.ExpectedVersion = w.ver(t, id)
	if _, err := w.store.PatchIssue(t.Context(), id, in, by); err != nil {
		t.Fatalf("PatchIssue %s: %v", id, err)
	}
}

func (w *world) status(t *testing.T, id string, by issues.Actor, status string) {
	t.Helper()
	if _, err := w.store.UpdateStatus(t.Context(), id, issues.UpdateStatusInput{ExpectedVersion: w.ver(t, id), Status: status}, by); err != nil {
		t.Fatalf("UpdateStatus %s → %s: %v", id, status, err)
	}
}

func (w *world) close(t *testing.T, id string) {
	t.Helper()
	if _, err := w.store.Close(t.Context(), issues.CloseInput{ID: id, ExpectedVersion: w.ver(t, id), Reason: "done", Actor: w.human("author")}); err != nil {
		t.Fatalf("Close %s: %v", id, err)
	}
}

func (w *world) watch(t *testing.T, id, who string, subtree bool) {
	t.Helper()
	if err := w.notif.AddIssueWatch(t.Context(), id, w.ids[who], subtree); err != nil {
		t.Fatalf("watch %s by %s: %v", id, who, err)
	}
}

// chain builds the delegation chain of K13 on a fresh issue: assigned to
// previous, then to dismissed, then to assignee; dismissed declines the
// delegation. previous_assignees = {previous, dismissed}, dismissed_by =
// {dismissed}. The subscriber watches the issue.
func (w *world) chain(t *testing.T) string {
	t.Helper()
	id := w.create(t, "previous", nil)
	w.patch(t, id, w.human("author"), issues.PatchIssueInput{Assignee: ptr(w.names["dismissed"])})
	w.patch(t, id, w.human("author"), issues.PatchIssueInput{Assignee: ptr(w.names["assignee"])})
	if _, err := w.store.DismissDelegated(t.Context(), issues.DelegationActionInput{ID: id, Actor: w.human("dismissed")}); err != nil {
		t.Fatalf("DismissDelegated: %v", err)
	}
	w.watch(t, id, "subscriber", false)
	return id
}

// ---------------------------------------------------------------- table

type signalRow struct {
	name   string
	setup  func(t *testing.T, w *world) string
	act    func(t *testing.T, w *world, id string)
	want   map[string][]string
	forbid map[string][]string
	silent []string
	// noNotice: people for whom the action must write NO notification row
	// (feed or push) — the premise of the "No notification" rows, asserted
	// rather than assumed, so a leak of the suppression turns them red.
	noNotice []string
}

// notificationCount is how many notification rows (any channel) who has.
func (w *world) notificationCount(t *testing.T, who string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM notifications WHERE user_id = $1`, w.ids[who]).Scan(&n); err != nil {
		t.Fatalf("count notifications of %s: %v", who, err)
	}
	return n
}

func runSignalRows(t *testing.T, rows []signalRow) {
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			w := newWorld(t)
			sink := startSigSink(t)
			var id string
			if row.setup != nil {
				id = row.setup(t, w)
			}
			// Let the setup's own signals arrive before the action starts:
			// they are published asynchronously and must not be counted as
			// the action's.
			sink.settle(t, w.pool, sink.mark())
			before := make(map[string]int, len(row.noNotice))
			for _, who := range row.noNotice {
				before[who] = w.notificationCount(t, who)
			}
			from := sink.mark()
			row.act(t, w, id)
			got, _ := union(sink.settle(t, w.pool, from))
			for _, who := range row.noNotice {
				if n := w.notificationCount(t, who); n != before[who] {
					t.Errorf("%s: the action wrote %d notification row(s); this row's premise is that none is written", who, n-before[who])
				}
			}

			if len(row.want) == 0 {
				t.Fatal("row names no addressee — it would pass on a path that publishes nothing")
			}
			for who, areas := range row.want {
				for _, a := range areas {
					if !got[w.ids[who]][a] {
						t.Errorf("%s: got areas %v, want %q among them", who, keys(got[w.ids[who]]), a)
					}
				}
			}
			for who, areas := range row.forbid {
				for _, a := range areas {
					if got[w.ids[who]][a] {
						t.Errorf("%s: got area %q, which this row must not give them (all: %v)", who, a, keys(got[w.ids[who]]))
					}
				}
			}
			for _, who := range row.silent {
				if g := got[w.ids[who]]; len(g) != 0 {
					t.Errorf("%s is not an addressee of this row, got areas %v", who, keys(g))
				}
			}
		})
	}
}

var (
	roleAreas   = []string{"inbox", "lens", "tasks"}
	statusAreas = []string{"inbox", "lens", "tasks", "unreviewed"}
)

// TestSignalRecipients_Roles — matrix row 31: the role rows.
func TestSignalRecipients_Roles(t *testing.T) {
	runSignalRows(t, []signalRow{
		{
			name: "Issue created",
			act:  func(t *testing.T, w *world, _ string) { w.create(t, "assignee", nil) },
			// The creator and the assignee are also auto-subscribed
			// ("auto-subscribe" -> watching).
			want:   map[string][]string{"author": {"lens", "tasks", "watching"}, "assignee": {"inbox", "lens", "tasks", "watching"}},
			forbid: map[string][]string{"author": {"inbox"}},
			silent: []string{"bystander", "other"},
		},
		{
			name: "Issue created (epic_create)",
			act: func(t *testing.T, w *world, _ string) {
				_, _, err := w.store.CreateEpicWithChildren(t.Context(), w.pid, w.prefix, "counter",
					issues.CreateIssueInput{Title: "epic", Type: "epic", Priority: 2},
					[]issues.CreateIssueInput{{Title: "child", Type: "task", Priority: 2, Assignee: ptr(w.names["assignee"])}},
					issues.SourceHuman, w.ids["author"], w.human("author"))
				if err != nil {
					t.Fatalf("CreateEpicWithChildren: %v", err)
				}
			},
			want:   map[string][]string{"author": {"lens", "tasks"}, "assignee": {"inbox", "lens", "tasks"}},
			forbid: map[string][]string{"author": {"inbox"}},
			silent: []string{"bystander", "other"},
		},
		{
			name:  "Assignee change",
			setup: func(t *testing.T, w *world) string { return w.chain(t) },
			act: func(t *testing.T, w *world, id string) {
				w.patch(t, id, w.human("author"), issues.PatchIssueInput{Assignee: ptr(w.names["other"])})
			},
			want: map[string][]string{
				"assignee": roleAreas, "other": roleAreas, "previous": roleAreas,
				"author": {"inbox", "lens", "tasks", "unreviewed"},
			},
			// K13: a previous assignee in dismissed_by is not one.
			silent: []string{"bystander", "dismissed"},
		},
		{
			name:  "Issue accepted",
			setup: func(t *testing.T, w *world) string { return w.create(t, "assignee", nil) },
			act: func(t *testing.T, w *world, id string) {
				v := w.ver(t, id)
				if _, err := w.store.Accept(t.Context(), issues.AcceptInput{ID: id, ExpectedVersion: &v, Actor: w.human("assignee")}); err != nil {
					t.Fatalf("Accept: %v", err)
				}
			},
			want:   map[string][]string{"assignee": roleAreas, "author": roleAreas},
			silent: []string{"bystander", "other"},
		},
		{
			name:  "Status change",
			setup: func(t *testing.T, w *world) string { return w.chain(t) },
			act:   func(t *testing.T, w *world, id string) { w.status(t, id, w.human("author"), "deferred") },
			want: map[string][]string{
				"assignee": statusAreas, "author": statusAreas, "previous": statusAreas,
				"subscriber": {"watching"},
			},
			silent: []string{"bystander", "dismissed", "other"},
		},
		{
			name:  "Close",
			setup: func(t *testing.T, w *world) string { return w.chain(t) },
			act:   func(t *testing.T, w *world, id string) { w.close(t, id) },
			want: map[string][]string{
				// The assignee's automatic subscription is removed on close
				// ("auto-unsubscribe" -> watching).
				"assignee": append([]string{"watching"}, statusAreas...),
				"author":   statusAreas, "previous": statusAreas,
				"subscriber": {"watching"},
			},
			silent: []string{"bystander", "dismissed", "other"},
		},
		{
			name: "Reopen",
			setup: func(t *testing.T, w *world) string {
				id := w.chain(t)
				w.close(t, id)
				return id
			},
			act: func(t *testing.T, w *world, id string) { w.status(t, id, w.human("author"), "open") },
			want: map[string][]string{
				"assignee": statusAreas, "author": statusAreas, "previous": statusAreas,
				"subscriber": {"watching"},
			},
			silent: []string{"bystander", "dismissed", "other"},
		},
		{
			name: "Outcome",
			setup: func(t *testing.T, w *world) string {
				id := w.chain(t)
				w.close(t, id)
				return id
			},
			act: func(t *testing.T, w *world, id string) {
				w.patch(t, id, w.human("author"), issues.PatchIssueInput{CloseOutcome: ptr("wont_do")})
			},
			want: map[string][]string{
				"assignee": statusAreas, "author": statusAreas, "previous": statusAreas,
				"subscriber": {"watching"},
			},
			silent: []string{"bystander", "dismissed", "other"},
		},
		{
			name:  "Claim",
			setup: func(t *testing.T, w *world) string { return w.create(t, "assignee", nil) },
			act: func(t *testing.T, w *world, id string) {
				if _, err := w.store.Claim(t.Context(), issues.ClaimInput{ID: id, SessionID: "sess-" + uuid.NewString(), Actor: w.human("assignee")}); err != nil {
					t.Fatalf("Claim: %v", err)
				}
			},
			want:   map[string][]string{"assignee": roleAreas, "author": roleAreas},
			silent: []string{"bystander", "other"},
		},
		{
			name: "Unclaim",
			setup: func(t *testing.T, w *world) string {
				id := w.create(t, "assignee", nil)
				if _, err := w.store.Claim(t.Context(), issues.ClaimInput{ID: id, SessionID: "sess-unclaim-" + id, Actor: w.human("assignee")}); err != nil {
					t.Fatalf("Claim: %v", err)
				}
				return id
			},
			act: func(t *testing.T, w *world, id string) {
				if _, err := w.store.Unclaim(t.Context(), issues.UnclaimInput{ID: id, SessionID: "sess-unclaim-" + id, NextStatus: "in_progress", Reason: "handoff", Actor: w.human("assignee")}); err != nil {
					t.Fatalf("Unclaim: %v", err)
				}
			},
			want:   map[string][]string{"assignee": roleAreas, "author": roleAreas},
			silent: []string{"bystander", "other"},
		},
		{
			name: "Stale lock released",
			setup: func(t *testing.T, w *world) string {
				id := w.create(t, "assignee", nil)
				if _, err := w.store.Claim(t.Context(), issues.ClaimInput{ID: id, SessionID: "sess-stale-" + id, Actor: w.human("assignee")}); err != nil {
					t.Fatalf("Claim: %v", err)
				}
				w.watch(t, id, "subscriber", false)
				if _, err := w.pool.Exec(t.Context(), `UPDATE issues SET locked_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
					t.Fatalf("age the lock: %v", err)
				}
				return id
			},
			act: func(t *testing.T, w *world, id string) {
				lockreap.New(w.pool, lockreap.Config{Interval: time.Hour, Threshold: time.Hour}).Tick(t.Context())
				var locked *string
				if err := w.pool.QueryRow(t.Context(), `SELECT locked_by FROM issues WHERE id = $1`, id).Scan(&locked); err != nil || locked != nil {
					t.Fatalf("the reaper did not release %s (locked_by=%v, err=%v)", id, locked, err)
				}
			},
			want: map[string][]string{
				"assignee": statusAreas, "author": statusAreas,
				"subscriber": {"watching"},
			},
			silent: []string{"bystander", "other"},
		},
	})
}

// TestSignalRecipients_OneMessagePerUserPerTransaction — edge 14: closing
// several issues of one user through skein_close_many gives that user exactly
// one message per committed transaction, however many roles and areas each
// close names them for. The number of transactions is taken from the tool's
// own answer — close_many commits each succeeded row in its own transaction
// — so the expectation follows the tool rather than a literal.
func TestSignalRecipients_OneMessagePerUserPerTransaction(t *testing.T) {
	w := newWorld(t)
	sink := startSigSink(t)
	ids := []string{w.create(t, "assignee", nil), w.create(t, "assignee", nil), w.create(t, "assignee", nil)}
	sink.settle(t, w.pool, sink.mark())

	items := make([]any, 0, len(ids))
	for _, id := range ids {
		items = append(items, map[string]any{
			"id": id, "expectedVersion": float64(w.ver(t, id)), "reason": "done", "closeOutcome": "done",
		})
	}
	from := sink.mark()
	committed := w.closeMany(t, items)
	got, count := union(sink.settle(t, w.pool, from))

	if committed != len(ids) {
		t.Fatalf("skein_close_many closed %d of %d issues — the edge needs every row committed", committed, len(ids))
	}
	for _, who := range []string{"assignee", "author"} {
		if count[w.ids[who]] != committed {
			t.Errorf("%s got %d messages for %d closing transactions, want exactly one per transaction", who, count[w.ids[who]], committed)
		}
		for _, a := range statusAreas {
			if !got[w.ids[who]][a] {
				t.Errorf("%s: areas %v, want %q among them", who, keys(got[w.ids[who]]), a)
			}
		}
	}
	if count[w.ids["bystander"]] != 0 {
		t.Errorf("bystander got %d messages", count[w.ids["bystander"]])
	}
}

// closeMany calls skein_close_many as the author through a real MCP server
// and returns how many rows the tool reports as closed.
func (w *world) closeMany(t *testing.T, items []any) int {
	t.Helper()
	s := server.NewMCPServer("signal-test", "1", server.WithToolCapabilities(false))
	tools.RegisterCloseMany(s, w.pool, w.store)
	ctx := auth.WithUserID(context.Background(), w.ids["author"])
	ctx = auth.WithUsername(ctx, w.names["author"])
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithSessionID(ctx, "mcp-session-"+uuid.NewString())
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "skein_close_many", "arguments": map[string]any{"items": items}},
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
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if resp.Error != nil || resp.Result.IsError || len(resp.Result.Content) == 0 {
		t.Fatalf("skein_close_many failed: %s", out)
	}
	var body struct {
		Results []struct {
			ID string `json:"id"`
			OK bool   `json:"ok"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &body); err != nil {
		t.Fatalf("decode close_many body %s: %v", resp.Result.Content[0].Text, err)
	}
	closed := 0
	for _, r := range body.Results {
		if !r.OK {
			t.Errorf("close_many row %s failed: %s", r.ID, resp.Result.Content[0].Text)
			continue
		}
		closed++
	}
	return closed
}

// subtreeFixture builds K under its canonical parent newParent after a
// reparent away from oldParent, so K's id keeps the old parent's prefix.
// Watchers: subscriber on K itself, other with subtree scope on newParent,
// previous with issue scope on newParent (not a subtree watcher), dismissed
// with subtree scope on oldParent (prefix-shaped, not an ancestor any more).
// K is assigned to assignee and not accepted (it is in their Inbox).
func subtreeFixture(t *testing.T, w *world) string {
	t.Helper()
	oldParent := w.create(t, "", nil)
	newParent := w.create(t, "", nil)
	k := w.create(t, "assignee", &oldParent)
	deps := dependencies.NewStore(w.pool)
	if _, err := deps.Reparent(t.Context(), k, newParent, w.ver(t, k),
		dependencies.CallerAccess{UserID: w.ids["author"], Role: "user"},
		dependencies.HumanActor(w.ids["author"], w.names["author"])); err != nil {
		t.Fatalf("Reparent: %v", err)
	}
	if !strings.HasPrefix(k, oldParent+".") {
		t.Fatalf("child id %s does not carry the old parent's prefix %s — the prefix trap is not set", k, oldParent)
	}
	w.watch(t, k, "subscriber", false)
	w.watch(t, newParent, "other", true)
	w.watch(t, newParent, "previous", false)
	w.watch(t, oldParent, "dismissed", true)
	return k
}

// disableAllReasons switches every notification reason of who off.
func disableAllReasons(t *testing.T, w *world, who string) {
	t.Helper()
	for _, r := range notifications.AllReasons() {
		if _, err := w.pool.Exec(t.Context(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled, updated_at)
VALUES ($1, $2, false, false, now())
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = false, push_enabled = false`, w.ids[who], string(r.Key)); err != nil {
			t.Fatalf("disable reason %s: %v", r.Key, err)
		}
	}
}

// TestSignalRecipients_Subscribers — matrix row 32: subscribers by hierarchy,
// the row-field and date rows, Inbox only when in Inbox, and independence
// from notification suppression.
func TestSignalRecipients_Subscribers(t *testing.T) {
	subtreeWant := map[string][]string{
		"subscriber": {"watching"}, // watcher of the issue
		"other":      {"watching"}, // subtree watcher of the canonical parent
		"author":     {"tasks"},
		"assignee":   {"tasks", "inbox"}, // the issue is in their Inbox
	}
	// previous watches the parent with issue scope; dismissed has a subtree
	// watch on the OLD parent, whose id is K's prefix.
	subtreeSilent := []string{"previous", "dismissed", "bystander"}

	runSignalRows(t, []signalRow{
		{
			name:  "Row field change",
			setup: subtreeFixture,
			act: func(t *testing.T, w *world, id string) {
				w.patch(t, id, w.human("author"), issues.PatchIssueInput{Title: ptr("renamed"), Priority: ptr(1)})
			},
			want:   subtreeWant,
			silent: subtreeSilent,
		},
		{
			name:  "Due date and estimate change",
			setup: subtreeFixture,
			act: func(t *testing.T, w *world, id string) {
				w.patch(t, id, w.human("author"), issues.PatchIssueInput{
					DueAt:            ptr(time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)),
					EstimatedMinutes: ptr(90),
				})
			},
			want:   subtreeWant,
			silent: subtreeSilent,
		},
		{
			name:  "Label",
			setup: subtreeFixture,
			act: func(t *testing.T, w *world, id string) {
				if _, err := w.store.AddLabel(t.Context(), id, "sig-label", w.human("author"), issues.LockOpts{}); err != nil {
					t.Fatalf("AddLabel: %v", err)
				}
			},
			want:   subtreeWant,
			silent: subtreeSilent,
		},
		{
			name: "Row fields: an accepted issue is not in Inbox",
			setup: func(t *testing.T, w *world) string {
				id := w.create(t, "assignee", nil)
				v := w.ver(t, id)
				if _, err := w.store.Accept(t.Context(), issues.AcceptInput{ID: id, ExpectedVersion: &v, Actor: w.human("assignee")}); err != nil {
					t.Fatalf("Accept: %v", err)
				}
				return id
			},
			act: func(t *testing.T, w *world, id string) {
				w.patch(t, id, w.human("author"), issues.PatchIssueInput{Title: ptr("renamed")})
			},
			want:   map[string][]string{"assignee": {"tasks"}, "author": {"tasks"}},
			forbid: map[string][]string{"assignee": {"inbox"}},
			silent: []string{"bystander"},
		},
		{
			name: "No notification: the subscriber's own action",
			setup: func(t *testing.T, w *world) string {
				id := w.create(t, "assignee", nil)
				w.watch(t, id, "subscriber", false)
				return id
			},
			act:      func(t *testing.T, w *world, id string) { w.status(t, id, w.human("subscriber"), "deferred") },
			want:     map[string][]string{"subscriber": {"watching"}},
			silent:   []string{"bystander"},
			noNotice: []string{"subscriber"},
		},
		{
			name: "No notification: agent action",
			setup: func(t *testing.T, w *world) string {
				id := w.create(t, "assignee", nil)
				w.watch(t, id, "subscriber", false) // suppress_agent_actions is on by default
				return id
			},
			act:      func(t *testing.T, w *world, id string) { w.status(t, id, w.agent("author"), "deferred") },
			want:     map[string][]string{"subscriber": {"watching"}},
			silent:   []string{"bystander"},
			noNotice: []string{"subscriber"},
		},
		{
			name: "No notification: reasons disabled",
			setup: func(t *testing.T, w *world) string {
				id := w.create(t, "assignee", nil)
				w.watch(t, id, "subscriber", false)
				disableAllReasons(t, w, "subscriber")
				return id
			},
			act:      func(t *testing.T, w *world, id string) { w.status(t, id, w.human("author"), "deferred") },
			want:     map[string][]string{"subscriber": {"watching"}},
			silent:   []string{"bystander"},
			noNotice: []string{"subscriber"},
		},
	})
}

// TestSignalRecipients_DelegationAndReview — matrix row 33: declining and
// resuming a delegation reach the acting user only; the review flag and mark
// reach the author only (review data is private).
func TestSignalRecipients_DelegationAndReview(t *testing.T) {
	// previous was the assignee before assignee, and keeps the automatic
	// subscription it got then.
	delegated := func(t *testing.T, w *world) string {
		id := w.create(t, "previous", nil)
		w.patch(t, id, w.human("author"), issues.PatchIssueInput{Assignee: ptr(w.names["assignee"])})
		return id
	}
	runSignalRows(t, []signalRow{
		{
			name:  "Delegation refused",
			setup: delegated,
			act: func(t *testing.T, w *world, id string) {
				if _, err := w.store.DismissDelegated(t.Context(), issues.DelegationActionInput{ID: id, Actor: w.human("previous")}); err != nil {
					t.Fatalf("DismissDelegated: %v", err)
				}
			},
			// The decline stops previous's subscription → watching.
			want:   map[string][]string{"previous": {"lens", "tasks", "unreviewed", "watching"}},
			silent: []string{"author", "assignee", "bystander"},
		},
		{
			name: "Delegation resumed",
			setup: func(t *testing.T, w *world) string {
				id := delegated(t, w)
				if _, err := w.store.DismissDelegated(t.Context(), issues.DelegationActionInput{ID: id, Actor: w.human("previous")}); err != nil {
					t.Fatalf("DismissDelegated: %v", err)
				}
				return id
			},
			act: func(t *testing.T, w *world, id string) {
				if _, err := w.store.ResumeDelegated(t.Context(), issues.DelegationActionInput{ID: id, Actor: w.human("previous")}); err != nil {
					t.Fatalf("ResumeDelegated: %v", err)
				}
			},
			want:   map[string][]string{"previous": {"lens", "tasks", "unreviewed"}},
			silent: []string{"author", "assignee", "bystander"},
		},
		{
			name:  "Review flag",
			setup: func(t *testing.T, w *world) string { return w.create(t, "assignee", nil) },
			act: func(t *testing.T, w *world, id string) {
				var current bool
				if err := w.pool.QueryRow(t.Context(), `SELECT review_required FROM issues WHERE id = $1`, id).Scan(&current); err != nil {
					t.Fatalf("read flag: %v", err)
				}
				if _, err := w.store.SetReviewRequired(t.Context(), issues.ReviewFlagInput{ID: id, ExpectedVersion: w.ver(t, id), Required: !current, Actor: w.human("author")}); err != nil {
					t.Fatalf("SetReviewRequired: %v", err)
				}
			},
			want:   map[string][]string{"author": {"unreviewed"}},
			silent: []string{"assignee", "bystander", "other"},
		},
		{
			name: "Review mark",
			setup: func(t *testing.T, w *world) string {
				id := w.create(t, "assignee", nil)
				if _, err := w.pool.Exec(t.Context(), `UPDATE issues SET review_required = true WHERE id = $1`, id); err != nil {
					t.Fatalf("set flag: %v", err)
				}
				w.close(t, id)
				return id
			},
			act: func(t *testing.T, w *world, id string) {
				if _, err := w.store.MarkReviewed(t.Context(), issues.ReviewMarkInput{ID: id, ExpectedVersion: w.ver(t, id), Actor: w.human("author")}); err != nil {
					t.Fatalf("MarkReviewed: %v", err)
				}
			},
			want:   map[string][]string{"author": {"unreviewed"}},
			silent: []string{"assignee", "bystander", "other"},
		},
	})
}

// TestSignalRecipients_OutsiderNamedByTextGetsNothing —
// INV-30, INV-11: an issue whose assignee TEXT is the email of a person
// with no access to its project delivers nothing to the account that later
// appears with that email (a self-registration from outside, E-37): neither
// the creation of the account nor a write to the issue signals it. A
// personal signal goes to the assignee by the delivery rule — the same as
// the Inbox — not by the identity of the text alone. Positive control: once
// that person is a member of the project, the same write signals them.
func TestSignalRecipients_OutsiderNamedByTextGetsNothing(t *testing.T) {
	w := newWorld(t)
	sink := startSigSink(t)
	email := "outsider-" + uuid.NewString()[:8] + "@contractor.test"

	in := issues.CreateIssueInput{Title: "named by text " + uuid.NewString()[:8], Type: "task", Priority: 2, Assignee: ptr(email)}
	d, err := w.store.CreateIssue(t.Context(), w.pid, w.prefix, "counter", in, w.ids["author"], w.human("author"))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	sink.settle(t, w.pool, sink.mark())

	// The account appears: created by an admin, the path that signals the
	// people whose assignee resolution changed.
	from := sink.mark()
	usersStore := users.NewStore(w.pool, auth.MinBcryptCost, apikeys.New(w.pool), "local", "", nil)
	outsider, err := usersStore.CreateUser(t.Context(), users.CreateUserInput{
		Email: email, Username: "out" + uuid.NewString()[:8], Name: "Outsider",
		Password: "correct horse battery staple", Role: "user",
	}, users.HumanActor(w.ids["author"], w.names["author"]))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = w.pool.Exec(ctx, `DELETE FROM events WHERE actor_user_id = $1 OR entity_id = $2`, outsider.ID, outsider.ID.String())
		_, _ = w.pool.Exec(ctx, `DELETE FROM project_members WHERE user_id = $1`, outsider.ID)
		_, _ = w.pool.Exec(ctx, `DELETE FROM space_members WHERE user_id = $1 OR space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`, outsider.ID)
		_, _ = w.pool.Exec(ctx, `DELETE FROM spaces WHERE owner_id = $1`, outsider.ID)
		_, _ = w.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, outsider.ID)
	})
	got, _ := union(sink.settle(t, w.pool, from))
	if g := got[outsider.ID]; len(g) != 0 {
		t.Errorf("creating the outsider's account signalled them %v about an issue they cannot see", keys(g))
	}
	if len(got[w.ids["author"]]) == 0 {
		t.Error("creating the account signalled nobody — the author of the issue whose assignee resolved is an addressee")
	}

	// A write to the issue.
	from = sink.mark()
	w.patch(t, d.ID, w.human("author"), issues.PatchIssueInput{Title: ptr("renamed " + uuid.NewString()[:8])})
	got, _ = union(sink.settle(t, w.pool, from))
	if g := got[outsider.ID]; len(g) != 0 {
		t.Errorf("a write to the issue signalled the outsider %v", keys(g))
	}
	if !got[w.ids["author"]]["tasks"] {
		t.Errorf("author: got %v, want tasks", keys(got[w.ids["author"]]))
	}

	// Positive control: as a member of the project the same person is the
	// delivered assignee.
	testsupport.AddProjectMember(t, w.pool, w.pid, outsider.ID, "member")
	from = sink.mark()
	w.patch(t, d.ID, w.human("author"), issues.PatchIssueInput{Title: ptr("renamed " + uuid.NewString()[:8])})
	got, _ = union(sink.settle(t, w.pool, from))
	if !got[outsider.ID]["tasks"] {
		t.Errorf("the assignee, now a member: got %v, want tasks", keys(got[outsider.ID]))
	}
}
