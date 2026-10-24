package tools

import (
	"context"
	"strconv"
	"testing"
	"time"

	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/projects"
	"skein-api/internal/search"
)

func TestSearch_Smoke(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "srch")
	for i := 1; i <= 2; i++ {
		seedIssue(t, pool, pid, userID, prefix+"-"+strconv.Itoa(i), "alpha bravo charlie", "open", 2)
	}

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}

	pStore := projects.NewStore(pool)
	handler := handleSearch(pool, svc, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query": "alpha",
		"limit": float64(10),
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	// Nil embedder → service runs degraded — just assert items is shaped
	// like an array (possibly empty; FTS depends on embedding column
	// having been backfilled, which doesn't apply for degraded mode).
	if _, ok := body["items"].([]any); !ok {
		t.Fatalf("expected items array, got %T", body["items"])
	}
}

func TestSearch_ValidationEmptyQuery(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	pStore := projects.NewStore(pool)
	handler := handleSearch(pool, svc, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "   "}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for empty query")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error code = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestSearch_StatusesStanding asserts that skein_search accepts
// statuses=["standing"]: the value passes the wire-level enum schema AND
// the ValidateStatus pass in the handler (an invalid status would surface
// validation_error). The query reaches the search service and returns a
// well-shaped items array. (Degraded mode — nil embedder — may return an
// empty set, exactly like TestSearch_Smoke; the assertion here is that
// `standing` is a recognised filter value, not rejected.)
func TestSearch_StatusesStanding(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "sstand")
	seedIssue(t, pool, pid, userID, prefix+"-1", "delta echo foxtrot", "standing", 2)

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	pStore := projects.NewStore(pool)
	handler := handleSearch(pool, svc, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query":    "delta",
		"kinds":    []any{"issue"},
		"statuses": []any{"standing"},
		"limit":    float64(10),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true (statuses=standing should validate): %+v",
			extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if _, ok := body["items"].([]any); !ok {
		t.Fatalf("expected items array, got %T", body["items"])
	}
}

// seedCommentKind inserts a comment carrying the token in its body with an
// explicit author_kind, for the comment-author-kind filter tests. Direct
// insert (the tools test package has no SeedComment helper); the generated
// tsv columns populate from body so the comment is immediately FTS-hittable.
func seedCommentKind(t *testing.T, pool *pgxpool.Pool, issueID string, author uuid.UUID, body, kind string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', $4, $5, now(), now())`,
		id, issueID, author, kind, body)
	if err != nil {
		t.Fatalf("seed comment kind=%s on %s: %v", kind, issueID, err)
	}
	return id
}

// TestSearch_CommentAuthorKindsFilter exercises the MCP door: a
// commentAuthorKinds=["human"] arg restricts COMMENT results to the human
// comment, excluding the agent_autonomous one, both sharing the token.
func TestSearch_CommentAuthorKindsFilter(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "scak")
	tok := "zcak" + uuid.Must(uuid.NewRandom()).String()[:8]

	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "host issue", "open", 2)
	humanID := seedCommentKind(t, pool, issueID, userID, "human note "+tok, "human")
	autoID := seedCommentKind(t, pool, issueID, userID, "agent note "+tok, "agent_autonomous")

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	pStore := projects.NewStore(pool)
	handler := handleSearch(pool, svc, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query":              tok,
		"project":            pid.String(),
		"kinds":              []any{"comment"},
		"commentAuthorKinds": []any{"human"},
		"limit":              float64(50),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items not an array: %T", body["items"])
	}
	ids := map[string]bool{}
	for _, raw := range items {
		m := raw.(map[string]any)
		ids[m["id"].(string)] = true
	}
	if !ids[humanID.String()] {
		t.Errorf("human comment %s missing under human filter; ids=%v", humanID, ids)
	}
	if ids[autoID.String()] {
		t.Errorf("agent_autonomous comment %s leaked under human filter; ids=%v", autoID, ids)
	}
}

// TestSearch_InvalidCommentAuthorKind asserts the MCP door rejects an
// unknown commentAuthorKinds value with a validation_error.
func TestSearch_InvalidCommentAuthorKind(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	pStore := projects.NewStore(pool)
	handler := handleSearch(pool, svc, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query":              "anything",
		"commentAuthorKinds": []any{"banana"},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for invalid commentAuthorKinds")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error code = %v, want %s", got, ErrCodeValidationError)
	}
}

func TestSearch_InvalidKind(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	pStore := projects.NewStore(pool)
	handler := handleSearch(pool, svc, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query": "anything",
		"kinds": []any{"banana"},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for invalid kind")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error code = %v, want %s", got, ErrCodeValidationError)
	}
}

// ---------------------------------------------------------------------
// sk-282 T015 — the search projection (test-plan rows 60 and 61). Row 62
// is the REST side and lives with the REST handler.
// ---------------------------------------------------------------------

// searchEnv bundles a seeded project plus a wired MCP search handler.
type searchEnv struct {
	pool    *pgxpool.Pool
	userID  uuid.UUID
	pid     uuid.UUID
	prefix  string
	handler server.ToolHandlerFunc
	svc     *search.Service
}

func newSearchEnv(t *testing.T, hint string) searchEnv {
	t.Helper()
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, hint)

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	return searchEnv{
		pool:    pool,
		userID:  userID,
		pid:     pid,
		prefix:  prefix,
		handler: handleSearch(pool, svc, projects.NewStore(pool)),
		svc:     svc,
	}
}

