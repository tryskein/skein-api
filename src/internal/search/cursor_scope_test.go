package search_test

// cursor_scope_test.go — sk-674.18, the test delivery of rows 137–139 (T031,
// E-18, K-5, US-13). The references are E-18 and the acceptance of US-13 as
// the spec words them, on the fixture legend of testsupport.SeedTenancy,
// written before the code:
//
//   - 137: a search cursor serves a page only to the user it was issued to,
//     for the scope it was issued for. Another user's cursor — verbatim, or
//     re-encoded around the stolen key — and the caller's own cursor issued
//     for another area (his visible set changed since) answer as an invalid
//     cursor; the caller's
//     own cursor gives the next page (the positive control). The other user
//     here sees the very same project, so only the ownership of the cache
//     entry can tell the two apart.
//   - 138: when access is lost between pages, the next page carries nothing
//     of the project no longer accessible, and neither does a new search.
//   - 139: `/search?space=` and the MCP `skein_search` with a space narrow
//     the same way, and their rows carry `spaceId` and the project name —
//     the label «Space · Project».

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/projects"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// seedTokenIssues seeds n issues of project p whose title carries tok.
func seedTokenIssues(t *testing.T, pool *pgxpool.Pool, p testsupport.TenancyProject, tok string, from, n int) []string {
	t.Helper()
	var ids []string
	for i := from; i < from+n; i++ {
		id := fmt.Sprintf("%s-%d", p.IDPrefix, i)
		testsupport.SeedIssue(t, pool, p.ID, p.OwnerID, id, tok+" scoped item", "open", 2)
		ids = append(ids, id)
	}
	return ids
}

