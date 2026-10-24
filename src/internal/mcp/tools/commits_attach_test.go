package tools

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// TestCommitsAttach_OnClosedIssue_PreservesClosedReason is the
// headline scenario from sk-35: the agent closed an issue with a
// detailed reason but forgot commits[]; calling skein_commits_attach
// adds the commit-ref WITHOUT touching closed_reason / closed_at /
// version (sk-35).
func TestCommitsAttach_OnClosedIssue_PreservesClosedReason(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, repoIDs := seedProjectWithRepos(t, pool, userID, "catpr", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "ship feature", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-catpr")

	// Step 1: close with a detailed reason — but forget commits[].
	const originalReason = "Fix applied: token rotation handler in middleware/auth.go; tests cover concurrent refresh"
	closeReq := mcpgo.CallToolRequest{}
	closeReq.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          originalReason,
	}
	closeRes, err := handleClose(pool, store)(ctx, closeReq)
	if err != nil {
		t.Fatalf("close handler error: %v", err)
	}
	if closeRes.IsError {
		t.Fatalf("close unexpected isError=true: %+v", extractToolJSON(t, closeRes))
	}
	closeBody := extractToolJSON(t, closeRes)
	versionAfterClose, _ := closeBody["version"].(float64)
	closedAtAfterClose, _ := closeBody["closedAt"].(string)
	if closedAtAfterClose == "" {
		t.Fatalf("expected closedAt after close; got %v", closeBody["closedAt"])
	}

	// Step 2: attach the forgotten commit-ref via the new tool.
	attachReq := mcpgo.CallToolRequest{}
	attachReq.Params.Arguments = map[string]any{
		"issueId": issueID,
		"commits": []any{
			map[string]any{
				"hash":    "cffd5ab",
				"message": "fix: token rotation race",
			},
		},
	}
	attachRes, err := handleCommitsAttach(pool, store)(ctx, attachReq)
	if err != nil {
		t.Fatalf("attach handler error: %v", err)
	}
	if attachRes.IsError {
		t.Fatalf("attach unexpected isError=true: %+v", extractToolJSON(t, attachRes))
	}
	body := extractToolJSON(t, attachRes)

	// sk-282 T020 / matrix row 95. The ORACLE for "the reason survived"
	// is a READ of the issue, not the body of the write's response —
	// deliberately, and this is the whole point of the row. The
	// acknowledgement does not carry closedReason at all (US-5 forbids
	// echoing text the caller is holding), so an assertion against the
	// response body could only be satisfied by putting the echo back.
	// Reading it back proves the stronger statement INV-3 actually
	// makes: the text is still IN THE SYSTEM and still reachable
	// through a reading tool.
	if _, echoed := body["closedReason"]; echoed {
		t.Fatalf("acknowledgement echoes closedReason = %v — US-5 forbids it",
			body["closedReason"])
	}
	show := callShow(t, handleShow(pool, store, dependencies.NewStore(pool)), ctx,
		map[string]any{"id": issueID, "includeComments": false, "includeDeps": false})
	shown, ok := show["issue"].(map[string]any)
	if !ok {
		t.Fatalf("skein_show issue type %T", show["issue"])
	}
	// closed_reason must survive intact — the core sk-35 invariant.
	if shown["closedReason"] != originalReason {
		t.Fatalf("closedReason changed: got %q, want %q", shown["closedReason"], originalReason)
	}
	// closed_at must not move forward.
	if shown["closedAt"] != closedAtAfterClose {
		t.Fatalf("closedAt changed: got %v, want %v", shown["closedAt"], closedAtAfterClose)
	}
	// version must NOT bump — attach is metadata-add, not a write to issues row.
	// Read off the acknowledgement, which is where INV-7 says the caller
	// gets its next expectedVersion from.
	if got, _ := body["version"].(float64); got != versionAfterClose {
		t.Fatalf("version changed: got %v, want %v (attach must not bump)", got, versionAfterClose)
	}
	// status stays closed — the acknowledgement carries it, and the read
	// agrees.
	if body["status"] != "closed" {
		t.Fatalf("status changed: got %v, want closed", body["status"])
	}
	if shown["status"] != "closed" {
		t.Fatalf("status = %v, want closed", shown["status"])
	}
	// commit-ref present — read back, not echoed.
	commitRefs, _ := shown["commitRefs"].([]any)
	if len(commitRefs) != 1 {
		t.Fatalf("commitRefs len = %d, want 1; issue=%+v", len(commitRefs), shown)
	}
	first, _ := commitRefs[0].(map[string]any)
	if first["hash"] != "cffd5ab" {
		t.Fatalf("commitRefs[0].hash = %v, want cffd5ab", first["hash"])
	}
	if first["repoId"] != repoIDs[0].String() {
		t.Fatalf("commitRefs[0].repoId = %v, want %s", first["repoId"], repoIDs[0])
	}

	// DB-level invariants: closed_reason and version on the row are still the originals.
	var dbReason string
	var dbVersion int
	if err := pool.QueryRow(context.Background(),
		`SELECT closed_reason, version FROM issues WHERE id = $1`, issueID).Scan(&dbReason, &dbVersion); err != nil {
		t.Fatalf("query issue: %v", err)
	}
	if dbReason != originalReason {
		t.Fatalf("DB closed_reason changed: got %q, want %q", dbReason, originalReason)
	}
	if dbVersion != int(versionAfterClose) {
		t.Fatalf("DB version changed: got %d, want %d", dbVersion, int(versionAfterClose))
	}

	// issue.commits_attached event recorded.
	var nEvents int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE entity_id = $1 AND type = 'issue.commits_attached'`,
		issueID).Scan(&nEvents); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if nEvents != 1 {
		t.Fatalf("issue.commits_attached events = %d, want 1", nEvents)
	}
}

// TestCommitsAttach_OnOpenIssue_AddsRefWithoutClosing: attaching to an
// open issue persists the commit-ref without flipping status.
func TestCommitsAttach_OnOpenIssue_AddsRefWithoutClosing(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "caopen", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "wip", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-caopen")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"commits": []any{
			map[string]any{"hash": "abc1234"},
		},
	}
	res, err := handleCommitsAttach(pool, store)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	// The acknowledgement carries the authoritative post-state — status
	// is part of it, so "attach did not flip status" is answerable
	// without a second call.
	if body["status"] != "open" {
		t.Fatalf("status = %v, want open (attach must not flip status)", body["status"])
	}
	// commitRefs is card data, not post-state, so it is read back rather
	// than echoed (sk-282 T020).
	show := callShow(t, handleShow(pool, store, dependencies.NewStore(pool)), ctx,
		map[string]any{"id": issueID, "includeComments": false, "includeDeps": false})
	shown, ok := show["issue"].(map[string]any)
	if !ok {
		t.Fatalf("skein_show issue type %T", show["issue"])
	}
	commitRefs, _ := shown["commitRefs"].([]any)
	if len(commitRefs) != 1 {
		t.Fatalf("commitRefs len = %d, want 1", len(commitRefs))
	}
}

// TestCommitsAttach_ReAttachSameHash_Idempotent: attaching the same
// (hash, repo) pair twice yields one row at the DB level (ON CONFLICT
// DO NOTHING) but each call writes an event so the audit trail
// captures the intent.
func TestCommitsAttach_ReAttachSameHash_Idempotent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "caidem", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "dup", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-caidem")

	mkReq := func() mcpgo.CallToolRequest {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"issueId": issueID,
			"commits": []any{
				map[string]any{"hash": "deadbeef"},
			},
		}
		return req
	}

	for i := range 2 {
		res, err := handleCommitsAttach(pool, store)(ctx, mkReq())
		if err != nil {
			t.Fatalf("attach #%d error: %v", i+1, err)
		}
		if res.IsError {
			t.Fatalf("attach #%d unexpected isError=true: %+v", i+1, extractToolJSON(t, res))
		}
	}

	// DB row count is 1, not 2.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_commits WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("query commits: %v", err)
	}
	if n != 1 {
		t.Fatalf("issue_commits rows = %d, want 1 (ON CONFLICT DO NOTHING)", n)
	}

	// Two events though — one per call.
	var nEvents int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE entity_id = $1 AND type = 'issue.commits_attached'`,
		issueID).Scan(&nEvents); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if nEvents != 2 {
		t.Fatalf("issue.commits_attached events = %d, want 2 (one per call)", nEvents)
	}
}

