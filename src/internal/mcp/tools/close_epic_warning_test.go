package tools

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// TestClose_EpicWarningEntryShapeUnchanged is sk-421.1 test-plan row 10.
//
// The warning list shares its entry type with the detail's blockers
// (issues.BlockerRef, plan decision 3). sk-421.1 T001 made Title and Status
// optional and added Redacted for the blockers list — and the warning must
// NOT follow: its entries always carry title and status, never redacted, and
// a child the closer cannot see is OMITTED from it, not listed as a
// redacted entry.
func TestClose_EpicWarningEntryShapeUnchanged(t *testing.T) {
	pool := newTestPool(t)
	closer, closerName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, closer, "cewshape")
	epicID := prefix + "-1"
	seedEpicWithChildren(t, pool, pid, closer, epicID, []string{"open", "in_progress"})
	assignIssue(t, pool, epicID, closerName)

	// A child hung from a project the closer is not a member of.
	outsider, _ := seedUser(t, pool, "user")
	hiddenPID, hiddenPrefix := seedProject(t, pool, outsider, "cewhid")
	const hiddenTitle = "secret child of another project"
	hiddenID := seedIssue(t, pool, hiddenPID, outsider, hiddenPrefix+"-1", hiddenTitle, "open", 2)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`,
		hiddenID, epicID, outsider); err != nil {
		t.Fatalf("seed cross-project parent-child: %v", err)
	}

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, closer, "user", "sess-cewshape")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              epicID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "closing with children still open",
	}
	res, err := handleClose(pool, store)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)

	// Two visible unclosed children; the hidden one is omitted, not redacted.
	assertWarningKeysAtTopLevel(t, body, 2)
	list, _ := body["openChildList"].([]any)
	for i, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("openChildList[%d] is %T, want an object", i, raw)
		}
		keys := sortedKeys(entry)
		sort.Strings(keys)
		if got := strings.Join(keys, ","); got != "id,status,title" {
			t.Errorf("openChildList[%d] keys = %s, want id,status,title — the warning entry "+
				"must not take the blockers' redacted form: %v", i, got, entry)
		}
		if title, _ := entry["title"].(string); title == "" {
			t.Errorf("openChildList[%d] title = %v, want the child's title", i, entry["title"])
		}
		if status, _ := entry["status"].(string); status == "" {
			t.Errorf("openChildList[%d] status = %v, want the child's status", i, entry["status"])
		}
		if entry["id"] == hiddenID {
			t.Errorf("openChildList[%d] is the invisible child %s — it must be omitted", i, hiddenID)
		}
	}
	if text := toolText(t, res); strings.Contains(text, hiddenTitle) || strings.Contains(text, hiddenID) {
		t.Errorf("the invisible child leaked into the close response:\n%s", text)
	}
}

// seedEpicWithChildren is a low-level fixture helper for slice 5
// tests. Inserts an epic + N children with the supplied statuses.
// Children are hierarchical under the epic (parent_id = epicID).
func seedEpicWithChildren(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID,
	epicID string, childStatuses []string) {
	t.Helper()
	ctx := context.Background()
	// Epic
	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, 'the epic', '', 'epic', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $3, now(), now(), '{}'::jsonb)`,
		epicID, projectID, creator); err != nil {
		t.Fatalf("seed epic %s: %v", epicID, err)
	}
	for i, status := range childStatuses {
		childID := epicID + "." + itoaInt(i+1)
		closedAt := "NULL"
		if status == "closed" {
			closedAt = "now()"
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1::varchar, $2, $3, 1, $1::varchar, '', 'task', 2, $4, false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, `+closedAt+`, NULL,
        NULL, $5, now(), now(), '{}'::jsonb)`,
			childID, projectID, epicID, status, creator); err != nil {
			t.Fatalf("seed child %s: %v", childID, err)
		}
		// sk-35: readers now derive epic children from the dep graph,
		// not from issues.parent_id. Write the canonical parent-child
		// edge so the close-warning predicate finds these children.
		if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())
ON CONFLICT (from_id, to_id, type) DO NOTHING`,
			childID, epicID, creator); err != nil {
			t.Fatalf("seed parent-child dep for %s: %v", childID, err)
		}
	}
}

