package handler_test

// sk-1007 US-1 through HTTP — matrix rows No. 12 and No. 13. Reference — the
// spec, written before the code:
//
//   - SC-001 / INV-3: for any reader, any key scope and any state of access
//     the column's unread total equals the Watching badge; here unreadTotal
//     of GET /ui/v1/me/watching against watching of
//     GET /ui/v1/me/notification-counts, two handlers that assemble the two
//     numbers in two places;
//   - US-1 "the card's parent": the parent of a Watching card is given only
//     when the reader sees it now, with the key's scope; otherwise the card
//     has no parent id.
//
// The routes are mounted as the product mounts them for a user
// (ParseUserHeaders + RequireUser); a key request is the same request whose
// context carries the key and its grant (cmd/api key_scope_matrix_test.go
// scopedKey.ctx). Tests skip without TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"skein-api/internal/testsupport"
)

// mwvEnv is a reader, the owner who writes every event, and the projects
// the tests place issues in.
type mwvEnv struct {
	pool   *pgxpool.Pool
	owner  uuid.UUID
	reader uuid.UUID
	name   string
	n      int
}

func newMWVEnv(t *testing.T) *mwvEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	reader, name := testsupport.SeedUser(t, pool, "user")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id = $1`, reader)
	})
	return &mwvEnv{pool: pool, owner: owner, reader: reader, name: name}
}

// project seeds a project of the owner in space (uuid.Nil = the shared test
// space), the reader a member when member is set. Visibility is members, so
// the reader sees it only as a member.
func (e *mwvEnv) project(t *testing.T, space uuid.UUID, member bool) (uuid.UUID, string) {
	t.Helper()
	var id uuid.UUID
	var prefix string
	if space == uuid.Nil {
		id, prefix = testsupport.SeedProject(t, e.pool, e.owner, "mwv")
	} else {
		id, prefix = testsupport.SeedProjectInSpace(t, e.pool, e.owner, space, "mwv")
	}
	if member {
		testsupport.AddProjectMember(t, e.pool, id, e.reader, "member")
	}
	return id, prefix
}

func (e *mwvEnv) issue(t *testing.T, project uuid.UUID, prefix string) string {
	t.Helper()
	e.n++
	return testsupport.SeedIssue(t, e.pool, project, e.owner, prefix+"-"+strconv.Itoa(e.n), "visibility", "open", 2)
}

func (e *mwvEnv) watch(t *testing.T, issueID, scope string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, scope, since) VALUES ($1, $2, ARRAY['manual']::text[], $3, now())`,
		issueID, e.reader, scope); err != nil {
		t.Fatalf("watch %s: %v", issueID, err)
	}
}

