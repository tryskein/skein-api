package tools

// key_scope_writes_test.go — matrix row 29 (US-3 "write acceptance", SC-022).
// Reference — the spec, written before the code: a write by a key changes
// only objects inside its scope. With scope "project P": "read all" leaves
// Q's notifications unread; a subscription filter over Q, or over any
// project, is not listed and is not deleted (as a nonexistent one); a filter
// over Q cannot be created, one over any project only with scope "all";
// reminders on Q are neither listed nor counted, and snooze / dismiss treat
// them as nonexistent; reading a Watching card with its subtree leaves the
// events of Q's issues unread; a reference to Q in the body (parent,
// dependency, close target) answers the code of a nonexistent one. Each
// subtest compares Q's state before and after, and the answer with the one
// for a really nonexistent id.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/testsupport"
)

type keyWritesEnv struct {
	pool   *pgxpool.Pool
	srv    *server.MCPServer
	user   uuid.UUID
	p, q   uuid.UUID
	pIssue string
	qIssue string
}

func newKeyWritesEnv(t *testing.T) *keyWritesEnv {
	t.Helper()
	pool := newTestPool(t)
	user, _ := seedUser(t, pool, "user")
	p, pPrefix := seedProject(t, pool, user, "kwp")
	q, qPrefix := seedProject(t, pool, user, "kwq")
	e := &keyWritesEnv{
		pool: pool, user: user, p: p, q: q,
		pIssue: seedIssue(t, pool, p, user, pPrefix+"-1", "inside the scope", "open", 2),
		qIssue: seedIssue(t, pool, q, user, qPrefix+"-1", "outside the scope", "open", 2),
	}
	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	notifStore := notifications.NewStore(pool)
	s := server.NewMCPServer("key-scope-writes", "1", server.WithToolCapabilities(true))
	RegisterNotifications(s, pool, notifStore)
	RegisterWatchers(s, pool, projectsStore, notifStore, monitorcreds.New(pool, 0))
	RegisterRemind(s, pool, reminders.NewStore(pool), issuesStore, projectsStore)
	RegisterCreate(s, pool, issuesStore, depsStore, projectsStore)
	RegisterDepAdd(s, pool, depsStore, issuesStore)
	RegisterClaim(s, pool, issuesStore)
	RegisterClose(s, pool, issuesStore)
	e.srv = s
	return e
}

// keyCtx is the per-RPC context of a key of the user with the given scope.
func (e *keyWritesEnv) keyCtx(sc apikeys.Scope) context.Context {
	ctx := auth.WithUserID(context.Background(), e.user)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithSessionID(ctx, "key-scope-writes-"+e.user.String()[:8])
	ctx = auth.WithAPIKeyID(ctx, uuid.New())
	return auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelManage, Scope: sc})
}

func (e *keyWritesEnv) scopeP() context.Context {
	return e.keyCtx(apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{e.p}})
}

// answer is a tools/call result: the decoded JSON object and the error flag.
type answer struct {
	body    map[string]any
	isError bool
}

func (e *keyWritesEnv) call(t *testing.T, ctx context.Context, tool string, args map[string]any) answer {
	t.Helper()
	r := call(t, e.srv, ctx, tool, args)
	texts := r.texts()
	if len(texts) == 0 {
		t.Fatalf("%s: empty result", tool)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(texts[0]), &body); err != nil {
		t.Fatalf("%s: result is not a JSON object: %v\n%s", tool, err, texts[0])
	}
	return answer{body: body, isError: r.Result.IsError}
}

// code is the machine code of a refusal; "" for a success.
func (a answer) code() string {
	if !a.isError {
		return ""
	}
	c, _ := a.body["error"].(string)
	return c
}

func wantSameRefusal(t *testing.T, what string, got, nonexistent answer, wantCode string) {
	t.Helper()
	if !got.isError || got.code() != wantCode {
		t.Fatalf("%s = %v, want refusal %q", what, got.body, wantCode)
	}
	if got.code() != nonexistent.code() {
		t.Errorf("%s = %q, nonexistent id %q — must be the same", what, got.code(), nonexistent.code())
	}
}

func notificationRead(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	return readAt(t, pool, id)
}

