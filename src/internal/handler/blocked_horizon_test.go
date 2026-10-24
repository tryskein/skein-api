package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// sk-423 through REST: the card, the project list and its isBlocked filter,
// the cross-project list and its filter, and both tree routes with and
// without includeBlockers. For a reader outside the blocker's project the
// response must not depend on whether the hidden blocker is closed — every
// hidden status is compared with the open one on every field the rule
// touches. On the code before sk-423 the open case reads «blocked».

var bhRESTStatuses = []string{"open", "in_progress", "deferred", "closed", "standing"}

type bhRESTFixture struct {
	pool      *pgxpool.Pool
	h         *IssuesHandler
	all       *IssuesAllHandler
	pID       uuid.UUID
	subjectOf map[string]string // hidden blocker status → subject in P
	visOpen   string
	visBy     string
	readers   []bhRESTReader
}

type bhRESTReader struct {
	name     string
	user     uuid.UUID
	external bool
}

func newBHRESTFixture(t *testing.T) *bhRESTFixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	pOwner, _ := testsupport.SeedUser(t, pool, "user")
	qOwner, _ := testsupport.SeedUser(t, pool, "user")
	pID, pPrefix := testsupport.SeedProject(t, pool, pOwner, "bhrp")
	qID, qPrefix := testsupport.SeedProject(t, pool, qOwner, "bhrq")
	ext, _ := testsupport.SeedUser(t, pool, "user")
	testsupport.AddProjectMember(t, pool, pID, ext, "member")

	f := &bhRESTFixture{
		pool:      pool,
		h:         NewIssuesHandler(pool, projects.NewStore(pool), dependencies.NewStore(pool)),
		all:       NewIssuesAllHandler(pool, issues.NewStore(pool), nil),
		pID:       pID,
		subjectOf: make(map[string]string, len(bhRESTStatuses)),
		readers: []bhRESTReader{
			{name: "member of P, not of Q", user: pOwner},
			{name: "external member of P", user: ext, external: true},
		},
	}
	for k, st := range bhRESTStatuses {
		subject := testsupport.SeedIssue(t, pool, pID, pOwner, fmt.Sprintf("%s-%d", pPrefix, 10+k), "bh subject", "open", 2)
		blocker := testsupport.SeedIssue(t, pool, qID, qOwner, fmt.Sprintf("%s-%d", qPrefix, 10+k), "bh hidden blocker", st, 2)
		testsupport.SeedDep(t, pool, subject, blocker, dependencies.DepBlocks, pOwner)
		f.subjectOf[st] = subject
	}
	f.visOpen = testsupport.SeedIssue(t, pool, pID, pOwner, pPrefix+"-1", "bh visible subject", "open", 2)
	f.visBy = testsupport.SeedIssue(t, pool, pID, pOwner, pPrefix+"-2", "bh visible blocker", "in_progress", 2)
	testsupport.SeedDep(t, pool, f.visOpen, f.visBy, dependencies.DepBlocks, pOwner)
	return f
}

// call runs one REST handler as r and decodes the body.
func (f *bhRESTFixture) call(t *testing.T, r bhRESTReader, h http.HandlerFunc, query string, params ...string) map[string]any {
	t.Helper()
	url := "/bh"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	for i := 0; i+1 < len(params); i += 2 {
		rctx.URLParams.Add(params[i], params[i+1])
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, r.user)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithIsExternal(ctx, r.external)
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: status %d: %s", r.name, url, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return body
}

func bhPick(m map[string]any, keys ...string) string {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		} else {
			out[k] = "<absent>"
		}
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func bhItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		out = append(out, it.(map[string]any))
	}
	return out
}

func bhRESTIndistinguishable(t *testing.T, surface string, project func(status string) string) {
	t.Helper()
	ref := project(bhRESTStatuses[0])
	for _, st := range bhRESTStatuses[1:] {
		if got := project(st); got != ref {
			t.Errorf("%s: hidden blocker open answers %s, hidden blocker %s answers %s — "+
				"the response reveals the hidden issue's status", surface, ref, st, got)
		}
	}
}

