package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// extractToolJSON unwraps the JSON envelope mcp-go puts on the result.
// Tool handlers always return a single TextContent with a JSON-string body.
func extractToolJSON(t *testing.T, res *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatalf("nil result")
	}
	if len(res.Content) == 0 {
		t.Fatalf("empty content")
	}
	tc, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatalf("unmarshal tool response: %v\nbody=%s", err, tc.Text)
	}
	return out
}

// TestShow_HappyPath builds a single project + issue, calls the show
// handler, and asserts the wire-JSON has the expected top-level keys.
func TestShow_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shtest")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "demo issue", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)

	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	// sk-282 T009: `includeDeps: true` on its own is now a refusal — it
	// used to mean «every edge», which is the 20 KB skein_show no longer
	// sends by accident. This test is about the happy envelope, so it asks
	// for edges the way a caller must now ask: by naming the types.
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"includeComments": true,
		"depsTypes":       []any{"parent-child"},
		"includeEvents":   false,
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	if _, ok := body["issue"]; !ok {
		t.Fatalf("missing 'issue' key: %+v", body)
	}
	if _, ok := body["deps"]; !ok {
		t.Fatalf("missing 'deps' key (depsTypes named): %+v", body)
	}
	if _, ok := body["depsCounts"]; !ok {
		t.Fatalf("missing 'depsCounts' key — a typed edge request keeps the counters so the "+
			"caller still sees the types it did not name: %+v", body)
	}
	// Comments default to true; key MUST be present and render as an
	// empty array (not absent) when there are zero comments — the
	// canonical contract is "include flag honored => slice always
	// emitted".
	v, ok := body["comments"]
	if !ok {
		t.Fatalf("expected 'comments' key to be present when includeComments=true: %+v", body)
	}
	arr, isArr := v.([]any)
	if !isArr {
		t.Fatalf("expected 'comments' to be an array, got %T", v)
	}
	if len(arr) != 0 {
		t.Fatalf("expected empty comments, got %d", len(arr))
	}
	// sk-242.1 FR-004: empty thread still carries three meta keys with
	// 0 / 0 / false (pointers force presence of zero values).
	if got, ok := body["commentsTotal"].(float64); !ok || got != 0 {
		t.Fatalf("commentsTotal = %v (%T), want 0", body["commentsTotal"], body["commentsTotal"])
	}
	if got, ok := body["commentsReturned"].(float64); !ok || got != 0 {
		t.Fatalf("commentsReturned = %v (%T), want 0", body["commentsReturned"], body["commentsReturned"])
	}
	if got, ok := body["commentsTruncated"].(bool); !ok || got {
		t.Fatalf("commentsTruncated = %v (%T), want false", body["commentsTruncated"], body["commentsTruncated"])
	}
	// Events default to false (not requested in this call) — the field
	// renders as null on the wire. The key still appears (no omitempty).
	if v, ok := body["events"]; !ok {
		t.Fatalf("expected 'events' key to be present (renders as null when includeEvents=false): %+v", body)
	} else if v != nil {
		t.Fatalf("expected 'events' to be null when includeEvents=false, got %T %v", v, v)
	}
}