// unread writes an event about issueID in project and the reader's unread
// feed row of it, carded to card.
func (e *mwvEnv) unread(t *testing.T, project uuid.UUID, issueID, card string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
    VALUES (now(), 'issue.status_changed', 'issue', $3, $2, 'tester', $4, 'human', '{}'::jsonb)
    RETURNING id
)
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_issue_id, card_resolved)
SELECT $1, ev.id, 'issue.status_changed', $2, true, $5, true FROM ev`,
		e.reader, project, issueID, e.owner, card); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
}

func (e *mwvEnv) removeFrom(t *testing.T, project uuid.UUID) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, project, e.reader); err != nil {
		t.Fatalf("remove the reader from %s: %v", project, err)
	}
}

// get serves path through the two routes as the reader, under the key
// scope sc (nil — no key), and returns the body.
func (e *mwvEnv) get(t *testing.T, sc *apikeys.Scope, path string) []byte {
	t.Helper()
	r := chi.NewRouter()
	r.Use(handler.ParseUserHeaders)
	r.Use(handler.RequireUser)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			if sc != nil {
				ctx = auth.WithAPIKeyID(ctx, uuid.New())
				ctx = auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelManage, Scope: *sc})
			}
			next.ServeHTTP(w, req.WithContext(authz.WithRequestCache(ctx)))
		})
	})
	meH := handler.NewMeHandler(e.pool, issues.NewStore(e.pool), notifications.NewStore(e.pool))
	r.Get("/ui/v1/me/watching", meH.GetWatching)
	r.Get("/ui/v1/me/notification-counts", meH.NotificationCounts)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(auth.HeaderUserID, e.reader.String())
	req.Header.Set(auth.HeaderUsername, e.name)
	req.Header.Set(auth.HeaderUserRole, "user")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// totals returns unreadTotal of the column and the Watching badge.
func (e *mwvEnv) totals(t *testing.T, sc *apikeys.Scope) (column, badge int) {
	t.Helper()
	var w struct {
		UnreadTotal int `json:"unreadTotal"`
	}
	if err := json.Unmarshal(e.get(t, sc, "/ui/v1/me/watching"), &w); err != nil {
		t.Fatalf("decode watching: %v", err)
	}
	var c struct {
		Watching int `json:"watching"`
	}
	if err := json.Unmarshal(e.get(t, sc, "/ui/v1/me/notification-counts"), &c); err != nil {
		t.Fatalf("decode counts: %v", err)
	}
	return w.UnreadTotal, c.Watching
}

func projectsScope(ids ...uuid.UUID) *apikeys.Scope {
	return &apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: ids}
}

func spacesScope(ids ...uuid.UUID) *apikeys.Scope {
	return &apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: ids}
}

func allScope() *apikeys.Scope { return &apikeys.Scope{Mode: apikeys.ScopeAll} }

// TestMeWatching_UnreadTotalEqualsTheBadge — No. 12, INV-3 / SC-001.
//
// Corpus: A (P, shared space) is watched with its subtree, C (Q, a space of
// its own) is watched. Unread rows: two of P on A, one of Q on A (an event
// of A's descendant B in Q), one of Q on C. Each state names the rows of the
// reader's set — the expected value of both numbers.
//
// The second corpus is No. 8's topology: H-1 (H) watched with its subtree,
// an unread row of P-9 (P) carded to H-1, and the reader removed from H.
func TestMeWatching_UnreadTotalEqualsTheBadge(t *testing.T) {
	t.Run("column and badge", func(t *testing.T) {
		e := newMWVEnv(t)
		qSpace := testsupport.SeedSpace(t, e.pool, e.owner)
		p, pPrefix := e.project(t, uuid.Nil, true)
		q, qPrefix := e.project(t, qSpace, true)
		a := e.issue(t, p, pPrefix)
		b := e.issue(t, q, qPrefix)
		c := e.issue(t, q, qPrefix)
		testsupport.SeedDep(t, e.pool, b, a, "parent-child", e.owner)
		e.watch(t, a, "subtree")
		e.watch(t, c, "issue")
		e.unread(t, p, a, a)
		e.unread(t, p, a, a)
		e.unread(t, q, b, a)
		e.unread(t, q, c, c)

		for _, st := range []struct {
			name string
			sc   *apikeys.Scope
			want int
		}{
			{"no key", nil, 4},
			{"key projects=[P]", projectsScope(p), 2},
			{"key spaces=[space of Q]", spacesScope(qSpace), 2},
		} {
			column, badge := e.totals(t, st.sc)
			if column != st.want || badge != st.want {
				t.Errorf("%s: unreadTotal %d, badge %d; want both %d", st.name, column, badge, st.want)
			}
		}

		e.removeFrom(t, q)
		if column, badge := e.totals(t, nil); column != 2 || badge != 2 {
			t.Errorf("after losing Q: unreadTotal %d, badge %d; want both 2 (the rows of P)", column, badge)
		}
	})

	t.Run("ancestor in a project the reader cannot see", func(t *testing.T) {
		e := newMWVEnv(t)
		p, pPrefix := e.project(t, uuid.Nil, true)
		h, hPrefix := e.project(t, uuid.Nil, true)
		h1 := e.issue(t, h, hPrefix)
		p9 := e.issue(t, p, pPrefix)
		testsupport.SeedDep(t, e.pool, p9, h1, "parent-child", e.owner)
		e.watch(t, h1, "subtree")
		e.unread(t, p, p9, h1)
		e.removeFrom(t, h)

		for _, st := range []struct {
			name string
			sc   *apikeys.Scope
		}{
			{"no key", nil},
			{"key projects=[P]", projectsScope(p)},
		} {
			column, badge := e.totals(t, st.sc)
			if column != 1 || badge != 1 {
				t.Errorf("%s: unreadTotal %d, badge %d; want both 1 (the row of P-9)", st.name, column, badge)
			}
		}
	})
}

// cardParents returns, per card id, the card item's parentIdCurrent and
// whether the key is present.
func cardParents(t *testing.T, body []byte) map[string]*string {
	t.Helper()
	var w struct {
		Items []struct {
			Item map[string]json.RawMessage `json:"item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode watching: %v", err)
	}
	out := map[string]*string{}
	for _, it := range w.Items {
		var id string
		if err := json.Unmarshal(it.Item["id"], &id); err != nil {
			t.Fatalf("decode card id: %v", err)
		}
		raw, ok := it.Item["parentIdCurrent"]
		if !ok {
			out[id] = nil
			continue
		}
		var parent string
		if err := json.Unmarshal(raw, &parent); err != nil {
			t.Fatalf("decode parentIdCurrent of %s: %v", id, err)
		}
		out[id] = &parent
	}
	return out
}

