package main

// key_scope_matrix_test.go — matrix rows 26 and 27 (INV-2, SC-003, US-3).
// Reference — the spec, written before the code: an object outside the key's
// scope behaves exactly as an object of a project the key's owner does not
// see, in every answer, derived ones included — lists, ready, prime, search,
// project list and resolution, counters, notifications, Watching, review,
// links ("unavailable"; readiness still counts the hidden blocker), whoami.
// Scope "spaces" reaches the projects of the chosen spaces, scope "all" is
// the owner's own view, and a request without a key is not narrowed.
//
// Actors of the tenancy fixture (testsupport.SeedTenancy): the caller is uAB
// (ActorBoth), who sees A.space, A.members, B.space, B.members and his own
// Personal. His key K_P has scope "project A.space"; so Q = A.members (same
// space) and the B projects (another of his spaces) are inside his reach and
// outside the key's. The reference for "a project he does not see, in his
// own space" is uch¬p (ActorParticipantNoMembers): a participant of A
// without A.members.
//
// One subtest per place of the T010 inventory: through the real /mcp where
// the place has an MCP surface (the grant rides in the per-RPC context), by a
// store call with the key's request context where it has none.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/testsupport"
)

// scopedKey is a key of uAB with a chosen grant, usable on /mcp and as a
// request context for the store-level places.
type scopedKey struct {
	s     *parityStack
	user  testsupport.TenancyUser
	id    uuid.UUID
	plain string
	grant apikeys.Grant
	sid   string
}

func mintScopedKey(t *testing.T, s *parityStack, a testsupport.Actor, sc apikeys.Scope) *scopedKey {
	t.Helper()
	ctx := context.Background()
	u := s.f.User(t, a)
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	g := apikeys.Grant{Level: apikeys.LevelManage, Scope: sc}
	c, err := apikeys.New(s.pool).Create(ctx, apikeys.NewKey{
		UserID: u.ID, Label: "key scope matrix", Prefix: prefix, Hash: hash, Grant: g,
	}, testsupport.KeyTTLPolicy)
	if err != nil {
		t.Fatalf("create scoped key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM monitor_credentials WHERE minted_by_api_key_id = $1`, c.ID)
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, c.ID)
	})
	k := &scopedKey{s: s, user: u, id: c.ID, plain: plain, grant: g}
	k.sid = k.initialize(t)
	return k
}

// call posts one JSON-RPC message with this key.
func (k *scopedKey) call(t *testing.T, session, method string, params any) (int, map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderAPIKey, k.plain)
	req.Header.Set(auth.HeaderSurface, auth.SurfaceInternal)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	rec := httptest.NewRecorder()
	k.s.router.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: response is not JSON: %v\n%s", method, err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Header().Get("Mcp-Session-Id")
}

func (k *scopedKey) initialize(t *testing.T) string {
	t.Helper()
	code, out, sid := k.call(t, "", "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "key-scope-matrix", "version": "1"},
	})
	if code != http.StatusOK || out["error"] != nil || sid == "" {
		t.Fatalf("initialize with a scoped key: %d %v", code, out)
	}
	return sid
}

func (k *scopedKey) tool(t *testing.T, name string, args map[string]any) toolResult {
	t.Helper()
	code, out, _ := k.call(t, k.sid, "tools/call", map[string]any{"name": name, "arguments": args})
	if code != http.StatusOK || out["error"] != nil {
		t.Fatalf("tools/call %s: %d %v", name, code, out)
	}
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("tools/call %s: no result: %v", name, out)
	}
	isErr, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	var text string
	if len(content) > 0 {
		if c, ok := content[0].(map[string]any); ok {
			text, _ = c["text"].(string)
		}
	}
	return toolResult{text: text, isError: isErr}
}

// ctx is the request context of this key as /mcp builds it: identity, key
// id, grant, and the per-request memo of the visible set.
func (k *scopedKey) ctx() context.Context {
	ctx := auth.WithUserID(context.Background(), k.user.ID)
	ctx = auth.WithUserRole(ctx, k.user.Role)
	ctx = auth.WithAPIKeyID(ctx, k.id)
	ctx = auth.WithKeyGrant(ctx, k.grant)
	return authz.WithRequestCache(ctx)
}