// TestShow_BlockerRedactionPerReader is sk-421.1 T001 through MCP skein_show
// (US-14, SC-003, test-plan row 8). The subject in P is blocked by one issue
// from P and one from Q; four readers — member of P only, external member of
// P, member of both, internal admin. A reader outside Q gets the Q entry as
// exactly {id, redacted:true}; the other readers get it full. The redaction
// lives in the detail assembly shared with REST, so a redaction that lived
// only in the REST handler would leave this surface leaking.
func TestShow_BlockerRedactionPerReader(t *testing.T) {
	pool := newTestPool(t)

	pOwner, _ := testsupport.SeedUser(t, pool, "user")
	qOwner, _ := testsupport.SeedUser(t, pool, "user")
	pID, pPrefix := testsupport.SeedProject(t, pool, pOwner, "shbvp")
	qID, qPrefix := testsupport.SeedProject(t, pool, qOwner, "shbvq")

	both, _ := testsupport.SeedUser(t, pool, "user")
	testsupport.AddProjectMember(t, pool, pID, both, "member")
	testsupport.AddProjectMember(t, pool, qID, both, "member")
	ext, _ := testsupport.SeedUser(t, pool, "user")
	testsupport.AddProjectMember(t, pool, pID, ext, "member")
	admin, _ := testsupport.SeedUser(t, pool, "admin")

	const (
		pTitle = "p blocker title"
		qTitle = "q secret blocker title"
	)
	subject := testsupport.SeedIssue(t, pool, pID, pOwner, pPrefix+"-1", "subject", "open", 2)
	pBlocker := testsupport.SeedIssue(t, pool, pID, pOwner, pPrefix+"-2", pTitle, "open", 2)
	qBlocker := testsupport.SeedIssue(t, pool, qID, qOwner, qPrefix+"-1", qTitle, "in_progress", 2)
	testsupport.SeedDep(t, pool, subject, pBlocker, dependencies.DepBlocks, pOwner)
	testsupport.SeedDep(t, pool, subject, qBlocker, dependencies.DepBlocks, pOwner)

	wantOrder := []string{pBlocker, qBlocker}
	sort.Strings(wantOrder)

	handler := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	readers := []struct {
		name  string
		ctx   context.Context
		seesQ bool
	}{
		{"member of P, not of Q", withCtx(t, pOwner, "user"), false},
		{"external member of P", auth.WithIsExternal(withCtx(t, ext, "user"), true), false},
		{"member of P and Q", withCtx(t, both, "user"), true},
		{"internal admin", withCtx(t, admin, "admin"), true},
	}

	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			body := callShow(t, handler, r.ctx, map[string]any{"id": subject, "includeComments": false})
			issue, ok := body["issue"].(map[string]any)
			if !ok {
				t.Fatalf("issue missing: %v", body)
			}
			if issue["isBlocked"] != true {
				t.Errorf("isBlocked = %v, want true", issue["isBlocked"])
			}
			want := []string{pBlocker}
			wantLinks := float64(1)
			if r.seesQ {
				want, wantLinks = wantOrder, 0
			}
			if issue["unavailableBlockingLinks"] != wantLinks {
				t.Errorf("unavailableBlockingLinks = %v, want %v", issue["unavailableBlockingLinks"], wantLinks)
			}
			list, ok := issue["blockers"].([]any)
			if !ok || len(list) != len(want) {
				t.Fatalf("blockers = %#v, want %d entries", issue["blockers"], len(want))
			}
			for i, item := range list {
				entry, _ := item.(map[string]any)
				if entry["id"] != want[i] {
					t.Errorf("blockers[%d].id = %v, want %s", i, entry["id"], want[i])
				}
				keys := sortedKeys(entry)
				sort.Strings(keys)
				gotKeys := strings.Join(keys, ",")
				wantTitle, wantStatus := pTitle, "open"
				if entry["id"] == qBlocker {
					wantTitle, wantStatus = qTitle, "in_progress"
				}
				if gotKeys != "id,status,title" || entry["title"] != wantTitle || entry["status"] != wantStatus {
					t.Errorf("entry = %v (keys %s), want {id, title:%q, status:%s}",
						entry, gotKeys, wantTitle, wantStatus)
				}
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !r.seesQ && strings.Contains(string(raw), qTitle) {
				t.Errorf("the hidden blocker's title reached skein_show: %s", raw)
			}
		})
	}
}

// TestShow_LockHolderName is sk-421.1 test-plan row 21, MCP half (US-15,
// INV-4): skein_show carries lockedByUsername = the user who performed the
// claim; lockedBy stays the session and the session is not repeated in the
// new key; an unclaimed issue has no key.
func TestShow_LockHolderName(t *testing.T) {
	pool := newTestPool(t)
	u, uName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, u, "shlhn")
	locked := testsupport.SeedIssue(t, pool, pid, u, prefix+"-1", "locked", "open", 2)
	free := testsupport.SeedIssue(t, pool, pid, u, prefix+"-2", "free", "open", 2)

	store := issues.NewStore(pool)
	const session = "mcp-session-show-holder"
	claim := mcpgo.CallToolRequest{}
	claim.Params.Arguments = map[string]any{"id": locked}
	if res, err := handleClaim(pool, store)(withCtxSession(t, u, "user", session), claim); err != nil || res.IsError {
		t.Fatalf("claim failed: %v %+v", err, res)
	}

	handler := handleShow(pool, store, dependencies.NewStore(pool))
	ctx := withCtx(t, u, "user")

	issue, _ := callShow(t, handler, ctx, map[string]any{"id": locked, "includeComments": false})["issue"].(map[string]any)
	if issue["lockedByUsername"] != uName {
		t.Fatalf("lockedByUsername = %v, want %q", issue["lockedByUsername"], uName)
	}
	if issue["lockedBy"] != session {
		t.Errorf("lockedBy = %v, want the session %q unchanged", issue["lockedBy"], session)
	}
	if name, _ := issue["lockedByUsername"].(string); strings.Contains(name, session) {
		t.Errorf("lockedByUsername %q repeats the session id (INV-4)", name)
	}

	freeIssue, _ := callShow(t, handler, ctx, map[string]any{"id": free, "includeComments": false})["issue"].(map[string]any)
	if v, present := freeIssue["lockedByUsername"]; present {
		t.Errorf("lockedByUsername = %v on an unclaimed issue, want the key absent", v)
	}
}

// TestShow_NotFound asserts that an unknown id returns issue_not_found.
func TestShow_NotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id": "no-such-9999",
	}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing issue")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIssueNotFound {
		t.Fatalf("error code = %v, want %s", got, ErrCodeIssueNotFound)
	}
}

