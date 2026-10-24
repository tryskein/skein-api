package tools

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// sk-607. The update chain, skein_create with duplicateOf,
// and verbose answers when the card cannot be read back. Same seam and same
// rule as readback_after_commit_test.go: no t.Parallel here.

// rbLockTable holds an ACCESS EXCLUSIVE lock on `table` in a transaction of
// its own until the returned release runs (also run at cleanup). Any read
// of that table — the read-back's hydration among them — waits meanwhile.
func rbLockTable(t *testing.T, pool *pgxpool.Pool, table string) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatalf("begin lock tx: %v", err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		_ = tx.Rollback(ctx)
		conn.Release()
		t.Fatalf("lock %s: %v", table, err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			conn.Release()
		})
	}
	t.Cleanup(release)
	return release
}

func rbLabelCount(t *testing.T, pool *pgxpool.Pool, id, label string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_labels WHERE issue_id = $1 AND label = $2`, id, label).Scan(&n); err != nil {
		t.Fatalf("count labels: %v", err)
	}
	return n
}

// ---- skein_update: the final read-back fails after every step committed ----

func TestUpdateMCP_FinalReadBackFails_AcknowledgedFromCommittedState(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "verbose"}[verbose], func(t *testing.T) {
			env := comSetup(t)
			id := comSeed(t, env, env.pid, env.prefix, map[bool]string{false: "rbu-final", true: "rbu-final-v"}[verbose])
			before := rbRead(t, env, id)
			var snap map[string]rbCommitted
			installAfterCommitHook(t, func(_ context.Context, op, issueID string) {
				if op == "close" && issueID == id {
					snap = rbSnapshotAndDelete(t, env.pool, id, id)
				}
			})

			body := ucoUpdate(t, env, map[string]any{
				"id": id, "expectedVersion": float64(before.Version),
				"status": "closed", "closeOutcome": "done", "reason": "shipped",
				"verbose": verbose,
			})
			if comErr(body) != "" {
				t.Fatalf("update committed but answered %+v", body)
			}
			if snap[id].Status != issues.StatusClosed || snap[id].Version != before.Version+1 {
				t.Fatalf("committed row = %+v, want closed at version %d", snap[id], before.Version+1)
			}
			rbAssertAckMatches(t, body, id, snap[id])
			if body["closeOutcome"] != "done" {
				t.Errorf("closeOutcome = %v, want done", body["closeOutcome"])
			}
			_, marked := body["cardUnavailable"]
			if marked != verbose {
				t.Errorf("cardUnavailable present = %v, want %v (only a verbose call lacks its card)", marked, verbose)
			}
			if verbose && body["cardUnavailable"] != true {
				t.Errorf("cardUnavailable = %v, want true", body["cardUnavailable"])
			}
		})
	}
}

// ---- skein_update: the read-back after an INTERMEDIATE step cannot finish ----
//
// The chain is a field patch then a close. Right after the patch commits,
// a lock on issue_commits (read by the card's hydration, written by neither
// step) makes any read-back of that card wait out its whole budget. The
// chain must still run the close and acknowledge both steps.
func TestUpdateMCP_IntermediateReadBackBlocked_ChainCompletes(t *testing.T) {
	env := comSetup(t)
	id := comSeed(t, env, env.pid, env.prefix, "rbu-mid")
	before := rbRead(t, env, id)

	var release func()
	installAfterCommitHook(t, func(_ context.Context, op, issueID string) {
		if issueID != id {
			return
		}
		switch op {
		case "patch":
			release = rbLockTable(t, env.pool, "issue_commits")
		case "close":
			if release != nil {
				release()
			}
		}
	})

	body := ucoUpdate(t, env, map[string]any{
		"id": id, "expectedVersion": float64(before.Version),
		"title":  "renamed before close",
		"status": "closed", "closeOutcome": "done", "reason": "shipped",
	})
	if comErr(body) != "" {
		t.Fatalf("both steps were requested; answered %+v", body)
	}
	after := rbRead(t, env, id)
	// Spec: two writes — the patch and the close — each bump the version.
	if after.Status != issues.StatusClosed || after.Outcome != "done" || after.Version != before.Version+2 {
		t.Fatalf("db = %+v, want closed/done at version %d", after, before.Version+2)
	}
	if title := corTitle(t, env, id); title != "renamed before close" {
		t.Errorf("title = %q, want the patch applied", title)
	}
	if got := jsonInt(t, body, "version"); got != after.Version {
		t.Errorf("acknowledged version = %d, want the committed %d", got, after.Version)
	}
}

// ---- skein_update_many ----

// The row's final version read fails after the chain committed.
func TestUpdateManyMCP_FinalReadBackFails_RowAcknowledged(t *testing.T) {
	env := comSetup(t)
	id := comSeed(t, env, env.pid, env.prefix, "rbum-final")
	before := rbRead(t, env, id)
	var snap map[string]rbCommitted
	installAfterCommitHook(t, func(_ context.Context, op, issueID string) {
		if op == "close" && issueID == id {
			snap = rbSnapshotAndDelete(t, env.pool, id, id)
		}
	})

	body := ucoUpdateMany(t, env, []any{
		map[string]any{"id": id, "expectedVersion": float64(before.Version),
			"status": "closed", "closeOutcome": "done", "reason": "batch"},
	})
	row := comRow(t, body, 0)
	if row["ok"] != true {
		t.Fatalf("row committed but answered %+v", row)
	}
	if got := jsonInt(t, row, "version"); got != snap[id].Version {
		t.Errorf("row version = %d, want the committed %d", got, snap[id].Version)
	}
}

// The batch's own deadline fires right after a row's close committed — the
// skein_update_many twin of the close_many incident.
func TestUpdateManyMCP_LoopDeadlineAfterCloseCommit_RowAcknowledged(t *testing.T) {
	env := comSetup(t)
	id := comSeed(t, env, env.pid, env.prefix, "rbum-deadline")
	before := rbRead(t, env, id)

	withShortBatchDeadline(t, 2*time.Second)
	installAfterCommitHook(t, func(ctx context.Context, op, issueID string) {
		if op == "close" && issueID == id {
			<-ctx.Done() // the chain runs under the batch's loop context
		}
	})

	body := ucoUpdateMany(t, env, []any{
		map[string]any{"id": id, "expectedVersion": float64(before.Version),
			"status": "closed", "closeOutcome": "wont_do", "reason": "batch"},
	})
	after := rbRead(t, env, id)
	if after.Status != issues.StatusClosed || after.Version != before.Version+1 {
		t.Fatalf("db = %+v, want closed at version %d", after, before.Version+1)
	}
	row := comRow(t, body, 0)
	if row["ok"] != true {
		t.Fatalf("row committed but answered %+v", row)
	}
	if got := jsonInt(t, row, "version"); got != after.Version {
		t.Errorf("row version = %d, want the committed %d", got, after.Version)
	}
}

// A step that did NOT apply stays an error: the close commits, the deadline
// then fires, and the label step after it never writes. Acknowledging that
// row as ok would report a write that is not in the database.
func TestUpdateManyMCP_StepAfterDeadlineNotApplied_RowIsNotOK(t *testing.T) {
	env := comSetup(t)
	id := comSeed(t, env, env.pid, env.prefix, "rbum-partial")
	before := rbRead(t, env, id)

	withShortBatchDeadline(t, 2*time.Second)
	installAfterCommitHook(t, func(ctx context.Context, op, issueID string) {
		if op == "close" && issueID == id {
			<-ctx.Done()
		}
	})

	body := ucoUpdateMany(t, env, []any{
		map[string]any{"id": id, "expectedVersion": float64(before.Version),
			"status": "closed", "closeOutcome": "done", "reason": "batch",
			"labelsAdd": []any{"after-deadline"}},
	})
	if n := rbLabelCount(t, env.pool, id, "after-deadline"); n != 0 {
		t.Fatalf("label written %d times; the step was meant to be cut by the deadline", n)
	}
	if row := comRow(t, body, 0); row["ok"] == true {
		t.Errorf("row reported ok although its label step did not apply: %+v", row)
	}
}

// ---- skein_create with duplicateOf ----

func TestCreateMCP_DuplicateOf_CallerContextEndsAfterCommit(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "rbdup")
	target := seedIssue(t, pool, pid, userID, prefix+"-orig", "the original", "open", 2)
	handler := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))

	ctx, cancel := context.WithCancel(withCtx(t, userID, "admin"))
	defer cancel()
	installAfterCommitHook(t, func(_ context.Context, op, _ string) {
		if op == "create" {
			cancel()
		}
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":     pid.String(),
		"title":       "the duplicate",
		"type":        "task",
		"kind":        "directed",
		"duplicateOf": target,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("create committed but the tool answered with an error: %+v", body)
	}
	issue, _ := body["issue"].(map[string]any)
	newID, _ := issue["id"].(string)

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1 AND title = 'the duplicate'`, pid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("issues titled 'the duplicate' = %d, want 1", n)
	}
	var edges int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'duplicates'`,
		newID, target).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edges != 1 {
		t.Errorf("duplicates edges %s → %s = %d, want 1", newID, target, edges)
	}
}

// ---- verbose create / epic create when the card cannot be read back ----

func TestCreateMCP_ReadBackFails_VerboseAcknowledgesWithCardUnavailable(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "rbfcv")
	handler := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))

	var snap map[string]rbCommitted
	installAfterCommitHook(t, func(_ context.Context, op, id string) {
		if op == "create" {
			snap = rbSnapshotAndDelete(t, pool, id, id)
		}
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(), "title": "verbose read-back failure",
		"type": "task", "kind": "directed", "verbose": true,
	}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("issue committed but the tool answered with an error: %+v", body)
	}
	if body["cardUnavailable"] != true {
		t.Errorf("cardUnavailable = %v, want true", body["cardUnavailable"])
	}
	issue, _ := body["issue"].(map[string]any)
	id, _ := issue["id"].(string)
	if _, ok := snap[id]; !ok {
		t.Fatalf("hook snapshot %+v has no row for the acknowledged id %q", snap, id)
	}
	rbAssertAckMatches(t, issue, id, snap[id])
	if _, hasTitle := issue["title"]; hasTitle {
		t.Errorf("issue carries card fields it could not have loaded: %+v", issue)
	}
}

func TestEpicCreateMCP_ReadBackFails_VerboseAcknowledgesWithCardUnavailable(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "rbfev")
	handler := handleEpicCreate(pool, issues.NewStore(pool), projects.NewStore(pool))

	var snap map[string]rbCommitted
	installAfterCommitHook(t, func(_ context.Context, op, epicID string) {
		if op == "epic_create" {
			snap = rbSnapshotAndDelete(t, pool, epicID+".1", epicID, epicID+".1")
		}
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(), "title": "verbose epic read-back failure",
		"kind": "directed", "verbose": true,
		"childTemplates": []any{map[string]any{"title": "only child"}},
	}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("epic committed but the tool answered with an error: %+v", body)
	}
	if body["cardUnavailable"] != true {
		t.Errorf("cardUnavailable = %v, want true", body["cardUnavailable"])
	}
	epic, _ := body["epic"].(map[string]any)
	epicID, _ := epic["id"].(string)
	rbAssertAckMatches(t, epic, epicID, snap[epicID])
	children, _ := body["children"].([]any)
	if len(children) != 1 {
		t.Fatalf("children = %+v, want 1 row", body["children"])
	}
	rbAssertAckMatches(t, children[0].(map[string]any), epicID+".1", snap[epicID+".1"])
}
