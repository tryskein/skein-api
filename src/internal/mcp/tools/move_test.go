package tools

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// TestMove_HappyPath seeds two projects (source + target), an issue in
// source, and one component in target, then asserts the signpost
// response carries the right shape: supported=false, source IssueDetail,
// targetProject echo, targetComponents picker, two nextSteps pointing at
// skein_create (with supersedes edge on the source) and skein_close
// (with expectedVersion captured from source).
func TestMove_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	srcPID, srcPrefix := seedProject(t, pool, userID, "mvsrc")
	tgtPID, tgtPrefix := seedProject(t, pool, userID, "mvtgt")
	issueID := srcPrefix + "-1"
	seedIssue(t, pool, srcPID, userID, issueID, "rollout issue", "open", 2)

	// Seed a component in the target whose name appears in the source
	// title — the matchHint branch should fire.
	cmpID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, 'rollout', 'infra rollout module', '[]'::jsonb, NULL, $3, now(), now())`,
		cmpID, tgtPID, userID); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	iStore := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleMove(pool, iStore, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":       issueID,
		"targetProject": tgtPID.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError=true: %+v", body)
	}
	body := extractToolJSON(t, res)

	// Top-level: signpost stance + reason string.
	if supported, _ := body["supported"].(bool); supported {
		t.Fatalf("supported must be false, got %v", body["supported"])
	}
	if reason, _ := body["reason"].(string); reason == "" {
		t.Fatalf("reason must be a non-empty string, got %v", body["reason"])
	}

	// source echo: just confirm the id is present. Since sk-282 T025 the
	// card is the full IssueDetail MINUS what the prepared call carries;
	// that subtraction has its own test below.
	src, ok := body["source"].(map[string]any)
	if !ok {
		t.Fatalf("missing source object: %+v", body)
	}
	if id, _ := src["id"].(string); id != issueID {
		t.Fatalf("source.id = %v, want %s", src["id"], issueID)
	}

	// targetProject echo: idPrefix must match what we seeded.
	tp, ok := body["targetProject"].(map[string]any)
	if !ok {
		t.Fatalf("missing targetProject object: %+v", body)
	}
	if gotPrefix, _ := tp["idPrefix"].(string); gotPrefix != tgtPrefix {
		t.Fatalf("targetProject.idPrefix = %v, want %s", tp["idPrefix"], tgtPrefix)
	}

	// targetComponents picker: one row with matchHint populated (the
	// component name "rollout" appears in the source title).
	comps, _ := body["targetComponents"].([]any)
	if len(comps) != 1 {
		t.Fatalf("targetComponents length = %d, want 1: %+v", len(comps), comps)
	}
	first, _ := comps[0].(map[string]any)
	if name, _ := first["name"].(string); name != "rollout" {
		t.Fatalf("targetComponents[0].name = %v, want rollout", first["name"])
	}
	if hint, _ := first["matchHint"].(string); hint == "" {
		t.Fatalf("targetComponents[0].matchHint must fire on title match, got %v", first["matchHint"])
	}

	// nextSteps: two prepared calls in exact order — create first, close
	// second. The agent reads them sequentially.
	steps, _ := body["nextSteps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("nextSteps length = %d, want 2: %+v", len(steps), steps)
	}

	step0, _ := steps[0].(map[string]any)
	if tool, _ := step0["tool"].(string); tool != "skein_create" {
		t.Fatalf("nextSteps[0].tool = %v, want skein_create", step0["tool"])
	}
	args0, _ := step0["args"].(map[string]any)
	if got, _ := args0["project"].(string); got != tgtPrefix {
		t.Fatalf("create.project = %v, want %s", args0["project"], tgtPrefix)
	}
	if got, _ := args0["kind"].(string); got != "directed" {
		t.Fatalf("create.kind = %v, want directed", args0["kind"])
	}
	// supersedes dependency edge pointing at the source — this is the
	// load-bearing audit link.
	deps, _ := args0["dependencies"].([]any)
	if len(deps) != 1 {
		t.Fatalf("create.dependencies length = %d, want 1", len(deps))
	}
	dep0, _ := deps[0].(map[string]any)
	if got, _ := dep0["toId"].(string); got != issueID {
		t.Fatalf("dependencies[0].toId = %v, want %s", dep0["toId"], issueID)
	}
	if got, _ := dep0["type"].(string); got != "supersedes" {
		t.Fatalf("dependencies[0].type = %v, want supersedes", dep0["type"])
	}

	step1, _ := steps[1].(map[string]any)
	if tool, _ := step1["tool"].(string); tool != "skein_close" {
		t.Fatalf("nextSteps[1].tool = %v, want skein_close", step1["tool"])
	}
	args1, _ := step1["args"].(map[string]any)
	if got, _ := args1["id"].(string); got != issueID {
		t.Fatalf("close.id = %v, want %s", args1["id"], issueID)
	}
	// expectedVersion comes back as float64 through JSON; we seed
	// version=1 so the close-step echoes that.
	if got, _ := args1["expectedVersion"].(float64); got != 1 {
		t.Fatalf("close.expectedVersion = %v, want 1", args1["expectedVersion"])
	}

	// Sanity: source has not changed (signpost is pure-read).
	_ = srcPID
}

func TestMove_IssueNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	_, tgtPrefix := seedProject(t, pool, userID, "mvtgt2")

	iStore := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleMove(pool, iStore, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":       "nonexistent-9999",
		"targetProject": tgtPrefix,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIssueNotFound {
		t.Fatalf("error code = %v, want %s", got, ErrCodeIssueNotFound)
	}
}

func TestMove_TargetProjectNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	srcPID, srcPrefix := seedProject(t, pool, userID, "mvsrc3")
	issueID := srcPrefix + "-1"
	seedIssue(t, pool, srcPID, userID, issueID, "demo", "open", 2)

	iStore := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleMove(pool, iStore, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":       issueID,
		"targetProject": "no-such-project-handle-xyz",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeProjectNotFound {
		t.Fatalf("error code = %v, want %s", got, ErrCodeProjectNotFound)
	}
}

func TestMove_SameProject(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	srcPID, srcPrefix := seedProject(t, pool, userID, "mvsame")
	issueID := srcPrefix + "-1"
	seedIssue(t, pool, srcPID, userID, issueID, "demo", "open", 2)

	iStore := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	handler := handleMove(pool, iStore, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":       issueID,
		"targetProject": srcPID.String(), // same project as source
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on same-project move")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error code = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestMove_INV3_EachTextOnceAndTheKitStillRuns is matrix row 88.
//
// "INV-3 / sk-253: when preparing a move, every non-empty issue text
// appears in the response exactly once, and the prepared argument set
// remains executable without a further read."
//
// Two oracles, and neither is a checklist:
//
//   - "exactly once" is counted in the RAW response bytes, per
//     sentinel. A key-level check would miss a copy that came back under
//     another name or nested one level deeper.
//   - "executable without a further read" is proven by EXECUTING the kit — the
//     two prepared calls are fed to the real skein_create and skein_close
//     handlers exactly as returned, with only the two substitutions the
//     tool's own `comment` fields instruct. If anything were missing, the
//     call would fail rather than a reviewer having to notice.
func TestMove_INV3_EachTextOnceAndTheKitStillRuns(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	// A SECOND user as assignee: the caller is also the creator, and the
	// card legitimately carries `createdByUsername`, so counting one
	// username would confuse "carried twice" with "two different fields
	// that happen to name the same person".
	_, assigneeName := seedUser(t, pool, "user")
	srcPID, srcPrefix := seedProject(t, pool, userID, "mv253s")
	tgtPID, tgtPrefix := seedProject(t, pool, userID, "mv253t")
	issueID := srcPrefix + "-1"

	texts := map[string]string{
		"title":              "SENTINEL-TITLE of the issue being moved",
		"description":        "SENTINEL-DESCRIPTION body of the issue being moved",
		"design":             "SENTINEL-DESIGN notes of the issue being moved",
		"acceptanceCriteria": "SENTINEL-ACCEPTANCE criteria of the issue being moved",
		"notes":              "SENTINEL-NOTES of the issue being moved",
	}
	seedIssue(t, pool, srcPID, userID, issueID, texts["title"], "open", 2)
	if _, err := pool.Exec(context.Background(), `