// TestShow_ValidationRejectsBareNumber asserts that "42" is rejected
// before any DB lookup.
func TestShow_ValidationRejectsBareNumber(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id": "42",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for bare numeric id")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error code = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestShow_ForbiddenForeignProject asserts a non-admin user who isn't a
// member of the issue's project gets issue_not_found (not a leakier
// "forbidden") for visibility safety.
func TestShow_ForbiddenForeignProject(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "shfor")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "secret", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)

	// Stranger calls — they have no project_members row, so visibility
	// resolves to "not visible" → mask as not_found.
	ctx := withCtx(t, stranger, "user")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-member access")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIssueNotFound {
		t.Fatalf("error code = %v, want %s (no leak via 'forbidden')", got, ErrCodeIssueNotFound)
	}
}

// TestShow_LooksLikeHierarchicalID exercises the validator directly.
func TestShow_LooksLikeHierarchicalID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"demo-42", true},
		{"demo-42.1", true},
		{"demo-migration-001", true},
		{"42", false},
		{"", false},
		{"1.2.3", false},
	}
	for _, c := range cases {
		got := looksLikeHierarchicalID(c.in)
		if got != c.want {
			t.Errorf("looksLikeHierarchicalID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Sanity-check: the wire JSON for a populated issue serialises with the
// canonical contract — when an include flag is true the slice is always
// emitted (empty becomes []), when the flag is false the field renders
// as null.
func TestShow_WireShapeContainsExpectedKeys(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shape")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "demo", "open", 2)

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":            issueID,
		"includeEvents": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	tc := res.Content[0].(mcpgo.TextContent)
	// `issue` is always rendered. `deps` is NOT: since sk-282 T009 the
	// dependency block defaults to counts, and the edge block appears only
	// on an explicit typed request. This assertion used to require `deps`
	// and is the wire-shape contract that deliberately changed — US-1's
	// "the deps key is no longer guaranteed as an edge block by default."
	for _, key := range []string{`"issue"`, `"depsCounts"`} {
		if !strings.Contains(tc.Text, key) {
			t.Errorf("expected response to contain %s\nbody=%s", key, tc.Text)
		}
	}
	if strings.Contains(tc.Text, `"deps"`) {
		t.Errorf("a parameterless read still renders a `deps` block\nbody=%s", tc.Text)
	}
	// `comments` and `events` keys are always present (no omitempty).
	// includeComments defaults to true → should render as []. We set
	// includeEvents=true → should render as [] when no events exist.
	body := extractToolJSON(t, res)
	commentsRaw, ok := body["comments"]
	if !ok {
		t.Errorf("expected 'comments' key to be present (no omitempty)")
	}
	if arr, isArr := commentsRaw.([]any); !isArr || len(arr) != 0 {
		t.Errorf("expected 'comments' to be empty array, got %T %v", commentsRaw, commentsRaw)
	}
	eventsRaw, ok := body["events"]
	if !ok {
		t.Errorf("expected 'events' key to be present when includeEvents=true")
	}
	if arr, isArr := eventsRaw.([]any); !isArr || len(arr) != 0 {
		t.Errorf("expected 'events' to be empty array when includeEvents=true and no events, got %T %v", eventsRaw, eventsRaw)
	}
}

// TestShow_EventsExposeActorSessionID verifies the MCP wire contract keeps
// session-backed events distinguishable while rendering an explicit null for
// historical, Web UI, REST, and system events that have no MCP session.
func TestShow_EventsExposeActorSessionID(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "event-session")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "demo", "open", 2)

	const sessionID = "mcp-session-show-event"
	sessionPtr := sessionID
	for _, session := range []*string{&sessionPtr, nil} {
		if _, err := pool.Exec(t.Context(), `
INSERT INTO events (type, actor, actor_user_id, actor_session_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ('issue.claimed', 'admin', $1, $2, 'agent',
        'issue', $3, $4, '{}'::jsonb)`, userID, session, issueID, pid); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	handler := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID, "includeEvents": true}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}

	events, ok := extractToolJSON(t, res)["events"].([]any)
	if !ok || len(events) != 2 {
		t.Fatalf("events = %T %v, want two events", events, events)
	}
	// ListEvents is newest-first, so the session-less row inserted second is first.
	withoutSession := events[0].(map[string]any)
	if value, present := withoutSession["actorSessionId"]; !present || value != nil {
		t.Fatalf("session-less actorSessionId = %v (present=%v), want explicit null", value, present)
	}
	withSession := events[1].(map[string]any)
	if got := withSession["actorSessionId"]; got != sessionID {
		t.Fatalf("actorSessionId = %v, want %q", got, sessionID)
	}
}

// ---------------------------------------------------------------------
// sk-282 T009 — the dependency block of skein_show (test-plan rows 21-25,
// 27a). Row 20 is the pure binding and lives in show_parse_test.go; row 27
// is SC-001 and is measured by the cmd/api harness; rows 24 and 26 are
// fixes to existing tests and stay where those tests are.
// ---------------------------------------------------------------------

// showDepsFixture is a node with edges of two types in both directions, so
// every assertion below can tell «filtered correctly» from «returned
// nothing».
type showDepsFixture struct {
	userID  uuid.UUID
	node    string
	parent  string
	child   string
	blocker string
	related string
}

