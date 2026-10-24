package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// T062 / US-5/FR matrix #15–20, #23–24 — skein_show comments limit/since/meta
// and tool-schema registration for comment_get + show params.

func forceCreatedAt(t *testing.T, pool *pgxpool.Pool, commentID uuid.UUID, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE comments SET created_at = $2, updated_at = $2 WHERE id = $1`,
		commentID, at.UTC())
	if err != nil {
		t.Fatalf("force created_at: %v", err)
	}
}

func seedShowCommentThread(t *testing.T, store *issues.Store, pool *pgxpool.Pool, issueID string, author uuid.UUID, uname string, bodies []string, base time.Time) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, len(bodies))
	for i, body := range bodies {
		c, err := store.AddComment(context.Background(), issueID, body, author,
			uname, issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
			issues.HumanActor(author, uname))
		if err != nil {
			t.Fatalf("AddComment[%d]: %v", i, err)
		}
		forceCreatedAt(t, pool, c.ID, base.Add(time.Duration(i)*time.Second))
		ids = append(ids, c.ID)
	}
	return ids
}

func callShow(t *testing.T, handler server.ToolHandlerFunc, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

func callShowErr(t *testing.T, handler server.ToolHandlerFunc, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError, got success: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

func metaInt(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	v, ok := body[key]
	if !ok {
		t.Fatalf("missing meta key %q", key)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s type %T, want number", key, v)
	}
	return int(f)
}

// TestShow_DefaultLast3AndMeta — US-5 #15: ≥4 comments, default last 3 ASC + meta.
func TestShow_DefaultLast3AndMeta(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shdef")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "default last3", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	ids := seedShowCommentThread(t, store, pool, issueID, userID, uname,
		[]string{"c1", "c2", "c3", "c4"}, base)

	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")
	body := callShow(t, handler, ctx, map[string]any{"id": issueID})

	comments, ok := body["comments"].([]any)
	if !ok {
		t.Fatalf("comments type %T", body["comments"])
	}
	if len(comments) != 3 {
		t.Fatalf("len(comments) = %d, want 3", len(comments))
	}
	// last-3 ASC → c2, c3, c4
	wantBodies := []string{"c2", "c3", "c4"}
	wantIDs := ids[1:]
	for i, want := range wantBodies {
		c := comments[i].(map[string]any)
		if c["body"] != want {
			t.Errorf("comments[%d].body = %v, want %s", i, c["body"], want)
		}
		if c["id"] != wantIDs[i].String() {
			t.Errorf("comments[%d].id = %v, want %s", i, c["id"], wantIDs[i])
		}
	}
	if metaInt(t, body, "commentsTotal") != 4 {
		t.Errorf("commentsTotal = %v, want 4", body["commentsTotal"])
	}
	if metaInt(t, body, "commentsReturned") != 3 {
		t.Errorf("commentsReturned = %v, want 3", body["commentsReturned"])
	}
	if got, ok := body["commentsTruncated"].(bool); !ok || !got {
		t.Errorf("commentsTruncated = %v, want true", body["commentsTruncated"])
	}
}

// TestShow_CommentsLimitZero — US-5 #16: limit 0 returns all, truncated false.
func TestShow_CommentsLimitZero(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shz")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "limit0", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	seedShowCommentThread(t, store, pool, issueID, userID, uname,
		[]string{"a", "b", "c", "d", "e"}, base)

	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")
	body := callShow(t, handler, ctx, map[string]any{
		"id":            issueID,
		"commentsLimit": 0,
	})

	// sk-282 T011 (test-plan row 32): «whole thread» is now the INDEX —
	// one entry per comment — plus the last three in full. `comments` no
	// longer carries every body, and `commentsReturned` counts INDEX
	// entries, because the flag beside it means «the enumeration is
	// incomplete», not «some bodies were withheld».
	index, ok := body["commentsIndex"].([]any)
	if !ok {
		t.Fatalf("commentsIndex missing or not an array: %#v", body["commentsIndex"])
	}
	if len(index) != 5 {
		t.Fatalf("index = %d entries, want one per comment (5)", len(index))
	}
	for i, raw := range index {
		entry := raw.(map[string]any)
		for _, key := range []string{"id", "author", "createdAt", "preview"} {
			if _, present := entry[key]; !present {
				t.Errorf("index[%d] is missing %q — an entry has to identify the comment and "+
					"give the id to fetch it with", i, key)
			}
		}
	}

	comments := body["comments"].([]any)
	if len(comments) != 3 {
		t.Fatalf("comments = %d, want the last 3 in full", len(comments))
	}
	// The tail is the LAST three, in order.
	for i, want := range []string{"c", "d", "e"} {
		if got := comments[i].(map[string]any)["body"]; got != want {
			t.Errorf("comments[%d].body = %v, want %q", i, got, want)
		}
	}

	if metaInt(t, body, "commentsTotal") != 5 {
		t.Errorf("total = %v", body["commentsTotal"])
	}
	if metaInt(t, body, "commentsReturned") != 5 {
		t.Errorf("returned = %v, want 5 — listed is the index, not the bodies",
			body["commentsReturned"])
	}
	if got, ok := body["commentsTruncated"].(bool); !ok || got {
		t.Errorf("truncated = %v, want false", body["commentsTruncated"])
	}
}

// TestShow_CommentsSinceAndLimit — US-5/FR-008 #17: exclusive since then last-N.
func TestShow_CommentsSinceAndLimit(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shs")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "since", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	// T1..T4 at +0..+3s; since=T2 → population {T3,T4}, limit 3 → both.
	base := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	seedShowCommentThread(t, store, pool, issueID, userID, uname,
		[]string{"t1", "t2", "t3", "t4"}, base)
	sinceT2 := base.Add(1 * time.Second).Format(time.RFC3339)

	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")
	body := callShow(t, handler, ctx, map[string]any{
		"id":            issueID,
		"commentsSince": sinceT2,
		"commentsLimit": 3,
	})

	comments := body["comments"].([]any)
	if len(comments) != 2 {
		t.Fatalf("len = %d, want 2 (t3,t4)", len(comments))
	}
	if comments[0].(map[string]any)["body"] != "t3" || comments[1].(map[string]any)["body"] != "t4" {
		t.Errorf("bodies = %v, %v", comments[0].(map[string]any)["body"], comments[1].(map[string]any)["body"])
	}
	if metaInt(t, body, "commentsTotal") != 2 {
		t.Errorf("total = %v, want 2 (filtered population)", body["commentsTotal"])
	}
	if metaInt(t, body, "commentsReturned") != 2 {
		t.Errorf("returned = %v", body["commentsReturned"])
	}
	if got, ok := body["commentsTruncated"].(bool); !ok || got {
		t.Errorf("truncated = %v, want false", body["commentsTruncated"])
	}
}

// TestShow_CommentsOff_IgnoresBadParams — FR-005 #19.
func TestShow_CommentsOff_IgnoresBadParams(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shoff")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "off", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")

	body := callShow(t, handler, ctx, map[string]any{
		"id":              issueID,
		"includeComments": false,
		"commentsLimit":   -99,
		"commentsSince":   "not-a-date",
	})
	if body["comments"] != nil {
		t.Errorf("comments = %v, want null", body["comments"])
	}
	for _, key := range []string{"commentsTotal", "commentsReturned", "commentsTruncated"} {
		if _, ok := body[key]; ok {
			t.Errorf("meta key %q must be absent when comments off", key)
		}
	}
}

// TestShow_CommentsOn_ValidationErrors — FR-009/010 #20.
func TestShow_CommentsOn_ValidationErrors(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shval")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "val", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")

	cases := []struct {
		name string
		args map[string]any
	}{
		{"negative limit", map[string]any{"id": issueID, "commentsLimit": -1}},
		{"non-int limit", map[string]any{"id": issueID, "commentsLimit": 3.7}},
		{"invalid since", map[string]any{"id": issueID, "commentsSince": "yesterday"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := callShowErr(t, handler, ctx, tc.args)
			if got := body["error"]; got != ErrCodeValidationError {
				t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
			}
		})
	}
}

// TestShow_EmptyThreadMetaPresent — FR-004 #18 (already in HappyPath; keep focused).
func TestShow_EmptyThreadMetaPresent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shempty")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "empty", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")
	body := callShow(t, handler, ctx, map[string]any{"id": issueID})

	arr, ok := body["comments"].([]any)
	if !ok || len(arr) != 0 {
		t.Fatalf("comments = %v, want []", body["comments"])
	}
	if metaInt(t, body, "commentsTotal") != 0 || metaInt(t, body, "commentsReturned") != 0 {
		t.Errorf("meta totals = %v/%v, want 0/0", body["commentsTotal"], body["commentsReturned"])
	}
	if got, ok := body["commentsTruncated"].(bool); !ok || got {
		t.Errorf("truncated = %v, want false", body["commentsTruncated"])
	}

	// sk-282 T011 (test-plan row 38): asked for the whole thread, an empty
	// thread answers with an EMPTY index, not with a missing one — the
	// same rule the counters follow, for the same reason.
	whole := callShow(t, handler, ctx, map[string]any{"id": issueID, "commentsLimit": 0})
	idx, ok := whole["commentsIndex"]
	if !ok {
		t.Error("commentsIndex is absent on an empty thread — the index is empty, not absent")
	} else if arr, isArr := idx.([]any); !isArr || len(arr) != 0 {
		t.Errorf("commentsIndex = %#v, want an empty array", idx)
	}
	if metaInt(t, whole, "commentsTotal") != 0 || metaInt(t, whole, "commentsReturned") != 0 {
		t.Errorf("meta on an empty whole-thread read = %v/%v, want 0/0",
			whole["commentsTotal"], whole["commentsReturned"])
	}
	if got, ok := whole["commentsTruncated"].(bool); !ok || got {
		t.Errorf("truncated = %v on an empty thread, want false", whole["commentsTruncated"])
	}
}

// TestShowAndCommentGet_ListToolsSchema — US-7 #24/#35 schema + name.
func TestShowAndCommentGet_ListToolsSchema(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)

	s := server.NewMCPServer("skein-test", "0.1.0", server.WithToolCapabilities(false))
	RegisterShow(s, pool, issuesStore, depsStore)
	RegisterCommentGet(s, pool, issuesStore)

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, "admin")
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "tests", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := map[string]mcpgo.Tool{}
	for _, tool := range listRes.Tools {
		byName[tool.Name] = tool
	}
	get, ok := byName["skein_comment_get"]
	if !ok {
		t.Fatal("ListTools missing skein_comment_get")
	}
	if _, ok := get.InputSchema.Properties["commentId"]; !ok {
		t.Error("skein_comment_get schema missing commentId")
	}
	show, ok := byName["skein_show"]
	if !ok {
		t.Fatal("ListTools missing skein_show")
	}
	for _, key := range []string{"commentsLimit", "commentsSince", "includeComments", "includeDescription"} {
		if _, ok := show.InputSchema.Properties[key]; !ok {
			t.Errorf("skein_show schema missing %q", key)
		}
	}
}

// ---------------------------------------------------------------------
// sk-282 T011 — the thread index (test-plan rows 33, 35, 36, 37, 39, 40,
// 41). Row 32 is the fix to TestShow_CommentsLimitZero above, row 38 the
// extension to TestShow_EmptyThreadMetaPresent, row 34 the pure
// completeness matrix in show_parse_test.go, and row 42 is SC-008 in the
// cmd/api harness.
// ---------------------------------------------------------------------

// indexThreadEnv bundles what every case below needs.
type indexThreadEnv struct {
	pool    *pgxpool.Pool
	store   *issues.Store
	handler server.ToolHandlerFunc
	ctx     context.Context
	userID  uuid.UUID
	uname   string
	issueID string
}

func newIndexThreadEnv(t *testing.T, prefixHint string) indexThreadEnv {
	t.Helper()
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, prefixHint)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "indexed thread", "open", 2)

	store := issues.NewStore(pool)
	return indexThreadEnv{
		pool:    pool,
		store:   store,
		handler: handleShow(pool, store, dependencies.NewStore(pool)),
		ctx:     withCtx(t, userID, "admin"),
		userID:  userID,
		uname:   uname,
		issueID: issueID,
	}
}

func (e indexThreadEnv) whole(t *testing.T, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"id": e.issueID, "commentsLimit": 0}
	for k, v := range extra {
		args[k] = v
	}
	return callShow(t, e.handler, e.ctx, args)
}

func indexEntries(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["commentsIndex"].([]any)
	if !ok {
		t.Fatalf("commentsIndex missing or not an array: %#v", body["commentsIndex"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

// row 33 — the two INV-1 markers pulled apart, on the ENTRY.
func TestShow_IndexEntryTruncationMarkerOnlyWhenCut(t *testing.T) {
	env := newIndexThreadEnv(t, "shidx33")
	base := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	short := "short one"
	exact := strings.Repeat("e", ThreadIndexPreviewMaxRunes)
	long := strings.Repeat("l", ThreadIndexPreviewMaxRunes+40)
	// Cyrillic, to prove the cut is by runes at the wire edge too.
	cyrillic := strings.Repeat("ж", ThreadIndexPreviewMaxRunes+10)
	seedShowCommentThread(t, env.store, env.pool, env.issueID, env.userID, env.uname,
		[]string{short, exact, long, cyrillic}, base)

	entries := indexEntries(t, env.whole(t, nil))
	if len(entries) != 4 {
		t.Fatalf("index = %d entries, want 4", len(entries))
	}

	cases := []struct {
		name          string
		entry         map[string]any
		wantPreview   string
		wantTruncated bool
	}{
		{"shorter than the fragment", entries[0], short, false},
		{"exactly the fragment", entries[1], exact, false},
		{"longer than the fragment", entries[2], strings.Repeat("l", ThreadIndexPreviewMaxRunes), true},
		{"multi-byte, longer", entries[3], strings.Repeat("ж", ThreadIndexPreviewMaxRunes), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry["preview"]; got != tc.wantPreview {
				t.Errorf("preview = %v, want %q", got, tc.wantPreview)
			}
			got, present := tc.entry["previewTruncated"]
			if tc.wantTruncated {
				if !present || got != true {
					t.Errorf("previewTruncated = %v (present=%v), want true", got, present)
				}
				return
			}
			if present {
				t.Errorf("previewTruncated = %v on a body that fits — on short text "+
					"the marker is not set, otherwise the marker lies", got)
			}
		})
	}
}

// row 35 — past the index cap: exactly the cap, a total, an incomplete
// flag, and NO second marker.
func TestShow_IndexCapListsExactlyTheCapAndNoSecondMarker(t *testing.T) {
	env := newIndexThreadEnv(t, "shidx35")
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	over := ShowThreadIndexCap + 7
	bodies := make([]string, 0, over)
	for i := range over {
		bodies = append(bodies, fmt.Sprintf("comment %03d", i))
	}
	seedShowCommentThread(t, env.store, env.pool, env.issueID, env.userID, env.uname, bodies, base)

	body := env.whole(t, nil)
	entries := indexEntries(t, body)

	if len(entries) != ShowThreadIndexCap {
		t.Errorf("index = %d entries, want exactly the cap of %d", len(entries), ShowThreadIndexCap)
	}
	if got := metaInt(t, body, "commentsTotal"); got != over {
		t.Errorf("commentsTotal = %d, want %d — the total is present", got, over)
	}
	if got := metaInt(t, body, "commentsReturned"); got != ShowThreadIndexCap {
		t.Errorf("commentsReturned = %d, want %d", got, ShowThreadIndexCap)
	}
	if got, ok := body["commentsTruncated"].(bool); !ok || !got {
		t.Errorf("commentsTruncated = %v, want true — past the cap the enumeration IS "+
			"incomplete", body["commentsTruncated"])
	}
	// Entries are the MOST RECENT of the population, not the oldest.
	if got := entries[len(entries)-1]["preview"]; got != fmt.Sprintf("comment %03d", over-1) {
		t.Errorf("last index entry = %v, want the newest comment", got)
	}

	// The point of the row: no second flag beside the one that was just
	// given a single meaning.
	for _, forbidden := range []string{"commentsIndexTruncated", "indexTruncated", "commentsIndexComplete"} {
		if v, ok := body[forbidden]; ok {
			t.Errorf("a second index-truncation marker %q = %v appeared — its job belongs to "+
				"commentsTruncated, and a flag beside a freshly-disambiguated one is how the "+
				"ambiguity comes back", forbidden, v)
		}
	}
}

// row 36 — the composite mode: the cap applies to the WINDOW.
func TestShow_IndexCapAppliesToTheWindow(t *testing.T) {
	env := newIndexThreadEnv(t, "shidx36")
	base := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	// 5 comments before the window, then more than the cap inside it.
	const outside = 5
	inside := ShowThreadIndexCap + 4
	bodies := make([]string, 0, outside+inside)
	for i := range outside + inside {
		bodies = append(bodies, fmt.Sprintf("c%03d", i))
	}
	seedShowCommentThread(t, env.store, env.pool, env.issueID, env.userID, env.uname, bodies, base)

	// The window starts strictly after the last comment outside it.
	since := base.Add(time.Duration(outside-1) * time.Second)
	body := env.whole(t, map[string]any{"commentsSince": since.Format(time.RFC3339Nano)})

	entries := indexEntries(t, body)
	if len(entries) != ShowThreadIndexCap {
		t.Errorf("index = %d entries, want the cap of %d applied to the window",
			len(entries), ShowThreadIndexCap)
	}
	if got := metaInt(t, body, "commentsTotal"); got != inside {
		t.Errorf("commentsTotal = %d, want %d — total relates to the WINDOW, not to the "+
			"thread behind it", got, inside)
	}
	if got, ok := body["commentsTruncated"].(bool); !ok || !got {
		t.Errorf("commentsTruncated = %v, want true", body["commentsTruncated"])
	}
	// Nothing from outside the window leaked in.
	for _, e := range entries {
		if e["preview"] == "c000" {
			t.Error("an entry from before the window appeared in the index")
		}
	}
}

// row 37 — N last on a thread shorter than N.
func TestShow_LastNShorterThanNIsComplete(t *testing.T) {
	env := newIndexThreadEnv(t, "shidx37")
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	seedShowCommentThread(t, env.store, env.pool, env.issueID, env.userID, env.uname,
		[]string{"a", "b"}, base)

	body := callShow(t, env.handler, env.ctx, map[string]any{
		"id":            env.issueID,
		"commentsLimit": 50,
	})

	if got := metaInt(t, body, "commentsReturned"); got != 2 {
		t.Errorf("commentsReturned = %d, want 2 — actually returned, not requested", got)
	}
	if got := metaInt(t, body, "commentsTotal"); got != 2 {
		t.Errorf("commentsTotal = %d, want 2", got)
	}
	if got, ok := body["commentsTruncated"].(bool); !ok || got {
		t.Errorf("commentsTruncated = %v, want false", body["commentsTruncated"])
	}
	if _, ok := body["commentsIndex"]; ok {
		t.Error("an N-last read produced an index — only «whole thread» does")
	}
}

// row 39 — a deleted comment keeps its slot and reads as a tombstone.
func TestShow_IndexKeepsDeletedCommentAsTombstone(t *testing.T) {
	env := newIndexThreadEnv(t, "shidx39")
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	ids := seedShowCommentThread(t, env.store, env.pool, env.issueID, env.userID, env.uname,
		[]string{"kept", "removed", "also kept"}, base)

	if _, err := env.pool.Exec(context.Background(),
		`UPDATE comments SET deleted_at = now(), body = '' WHERE id = $1`, ids[1]); err != nil {
		t.Fatalf("soft-delete comment: %v", err)
	}

	entries := indexEntries(t, env.whole(t, nil))
	if len(entries) != 3 {
		t.Fatalf("index = %d entries, want 3 — a deleted comment keeps its slot in the thread",
			len(entries))
	}
	tomb := entries[1]
	if tomb["id"] != ids[1].String() {
		t.Fatalf("index[1].id = %v, want the deleted comment %s", tomb["id"], ids[1])
	}
	if _, ok := tomb["deletedAt"]; !ok {
		t.Error("the tombstone entry carries no deletedAt — without it a deleted comment is " +
			"indistinguishable from one that happens to be empty")
	}
	if tomb["preview"] != "" {
		t.Errorf("tombstone preview = %v, want empty — the store blanks the body and the "+
			"deleted text must not reach the wire", tomb["preview"])
	}
	if _, ok := tomb["previewTruncated"]; ok {
		t.Error("the tombstone claims its preview was truncated — nothing was cut")
	}
}

// row 40 — the loop acceptance, stated on the object US-4 measures.
//
// This is the row the whole change is FOR: an agent following the guide's
// prescription reads the thread in full and must come back with a complete
// answer, or the cure loops.
func TestShow_WholeThreadOf48DoesNotRetriggerThePrescription(t *testing.T) {
	env := newIndexThreadEnv(t, "shidx40")
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	const measured = 48
	bodies := make([]string, 0, measured)
	for i := range measured {
		bodies = append(bodies, fmt.Sprintf("turn %02d: %s", i, strings.Repeat("x", 300)))
	}
	seedShowCommentThread(t, env.store, env.pool, env.issueID, env.userID, env.uname, bodies, base)

	// Step 1: the default read reports the thread is incomplete — this is
	// what triggers the prescription.
	def := callShow(t, env.handler, env.ctx, map[string]any{"id": env.issueID})
	if got, ok := def["commentsTruncated"].(bool); !ok || !got {
		t.Fatalf("the default read reports commentsTruncated = %v on a 48-comment thread; "+
			"the prescription would never fire and this test would prove nothing",
			def["commentsTruncated"])
	}

	// Step 2: the agent does what it is told.
	body := env.whole(t, nil)

	if got, ok := body["commentsTruncated"].(bool); !ok || got {
		t.Errorf("commentsTruncated = %v after reading the whole thread — the answer to the "+
			"prescription is itself an input that re-triggers it, and the agent loops",
			body["commentsTruncated"])
	}
	entries := indexEntries(t, body)
	if len(entries) != measured {
		t.Errorf("index = %d entries, want one per comment (%d) — listing EVERY "+
			"comment is what removes the ignorance the prescription existed for",
			len(entries), measured)
	}
	if got := metaInt(t, body, "commentsTotal"); got != measured {
		t.Errorf("commentsTotal = %d, want %d", got, measured)
	}
	// And every entry can be turned back into a full comment.
	for i, e := range entries {
		if id, _ := e["id"].(string); id == "" {
			t.Errorf("index[%d] has no id — the index is only useful because each entry can "+
				"be fetched by id", i)
		}
	}
}

// row 41 — the three unchanged modes still answer as they did.
//
// Asserted together, in one place, because «Unchanged» is a claim about
// all three and a per-mode drift is exactly what a change of this size
// causes.
func TestShow_UnchangedCommentModesStillBehave(t *testing.T) {
	env := newIndexThreadEnv(t, "shidx41")
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	seedShowCommentThread(t, env.store, env.pool, env.issueID, env.userID, env.uname,
		[]string{"a", "b", "c", "d", "e"}, base)

	t.Run("no parameters — last three plus meta", func(t *testing.T) {
		body := callShow(t, env.handler, env.ctx, map[string]any{"id": env.issueID})
		comments := body["comments"].([]any)
		if len(comments) != 3 {
			t.Errorf("comments = %d, want 3", len(comments))
		}
		if metaInt(t, body, "commentsTotal") != 5 || metaInt(t, body, "commentsReturned") != 3 {
			t.Errorf("meta = %v/%v, want 5/3", body["commentsTotal"], body["commentsReturned"])
		}
		if got, ok := body["commentsTruncated"].(bool); !ok || !got {
			t.Errorf("commentsTruncated = %v, want true", body["commentsTruncated"])
		}
		if _, ok := body["commentsIndex"]; ok {
			t.Error("the default read produced an index — only «whole thread» does")
		}
	})

	t.Run("N last — the bodies, in full", func(t *testing.T) {
		body := callShow(t, env.handler, env.ctx, map[string]any{
			"id": env.issueID, "commentsLimit": 2,
		})
		comments := body["comments"].([]any)
		if len(comments) != 2 {
			t.Fatalf("comments = %d, want 2", len(comments))
		}
		if comments[1].(map[string]any)["body"] != "e" {
			t.Errorf("last body = %v, want %q — N last is the give-me-everything-I-pay path and "+
				"returns bodies, not previews", comments[1].(map[string]any)["body"], "e")
		}
		if _, ok := body["commentsIndex"]; ok {
			t.Error("an N-last read produced an index")
		}
	})

	t.Run("time window — the window's comments", func(t *testing.T) {
		since := base.Add(2 * time.Second)
		body := callShow(t, env.handler, env.ctx, map[string]any{
			"id": env.issueID, "commentsSince": since.Format(time.RFC3339Nano),
		})
		if got := metaInt(t, body, "commentsTotal"); got != 2 {
			t.Errorf("commentsTotal = %d, want 2 (the window's population)", got)
		}
		if _, ok := body["commentsIndex"]; ok {
			t.Error("a window read produced an index")
		}
	})
}