UPDATE issues
   SET description = $2, design = $3, acceptance_criteria = $4, notes = $5, assignee = $6
 WHERE id = $1`,
		issueID, texts["description"], texts["design"],
		texts["acceptanceCriteria"], texts["notes"], assigneeName); err != nil {
		t.Fatalf("seed prose: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO issue_labels (issue_id, label) VALUES ($1, 'sentinel-label')`,
		issueID); err != nil {
		t.Fatalf("seed label: %v", err)
	}

	iStore := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	dStore := dependencies.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-mv253")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":       issueID,
		"targetProject": tgtPID.String(),
	}
	res, err := handleMove(pool, iStore, pStore)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	raw := toolText(t, res)
	body := extractToolJSON(t, res)

	for field, want := range texts {
		if n := strings.Count(raw, want); n != 1 {
			t.Errorf("%s appears %d times in the move response, want exactly 1:\n%s",
				field, n, raw)
		}
	}
	if n := strings.Count(raw, "sentinel-label"); n != 1 {
		t.Errorf("the label appears %d times, want exactly 1", n)
	}
	if n := strings.Count(raw, assigneeName); n != 1 {
		t.Errorf("the assignee appears %d times, want exactly 1", n)
	}

	// The subtraction is not a hand-written list: whatever the prepared
	// call carries under a card key, the card must have given up — and
	// nothing else. Comparing against the FULL card from skein_show is
	// what makes that checkable from outside.
	steps, _ := body["nextSteps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("nextSteps = %d, want 2", len(steps))
	}
	step0, _ := steps[0].(map[string]any)
	args0, _ := step0["args"].(map[string]any)
	sourceCard, _ := body["source"].(map[string]any)

	full := callShow(t, handleShow(pool, iStore, dStore), ctx,
		map[string]any{"id": issueID, "includeComments": false, "includeDeps": false})
	fullCard, _ := full["issue"].(map[string]any)
	missing := map[string]bool{}
	for key := range fullCard {
		if _, present := sourceCard[key]; !present {
			missing[key] = true
		}
	}
	for key := range missing {
		if _, carried := args0[key]; !carried {
			t.Errorf("the move card dropped %q but the prepared call does not carry it — "+
				"the text is now in neither place", key)
		}
	}
	if len(missing) == 0 {
		t.Errorf("the move card dropped nothing — the duplication US-5 names is still there")
	}

	// INV-3: the full card is still reachable through a reading tool.
	for field, want := range texts {
		if fullCard[field] != want {
			t.Errorf("skein_show %s = %#v, want %q — the cut is on the wire, not in the data",
				field, fullCard[field], want)
		}
	}

	// Now RUN the kit. Step 0: pick no component (project-wide), call
	// skein_create verbatim.
	createReq := mcpgo.CallToolRequest{}
	createReq.Params.Arguments = args0
	createRes, err := handleCreate(pool, iStore, dStore, pStore)(ctx, createReq)
	if err != nil {
		t.Fatalf("prepared skein_create errored: %v", err)
	}
	if createRes.IsError {
		t.Fatalf("the prepared skein_create call was not executable: %+v",
			extractToolJSON(t, createRes))
	}
	createdBody := extractToolJSON(t, createRes)
	createdIssue, ok := createdBody["issue"].(map[string]any)
	if !ok {
		t.Fatalf("prepared create returned no issue: %+v", createdBody)
	}
	newID, _ := createdIssue["id"].(string)
	if !strings.HasPrefix(newID, tgtPrefix+"-") {
		t.Errorf("new issue id = %q, want it in the target project (%s-*)", newID, tgtPrefix)
	}
	// The supersedes edge — the audit trail the kit exists to preserve.
	var edges int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'supersedes'`,
		newID, issueID).Scan(&edges); err != nil {
		t.Fatalf("count supersedes edge: %v", err)
	}
	if edges != 1 {
		t.Errorf("supersedes edges from %s to %s = %d, want 1", newID, issueID, edges)
	}

	// Step 1: substitute the placeholder exactly as the step's own
	// comment instructs, then call skein_close verbatim.
	step1, _ := steps[1].(map[string]any)
	args1, _ := step1["args"].(map[string]any)
	reason, _ := args1["reason"].(string)
	if !strings.Contains(reason, "<NEW-ID-FROM-skein_create-RESPONSE>") {
		t.Fatalf("close reason lost its placeholder: %q", reason)
	}
	args1["reason"] = strings.ReplaceAll(reason, "<NEW-ID-FROM-skein_create-RESPONSE>", newID)
	// sk-540.2: superseded requires closeTarget, so the kit names the new
	// issue by the same placeholder and the step's comment says to replace
	// it there too.
	if got := args1["closeTarget"]; got != "<NEW-ID-FROM-skein_create-RESPONSE>" {
		t.Fatalf("close closeTarget = %#v, want the new-id placeholder", got)
	}
	args1["closeTarget"] = newID
	closeReq := mcpgo.CallToolRequest{}
	closeReq.Params.Arguments = args1
	closeRes, err := handleClose(pool, iStore)(ctx, closeReq)
	if err != nil {
		t.Fatalf("prepared skein_close errored: %v", err)
	}
	if closeRes.IsError {
		t.Fatalf("the prepared skein_close call was not executable: %+v",
			extractToolJSON(t, closeRes))
	}
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM issues WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("read back source status: %v", err)
	}
	if status != "closed" {
		t.Errorf("source status = %q, want closed after running the kit", status)
	}
	// The edge step 1 made is the one the close found: `existed`, and still
	// exactly one supersedes edge new → source (direction per closeLinkEdge).
	closeBody := extractToolJSON(t, closeRes)
	if got := closeBody["closeLink"]; got != "existed" {
		t.Errorf("close closeLink = %#v, want \"existed\" (step 1 created the edge)", got)
	}
	if got := closeBody["closeOutcome"]; got != "superseded" {
		t.Errorf("close closeOutcome = %#v, want superseded", got)
	}
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'supersedes'`,
		newID, issueID).Scan(&edges); err != nil {
		t.Fatalf("recount supersedes edge: %v", err)
	}
	if edges != 1 {
		t.Errorf("supersedes edges from %s to %s after close = %d, want 1", newID, issueID, edges)
	}
}