func seedShowDepsFixture(t *testing.T, pool *pgxpool.Pool, prefixHint string) showDepsFixture {
	t.Helper()
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, prefixHint)

	fx := showDepsFixture{userID: userID}
	fx.node = seedIssue(t, pool, pid, userID, prefix+"-1", "node", "open", 2)
	fx.parent = seedIssue(t, pool, pid, userID, prefix+"-2", "parent", "open", 2)
	fx.child = seedIssue(t, pool, pid, userID, prefix+"-1.1", "child", "closed", 2)
	fx.blocker = seedIssue(t, pool, pid, userID, prefix+"-3", "blocker", "open", 2)
	fx.related = seedIssue(t, pool, pid, userID, prefix+"-4", "related", "open", 2)

	testsupport.SeedDep(t, pool, fx.node, fx.parent, dependencies.DepParentChild, userID)
	testsupport.SeedDep(t, pool, fx.child, fx.node, dependencies.DepParentChild, userID)
	testsupport.SeedDep(t, pool, fx.node, fx.blocker, dependencies.DepBlocks, userID)
	testsupport.SeedDep(t, pool, fx.related, fx.node, dependencies.DepRelated, userID)
	return fx
}

// callShowDeps runs the tool against the fixture's caller and returns the
// decoded body, failing on a tool error.
func callShowDeps(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, args map[string]any) map[string]any {
	t.Helper()
	handler := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

// callShowDepsExpectingError runs the tool and requires a business failure.
func callShowDepsExpectingError(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, args map[string]any) map[string]any {
	t.Helper()
	handler := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("call was accepted, want a refusal: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

// row 21 — the new default: counters, not one edge.
func TestShow_DefaultCarriesCountsAndNoEdges(t *testing.T) {
	pool := newTestPool(t)
	fx := seedShowDepsFixture(t, pool, "shdep21")

	body := callShowDeps(t, pool, fx.userID, map[string]any{"id": fx.node})

	if _, ok := body["deps"]; ok {
		t.Errorf("a parameterless read returned a `deps` block: %v — the whole point of T009 "+
			"is that the graph is no longer shipped by default", body["deps"])
	}
	counts, ok := body["depsCounts"].(map[string]any)
	if !ok {
		t.Fatalf("depsCounts missing or not an object: %#v", body["depsCounts"])
	}
	for _, dir := range []string{"up", "down"} {
		if _, ok := counts[dir].(map[string]any); !ok {
			t.Errorf("depsCounts.%s missing or not an object — no edges must be an empty "+
				"set, never an absent key: %#v", dir, counts[dir])
		}
	}
	// The fixture's shape must actually be visible in the counters, or the
	// assertion above would pass on an aggregate that counted nothing.
	down := counts["down"].(map[string]any)
	pc, ok := down["parent-child"].(map[string]any)
	if !ok {
		t.Fatalf("depsCounts.down has no parent-child cell: %#v", down)
	}
	if got := pc["closed"]; got != float64(1) {
		t.Errorf("depsCounts.down.parent-child.closed = %v, want 1", got)
	}
}

// row 27a — the aggregate's shape.
func TestShow_DepsCountsShapeIsFullBucketsForPresentTypesOnly(t *testing.T) {
	pool := newTestPool(t)
	fx := seedShowDepsFixture(t, pool, "shdep27a")

	body := callShowDeps(t, pool, fx.userID, map[string]any{"id": fx.node})
	counts := body["depsCounts"].(map[string]any)
	down := counts["down"].(map[string]any)
	up := counts["up"].(map[string]any)

	// A PRESENT type carries every bucket, zeros included: a zero inside a
	// present type has to read as «counted, and it is none».
	wantBuckets := []string{"open", "in_progress", "deferred", "closed", "standing", "unavailable"}
	cell, ok := down["parent-child"].(map[string]any)
	if !ok {
		t.Fatalf("down.parent-child missing: %#v", down)
	}
	for _, bucket := range wantBuckets {
		if _, present := cell[bucket]; !present {
			t.Errorf("down.parent-child is missing the %q bucket — a present type carries the "+
				"FULL set including zeros, or a zero is indistinguishable from «not counted»: %#v",
				bucket, cell)
		}
	}
	if len(cell) != len(wantBuckets) {
		t.Errorf("down.parent-child has %d keys, want exactly %d: %#v", len(cell), len(wantBuckets), cell)
	}

	// An ABSENT type is absent ENTIRELY — that is what keeps the aggregate
	// at a couple of hundred characters instead of one cell per type.
	if _, ok := down["blocks"]; ok {
		t.Errorf("down carries a blocks cell although every blocks edge points the other way: %#v", down)
	}
	for _, unused := range []string{"waits-for", "supersedes", "duplicates", "discovered-from"} {
		if _, ok := down[unused]; ok {
			t.Errorf("down carries a %q cell for a type the node has no edges of", unused)
		}
		if _, ok := up[unused]; ok {
			t.Errorf("up carries a %q cell for a type the node has no edges of", unused)
		}
	}
	// …and the types that ARE there are on the side they belong to.
	if _, ok := up["blocks"]; !ok {
		t.Errorf("up has no blocks cell although the node is blocked by one issue: %#v", up)
	}
	if _, ok := down["related"]; !ok {
		t.Errorf("down has no related cell although one issue points at the node: %#v", down)
	}
}

// row 22 — the US-10 route: a type list, no includeDeps.
func TestShow_TypedEdgesWithoutIncludeDeps(t *testing.T) {
	pool := newTestPool(t)
	fx := seedShowDepsFixture(t, pool, "shdep22")

	body := callShowDeps(t, pool, fx.userID, map[string]any{
		"id":        fx.node,
		"depsTypes": []any{"parent-child"},
	})

	deps, ok := body["deps"].(map[string]any)
	if !ok {
		t.Fatalf("deps missing — depsTypes alone must return edges, that is the route US-10 "+
			"teaches: %#v", body["deps"])
	}
	// Every parent-child edge, both directions, in full.
	outgoing := deps["outgoing"].([]any)
	incoming := deps["incoming"].([]any)
	if len(outgoing) != 1 || len(incoming) != 1 {
		t.Errorf("outgoing=%d incoming=%d, want one each (the parent above, the child below)",
			len(outgoing), len(incoming))
	}
	for _, raw := range append(append([]any{}, outgoing...), incoming...) {
		edge := raw.(map[string]any)
		if edge["type"] != "parent-child" {
			t.Errorf("an edge of type %v came back although only parent-child was asked for",
				edge["type"])
		}
	}
	// The counters ride along, so the agent still sees the types it did
	// NOT ask for.
	if _, ok := body["depsCounts"]; !ok {
		t.Error("a typed edge request dropped depsCounts — the caller would lose sight of the " +
			"types it did not name")
	}
}

// row 23 — the two refusals.
func TestShow_EdgesWithoutTypesAreRefused(t *testing.T) {
	pool := newTestPool(t)
	fx := seedShowDepsFixture(t, pool, "shdep23")

	cases := []struct {
		name string
		args map[string]any
	}{
		{"empty depsTypes", map[string]any{"id": fx.node, "depsTypes": []any{}}},
		{"explicit includeDeps:true with no depsTypes", map[string]any{"id": fx.node, "includeDeps": true}},
		{"explicit includeDeps:true with empty depsTypes", map[string]any{"id": fx.node, "includeDeps": true, "depsTypes": []any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := callShowDepsExpectingError(t, pool, fx.userID, tc.args)

			if body["error"] != ErrCodeValidationError {
				t.Errorf("error code = %v, want %s", body["error"], ErrCodeValidationError)
			}
			if body["field"] != "depsTypes" && body["field"] != "includeDeps" {
				t.Errorf("the refusal names field %v — it must name the parameter to fix",
					body["field"])
			}
			msg, _ := body["message"].(string)
			if !strings.Contains(msg, "depsTypes") {
				t.Errorf("the refusal does not mention depsTypes: %q", msg)
			}
			// The whole point: a refusal, not the full graph under another
			// name.
			for _, key := range []string{"deps", "depsCounts", "issue"} {
				if _, ok := body[key]; ok {
					t.Errorf("the refusal carries %q — the full graph is not returned means the "+
						"call fails, it does not quietly succeed", key)
				}
			}
		})
	}
}

// row 25 — the per-type safety valve.
func TestShow_EdgeCapMarksTruncationAndNamesTheRoute(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shdep25")
	node := seedIssue(t, pool, pid, userID, prefix+"-1", "fat node", "open", 2)

	over := ShowDepsEdgeCapPerType + 5
	for i := 1; i <= over; i++ {
		childID := seedIssue(t, pool, pid, userID,
			fmt.Sprintf("%s-1.%d", prefix, i), "child", "open", 2)
		testsupport.SeedDep(t, pool, childID, node, dependencies.DepParentChild, userID)
	}

	body := callShowDeps(t, pool, userID, map[string]any{
		"id":        node,
		"depsTypes": []any{"parent-child"},
	})
	deps := body["deps"].(map[string]any)

	got := len(deps["outgoing"].([]any)) + len(deps["incoming"].([]any))
	if got != ShowDepsEdgeCapPerType {
		t.Errorf("returned %d edges, want the cap of %d", got, ShowDepsEdgeCapPerType)
	}

	truncated, ok := deps["truncated"].(map[string]any)
	if !ok {
		t.Fatalf("no truncation marker although %d of %d edges were cut — INV-1: a response "+
			"that carries less than exists says so; silently stopping at the cap is the "+
			"failure this marker exists for", over-ShowDepsEdgeCapPerType, over)
	}
	cell, ok := truncated["parent-child"].(map[string]any)
	if !ok {
		t.Fatalf("truncation marker has no parent-child entry: %#v", truncated)
	}
	if cell["returned"] != float64(ShowDepsEdgeCapPerType) {
		t.Errorf("truncated.returned = %v, want %d", cell["returned"], ShowDepsEdgeCapPerType)
	}
	if cell["total"] != float64(over) {
		t.Errorf("truncated.total = %v, want %d — without the total the caller knows it is "+
			"missing something but not how much", cell["total"], over)
	}
	// The direction split: every edge in this fixture points AT the node,
	// so the whole budget is spent on `incoming` and `outgoing` is empty
	// for the honest reason. The marker has to make that legible — the
	// caller must be able to tell an empty direction that is empty from
	// one the other direction's edges pushed out.
	for _, dir := range []string{"outgoing", "incoming"} {
		half, ok := cell[dir].(map[string]any)
		if !ok {
			t.Fatalf("truncation marker has no %q half: %#v — an aggregate returned/total "+
				"cannot say WHICH direction was cut, and a zeroed direction then reads as "+
				"«there are none»", dir, cell)
		}
		wantReturned, wantTotal := float64(0), float64(0)
		if dir == "incoming" {
			wantReturned, wantTotal = float64(ShowDepsEdgeCapPerType), float64(over)
		}
		if half["returned"] != wantReturned || half["total"] != wantTotal {
			t.Errorf("truncated.%s = {returned:%v total:%v}, want {%v %v}",
				dir, half["returned"], half["total"], wantReturned, wantTotal)
		}
	}

	route, _ := cell["continue"].(string)
	if !strings.Contains(route, "skein_list") || !strings.Contains(route, "parentId") {
		t.Errorf("truncated.continue = %q — INV-4 requires a NAMED route to the rest, and for "+
			"parent-child that route is paging children through the list tool", route)
	}
}

// row 25, negative half — the marker must NOT appear when nothing was cut.
// A marker that is always there carries no information, which is the other
// half of INV-1's rule for it.
func TestShow_NoTruncationMarkerUnderTheCap(t *testing.T) {
	pool := newTestPool(t)
	fx := seedShowDepsFixture(t, pool, "shdep25n")

	body := callShowDeps(t, pool, fx.userID, map[string]any{
		"id":        fx.node,
		"depsTypes": []any{"parent-child"},
	})
	deps := body["deps"].(map[string]any)
	if v, ok := deps["truncated"]; ok {
		t.Errorf("truncation marker present on a node with two edges: %#v — the marker is set "+
			"only when truncation actually happened", v)
	}
}

// ---------------------------------------------------------------------
// sk-282 T012 — child counters on the card (test-plan rows 45, 46, 48, 49,
// 50). Row 47 is the epic rollup and lives with internal/issues; row 51 is
// the REST card and lives with the REST handler.
// ---------------------------------------------------------------------

// childCountsFixture is US-3's acceptance case, verbatim: children in
// open / in_progress / deferred / closed plus one standing.
type childCountsFixture struct {
	userID  uuid.UUID
	project uuid.UUID
	node    string
}

func seedChildCountsFixture(t *testing.T, pool *pgxpool.Pool, prefixHint string) childCountsFixture {
	t.Helper()
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, prefixHint)

	// Deliberately type "task", not "epic": the whole point of US-3 is
	// that the head of a run is usually an ordinary task and could not
	// answer «is the wave finished» at all.
	node := seedIssue(t, pool, pid, userID, prefix+"-1", "wave head", "open", 2)

	for i, st := range []string{"open", "in_progress", "deferred", "closed", "standing"} {
		childID := seedIssue(t, pool, pid, userID,
			fmt.Sprintf("%s-1.%d", prefix, i+1), "child "+st, st, 2)
		testsupport.SeedDep(t, pool, childID, node, dependencies.DepParentChild, userID)
	}
	return childCountsFixture{userID: userID, project: pid, node: node}
}

func cardOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	card, ok := body["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue is not an object: %#v", body["issue"])
	}
	return card
}