// TestCommitsAttach_EmptyCommits_ValidationError: commits array must
// be non-empty.
func TestCommitsAttach_EmptyCommits_ValidationError(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "caempty", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "empty", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-caempty")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"commits": []any{},
	}
	res, err := handleCommitsAttach(pool, store)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on empty commits")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}
}

// TestCommitsAttach_LockHeldByOther_Forbidden: when the issue is
// locked by another session and the caller is not admin/force, the
// attach is rejected with locked_by_other (mirrors skein_close).
func TestCommitsAttach_LockHeldByOther_Forbidden(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "calck", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "locked", "open", 2)

	// Lock the issue by a different session.
	_, err := pool.Exec(context.Background(),
		`UPDATE issues SET locked_by = 'sess-other', locked_at = now() WHERE id = $1`, issueID)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "user", "sess-calck")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"commits": []any{
			map[string]any{"hash": "abc1234"},
		},
	}
	res, err := handleCommitsAttach(pool, store)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on locked-by-other")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeLockedByOther {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeLockedByOther)
	}
}

// TestCommitsAttach_AmbiguousRepo_ListsRepos: sk-303.36 covers
// skein_commits_attach through the same classifyCloseError path as
// skein_close — this pins that the repo list actually reaches this
// tool's error body too, not just skein_close's.
func TestCommitsAttach_AmbiguousRepo_ListsRepos(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, repoIDs := seedProjectWithRepos(t, pool, userID, "caamb", 2)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "ambiguous", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-caamb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"commits": []any{
			map[string]any{"hash": "abc1234"},
		},
	}
	res, err := handleCommitsAttach(pool, store)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on ambiguous repo")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeRepoAmbiguous {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeRepoAmbiguous)
	}
	repos, ok := body["repos"].([]any)
	if !ok || len(repos) != 2 {
		t.Fatalf("repos = %v, want a 2-entry list", body["repos"])
	}
	gotIDs := map[string]bool{}
	for _, r := range repos {
		row := r.(map[string]any)
		gotIDs[row["id"].(string)] = true
	}
	for _, rid := range repoIDs {
		if !gotIDs[rid.String()] {
			t.Errorf("repos list missing seeded repo id %s: %v", rid, repos)
		}
	}
}