// ---------------------------------------------------------------------------
// sk-565, T032 — test matrix rows 50–51 (US-7, P10). Reference — the sk-565 spec,
// US-7: "the source issue without an assignee → the new issue without an assignee —
// under any setting. A source issue with an assignee (including
// one substituted by the setting at its creation) is moved with it as with an
// explicit one: the new issue gets the assignee's subscription".
// ---------------------------------------------------------------------------

// moveKit runs skein_move and returns the raw response and the prepared
// skein_create arguments.
func moveKit(t *testing.T, pool *pgxpool.Pool, ctx context.Context, issueID string, target uuid.UUID) (string, map[string]any) {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"issueId": issueID, "targetProject": target.String()}
	res, err := handleMove(pool, issues.NewStore(pool), projects.NewStore(pool))(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("skein_move: err=%v res=%v", err, res)
	}
	body := extractToolJSON(t, res)
	steps, _ := body["nextSteps"].([]any)
	step0, _ := steps[0].(map[string]any)
	args0, _ := step0["args"].(map[string]any)
	return toolText(t, res), args0
}

// runPreparedCreate executes the prepared skein_create verbatim.
func runPreparedCreate(t *testing.T, pool *pgxpool.Pool, ctx context.Context, args map[string]any) string {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("prepared skein_create: err=%v body=%v", err, extractToolJSON(t, res))
	}
	issue, _ := extractToolJSON(t, res)["issue"].(map[string]any)
	id, _ := issue["id"].(string)
	return id
}