func TestKeyScopeWrites_ChangeOnlyInsideTheScope(t *testing.T) {
	e := newKeyWritesEnv(t)
	ctx := context.Background()
	kctx := e.scopeP()

	t.Run("read all leaves Q's notifications unread", func(t *testing.T) {
		inP := seedNotificationOnIssue(t, e.pool, e.user, e.p, e.pIssue)
		inQ := seedNotificationOnIssue(t, e.pool, e.user, e.q, e.qIssue)
		if a := e.call(t, kctx, "skein_notifications", map[string]any{"action": "mark_read_all"}); a.isError {
			t.Fatalf("mark_read_all: %v", a.body)
		}
		if !notificationRead(t, e.pool, inP) {
			t.Errorf("the notification of P is still unread after read all")
		}
		if notificationRead(t, e.pool, inQ) {
			t.Errorf("read all by a key with scope P marked Q's notification read")
		}
	})

	t.Run("reading a card with its subtree leaves Q's events unread", func(t *testing.T) {
		child := seedIssue(t, e.pool, e.q, e.user, e.pIssue+".1", "a child in Q", "open", 2)
		own := seedCardedNotification(t, e.pool, e.user, e.p, e.pIssue, e.pIssue)
		fromQ := seedCardedNotification(t, e.pool, e.user, e.q, child, e.pIssue)
		if a := e.call(t, kctx, "skein_notifications", map[string]any{"action": "mark_read_card", "issueId": e.pIssue}); a.isError {
			t.Fatalf("mark_read_card: %v", a.body)
		}
		if !notificationRead(t, e.pool, own) {
			t.Errorf("the card's own event is still unread")
		}
		if notificationRead(t, e.pool, fromQ) {
			t.Errorf("reading the card with scope P marked an event of Q's issue read")
		}
	})

	t.Run("subscription filters", func(t *testing.T) {
		seedFilter := func(name string, f notifications.CustomWatcherFilter) uuid.UUID {
			raw, _ := json.Marshal(f)
			id := uuid.New()
			if _, err := e.pool.Exec(ctx, `INSERT INTO custom_watchers (id, user_id, name, filter) VALUES ($1, $2, $3, $4::jsonb)`,
				id, e.user, name, raw); err != nil {
				t.Fatal(err)
			}
			return id
		}
		inP := seedFilter("over P", notifications.CustomWatcherFilter{Projects: []string{e.p.String()}})
		overQ := seedFilter("over Q", notifications.CustomWatcherFilter{Projects: []string{e.q.String()}})
		anyProject := seedFilter("over any project", notifications.CustomWatcherFilter{})
		exists := func(id uuid.UUID) bool {
			var n int
			if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM custom_watchers WHERE id = $1`, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n == 1
		}

		list := e.call(t, kctx, "skein_watchers", map[string]any{"action": "filter_list"})
		if list.isError {
			t.Fatalf("filter_list: %v", list.body)
		}
		var listed []string
		ws, _ := list.body["watchers"].([]any)
		for _, w := range ws {
			if m, ok := w.(map[string]any); ok {
				id, _ := m["id"].(string)
				listed = append(listed, id)
			}
		}
		if len(listed) != 1 || listed[0] != inP.String() {
			t.Errorf("filter_list with scope P = %v, want only %s", listed, inP)
		}

		nonexistent := e.call(t, kctx, "skein_watchers", map[string]any{"action": "filter_delete", "filterId": uuid.NewString()})
		for name, id := range map[string]uuid.UUID{"over Q": overQ, "over any project": anyProject} {
			got := e.call(t, kctx, "skein_watchers", map[string]any{"action": "filter_delete", "filterId": id.String()})
			wantSameRefusal(t, "filter_delete "+name, got, nonexistent, ErrCodeValidationError)
			if !exists(id) {
				t.Errorf("filter %s was deleted by a key with scope P", name)
			}
		}

		createQ := e.call(t, kctx, "skein_watchers", map[string]any{"action": "filter_create", "name": "new over Q",
			"filter": map[string]any{"projects": []any{e.q.String()}}})
		createMissing := e.call(t, kctx, "skein_watchers", map[string]any{"action": "filter_create", "name": "new over nothing",
			"filter": map[string]any{"projects": []any{uuid.NewString()}}})
		wantSameRefusal(t, "filter_create over Q", createQ, createMissing, ErrCodeProjectNotFound)

		createAny := e.call(t, kctx, "skein_watchers", map[string]any{"action": "filter_create", "name": "new over any",
			"filter": map[string]any{}})
		if !createAny.isError || createAny.code() != ErrCodeForbidden {
			t.Errorf("filter_create over any project with scope P = %v, want %q", createAny.body, ErrCodeForbidden)
		}
		allCtx := e.keyCtx(apikeys.Scope{Mode: apikeys.ScopeAll})
		if a := e.call(t, allCtx, "skein_watchers", map[string]any{"action": "filter_create", "name": "new over any, scope all",
			"filter": map[string]any{}}); a.isError {
			t.Errorf("filter_create over any project with scope all refused: %v", a.body)
		}
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM custom_watchers WHERE user_id = $1 AND name IN ('new over Q', 'new over any')`, e.user).Scan(&n); err != nil || n != 0 {
			t.Errorf("refused filters written: %d (%v)", n, err)
		}
	})

	t.Run("reminders on Q are as nonexistent", func(t *testing.T) {
		inP := testsupport.SeedReminder(t, e.pool, e.pIssue, e.user)
		inQ := testsupport.SeedReminder(t, e.pool, e.qIssue, e.user)
		if _, err := e.pool.Exec(ctx, `UPDATE reminders SET remind_at = now() - interval '1 minute' WHERE id = ANY($1)`,
			[]uuid.UUID{inP, inQ}); err != nil {
			t.Fatal(err)
		}
		list := e.call(t, kctx, "skein_remind", map[string]any{"action": "list"})
		if list.isError {
			t.Fatalf("remind list: %v", list.body)
		}
		var ids []string
		rs, _ := list.body["reminders"].([]any)
		for _, r := range rs {
			if m, ok := r.(map[string]any); ok {
				id, _ := m["reminderId"].(string)
				ids = append(ids, id)
			}
		}
		if len(ids) != 1 || ids[0] != inP.String() {
			t.Errorf("remind list with scope P = %v, want only %s", ids, inP)
		}
		if n, _ := list.body["ringingTaskCount"].(float64); n != 1 {
			t.Errorf("ringingTaskCount with scope P = %v, want 1", list.body["ringingTaskCount"])
		}

		state := func() (string, int) {
			var at string
			var snoozes int
			if err := e.pool.QueryRow(ctx, `SELECT remind_at::text, snooze_count FROM reminders WHERE id = $1`, inQ).Scan(&at, &snoozes); err != nil {
				t.Fatalf("Q's reminder is gone: %v", err)
			}
			return at, snoozes
		}
		beforeAt, beforeSnoozes := state()
		missing := uuid.NewString()
		for _, action := range []string{"snooze", "dismiss"} {
			args := func(id string) map[string]any {
				a := map[string]any{"action": action, "reminderId": id}
				if action == "snooze" {
					a["by"] = "1h"
				}
				return a
			}
			got := e.call(t, kctx, "skein_remind", args(inQ.String()))
			wantSameRefusal(t, action+" of Q's reminder", got, e.call(t, kctx, "skein_remind", args(missing)), ErrCodeReminderNotFound)
		}
		if at, snoozes := state(); at != beforeAt || snoozes != beforeSnoozes {
			t.Errorf("Q's reminder changed: remind_at %s → %s, snoozes %d → %d", beforeAt, at, beforeSnoozes, snoozes)
		}
	})

	t.Run("a reference to Q in the body is as a nonexistent one", func(t *testing.T) {
		missingIssue := "nope-" + uuid.NewString()[:8] + "-1"

		parentQ := e.call(t, kctx, "skein_create", map[string]any{"project": e.p.String(), "title": "child of Q", "type": "task", "kind": "autonomous", "parentId": e.qIssue})
		parentMissing := e.call(t, kctx, "skein_create", map[string]any{"project": e.p.String(), "title": "child of nothing", "type": "task", "kind": "autonomous", "parentId": missingIssue})
		wantSameRefusal(t, "create with a parent in Q", parentQ, parentMissing, ErrCodeParentNotFound)

		depQ := e.call(t, kctx, "skein_dep_add", map[string]any{"fromId": e.pIssue, "toId": e.qIssue, "type": "blocks"})
		depMissing := e.call(t, kctx, "skein_dep_add", map[string]any{"fromId": e.pIssue, "toId": missingIssue, "type": "blocks"})
		wantSameRefusal(t, "dependency on Q", depQ, depMissing, ErrCodeIssueNotFound)

		if a := e.call(t, kctx, "skein_claim", map[string]any{"id": e.pIssue}); a.isError {
			t.Fatalf("claim: %v", a.body)
		}
		closeTo := func(target string) answer {
			var version int
			if err := e.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, e.pIssue).Scan(&version); err != nil {
				t.Fatal(err)
			}
			return e.call(t, kctx, "skein_close", map[string]any{"id": e.pIssue, "expectedVersion": version,
				"reason": "duplicate", "closeOutcome": "duplicate", "closeTarget": target})
		}
		closeQ := closeTo(e.qIssue)
		wantSameRefusal(t, "close with a target in Q", closeQ, closeTo(missingIssue), ErrCodeIssueNotFound)

		var children, deps int
		var status string
		if err := e.pool.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM issues WHERE parent_id = $1),
  (SELECT count(*) FROM dependencies WHERE to_id = $1 OR from_id = $1),
  (SELECT status FROM issues WHERE id = $2)`, e.qIssue, e.pIssue).Scan(&children, &deps, &status); err != nil {
			t.Fatal(err)
		}
		if children != 0 || deps != 0 || status == "closed" {
			t.Errorf("after the refused writes: Q's issue has %d children, %d links; P's issue is %s", children, deps, status)
		}
	})
}