// row 45 — US-3's acceptance sentence, on a task.
func TestShow_ChildCountsExcludeStandingAndCountUnclosedWidely(t *testing.T) {
	pool := newTestPool(t)
	fx := seedChildCountsFixture(t, pool, "shcc45")

	card := cardOf(t, callShowDeps(t, pool, fx.userID, map[string]any{"id": fx.node}))

	if got := card["childrenCount"]; got != float64(4) {
		t.Errorf("childrenCount = %v, want 4 — five children, of which the standing one is "+
			"not a unit of work and has no place in the denominator of «done / total»", got)
	}
	if got := card["unclosedChildrenCount"]; got != float64(3) {
		t.Errorf("unclosedChildrenCount = %v, want 3 — a wave is held by «in progress» and "+
			"«deferred» just as much as by «open»; only «closed» and «standing» drop out", got)
	}
}

// row 46 — no children at all.
func TestShow_ChildCountsPresentAndZeroWithoutChildren(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "shcc46")
	lonely := seedIssue(t, pool, pid, userID, prefix+"-1", "childless", "open", 2)

	card := cardOf(t, callShowDeps(t, pool, userID, map[string]any{"id": lonely}))

	for _, key := range []string{"childrenCount", "unclosedChildrenCount"} {
		got, present := card[key]
		if !present {
			t.Errorf("%s is absent on a childless issue — a zero has to read as «counted, and "+
				"there are none», and an absent key reads as «not counted»", key)
			continue
		}
		if got != float64(0) {
			t.Errorf("%s = %v, want 0", key, got)
		}
	}
}