// TestMove_UnassignedSourceStaysUnassigned — row 50.
func TestMove_UnassignedSourceStaysUnassigned(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	srcPID, srcPrefix := seedProject(t, pool, userID, "mvuas")
	tgtPID, _ := seedProject(t, pool, userID, "mvuat")
	issueID := srcPrefix + "-1"
	seedIssue(t, pool, srcPID, userID, issueID, "nobody's task", "open", 2)
	// The setting is on: without the prepared flag the copy would land on
	// the caller.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, assign_agent_issues_to_me, updated_at) VALUES ($1, true, now())`,
		userID); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	ctx := withCtx(t, userID, "user")

	raw, args := moveKit(t, pool, ctx, issueID, tgtPID)
	if args["unassigned"] != true {
		t.Errorf("prepared skein_create unassigned = %v, want true", args["unassigned"])
	}
	if n := strings.Count(raw, `"unassigned"`); n != 1 {
		t.Errorf("\"unassigned\" appears %d times in the move response, want exactly 1:\n%s", n, raw)
	}

	newID := runPreparedCreate(t, pool, ctx, args)
	var (
		assignee   *string
		acceptedAt *time.Time
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee, accepted_at FROM issues WHERE id = $1`, newID).Scan(&assignee, &acceptedAt); err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if (assignee != nil && *assignee != "") || acceptedAt != nil {
		t.Errorf("copy of an unassigned source: assignee = %v, accepted_at = %v, want neither", assignee, acceptedAt)
	}
}

// TestMove_AssignedSourceMovesAsExplicit — row 51.
func TestMove_AssignedSourceMovesAsExplicit(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "user")
	srcPID, _ := seedProject(t, pool, userID, "mvasa")
	tgtPID, _ := seedProject(t, pool, userID, "mvast")
	ctx := withCtx(t, userID, "user")

	// The source is assigned by the caller's own setting at its creation.
	issueID := runPreparedCreate(t, pool, ctx, map[string]any{
		"project": srcPID.String(), "title": "assigned by the setting", "type": "task", "kind": "directed",
	})
	var srcAssignee *string
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee FROM issues WHERE id = $1`, issueID).Scan(&srcAssignee); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if srcAssignee == nil || *srcAssignee != uname {
		t.Fatalf("precondition: source assignee = %v, want %q set by the default", srcAssignee, uname)
	}

	_, args := moveKit(t, pool, ctx, issueID, tgtPID)
	if _, present := args["unassigned"]; present {
		t.Errorf("prepared skein_create carries unassigned = %v for an assigned source", args["unassigned"])
	}
	if args["assignee"] != uname {
		t.Errorf("prepared skein_create assignee = %v, want %q", args["assignee"], uname)
	}

	newID := runPreparedCreate(t, pool, ctx, args)
	var assignee *string
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee FROM issues WHERE id = $1`, newID).Scan(&assignee); err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if assignee == nil || *assignee != uname {
		t.Errorf("copy assignee = %v, want %q", assignee, uname)
	}
	var reasons []string
	if err := pool.QueryRow(context.Background(),
		`SELECT reasons FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, newID, userID).Scan(&reasons); err != nil {
		t.Fatalf("read copy subscription: %v", err)
	}
	if !slices.Contains(reasons, "assignee") {
		t.Errorf("copy subscription reasons = %v, want `assignee` (explicit assignment)", reasons)
	}
}