// TestMeWatching_CardParentOnlyWhenVisible — No. 13, US-1 "the card's
// parent", SC-002, K9.
//
// (a) Without a key: P-2's parent H-1 lies in H, which the reader does not
// see — no parent key, no "H-1" in the body; P-3's parent V-1 lies in
// another project the reader sees — given in full.
// (b) R-1 lies in R, visible by the relation; P-4 (P) is its child. Under a
// key of scope projects=[P] the parent is not given and "R-1" is not in the
// body; under a key of scope all it is R-1 (control).
func TestMeWatching_CardParentOnlyWhenVisible(t *testing.T) {
	e := newMWVEnv(t)
	p, pPrefix := e.project(t, uuid.Nil, true)
	h, hPrefix := e.project(t, uuid.Nil, false)
	v, vPrefix := e.project(t, uuid.Nil, true)
	r, rPrefix := e.project(t, uuid.Nil, true)
	h1 := e.issue(t, h, hPrefix)
	v1 := e.issue(t, v, vPrefix)
	r1 := e.issue(t, r, rPrefix)
	p2 := e.issue(t, p, pPrefix)
	p3 := e.issue(t, p, pPrefix)
	p4 := e.issue(t, p, pPrefix)
	testsupport.SeedDep(t, e.pool, p2, h1, "parent-child", e.owner)
	testsupport.SeedDep(t, e.pool, p3, v1, "parent-child", e.owner)
	testsupport.SeedDep(t, e.pool, p4, r1, "parent-child", e.owner)
	for _, id := range []string{p2, p3, p4} {
		e.watch(t, id, "issue")
	}

	t.Run("hidden project", func(t *testing.T) {
		body := e.get(t, nil, "/ui/v1/me/watching")
		parents := cardParents(t, body)
		if got, ok := parents[p2]; !ok || got != nil {
			t.Errorf("card %s: parentIdCurrent %v (present %v), want no key — its parent lies in a project the reader does not see", p2, got, ok)
		}
		if bytes.Contains(body, []byte(`"`+h1+`"`)) {
			t.Errorf("the body names %q, a task the reader does not see", h1)
		}
		if got := parents[p3]; got == nil || *got != v1 {
			t.Errorf("card %s: parentIdCurrent %v, want %s (a parent in another visible project)", p3, got, v1)
		}
	})

	t.Run("outside the key's scope", func(t *testing.T) {
		body := e.get(t, projectsScope(p), "/ui/v1/me/watching")
		if got, ok := cardParents(t, body)[p4]; !ok || got != nil {
			t.Errorf("card %s under key projects=[P]: parentIdCurrent %v (present %v), want no key", p4, got, ok)
		}
		if bytes.Contains(body, []byte(`"`+r1+`"`)) {
			t.Errorf("the body under key projects=[P] names %q, outside the key's scope", r1)
		}
		if got := cardParents(t, e.get(t, allScope(), "/ui/v1/me/watching"))[p4]; got == nil || *got != r1 {
			t.Errorf("card %s under key all: parentIdCurrent %v, want %s (control)", p4, got, r1)
		}
	})
}