func scopeSvc(t *testing.T) *search.Service {
	t.Helper()
	svc, err := search.NewService(search.NewStore(testsupport.NewTestPool(t)), nil, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestCursorScope_CursorServesOnlyItsOwnerAndScope — row 137.
func TestCursorScope_CursorServesOnlyItsOwnerAndScope(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	svc := scopeSvc(t)
	tok := token(t)
	seedTokenIssues(t, pool, f.ASpace, tok, 50, 5)
	owner := f.User(t, testsupport.ActorOnlyA)
	other := f.User(t, testsupport.ActorBoth) // sees A.space too
	base := search.Params{Query: tok, Kinds: []search.Kind{search.KindIssue}, Limit: 2}

	mine := base
	mine.CallerID = owner.ID
	page1, err := svc.Search(context.Background(), mine)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if page1.NextCursor == nil {
		t.Fatalf("no cursor after the first page (%d items) — the fixture proves nothing", len(page1.Items))
	}
	cursor := *page1.NextCursor
	key, _, err := search.DecodeCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("own cursor gives the next page", func(t *testing.T) {
		p := mine
		p.Cursor = cursor
		page2, err := svc.Search(context.Background(), p)
		if err != nil {
			t.Fatalf("own cursor refused: %v", err)
		}
		if len(page2.Items) == 0 {
			t.Errorf("own cursor gave an empty page")
		}
	})
	refused := func(t *testing.T, p search.Params) {
		t.Helper()
		res, err := svc.Search(context.Background(), p)
		if !errors.Is(err, search.ErrInvalidCursor) {
			t.Errorf("want invalid cursor, got %v (items %d)", err, func() int {
				if res == nil {
					return 0
				}
				return len(res.Items)
			}())
		}
	}
	t.Run("another user's cursor verbatim", func(t *testing.T) {
		p := base
		p.CallerID = other.ID
		p.Cursor = cursor
		refused(t, p)
	})
	t.Run("another user's key re-encoded with another offset", func(t *testing.T) {
		forged, err := search.EncodeCursor(key, 1)
		if err != nil {
			t.Fatal(err)
		}
		p := base
		p.CallerID = other.ID
		p.Cursor = forged
		refused(t, p)
	})
	t.Run("own cursor issued for another area", func(t *testing.T) {
		// The area is the caller's visible set (K-5): it changes when he
		// gains a project, and the cursor issued before is not his any more.
		testsupport.AddProjectMember(t, pool, f.BMembers.ID, owner.ID, "member")
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, f.BMembers.ID, owner.ID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM space_members WHERE space_id = $1 AND user_id = $2`, f.SpaceB, owner.ID)
		})
		p := mine
		p.Cursor = cursor
		refused(t, p)
	})
}

// TestCursorScope_LostAccessBetweenPages — row 138.
func TestCursorScope_LostAccessBetweenPages(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	svc := scopeSvc(t)
	tok := token(t)
	lost := seedTokenIssues(t, pool, f.AMembers, tok, 50, 5)
	member := f.User(t, testsupport.ActorOnlyA)
	p := search.Params{Query: tok, Kinds: []search.Kind{search.KindIssue}, Limit: 2, CallerID: member.ID}
	page1, err := svc.Search(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Items) == 0 || page1.NextCursor == nil {
		t.Fatalf("the member's first page is %d items without a cursor — the fixture proves nothing", len(page1.Items))
	}

	if _, err := pool.Exec(context.Background(),
		`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, f.AMembers.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
VALUES ($1, $2, 'member', now(), $3, true, false) ON CONFLICT DO NOTHING`, f.AMembers.ID, member.ID, f.AMembers.OwnerID)
	})

	next := p
	next.Cursor = *page1.NextCursor
	page2, err := svc.Search(context.Background(), next)
	if err != nil && !errors.Is(err, search.ErrInvalidCursor) {
		t.Fatalf("next page: %v", err)
	}
	if page2 != nil {
		for _, it := range page2.Items {
			if slices.Contains(lost, it.ID) {
				t.Errorf("the next page after losing access carries %s", it.ID)
			}
		}
	}
	fresh, err := svc.Search(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range fresh.Items {
		if slices.Contains(lost, it.ID) {
			t.Errorf("a new search after losing access carries %s", it.ID)
		}
	}
}

// TestCursorScope_SpaceNarrowsRESTAndMCPAlike — row 139.
func TestCursorScope_SpaceNarrowsRESTAndMCPAlike(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	svc := scopeSvc(t)
	tok := token(t)
	inA := seedTokenIssues(t, pool, f.ASpace, tok, 50, 2)
	inB := seedTokenIssues(t, pool, f.BSpace, tok, 50, 2)
	caller := f.User(t, testsupport.ActorBoth)
	shortOf := func(id uuid.UUID) string {
		var s string
		if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	r := chi.NewRouter()
	r.Use(handler.ParseUserHeaders)
	r.Use(handler.RequireUser)
	r.Get("/api/v1/search", handler.NewSearchHandler(svc).Search)
	mcp := server.NewMCPServer("t", "0", server.WithToolCapabilities(true))
	tools.RegisterSearch(mcp, pool, svc, projects.NewStore(pool))

	type row struct {
		ID          string  `json:"id"`
		SpaceID     string  `json:"spaceId"`
		ProjectName *string `json:"projectName"`
	}
	rest := func(t *testing.T, space string) []row {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/search?kind=issue&limit=50&q="+url.QueryEscape(tok)+"&space="+space, nil)
		req.Header.Set(auth.HeaderUserID, caller.ID.String())
		req.Header.Set(auth.HeaderUsername, caller.Username)
		req.Header.Set(auth.HeaderUserRole, caller.Role)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("REST search: %d %s", rec.Code, rec.Body.String())
		}
		var res struct {
			Items []row `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res.Items
	}
	viaMCP := func(t *testing.T, space string) []row {
		t.Helper()
		ctx := auth.WithUserID(context.Background(), caller.ID)
		ctx = auth.WithUserRole(ctx, caller.Role)
		ctx = auth.WithAPIKeyID(ctx, uuid.New())
		msg, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "skein_search", "arguments": map[string]any{
				"query": tok, "space": space, "kinds": []string{"issue"}, "limit": 50,
			}},
		})
		out, _ := json.Marshal(mcp.HandleMessage(ctx, msg))
		var envelope struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &envelope); err != nil || len(envelope.Result.Content) == 0 {
			t.Fatalf("MCP search envelope %s: %v", out, err)
		}
		if envelope.Result.IsError {
			t.Fatalf("MCP search error: %s", envelope.Result.Content[0].Text)
		}
		var res struct {
			Items []row `json:"items"`
		}
		if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &res); err != nil {
			t.Fatalf("MCP search body %s: %v", envelope.Result.Content[0].Text, err)
		}
		return res.Items
	}
	ids := func(rows []row) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		sort.Strings(out)
		return out
	}

	for space, want := range map[string][]string{shortOf(f.SpaceA): inA, shortOf(f.SpaceB): inB} {
		t.Run(space, func(t *testing.T) {
			sort.Strings(want)
			restRows, mcpRows := rest(t, space), viaMCP(t, space)
			if got := ids(restRows); !slices.Equal(got, want) {
				t.Errorf("REST space=%s: %v, want %v", space, got, want)
			}
			if got := ids(mcpRows); !slices.Equal(got, ids(restRows)) {
				t.Errorf("MCP space=%s: %v, REST: %v — the two surfaces narrow differently", space, got, ids(restRows))
			}
			for _, rows := range [][]row{restRows, mcpRows} {
				for _, it := range rows {
					if it.SpaceID != space || it.ProjectName == nil || *it.ProjectName == "" {
						t.Errorf("row %s: spaceId %q projectName %v, want %q and the project's name", it.ID, it.SpaceID, it.ProjectName, space)
					}
				}
			}
		})
	}
}