func (e searchEnv) search(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := e.handler(withCtx(t, e.userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

// row 60 — SC-004: no markup in either form, the matched text still
// there, and a default page of at most 25.
func TestSearch_SnippetHasNoHighlightMarkup(t *testing.T) {
	env := newSearchEnv(t, "srchmk")

	// A body long enough that ts_headline really produces a fragment, and
	// containing the term in the middle so a `<b>` would land inside it.
	const term = "zamboni"
	body := "prelude words before the match, then " + term + ", then words after the match"
	for i := 1; i <= 3; i++ {
		id := env.prefix + "-" + strconv.Itoa(i)
		seedIssue(t, env.pool, env.pid, env.userID, id, "search markup fixture", "open", 2)
		if _, err := env.pool.Exec(context.Background(),
			`UPDATE issues SET description = $2 WHERE id = $1`, id, body); err != nil {
			t.Fatalf("set description: %v", err)
		}
	}

	res := env.search(t, map[string]any{"query": term})
	items, _ := res["items"].([]any)
	if len(items) == 0 {
		t.Fatal("search returned nothing — the fixture does not exercise the snippet path")
	}

	sawTerm := false
	for _, raw := range items {
		snippet, _ := raw.(map[string]any)["snippet"].(string)
		for _, bad := range []string{"<b>", "</b>", "&lt;b&gt;", "&lt;/b&gt;"} {
			if strings.Contains(snippet, bad) {
				t.Errorf("snippet carries highlight markup %q — SC-004 says neither in original "+
					"nor in escaped form; an agent renders nothing and pays for the "+
					"tags:\n%s", bad, snippet)
			}
		}
		if strings.Contains(snippet, term) {
			sawTerm = true
		}
	}
	if !sawTerm {
		t.Error("no snippet contains the matched term — stripping the tags must leave the " +
			"words; a snippet reduced to «…» would cost nothing and say nothing")
	}
}

// row 60, second half — the default page size.
func TestSearch_DefaultPageIsTwentyFive(t *testing.T) {
	env := newSearchEnv(t, "srchpg")
	for i := 1; i <= SearchLimitDefault+6; i++ {
		seedIssue(t, env.pool, env.pid, env.userID, env.prefix+"-"+strconv.Itoa(i),
			"quokka sighting "+strconv.Itoa(i), "open", 2)
	}

	res := env.search(t, map[string]any{"query": "quokka"})
	items, _ := res["items"].([]any)
	if len(items) > SearchLimitDefault {
		t.Errorf("default search page returned %d items, want at most %d",
			len(items), SearchLimitDefault)
	}
	if len(items) != SearchLimitDefault {
		t.Errorf("default search page returned %d items, want exactly %d — the fixture has "+
			"more matches than the page", len(items), SearchLimitDefault)
	}
}

// row 61 — US-6 Unchanged: the cross-project identity fields survive, and
// so does the order the service produced.
func TestSearch_CrossProjectKeepsProjectAndOrder(t *testing.T) {
	env := newSearchEnv(t, "srchxp")
	other, otherPrefix := seedProject(t, env.pool, env.userID, "srchxp2")
	for i := 1; i <= 3; i++ {
		seedIssue(t, env.pool, env.pid, env.userID, env.prefix+"-"+strconv.Itoa(i),
			"numbat report", "open", 2)
		seedIssue(t, env.pool, other, env.userID, otherPrefix+"-"+strconv.Itoa(i),
			"numbat report", "open", 2)
	}

	res := env.search(t, map[string]any{"query": "numbat"})
	items, _ := res["items"].([]any)
	if len(items) < 4 {
		t.Fatalf("cross-project search returned %d items, want the fixtures from both projects",
			len(items))
	}
	seen := map[string]bool{}
	order := make([]string, 0, len(items))
	for _, raw := range items {
		row := raw.(map[string]any)
		if row["projectId"] == nil {
			t.Errorf("cross-project result %v has no projectId — US-6 declares it Unchanged, "+
				"and cross-project output is unreadable without it", row["id"])
		}
		if _, ok := row["updatedAt"].(string); !ok {
			t.Errorf("result %v has no updatedAt: %#v", row["id"], row["updatedAt"])
		}
		if pid, ok := row["projectId"].(string); ok {
			seen[pid] = true
		}
		order = append(order, row["id"].(string))
	}
	if len(seen) < 2 {
		t.Errorf("results came from %d project(s), want both", len(seen))
	}

	// Relevance and order are the service's, and the projection must not
	// reorder: the same query through the service directly must yield the
	// same sequence of ids.
	direct, err := env.svc.Search(context.Background(), search.Params{
		Query: "numbat", CallerID: env.userID,
		Limit: SearchLimitDefault,
	})
	if err != nil {
		t.Fatalf("service search: %v", err)
	}
	want := make([]string, 0, len(direct.Items))
	for _, it := range direct.Items {
		want = append(want, it.ID)
	}
	if len(want) != len(order) {
		t.Fatalf("projection returned %d items, the service returned %d", len(order), len(want))
	}
	for i := range want {
		if want[i] != order[i] {
			t.Errorf("order differs at %d: projection %q, service %q — the projection strips "+
				"markup, it does not rank", i, order[i], want[i])
		}
	}
}