// assignIssue stamps an assignee onto an already-seeded issue. The
// seed helpers insert assignee=NULL, which now trips the sk-71
// ownerless-on-close warning; tests that assert "no warning" must
// give the issue an owner first so only the condition under test
// drives the result.
func assignIssue(t *testing.T, pool *pgxpool.Pool, issueID, assignee string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET assignee = $2 WHERE id = $1`, issueID, assignee); err != nil {
		t.Fatalf("assign issue %s: %v", issueID, err)
	}
}

// itoaInt is a tiny strconv-free int→string for the test seed helper.
func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestClose_EpicWithOpenChildren verifies the non-blocking warning:
// closing an epic that still has open children proceeds but the
// response carries `warning` + `openChildList[]` for the agent to
// react on.
func TestClose_EpicWithOpenChildren(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ceow")
	epicID := prefix + "-1"
	// 2 open + 1 closed → warning + 2 entries in openChildList.
	seedEpicWithChildren(t, pool, pid, userID, epicID, []string{"open", "open", "closed"})
	// Give the epic an owner so only the open-children condition drives
	// the warning (not the sk-71 ownerless nudge).
	assignIssue(t, pool, epicID, "admin")

	store := issues.NewStore(pool)
	closeHandler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-mine")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              epicID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "closing despite open children",
	}
	res, err := closeHandler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if body["status"] != "closed" {
		t.Fatalf("status = %v, want closed (close should proceed regardless)", body["status"])
	}
	warning, _ := body["warning"].(string)
	if warning == "" {
		t.Fatalf("warning empty; want non-empty when closing epic with open children")
	}
	open, _ := body["openChildList"].([]any)
	if len(open) != 2 {
		t.Fatalf("openChildList = %d entries, want 2", len(open))
	}
}

// TestClose_EpicAllChildrenClosed verifies no warning fires when every
// child is already closed (the eligible-for-close path).
func TestClose_EpicAllChildrenClosed(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ceacc")
	epicID := prefix + "-1"
	seedEpicWithChildren(t, pool, pid, userID, epicID, []string{"closed", "closed"})
	// Owner present so the sk-71 ownerless nudge stays silent — this
	// test asserts the clean (no-warning) path.
	assignIssue(t, pool, epicID, "admin")

	store := issues.NewStore(pool)
	closeHandler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-mine")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              epicID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "all done",
	}
	res, err := closeHandler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if _, has := body["warning"]; has {
		t.Errorf("warning field present on clean close: %v", body["warning"])
	}
	if _, has := body["openChildList"]; has {
		t.Errorf("openChildList field present on clean close: %v", body["openChildList"])
	}
}

// TestClose_NonEpicNoWarning verifies the warning never fires for
// regular task/feature/bug issues — only for type=epic.
func TestClose_NonEpicNoWarning(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cnenw")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "regular task", "open", 2)
	// Owner present so the sk-71 ownerless nudge stays silent — this
	// test isolates the "non-epic never warns about children" rule.
	assignIssue(t, pool, issueID, "admin")

	store := issues.NewStore(pool)
	closeHandler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-mine")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "done",
	}
	res, err := closeHandler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if _, has := body["warning"]; has {
		t.Errorf("warning field set on non-epic close: %v", body["warning"])
	}
}

// TestClose_OwnerlessWarns verifies the sk-71 nudge: closing an issue
// with no assignee proceeds but the response carries a `warning`
// reminding the agent the issue is now owned by nobody.
func TestClose_OwnerlessWarns(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cow")
	issueID := prefix + "-1"
	// Seeded ownerless (assignee NULL) — no assignIssue call.
	seedIssue(t, pool, pid, userID, issueID, "ownerless task", "open", 2)

	store := issues.NewStore(pool)
	closeHandler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-mine")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "done",
	}
	res, err := closeHandler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	// Close still succeeds — the warning is a nudge, not a wall.
	if body["status"] != "closed" {
		t.Fatalf("status = %v, want closed", body["status"])
	}
	warning, _ := body["warning"].(string)
	if warning == "" {
		t.Fatalf("warning empty; want non-empty when closing an ownerless issue")
	}
	if !strings.Contains(warning, "no assignee") {
		t.Errorf("warning = %q, want it to mention the missing assignee", warning)
	}
	// Non-epic ownerless close must not carry openChildList.
	if _, has := body["openChildList"]; has {
		t.Errorf("openChildList present on non-epic close: %v", body["openChildList"])
	}
}

// TestClose_OwnerlessEpicWithChildren verifies both conditions stack:
// an ownerless epic closed with open children gets BOTH the
// open-children reason and the sk-71 ownerless reason in one warning.
func TestClose_OwnerlessEpicWithChildren(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cowec")
	epicID := prefix + "-1"
	// Ownerless epic (no assignIssue) + 1 open child → both reasons.
	seedEpicWithChildren(t, pool, pid, userID, epicID, []string{"open"})

	store := issues.NewStore(pool)
	closeHandler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-mine")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              epicID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "closing ownerless epic with open child",
	}
	res, err := closeHandler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	warning, _ := body["warning"].(string)
	if !strings.Contains(warning, "open children") {
		t.Errorf("warning = %q, want it to mention open children", warning)
	}
	if !strings.Contains(warning, "no assignee") {
		t.Errorf("warning = %q, want it to mention the missing assignee", warning)
	}
	open, _ := body["openChildList"].([]any)
	if len(open) != 1 {
		t.Fatalf("openChildList = %d entries, want 1", len(open))
	}
}

// assertWarningKeysAtTopLevel is the shared oracle for matrix rows 77,
// 78 and 79.
//
// The key names and their NESTING LEVEL are the contract US-5 protects
// by name ("Both names and both nesting levels are preserved"), so they are
// written here as LITERALS. Comparing against a constant lifted out of
// close.go would make the guard move whenever the production code moves,
// which is the one thing a name-freezing test must not do.
//
// `wantOpenChildren` < 0 means "this condition does not produce the
// list" — a non-epic ownerless close must NOT grow an openChildList.
func assertWarningKeysAtTopLevel(t *testing.T, body map[string]any, wantOpenChildren int) {
	t.Helper()
	warning, isString := body["warning"].(string)
	if !isString || warning == "" {
		t.Fatalf("top-level `warning` missing or not a string: %#v", body["warning"])
	}
	if wantOpenChildren < 0 {
		if v, present := body["openChildList"]; present {
			t.Errorf("openChildList present where no children condition fired: %v", v)
		}
		return
	}
	list, isArray := body["openChildList"].([]any)
	if !isArray {
		t.Fatalf("top-level `openChildList` missing or not an array: %#v", body["openChildList"])
	}
	if len(list) != wantOpenChildren {
		t.Fatalf("openChildList has %d entries, want %d", len(list), wantOpenChildren)
	}
	// The name must not have drifted to the counter's name. W1: an
	// integer lives at `epic.openChildren` and this list at the top
	// level; conflating them is the failure the distinct names exist to
	// prevent.
	if v, present := body["openChildren"]; present {
		t.Errorf("top-level `openChildren` appeared (= %v) — the list is `openChildList`; "+
			"`openChildren` is the nested epic counter and must stay nested", v)
	}
}

// TestClose_WarningKeysSurviveTheAcknowledgement is matrix rows 77 + 78:
// both conditions of the same warning, asserted on the acknowledgement
// rather than on the card that used to carry them.
func TestClose_WarningKeysSurviveTheAcknowledgement(t *testing.T) {
	cases := []struct {
		name             string
		epic             bool
		childStatuses    []string
		assign           bool
		wantOpenChildren int
		wantMentions     []string
	}{
		{
			// row 77 — epic with open children.
			name:             "epic with open children",
			epic:             true,
			childStatuses:    []string{"open", "open", "closed"},
			assign:           true,
			wantOpenChildren: 2,
			wantMentions:     []string{"open children"},
		},
		{
			// row 78 — the SECOND condition of the same warning. It is a
			// separate case and not a variation of the first because it
			// is the one that silently disappears if the warning branch
			// is rebuilt around the children snapshot alone.
			name:             "task with no assignee",
			epic:             false,
			assign:           false,
			wantOpenChildren: -1,
			wantMentions:     []string{"no assignee"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			userID, _ := seedUser(t, pool, "admin")
			pid, prefix := seedProject(t, pool, userID, "cwarn")
			issueID := prefix + "-1"
			if tc.epic {
				seedEpicWithChildren(t, pool, pid, userID, issueID, tc.childStatuses)
			} else {
				seedIssue(t, pool, pid, userID, issueID, "regular task", "open", 2)
			}
			if tc.assign {
				assignIssue(t, pool, issueID, "admin")
			}

			store := issues.NewStore(pool)
			ctx := withCtxSession(t, userID, "admin", "sess-warn")
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"id":              issueID,
				"expectedVersion": 1,
				"closeOutcome":    "done",
				"reason":          "SENTINEL-CLOSE-REASON supplied by the caller",
			}
			res, err := handleClose(pool, store)(ctx, req)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if res.IsError {
				t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
			}
			body := extractToolJSON(t, res)

			assertWarningKeysAtTopLevel(t, body, tc.wantOpenChildren)
			warning, _ := body["warning"].(string)
			for _, want := range tc.wantMentions {
				if !strings.Contains(warning, want) {
					t.Errorf("warning = %q, want it to mention %q", warning, want)
				}
			}
			// The acknowledgement rules still hold on the warning path —
			// the close still happened, and the reason still must not
			// come back.
			if body["status"] != "closed" {
				t.Errorf("status = %v, want closed (the warning is a nudge, not a wall)",
					body["status"])
			}
			if v, _ := body["version"].(float64); int(v) < 2 {
				t.Errorf("version = %v, want the post-close version", body["version"])
			}
			if strings.Contains(toolText(t, res), "SENTINEL-CLOSE-REASON") {
				t.Errorf("the warning path echoes the caller's reason:\n%s", toolText(t, res))
			}
		})
	}
}

// TestClose_WarningKeysIdenticalInBothModes is matrix row 79.
//
// "Edge 'warning and verbose=true': the warning keys at the same
// level and under the same names as in the short mode." The oracle is a
// COMPARISON of the two modes on the same fixture, not two independent
// assertions: two modes can each satisfy a checklist and still disagree
// with each other.
func TestClose_WarningKeysIdenticalInBothModes(t *testing.T) {
	run := func(t *testing.T, verbose bool) map[string]any {
		t.Helper()
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "admin")
		pid, prefix := seedProject(t, pool, userID, "cwmode")
		epicID := prefix + "-1"
		seedEpicWithChildren(t, pool, pid, userID, epicID, []string{"open", "in_progress"})
		assignIssue(t, pool, epicID, "admin")

		store := issues.NewStore(pool)
		ctx := withCtxSession(t, userID, "admin", "sess-cwmode")
		args := map[string]any{
			"id":              epicID,
			"expectedVersion": 1,
			"closeOutcome":    "done",
			"reason":          "closing with children still open",
		}
		if verbose {
			args["verbose"] = true
		}
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := handleClose(pool, store)(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
		}
		return extractToolJSON(t, res)
	}

	lean := run(t, false)
	full := run(t, true)

	// Both children are unclosed (open + in_progress) — EpicOpenChildrenFor
	// filters on NOT IN ('closed','standing'), not on status='open'.
	assertWarningKeysAtTopLevel(t, lean, 2)
	assertWarningKeysAtTopLevel(t, full, 2)

	if lean["warning"] != full["warning"] {
		t.Errorf("warning text differs between modes:\nlean %v\nfull %v",
			lean["warning"], full["warning"])
	}
	// The two runs seed independent projects, so the ids differ by
	// construction — what row 79 pins is the SHAPE: the same keys per
	// entry, in the same order, carrying the same statuses.
	if got, want := entryShapes(t, lean), entryShapes(t, full); !reflect.DeepEqual(got, want) {
		t.Errorf("openChildList entries differ between modes:\nlean %v\nfull %v", got, want)
	}
	// The verbose mode is the INV-4 way out: it is the FULL card, which
	// the lean one is not. That difference is expected — the warning keys
	// riding at the same level in both is what row 79 pins.
	if _, present := full["description"]; !present {
		t.Errorf("verbose close is not the full IssueDetail: %v", sortedKeys(full))
	}
	if _, present := lean["description"]; present {
		t.Errorf("the acknowledgement carries description = %v", lean["description"])
	}
}

// TestClose_FP7_ChildCountersAndWarningGateAgree is matrix row 80.
//
// "FP-7: the child counters (T012) and the warning gate by issue type do
// not contradict each other on one card." The two surfaces are read
// on the SAME issue — the card first, then the close — and the
// relationship between them is asserted as arithmetic, which is what
// INV-10 demands of coexisting counters.
func TestClose_FP7_ChildCountersAndWarningGateAgree(t *testing.T) {
	t.Run("epic: the list is the unclosed population, not the open one", func(t *testing.T) {
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "admin")
		pid, prefix := seedProject(t, pool, userID, "fp7e")
		epicID := prefix + "-1"
		// Hostile on purpose: the three statuses that separate the two
		// counters (open vs in_progress vs deferred), plus a closed child
		// and a STANDING one — the two populations that must be excluded
		// from different places for different reasons.
		seedEpicWithChildren(t, pool, pid, userID, epicID,
			[]string{"open", "in_progress", "deferred", "closed", "standing"})
		assignIssue(t, pool, epicID, "admin")

		store := issues.NewStore(pool)
		ctx := withCtxSession(t, userID, "admin", "sess-fp7e")

		// The US-3 counters ride with the dependency block by design, so
		// includeDeps stays ON here — switching it off switches them off
		// too, and the read would prove nothing.
		card := callShow(t, handleShow(pool, store, dependencies.NewStore(pool)), ctx,
			map[string]any{"id": epicID, "includeComments": false})
		issue, ok := card["issue"].(map[string]any)
		if !ok {
			t.Fatalf("skein_show issue type %T", card["issue"])
		}
		childrenCount := intField(t, issue, "childrenCount")
		unclosed := intField(t, issue, "unclosedChildrenCount")
		epicRollup, ok := issue["epic"].(map[string]any)
		if !ok {
			t.Fatalf("epic rollup missing on an epic card: %#v", issue["epic"])
		}
		epicOpen := intField(t, epicRollup, "openChildren")

		// US-3: standing is not a unit of work. 5 seeded, 1 standing.
		if childrenCount != 4 {
			t.Errorf("childrenCount = %d, want 4 (standing excluded)", childrenCount)
		}
		// Unclosed = open + in_progress + deferred.
		if unclosed != 3 {
			t.Errorf("unclosedChildrenCount = %d, want 3", unclosed)
		}
		// The nested epic counter is the NARROW one: status='open' only.
		if epicOpen != 1 {
			t.Errorf("epic.openChildren = %d, want 1", epicOpen)
		}

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"id":              epicID,
			"expectedVersion": 1,
			"closeOutcome":    "done",
			"reason":          "closing to compare the warning against the counters",
		}
		res, err := handleClose(pool, store)(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
		}
		body := extractToolJSON(t, res)
		list, _ := body["openChildList"].([]any)

		// THE identity: the warning list and the US-3 unclosed counter
		// answer the same question and must agree. The nested
		// epic.openChildren is narrower, and the gap is the explained
		// discrepancy INV-10 asks to be named rather than merely
		// tolerated.
		if len(list) != unclosed {
			t.Errorf("openChildList has %d entries but unclosedChildrenCount is %d — "+
				"the close warning and the card disagree about the same population",
				len(list), unclosed)
		}
		if len(list) <= epicOpen {
			t.Errorf("openChildList (%d) is not wider than epic.openChildren (%d) — the "+
				"fixture stopped exercising the difference between the two counters",
				len(list), epicOpen)
		}
		// The standing child is in neither.
		for _, raw := range list {
			entry, _ := raw.(map[string]any)
			if entry["status"] == "standing" {
				t.Errorf("openChildList contains a standing child: %v", entry)
			}
		}
	})

	t.Run("non-epic: the type gate is a named limit, not a disagreement", func(t *testing.T) {
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "admin")
		pid, prefix := seedProject(t, pool, userID, "fp7t")
		parentID := prefix + "-1"
		seedIssue(t, pool, pid, userID, parentID, "container task, not an epic", "open", 2)
		assignIssue(t, pool, parentID, "admin")
		childID := prefix + "-2"
		seedIssue(t, pool, pid, userID, childID, "child of a non-epic", "open", 2)
		if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`,
			childID, parentID, userID); err != nil {
			t.Fatalf("seed parent-child edge: %v", err)
		}

		store := issues.NewStore(pool)
		ctx := withCtxSession(t, userID, "admin", "sess-fp7t")

		card := callShow(t, handleShow(pool, store, dependencies.NewStore(pool)), ctx,
			map[string]any{"id": parentID, "includeComments": false})
		issue, _ := card["issue"].(map[string]any)
		if got := intField(t, issue, "unclosedChildrenCount"); got != 1 {
			t.Fatalf("unclosedChildrenCount = %d, want 1 — the fixture must have an "+
				"unclosed child for the gate to be observable", got)
		}

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"id":              parentID,
			"expectedVersion": 1,
			"closeOutcome":    "done",
			"reason":          "closing a non-epic container",
		}
		res, err := handleClose(pool, store)(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
		}
		body := extractToolJSON(t, res)

		// The children warning is gated on type=epic and extending it is
		// out of this run's scope. The counters on the card are
		// nonetheless correct and non-zero: the two surfaces answer
		// different questions, and neither claims the other's answer.
		// Asserted so that a future change to either one has to come
		// past this statement.
		if _, present := body["openChildList"]; present {
			t.Errorf("openChildList on a non-epic close: %v — the type gate changed; "+
				"if that is intended it is a behaviour change with its own acceptance",
				body["openChildList"])
		}
		if w, present := body["warning"]; present {
			t.Errorf("unexpected warning on an owned non-epic close: %v", w)
		}
	})
}

// intField reads an integer counter off a JSON-decoded card.
func intField(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s = %#v, want a number", key, m[key])
	}
	return int(v)
}

// entryShapes reduces an openChildList to what is comparable between two
// independently-seeded runs: per entry, its sorted key names plus its
// status. Ids and titles carry the fixture's random project prefix and
// would make any cross-run comparison fail for the wrong reason.
func entryShapes(t *testing.T, body map[string]any) []string {
	t.Helper()
	list, ok := body["openChildList"].([]any)
	if !ok {
		t.Fatalf("openChildList missing or not an array: %#v", body["openChildList"])
	}
	out := make([]string, 0, len(list))
	for i, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("openChildList[%d] is %T, want an object", i, raw)
		}
		keys := sortedKeys(entry)
		sort.Strings(keys)
		status, _ := entry["status"].(string)
		out = append(out, strings.Join(keys, ",")+"|"+status)
	}
	return out
}
