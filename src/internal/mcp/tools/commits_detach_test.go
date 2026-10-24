package tools

// commits_detach_test.go — sk-394, the MCP surface of detach.
//
// The store-level guard lives in internal/issues; here is checked what
// the agent sees: the `detached` / `notFound` report at the TOP level of the response.
// Without it the call "succeeds" the same way both when a made-up link is removed and
// when the hash has a typo and nothing is removed — and this very
// indistinguishability is what the issue is about.

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// TestCommitsDetach_ReportsWhatLeftAndWhatWasNeverThere — headline
// sk-394: the agent detaches a made-up hash and SEES from the answer that it
// really was removed, while the typo was not.
//
// The fixture reaches the discriminating case: in one call TWO hashes,
// an attached one and one never attached. On a call with a single hash the answers
// "detached is non-empty" and "notFound is empty" would be indistinguishable for
// an implementation that simply lists the input in one of the fields.
func TestCommitsDetach_ReportsWhatLeftAndWhatWasNeverThere(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, repoIDs := seedProjectWithRepos(t, pool, userID, "cdet", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "ship feature", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-cdet")

	const (
		linked      = "cffd5ab"
		neverLinked = "5cff4e28a1f9c0e1a5f2c8b0d3e4f5a6b7c8d9e0"
	)

	attachReq := mcpgo.CallToolRequest{}
	attachReq.Params.Arguments = map[string]any{
		"issueId": issueID,
		"commits": []any{map[string]any{"hash": linked}},
	}
	attachRes, err := handleCommitsAttach(pool, store)(ctx, attachReq)
	if err != nil || attachRes.IsError {
		t.Fatalf("attach (fixture) failed: err=%v res=%+v", err, attachRes)
	}
	versionAfterAttach, _ := extractToolJSON(t, attachRes)["version"].(float64)

	detachReq := mcpgo.CallToolRequest{}
	detachReq.Params.Arguments = map[string]any{
		"issueId": issueID,
		"hashes":  []any{linked, neverLinked},
	}
	detachRes, err := handleCommitsDetach(pool, store)(ctx, detachReq)
	if err != nil {
		t.Fatalf("detach handler error: %v", err)
	}
	if detachRes.IsError {
		t.Fatalf("detach unexpected isError=true: %+v", extractToolJSON(t, detachRes))
	}
	body := extractToolJSON(t, detachRes)

	detached, _ := body["detached"].([]any)
	if len(detached) != 1 {
		t.Fatalf("detached = %v, expected exactly one removed row", body["detached"])
	}
	row, _ := detached[0].(map[string]any)
	if row["hash"] != linked {
		t.Errorf("detached[0].hash = %v, want %q", row["hash"], linked)
	}
	// the caller did not name repoId — it learns it FROM the answer.
	if row["repoId"] != repoIDs[0].String() {
		t.Errorf("detached[0].repoId = %v, want %s", row["repoId"], repoIDs[0])
	}

	notFound, _ := body["notFound"].([]any)
	if len(notFound) != 1 || notFound[0] != neverLinked {
		t.Fatalf("notFound = %v, expected [%s]", body["notFound"], neverLinked)
	}

	// Detach touches neither the status nor the version — the same contract as attach.
	if got, _ := body["version"].(float64); got != versionAfterAttach {
		t.Errorf("version moved: %v → %v", versionAfterAttach, got)
	}

	// Read the card: the link really is gone.
	show := callShow(t, handleShow(pool, store, dependencies.NewStore(pool)), ctx,
		map[string]any{"id": issueID, "includeComments": false, "includeDeps": false})
	shown, _ := show["issue"].(map[string]any)
	if refs, _ := shown["commitRefs"].([]any); len(refs) != 0 {
		t.Errorf("commitRefs after detach = %v, expected empty", refs)
	}

	var nEvents int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE project_id = $1 AND type = $2`,
		pid, issues.EventIssueCommitsDetached).Scan(&nEvents); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if nEvents != 1 {
		t.Errorf("issue.commits_detached events = %d, expected 1", nEvents)
	}
}

// TestCommitsDetach_EmptyReportKeysArePresent — the CONTROL for the report.
// The check "notFound named the typo" is worth nothing if the keys
// appear only when non-empty: then "everything removed" and "the answer carries
// no report at all" look the same — as the absence of notFound.
func TestCommitsDetach_EmptyReportKeysArePresent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "cdet2", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "ship feature", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-cdet2")

	const linked = "cffd5ab"
	attachReq := mcpgo.CallToolRequest{}
	attachReq.Params.Arguments = map[string]any{
		"issueId": issueID,
		"commits": []any{map[string]any{"hash": linked}},
	}
	if res, err := handleCommitsAttach(pool, store)(ctx, attachReq); err != nil || res.IsError {
		t.Fatalf("attach (fixture) failed: err=%v res=%+v", err, res)
	}

	// Everything named is removed → notFound must be present and empty.
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"issueId": issueID, "hashes": []any{linked}}
	res, err := handleCommitsDetach(pool, store)(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("detach failed: err=%v res=%+v", err, res)
	}
	body := extractToolJSON(t, res)
	raw, present := body["notFound"]
	if !present {
		t.Fatalf("the notFound key is absent on a full detach — \"everything removed\" became indistinguishable from \"no report\"")
	}
	if arr, _ := raw.([]any); len(arr) != 0 {
		t.Errorf("notFound = %v, expected empty", raw)
	}

	// Nothing removed → detached must be present and empty.
	res2, err := handleCommitsDetach(pool, store)(ctx, req)
	if err != nil || res2.IsError {
		t.Fatalf("repeated detach failed: err=%v res=%+v", err, res2)
	}
	body2 := extractToolJSON(t, res2)
	raw2, present2 := body2["detached"]
	if !present2 {
		t.Fatalf("the detached key is absent when there was nothing to remove — exactly the case " +
			"that the caller must notice")
	}
	if arr, _ := raw2.([]any); len(arr) != 0 {
		t.Errorf("detached = %v, expected empty", raw2)
	}
}

// TestCommitsDetach_MalformedHashRefused — garbage is rejected at input and
// is not counted as "not found". The edge case is exactly the one that gave rise to
// the issue: the hash is indistinguishable from a real one in everything except its shape.
func TestCommitsDetach_MalformedHashRefused(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "cdet3", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "ship feature", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-cdet3")

	for _, bad := range []any{
		"CFFD5AB",        // upper case
		"cffd5",          // shorter than seven
		"not-a-hash-xyz", // not hex
		42,               // not a string at all
	} {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"issueId": issueID, "hashes": []any{bad}}
		res, err := handleCommitsDetach(pool, store)(ctx, req)
		if err != nil {
			t.Fatalf("detach(%v) handler error: %v", bad, err)
		}
		body := extractToolJSON(t, res)
		if body["error"] != ErrCodeValidationError {
			t.Errorf("detach(%v): error = %v, expected %q (otherwise garbage would answer notFound "+
				"and the caller would not learn about the typo)", bad, body["error"], ErrCodeValidationError)
		}
	}
}