// row 48 — INV-10's arithmetic identity, on the response the caller
// actually receives.
//
// The identity is the whole reason both aggregates are allowed to coexist:
// they count different things, and the difference has to be CHECKABLE, not
// merely explainable.
func TestShow_ChildCountsSatisfyINV10Identity(t *testing.T) {
	pool := newTestPool(t)

	owner, _ := seedUser(t, pool, "admin")
	viewer, _ := seedUser(t, pool, "user")
	visProj, visPrefix := seedProject(t, pool, owner, "shcc48")
	hidProj, hidPrefix := seedProject(t, pool, owner, "shcc48h")
	testsupport.AddProjectMember(t, pool, visProj, viewer, "member")

	node := seedIssue(t, pool, visProj, owner, visPrefix+"-1", "wave head", "open", 2)
	for i, st := range []string{"open", "closed", "standing"} {
		childID := seedIssue(t, pool, visProj, owner,
			fmt.Sprintf("%s-1.%d", visPrefix, i+1), "child "+st, st, 2)
		testsupport.SeedDep(t, pool, childID, node, dependencies.DepParentChild, owner)
	}
	hidden := seedIssue(t, pool, hidProj, owner, hidPrefix+"-1", "unreachable", "open", 2)
	testsupport.SeedDep(t, pool, hidden, node, dependencies.DepParentChild, owner)

	handler := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": node}
	res, err := handler(withCtx(t, viewer, "user"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)

	card := cardOf(t, body)
	counts := body["depsCounts"].(map[string]any)
	cell := counts["down"].(map[string]any)["parent-child"].(map[string]any)

	total := 0
	for _, bucket := range []string{"open", "in_progress", "deferred", "closed", "standing", "unavailable"} {
		total += int(cell[bucket].(float64))
	}
	standing := int(cell["standing"].(float64))
	unavailable := int(cell["unavailable"].(float64))
	children := int(card["childrenCount"].(float64))

	if want := total - standing - unavailable; children != want {
		t.Errorf("INV-10 identity broken: childrenCount = %d, but "+
			"down/parent-child total(%d) − standing(%d) − unavailable(%d) = %d.\n"+
			"The two aggregates are allowed to coexist only because this holds: the "+
			"dependency counters count LINKS (standing included, because a standing status is "+
			"a legitimate edge state), the child counters count UNITS OF WORK.",
			children, total, standing, unavailable, want)
	}
	// Non-vacuity: the fixture must actually exercise both subtractions.
	if standing == 0 || unavailable == 0 {
		t.Fatalf("the fixture has standing=%d unavailable=%d — with either at zero the "+
			"identity would hold trivially", standing, unavailable)
	}
}

// row 49 — «is the wave finished?» must not be answered by a zero alone.
func TestShow_ZeroUnclosedWithUnavailableIsNotAClosedWave(t *testing.T) {
	pool := newTestPool(t)

	owner, _ := seedUser(t, pool, "admin")
	viewer, _ := seedUser(t, pool, "user")
	visProj, visPrefix := seedProject(t, pool, owner, "shcc49")
	hidProj, hidPrefix := seedProject(t, pool, owner, "shcc49h")
	testsupport.AddProjectMember(t, pool, visProj, viewer, "member")

	node := seedIssue(t, pool, visProj, owner, visPrefix+"-1", "wave head", "open", 2)
	// Every VISIBLE child is closed …
	for i := range 2 {
		childID := seedIssue(t, pool, visProj, owner,
			fmt.Sprintf("%s-1.%d", visPrefix, i+1), "done", "closed", 2)
		testsupport.SeedDep(t, pool, childID, node, dependencies.DepParentChild, owner)
	}
	// … and one child is out of reach and OPEN.
	hidden := seedIssue(t, pool, hidProj, owner, hidPrefix+"-1", "still open", "open", 2)
	testsupport.SeedDep(t, pool, hidden, node, dependencies.DepParentChild, owner)

	handler := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": node}
	res, err := handler(withCtx(t, viewer, "user"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	card := cardOf(t, body)
	cell := body["depsCounts"].(map[string]any)["down"].(map[string]any)["parent-child"].(map[string]any)

	if got := card["unclosedChildrenCount"]; got != float64(0) {
		t.Fatalf("unclosedChildrenCount = %v, want 0 — the fixture is built so the visible "+
			"side looks finished", got)
	}
	if got := cell["unavailable"]; got != float64(1) {
		t.Errorf("down/parent-child unavailable = %v, want 1 — without it an agent reads the "+
			"zero above as permission to close a container that still has an invisible open "+
			"child, which is the exact scenario count-only-the-visible was rejected for",
			got)
	}
	// The invisible child's status must not have leaked into any bucket:
	// «cannot say» has to stay «cannot say».
	if got := cell["open"]; got != float64(0) {
		t.Errorf("down/parent-child open = %v, want 0 — the unreachable child's status leaked", got)
	}
}

// row 50 — an invisible STANDING child lands only in `unavailable`.
//
// It is its own row because it is where two exclusions meet: standing
// children drop out of the child counters, and invisible ones drop out of
// the status buckets. If the standing rule were applied to an invisible
// child, its status would be disclosed by the very act of classifying it.
func TestShow_InvisibleStandingChildOnlyInUnavailable(t *testing.T) {
	pool := newTestPool(t)

	owner, _ := seedUser(t, pool, "admin")
	viewer, _ := seedUser(t, pool, "user")
	visProj, visPrefix := seedProject(t, pool, owner, "shcc50")
	hidProj, hidPrefix := seedProject(t, pool, owner, "shcc50h")
	testsupport.AddProjectMember(t, pool, visProj, viewer, "member")

	node := seedIssue(t, pool, visProj, owner, visPrefix+"-1", "wave head", "open", 2)
	hiddenStanding := seedIssue(t, pool, hidProj, owner, hidPrefix+"-1", "perpetual", "standing", 2)
	testsupport.SeedDep(t, pool, hiddenStanding, node, dependencies.DepParentChild, owner)

	handler := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": node}
	res, err := handler(withCtx(t, viewer, "user"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	cell := body["depsCounts"].(map[string]any)["down"].(map[string]any)["parent-child"].(map[string]any)

	if got := cell["unavailable"]; got != float64(1) {
		t.Errorf("unavailable = %v, want 1", got)
	}
	if got := cell["standing"]; got != float64(0) {
		t.Errorf("standing = %v, want 0 — putting an INVISIBLE child in the standing bucket "+
			"discloses its status, which is the one thing the unavailable category exists to "+
			"prevent", got)
	}
	for _, bucket := range []string{"open", "in_progress", "deferred", "closed"} {
		if got := cell[bucket]; got != float64(0) {
			t.Errorf("%s = %v, want 0 — no status bucket may describe an unreachable child",
				bucket, got)
		}
	}
	// And it is not a unit of work for this caller either.
	card := cardOf(t, body)
	if got := card["childrenCount"]; got != float64(0) {
		t.Errorf("childrenCount = %v, want 0 — an invisible child is not counted as work", got)
	}
}

// TestShow_EpicRollupCarriesDeferredAndNarrowedBlocked pins the sk-421.2
// contract change ON THE MCP SURFACE. Two facts at once: the new
// `deferredChildren` bucket reaches the wire, and `blockedChildren` no
// longer counts a DEFERRED child carrying an active blocker (R-11, owner
// answer N-1a) — it is a subset of open + in_progress.
//
// Why this test has to exist: the MCP surface has NO composition guard at
// all. showResponse.Issue is `any`, so a new summary field travels to MCP
// consumers unchecked — nothing here would have noticed either change.
//
// RED BEFORE THE FIX: `deferredChildren` was absent from the wire, and the
// old predicate counted the deferred child, giving blockedChildren = 2.
func TestShow_EpicRollupCarriesDeferredAndNarrowedBlocked(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "epicroll")

	epicID := prefix + "-1"
	// child .1 deferred, child .2 open
	seedEpicWithChildren(t, pool, pid, userID, epicID, []string{"deferred", "open"})

	blockerID := prefix + "-2"
	seedIssue(t, pool, pid, userID, blockerID, "external blocker", "open", 2)

	// One gating blocker for BOTH children: only the open one may count.
	for _, child := range []string{epicID + ".1", epicID + ".2"} {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'blocks', '{}'::jsonb, 1, $3, now())
ON CONFLICT (from_id, to_id, type) DO NOTHING`,
			child, blockerID, userID); err != nil {
			t.Fatalf("seed blocks edge for %s: %v", child, err)
		}
	}

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": epicID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}

	body := extractToolJSON(t, res)
	issueRaw, ok := body["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue is %T, want object", body["issue"])
	}
	epicRaw, ok := issueRaw["epic"].(map[string]any)
	if !ok {
		t.Fatalf("issue.epic is %T, want object — the rollup must reach the MCP surface",
			issueRaw["epic"])
	}
	num := func(key string) float64 {
		t.Helper()
		v, present := epicRaw[key]
		if !present {
			t.Fatalf("issue.epic.%s missing on the MCP wire; keys present: %v", key, epicRaw)
		}
		f, isNum := v.(float64)
		if !isNum {
			t.Fatalf("issue.epic.%s is %T, want number", key, v)
		}
		return f
	}

	if got := num("deferredChildren"); got != 1 {
		t.Errorf("deferredChildren = %v, want 1", got)
	}
	if got := num("blockedChildren"); got != 1 {
		t.Errorf("blockedChildren = %v, want 1 — the deferred child carries a blocker "+
			"but must not be counted (R-11)", got)
	}
	if blocked, y := num("blockedChildren"), num("openChildren")+num("inProgressChildren"); blocked > y {
		t.Errorf("B > Y on the MCP surface: blocked = %v, open+in_progress = %v", blocked, y)
	}
}