// TestCursorScope_IDLookupHonoursSpace — row 139 over the id fast path
// Reference — US-13 and the T031
// contract as the spec words them: space= narrows every row of the page to
// one space, and an unknown space matches nothing — whatever shape the
// query has. A query that looks like an id (an issue code, a project
// prefix, a bare number) runs the fast path beside the kind searches;
// through REST and MCP alike it finds nothing of another space and nothing
// under an unknown space, and still finds the row of the space asked for
// (the positive control). A cursor continues only the query it was issued
// for: the cursor of space=A under space=B is an invalid cursor.
func TestCursorScope_IDLookupHonoursSpace(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	svc := scopeSvc(t)
	tok := token(t)
	inA := seedTokenIssues(t, pool, f.ASpace, tok, 70, 3)
	inB := seedTokenIssues(t, pool, f.BSpace, tok, 80, 1)
	caller := f.User(t, testsupport.ActorBoth)
	shortOf := func(id uuid.UUID) string {
		var s string
		if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	spaceA, spaceB := shortOf(f.SpaceA), shortOf(f.SpaceB)

	r := chi.NewRouter()
	r.Use(handler.ParseUserHeaders)
	r.Use(handler.RequireUser)
	r.Get("/api/v1/search", handler.NewSearchHandler(svc).Search)
	mcp := server.NewMCPServer("t", "0", server.WithToolCapabilities(true))
	tools.RegisterSearch(mcp, pool, svc, projects.NewStore(pool))

	type row struct {
		ID string `json:"id"`
	}
	rest := func(t *testing.T, q, space string) []string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/search?kind=issue,project&limit=50&q="+url.QueryEscape(q)+"&space="+space, nil)
		req.Header.Set(auth.HeaderUserID, caller.ID.String())
		req.Header.Set(auth.HeaderUsername, caller.Username)
		req.Header.Set(auth.HeaderUserRole, caller.Role)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("REST search %q: %d %s", q, rec.Code, rec.Body.String())
		}
		var res struct {
			Items []row `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range res.Items {
			out = append(out, it.ID)
		}
		return out
	}
	viaMCP := func(t *testing.T, q, space string) []string {
		t.Helper()
		ctx := auth.WithUserID(context.Background(), caller.ID)
		ctx = auth.WithUserRole(ctx, caller.Role)
		ctx = auth.WithAPIKeyID(ctx, uuid.New())
		msg, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "skein_search", "arguments": map[string]any{
				"query": q, "space": space, "kinds": []string{"issue", "project"}, "limit": 50,
			}},
		})
		out, _ := json.Marshal(mcp.HandleMessage(ctx, msg))
		var envelope struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &envelope); err != nil || len(envelope.Result.Content) == 0 {
			t.Fatalf("MCP search envelope %s: %v", out, err)
		}
		if envelope.Result.IsError {
			t.Fatalf("MCP search error: %s", envelope.Result.Content[0].Text)
		}
		var res struct {
			Items []row `json:"items"`
		}
		if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &res); err != nil {
			t.Fatalf("MCP search body %s: %v", envelope.Result.Content[0].Text, err)
		}
		var ids []string
		for _, it := range res.Items {
			ids = append(ids, it.ID)
		}
		return ids
	}

	queries := map[string]struct {
		q      string
		target string // the row of B the query finds
	}{
		"issue code":     {inB[0], inB[0]},
		"project prefix": {f.BSpace.IDPrefix, f.BSpace.ID.String()},
		"bare number":    {"80", inB[0]},
	}
	for name, c := range queries {
		for ch, search := range map[string]func(*testing.T, string, string) []string{"REST": rest, "MCP": viaMCP} {
			t.Run(name+" "+ch, func(t *testing.T) {
				if got := search(t, c.q, spaceA); slices.Contains(got, c.target) {
					t.Errorf("space=A finds %s of space B: %v", c.target, got)
				}
				if got := search(t, c.q, "zz-no-such-space"); len(got) != 0 {
					t.Errorf("an unknown space finds %v, want nothing", got)
				}
				if got := search(t, c.q, spaceB); !slices.Contains(got, c.target) {
					t.Errorf("space=B misses %s (positive control): %v", c.target, got)
				}
			})
		}
	}

	t.Run("a cursor continues only its own query", func(t *testing.T) {
		base := search.Params{Query: tok, Kinds: []search.Kind{search.KindIssue}, Limit: 1, CallerID: caller.ID, Space: spaceA}
		page1, err := svc.Search(context.Background(), base)
		if err != nil {
			t.Fatal(err)
		}
		if page1.NextCursor == nil {
			t.Fatalf("no cursor after the first page of %d A issues — the fixture proves nothing", len(inA))
		}
		same := base
		same.Cursor = *page1.NextCursor
		if _, err := svc.Search(context.Background(), same); err != nil {
			t.Fatalf("the cursor under its own query refused (positive control): %v", err)
		}
		other := same
		other.Space = spaceB
		page, err := svc.Search(context.Background(), other)
		if !errors.Is(err, search.ErrInvalidCursor) {
			var got []string
			if page != nil {
				for _, it := range page.Items {
					got = append(got, it.ID)
				}
			}
			t.Errorf("the cursor of space=A under space=B: err %v, rows %v — want an invalid cursor", err, got)
		}
	})
}