func TestBlockedHorizonREST_Card(t *testing.T) {
	f := newBHRESTFixture(t)
	for _, r := range f.readers {
		t.Run(r.name, func(t *testing.T) {
			bhRESTIndistinguishable(t, "REST card", func(st string) string {
				body := f.call(t, r, f.h.Get, "", "projectID", f.pID.String(), "issueID", f.subjectOf[st])
				return bhPick(body, "isBlocked", "blockers", "unavailableBlockingLinks")
			})
			body := f.call(t, r, f.h.Get, "", "projectID", f.pID.String(), "issueID", f.subjectOf["open"])
			if got := bhPick(body, "isBlocked", "blockers", "unavailableBlockingLinks"); got !=
				`{"blockers":[],"isBlocked":false,"unavailableBlockingLinks":1}` {
				t.Errorf("REST card, hidden open blocker: %s", got)
			}
			vis := f.call(t, r, f.h.Get, "", "projectID", f.pID.String(), "issueID", f.visOpen)
			want := fmt.Sprintf(`{"blockers":[{"id":%q,"status":"in_progress","title":"bh visible blocker"}],"isBlocked":true,"unavailableBlockingLinks":0}`, f.visBy)
			if got := bhPick(vis, "isBlocked", "blockers", "unavailableBlockingLinks"); got != want {
				t.Errorf("REST card, visible blocker: %s; want %s", got, want)
			}
		})
	}
}

func TestBlockedHorizonREST_Lists(t *testing.T) {
	f := newBHRESTFixture(t)
	lists := map[string]func(r bhRESTReader, query string) map[string]any{
		"project list": func(r bhRESTReader, query string) map[string]any {
			return f.call(t, r, f.h.List, query, "projectID", f.pID.String())
		},
		"cross-project list": func(r bhRESTReader, query string) map[string]any {
			return f.call(t, r, f.all.List, query)
		},
	}
	for _, r := range f.readers {
		for name, list := range lists {
			t.Run(r.name+"/"+name, func(t *testing.T) {
				rows := map[string]map[string]any{}
				for _, it := range bhItems(t, list(r, "limit=200")) {
					rows[it["id"].(string)] = it
				}
				member := func(query string) map[string]bool {
					out := map[string]bool{}
					for _, it := range bhItems(t, list(r, query)) {
						out[it["id"].(string)] = true
					}
					return out
				}
				blocked, unblocked := member("limit=200&isBlocked=true"), member("limit=200&isBlocked=false")
				bhRESTIndistinguishable(t, name, func(st string) string {
					id := f.subjectOf[st]
					row, ok := rows[id]
					if !ok {
						t.Fatalf("subject %s missing from the list", id)
					}
					return fmt.Sprintf("%s filter(true)=%v filter(false)=%v",
						bhPick(row, "isBlocked"), blocked[id], unblocked[id])
				})
				if !blocked[f.visOpen] || unblocked[f.visOpen] || rows[f.visOpen]["isBlocked"] != true {
					t.Errorf("%s: a visibly blocked subject must read blocked and pass only isBlocked=true", name)
				}
			})
		}
	}
}

func TestBlockedHorizonREST_Trees(t *testing.T) {
	f := newBHRESTFixture(t)
	for _, r := range f.readers {
		for _, include := range []string{"", "&includeBlockers=true"} {
			for _, route := range []string{"project tree", "cross-project tree"} {
				t.Run(fmt.Sprintf("%s/%s/%q", r.name, route, include), func(t *testing.T) {
					bhRESTIndistinguishable(t, route, func(st string) string {
						query := "rootId=" + f.subjectOf[st] + include
						var body map[string]any
						if route == "project tree" {
							body = f.call(t, r, f.h.Tree, query, "projectID", f.pID.String())
						} else {
							body = f.call(t, r, f.all.Tree, query)
						}
						items := bhItems(t, body)
						if len(items) == 0 {
							t.Fatalf("empty tree")
						}
						root := items[0]
						item, _ := root["item"].(map[string]any)
						return bhPick(item, "isBlocked") + bhPick(root, "blockers", "unavailableBlockingLinks")
					})
				})
			}
		}
	}
}