// itemIDs reads the "id" of every element of the array under key of a JSON
// object answer, sorted.
func itemIDs(t *testing.T, raw []byte, key string) []string {
	t.Helper()
	var res map[string]json.RawMessage
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var items []map[string]any
	if err := json.Unmarshal(res[key], &items); err != nil {
		t.Fatalf("decode %q of %s: %v", key, raw, err)
	}
	out := []string{}
	for _, it := range items {
		if v, ok := it["id"].(string); ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func showCard(t *testing.T, text string) (isBlocked bool, unavailable int) {
	t.Helper()
	var r struct {
		Issue struct {
			IsBlocked   bool `json:"isBlocked"`
			Unavailable int  `json:"unavailableBlockingLinks"`
		} `json:"issue"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("decode skein_show %s: %v", text, err)
	}
	return r.Issue.IsBlocked, r.Issue.Unavailable
}

func TestKeyScope_ObjectOutsideTheScopeIsAsNotVisible(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	f := s.f
	both := f.User(t, testsupport.ActorBoth)
	P, Q := f.ASpace, f.AMembers
	outside := []testsupport.TenancyProject{Q, f.BSpace, f.BMembers, f.Personal[testsupport.ActorBoth]}
	outsideIDs := map[string]bool{}
	outsideIssues := map[string]bool{}
	for _, p := range outside {
		outsideIDs[p.ID.String()] = true
		outsideIssues[p.IssueID] = true
	}

	// a2 in P is blocked by Q's marker issue: a link to Q.
	a2 := testsupport.SeedIssue(t, s.pool, P.ID, both.ID, P.IDPrefix+"-901", "blocked by a hidden one", "open", 2)
	testsupport.SeedDep(t, s.pool, a2, Q.IssueID, "blocks", both.ID)

	k := mintScopedKey(t, s, testsupport.ActorBoth, apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{P.ID}})
	reference := s.session(testsupport.ActorParticipantNoMembers) // uch¬p — does not see Q
	ownerSID := s.session(testsupport.ActorBoth)                  // uAB's own key, scope all

	t.Run("list (Scope.FromContext / Bind) — baseline: the owner sees Q", func(t *testing.T) {
		code, rest := s.rest(testsupport.ActorBoth, http.MethodGet, "/ui/v1/issues?limit=500&q="+url.QueryEscape(s.word), nil)
		if code != http.StatusOK {
			t.Fatalf("REST /issues: %d %s", code, rest)
		}
		if !slices.Contains(itemIDs(t, rest, "items"), Q.IssueID) {
			t.Fatalf("baseline: the owner through the BFF does not see %s", Q.IssueID)
		}
		res := k.tool(t, "skein_list", map[string]any{"crossProject": true, "q": s.word, "limit": 100})
		if res.isError {
			t.Fatalf("skein_list: %s", res.text)
		}
		if got, want := itemIDs(t, []byte(res.text), "items"), []string{P.IssueID}; !slices.Equal(got, want) {
			t.Errorf("skein_list with scope P = %v, want %v", got, want)
		}
	})

	t.Run("search (ScopeFor / Bind)", func(t *testing.T) {
		res := k.tool(t, "skein_search", map[string]any{"query": s.word, "limit": 100})
		if res.isError {
			t.Fatalf("skein_search: %s", res.text)
		}
		got := itemIDs(t, []byte(res.text), "items")
		if !slices.Contains(got, P.IssueID) {
			t.Fatalf("skein_search with scope P misses %s: %v", P.IssueID, got)
		}
		for _, id := range got {
			if outsideIssues[id] {
				t.Errorf("skein_search with scope P returns %s, outside the scope", id)
			}
		}
	})

	t.Run("ready (Bind; readiness counts the hidden blocker)", func(t *testing.T) {
		res := k.tool(t, "skein_ready", map[string]any{"project": P.ID.String(), "limit": 100})
		if res.isError {
			t.Fatalf("skein_ready: %s", res.text)
		}
		got := itemIDs(t, []byte(res.text), "items")
		if !slices.Contains(got, P.IssueID) || slices.Contains(got, a2) {
			t.Errorf("skein_ready of P = %v, want %s and not %s (its blocker is open, though hidden)", got, P.IssueID, a2)
		}
		inQ := k.tool(t, "skein_ready", map[string]any{"project": Q.ID.String()})
		missing := k.tool(t, "skein_ready", map[string]any{"project": uuid.NewString()})
		if !inQ.isError || errShape(t, []byte(inQ.text)) != errShape(t, []byte(missing.text)) {
			t.Errorf("skein_ready of Q = %s, want the answer for a nonexistent project %s", inQ.text, missing.text)
		}
	})

	t.Run("prime (Bind)", func(t *testing.T) {
		res := k.tool(t, "skein_prime", map[string]any{"limit": 25})
		if res.isError {
			t.Fatalf("skein_prime: %s", res.text)
		}
		var pr struct {
			MyInProgress []map[string]any `json:"myInProgress"`
			ReadyForMe   []map[string]any `json:"readyForMe"`
			Blocked      []map[string]any `json:"blocked"`
		}
		if err := json.Unmarshal([]byte(res.text), &pr); err != nil {
			t.Fatalf("decode prime: %v", err)
		}
		for _, rows := range [][]map[string]any{pr.MyInProgress, pr.ReadyForMe, pr.Blocked} {
			for _, r := range rows {
				if pid, _ := r["projectId"].(string); outsideIDs[pid] {
					t.Errorf("skein_prime row %v of a project outside the scope", r["id"])
				}
			}
		}
	})

	t.Run("project list and resolve (ScopeFor / Bind)", func(t *testing.T) {
		res := k.tool(t, "skein_project_list", map[string]any{"limit": 100})
		if res.isError {
			t.Fatalf("skein_project_list: %s", res.text)
		}
		if got, want := itemIDs(t, []byte(res.text), "items"), []string{P.ID.String()}; !slices.Equal(got, want) {
			t.Errorf("skein_project_list with scope P = %v, want %v", got, want)
		}
		hidden := k.tool(t, "skein_project_resolve", map[string]any{"q": Q.Name})
		missing := k.tool(t, "skein_project_resolve", map[string]any{"q": "no-such-project-" + uuid.NewString()[:8]})
		if hidden.isError != missing.isError {
			t.Fatalf("resolve Q: isError %v, nonexistent %v", hidden.isError, missing.isError)
		}
		if hidden.isError {
			if errShape(t, []byte(hidden.text)) != errShape(t, []byte(missing.text)) {
				t.Errorf("resolve Q = %s, want the nonexistent answer %s", hidden.text, missing.text)
			}
		} else if got, want := decodeResolve(t, []byte(hidden.text)), decodeResolve(t, []byte(missing.text)); got != want {
			t.Errorf("resolve Q = %+v, want the nonexistent answer %+v", got, want)
		}
	})

	t.Run("show and create (ProjectAccess / Can)", func(t *testing.T) {
		hidden := k.tool(t, "skein_show", map[string]any{"id": Q.IssueID})
		missing := k.tool(t, "skein_show", map[string]any{"id": Q.IDPrefix + "-999999"})
		if !hidden.isError || errShape(t, []byte(hidden.text)) != errShape(t, []byte(missing.text)) {
			t.Errorf("skein_show of Q's issue = %s, want the nonexistent answer %s", hidden.text, missing.text)
		}
		created := k.tool(t, "skein_create", map[string]any{"project": Q.ID.String(), "title": "must not land", "type": "task", "kind": "autonomous"})
		absent := k.tool(t, "skein_create", map[string]any{"project": uuid.NewString(), "title": "must not land", "type": "task", "kind": "autonomous"})
		if !created.isError || errShape(t, []byte(created.text)) != errShape(t, []byte(absent.text)) {
			t.Errorf("skein_create in Q = %s, want the nonexistent-project answer %s", created.text, absent.text)
		}
		var refusal struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(created.text), &refusal); err != nil || refusal.Error != "project_not_found" {
			t.Errorf("skein_create in Q refused with %q (%v), want project_not_found", refusal.Error, err)
		}
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM issues WHERE project_id = $1 AND title = 'must not land'`, Q.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("issues written in Q: %d (%v)", n, err)
		}
		if _, err := projects.EnsureProjectVisible(k.ctx(), s.pool, Q.ID, both.ID); !errors.Is(err, projects.ErrProjectNotVisible) {
			t.Errorf("EnsureProjectVisible(Q) with the key = %v, want ErrProjectNotVisible", err)
		}
	})

	t.Run("links and blockers (Bind; BindSpaces not narrowed)", func(t *testing.T) {
		owner := s.tool(testsupport.ActorBoth, ownerSID, "skein_show", map[string]any{"id": a2})
		if blocked, unavailable := showCard(t, owner.text); !blocked || unavailable != 0 {
			t.Fatalf("baseline: the owner's view of %s = blocked %v, unavailable %d; want blocked, 0", a2, blocked, unavailable)
		}
		ref := s.tool(testsupport.ActorParticipantNoMembers, reference, "skein_show", map[string]any{"id": a2})
		refBlocked, refUnavailable := showCard(t, ref.text)
		res := k.tool(t, "skein_show", map[string]any{"id": a2})
		if res.isError {
			t.Fatalf("skein_show %s: %s", a2, res.text)
		}
		blocked, unavailable := showCard(t, res.text)
		if blocked || unavailable != 1 || blocked != refBlocked || unavailable != refUnavailable {
			t.Errorf("%s with scope P = blocked %v, unavailable %d; the reader who does not see Q: blocked %v, unavailable %d; want false, 1 for both",
				a2, blocked, unavailable, refBlocked, refUnavailable)
		}
	})

	t.Run("whoami (VisibleJoin)", func(t *testing.T) {
		res := k.tool(t, "skein_whoami", map[string]any{})
		if res.isError {
			t.Fatalf("skein_whoami: %s", res.text)
		}
		var who struct {
			Projects []struct {
				ID string `json:"id"`
			} `json:"projects"`
			Total int `json:"projectsTotal"`
		}
		if err := json.Unmarshal([]byte(res.text), &who); err != nil {
			t.Fatalf("decode whoami: %v", err)
		}
		if who.Total != 1 || len(who.Projects) != 1 || who.Projects[0].ID != P.ID.String() {
			t.Errorf("whoami with scope P = %+v, want exactly %s", who, P.ID)
		}
	})

	t.Run("notifications inbox and counts (VisibleNowSetExpr)", func(t *testing.T) {
		inP := seedKeyScopeNotification(t, s, both.ID, P.ID, P.IssueID)
		inQ := seedKeyScopeNotification(t, s, both.ID, Q.ID, Q.IssueID)
		res := k.tool(t, "skein_notifications", map[string]any{"action": "inbox", "scope": "all", "limit": 100})
		if res.isError {
			t.Fatalf("inbox: %s", res.text)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(res.text), &body); err != nil {
			t.Fatalf("decode inbox: %v", err)
		}
		got := inboxIDs(body)
		if !slices.Contains(got, inP) || slices.Contains(got, inQ) {
			t.Errorf("inbox with scope P = %v, want %d and not %d", got, inP, inQ)
		}
		c, err := notifications.NewStore(s.pool).Counts(k.ctx(), both.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		full, err := notifications.NewStore(s.pool).Counts(ctx, both.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if full.Unread-c.Unread != 1 {
			t.Errorf("unread with scope P = %d, without a key %d; want exactly Q's one fewer", c.Unread, full.Unread)
		}
	})

	t.Run("Watching cards (RecipientVisibleExpr by the caller)", func(t *testing.T) {
		for _, id := range []string{P.IssueID, Q.IssueID} {
			if _, err := s.pool.Exec(ctx, `INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')
ON CONFLICT DO NOTHING`, id, both.ID); err != nil {
				t.Fatal(err)
			}
		}
		feed, err := notifications.NewStore(s.pool).ListWatchingFeed(k.ctx(), both.ID, notifications.WatchingStateAll)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range feed.Rows {
			got = append(got, r.IssueID)
		}
		if !slices.Contains(got, P.IssueID) || slices.Contains(got, Q.IssueID) {
			t.Errorf("Watching cards with scope P = %v, want %s and not %s", got, P.IssueID, Q.IssueID)
		}
	})

	t.Run("watcher list (issue and project subscriptions)", func(t *testing.T) {
		for _, id := range []string{P.IssueID, Q.IssueID} {
			if _, err := s.pool.Exec(ctx, `INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')
ON CONFLICT DO NOTHING`, id, both.ID); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []uuid.UUID{P.ID, Q.ID} {
			if _, err := s.pool.Exec(ctx, `INSERT INTO project_watchers (project_id, user_id, reasons) VALUES ($1, $2, '{manual}')
ON CONFLICT DO NOTHING`, id, both.ID); err != nil {
				t.Fatal(err)
			}
		}
		res := k.tool(t, "skein_watchers", map[string]any{"action": "list"})
		if res.isError {
			t.Fatalf("skein_watchers list: %s", res.text)
		}
		var w struct {
			Issues []struct {
				IssueID   string `json:"issueId"`
				ProjectID string `json:"projectId"`
			} `json:"issues"`
			Projects []struct {
				ProjectID string `json:"projectId"`
			} `json:"projects"`
		}
		if err := json.Unmarshal([]byte(res.text), &w); err != nil {
			t.Fatalf("decode watcher list: %v", err)
		}
		var issuesSeen, projectsSeen []string
		for _, i := range w.Issues {
			issuesSeen = append(issuesSeen, i.IssueID)
			if outsideIDs[i.ProjectID] {
				t.Errorf("watcher list with scope P holds %s of a project outside the scope", i.IssueID)
			}
		}
		for _, p := range w.Projects {
			projectsSeen = append(projectsSeen, p.ProjectID)
			if outsideIDs[p.ProjectID] {
				t.Errorf("watcher list with scope P holds the subscription to project %s, outside the scope", p.ProjectID)
			}
		}
		if !slices.Contains(issuesSeen, P.IssueID) || !slices.Contains(projectsSeen, P.ID.String()) {
			t.Errorf("watcher list with scope P = issues %v, projects %v; want the subscriptions of P", issuesSeen, projectsSeen)
		}
		if strings.Contains(res.text, Q.IssueID) || strings.Contains(res.text, Q.Name) {
			t.Errorf("watcher list with scope P names Q: %s", res.text)
		}
	})

	t.Run("review set (RecipientVisibleExpr by the caller)", func(t *testing.T) {
		other := f.User(t, testsupport.ActorOnlyA)
		var want []string
		for _, p := range []testsupport.TenancyProject{P, Q} {
			id := testsupport.SeedIssue(t, s.pool, p.ID, both.ID, p.IDPrefix+"-902", "to review", "closed", 2)
			if _, err := s.pool.Exec(ctx, `UPDATE issues SET assignee = $2, review_required = true WHERE id = $1`, id, other.Username); err != nil {
				t.Fatal(err)
			}
			if p.ID == P.ID {
				want = append(want, id)
			}
		}
		kctx := k.ctx()
		res, err := issues.NewStore(s.pool).ListByUser(kctx, issues.ListByUserArgs{
			UserID: both.ID, Mode: "created_by", Scope: authz.FromContext(kctx), Unreviewed: true, Limit: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, it := range res.Items {
			got = append(got, it.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("unreviewed with scope P = %v, want %v", got, want)
		}
	})

	t.Run("reminders (VisibleNowSetExpr)", func(t *testing.T) {
		testsupport.SeedReminder(t, s.pool, P.IssueID, both.ID)
		testsupport.SeedReminder(t, s.pool, Q.IssueID, both.ID)
		list, err := reminders.NewStore(s.pool).ListForUser(k.ctx(), both.ID, reminders.FilterAll, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range list {
			got = append(got, r.IssueID)
		}
		if !slices.Contains(got, P.IssueID) || slices.Contains(got, Q.IssueID) {
			t.Errorf("reminders with scope P = %v, want %s and not %s", got, P.IssueID, Q.IssueID)
		}
	})

	t.Run("stalled monitors (RecipientVisibleExpr by the caller)", func(t *testing.T) {
		for _, p := range []testsupport.TenancyProject{P, Q} {
			if _, err := s.pool.Exec(ctx, `
INSERT INTO monitor_credentials
    (token_digest, user_id, minted_by_api_key_id, audience, capability,
     project_id, scope_issue_id, scope_subtree, event_types,
     created_at, absolute_deadline, expires_at, issued_session_id, token_file_path)
VALUES (decode(md5($4::text), 'hex') || decode(md5($4::text || 'x'), 'hex'),
        $1, $2, 'notifications:poll', 'read', $3, $5, false, ARRAY['comment.created'],
        now() - interval '1 hour', now() + interval '11 hours', now() + interval '5 hours',
        'stalled-session', '/tmp/stalled.token')`,
				both.ID, k.id, p.ID, uuid.NewString(), p.IssueID); err != nil {
				t.Fatal(err)
			}
		}
		kctx := k.ctx()
		res, err := monitorcreds.New(s.pool, 6*time.Hour).Stalled(kctx, monitorcreds.StalledQuery{
			APIKeyID: k.id, UserID: both.ID, RecipientSession: "stalled-session",
			Threshold: time.Minute, Limit: 10, Key: authz.FromContext(kctx).KeyLimit(),
		})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range res.Monitors {
			got = append(got, m.IssueID)
		}
		if !slices.Equal(got, []string{P.IssueID}) {
			t.Errorf("stalled monitors with scope P = %v, want [%s]", got, P.IssueID)
		}
	})

	t.Run("HOT ZONE reach (ReachFor)", func(t *testing.T) {
		kctx := k.ctx()
		reach, err := authz.ReachFor(kctx, s.pool, authz.FromContext(kctx), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reach.Visible[P.ID]; !ok || len(reach.Visible) != 1 {
			t.Errorf("HOT ZONE visible with scope P = %v, want exactly %s", reach.Visible, P.ID)
		}
		// Q and the B projects lie in his own spaces: they contribute,
		// anonymized, like a project he does not see; his Personal does not.
		for _, p := range []testsupport.TenancyProject{Q, f.BSpace, f.BMembers} {
			if _, ok := reach.Reach[p.ID]; !ok {
				t.Errorf("HOT ZONE reach with scope P misses %s of his own space", p.Name)
			}
		}
		if _, ok := reach.Reach[f.Personal[testsupport.ActorBoth].ID]; ok {
			t.Errorf("HOT ZONE reach with scope P holds his Personal, which is in no shared space")
		}
	})
}

// seedKeyScopeNotification writes one unread feed notification for user on
// an issue of project and returns its id.
func seedKeyScopeNotification(t *testing.T, s *parityStack, user, project uuid.UUID, issueID string) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID, id int64
	if err := s.pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
VALUES (now(), 'issue.updated', 'issue', $1, $2, 'tester', $3, 'human', '{}'::jsonb)
RETURNING id`, issueID, project, user).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, eventID) })
	if err := s.pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel)
VALUES ($1, $2, 'issue.updated', $3, now(), true)
RETURNING id`, user, eventID, project).Scan(&id); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return id
}

// inboxIDs pulls the notification ids out of a decoded inbox payload.
func inboxIDs(body map[string]any) []int64 {
	items, _ := body["items"].([]any)
	out := make([]int64, 0, len(items))
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			if f, ok := item["id"].(float64); ok {
				out = append(out, int64(f))
			}
		}
	}
	return out
}

// TestKeyScope_Modes — row 27: scope "spaces {B}" reaches the projects of B
// and nothing else the owner sees; scope "all" equals the owner's view; a
// /ui/v1 request without a key is not narrowed; a key id without its grant
// reaches nothing.
func TestKeyScope_Modes(t *testing.T) {
	s := newParityStack(t)
	f := s.f
	both := f.User(t, testsupport.ActorBoth)

	code, rest := s.rest(testsupport.ActorBoth, http.MethodGet, "/ui/v1/projects?limit=100", nil)
	if code != http.StatusOK {
		t.Fatalf("REST /projects: %d %s", code, rest)
	}
	ownerView := itemIDs(t, rest, "items")
	for _, p := range []testsupport.TenancyProject{f.ASpace, f.AMembers, f.BSpace, f.BMembers} {
		if !slices.Contains(ownerView, p.ID.String()) {
			t.Fatalf("baseline: /ui/v1 without a key does not show %s — not the owner's full view: %v", p.Name, ownerView)
		}
	}
	projectsOf := func(k *scopedKey) []string {
		t.Helper()
		res := k.tool(t, "skein_project_list", map[string]any{"limit": 100})
		if res.isError {
			t.Fatalf("skein_project_list: %s", res.text)
		}
		return itemIDs(t, []byte(res.text), "items")
	}

	t.Run("spaces {B} reaches the projects of B only", func(t *testing.T) {
		k := mintScopedKey(t, s, testsupport.ActorBoth, apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{f.SpaceB}})
		want := []string{f.BSpace.ID.String(), f.BMembers.ID.String()}
		sort.Strings(want)
		if got := projectsOf(k); !slices.Equal(got, want) {
			t.Errorf("scope spaces {B} = %v, want %v", got, want)
		}
	})

	t.Run("all equals the owner's view", func(t *testing.T) {
		k := mintScopedKey(t, s, testsupport.ActorBoth, apikeys.Scope{Mode: apikeys.ScopeAll})
		if got := projectsOf(k); !slices.Equal(got, ownerView) {
			t.Errorf("scope all = %v, the owner's view %v", got, ownerView)
		}
	})

	t.Run("a key id without its grant reaches nothing", func(t *testing.T) {
		ctx := auth.WithAPIKeyID(auth.WithUserRole(auth.WithUserID(context.Background(), both.ID), both.Role), uuid.New())
		_, visible, err := authz.FromContext(ctx).Bind(ctx, s.pool)
		if err != nil {
			t.Fatal(err)
		}
		if len(visible) != 0 {
			t.Errorf("visible set of a key request without a grant = %v, want empty", visible)
		}
		plain := auth.WithUserRole(auth.WithUserID(context.Background(), both.ID), both.Role)
		_, all, err := authz.FromContext(plain).Bind(plain, s.pool)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) < 4 {
			t.Errorf("visible set of a request without a key = %v, want the owner's view", all)
		}
	})
}
