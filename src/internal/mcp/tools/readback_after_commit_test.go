package tools

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// sk-607. A write whose transaction has COMMITTED is a write that happened.
// What these tests pin is the answer the caller gets when its context ends
// in the window between that commit and the read-back of the post-state:
// the write is reported as applied, with the version the database holds —
// not as internal_error, which tells a retrying agent "nothing happened"
// and produced duplicate epics and overwritten close reasons in production.
//
// The window is opened deterministically with issues.AfterCommitHook, the
// same technique afterClosePrecheckHook uses for the update chain. None of
// these tests may run in parallel: the hook is a package-level variable.

// installAfterCommitHook sets the store's post-commit seam for one test.
func installAfterCommitHook(t *testing.T, hook func(ctx context.Context, op, issueID string)) {
	t.Helper()
	issues.AfterCommitHook = hook
	t.Cleanup(func() { issues.AfterCommitHook = nil })
}

// rbIssueState is what the database holds for one issue after the call.
type rbIssueState struct {
	Version int
	Status  string
	Outcome string
	Reason  string
}

func rbRead(t *testing.T, env *comEnv, id string) rbIssueState {
	t.Helper()
	var s rbIssueState
	var outcome, reason *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT version, status, close_outcome, closed_reason FROM issues WHERE id = $1`, id).
		Scan(&s.Version, &s.Status, &outcome, &reason); err != nil {
		t.Fatalf("read issue %s: %v", id, err)
	}
	if outcome != nil {
		s.Outcome = *outcome
	}
	if reason != nil {
		s.Reason = *reason
	}
	return s
}

// jsonInt reads a JSON number off a decoded body.
func jsonInt(t *testing.T, obj map[string]any, key string) int {
	t.Helper()
	v, ok := obj[key].(float64)
	if !ok {
		t.Fatalf("%s missing or not a number in %+v", key, obj)
	}
	return int(v)
}

// TestCloseMCP_CallerContextEndsAfterCommit_ServesTheCard: the single
// close. The caller's context is cancelled right after the close commits,
// and the call asks for the full card (verbose). Only a read-back detached
// from that context can still load the card: a read under the cancelled
// context fails, and the answer degrades to the acknowledgement marked
// cardUnavailable. So this test pins the detach, not only the fallback.
func TestCloseMCP_CallerContextEndsAfterCommit_ServesTheCard(t *testing.T) {
	env := comSetup(t)
	id := comSeed(t, env, env.pid, env.prefix, "rb-close")
	before := rbRead(t, env, id)

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	installAfterCommitHook(t, func(_ context.Context, op, issueID string) {
		if op == "close" && issueID == id {
			cancel()
		}
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id": id, "expectedVersion": float64(before.Version),
		"reason": "shipped", "closeOutcome": "done", "verbose": true,
	}
	res, err := handleClose(env.pool, env.store)(ctx, req)
	if err != nil {
		t.Fatalf("handleClose: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("close committed but the tool answered with an error: %+v", body)
	}

	after := rbRead(t, env, id)
	// Spec: a close is one write — status closed, outcome as asked, version +1.
	if after.Status != issues.StatusClosed || after.Outcome != "done" || after.Reason != "shipped" {
		t.Fatalf("db after close = %+v, want closed/done/shipped", after)
	}
	if after.Version != before.Version+1 {
		t.Fatalf("db version = %d, want %d", after.Version, before.Version+1)
	}
	rbAssertCardMatchesDB(t, env.pool, body, id)
	if body["closeOutcome"] != "done" || body["closedReason"] != "shipped" {
		t.Errorf("card closeOutcome/closedReason = %v/%v, want done/shipped",
			body["closeOutcome"], body["closedReason"])
	}
}

// TestCloseManyMCP_LoopDeadlineAfterCommit_RowReportsTheClose reproduces
// the production incident shape: the batch's own deadline fires after a
// row's close committed. That row must answer ok with its version and
// outcome; the row that never ran keeps batch_deadline_exceeded.
func TestCloseManyMCP_LoopDeadlineAfterCommit_RowReportsTheClose(t *testing.T) {
	env := comSetup(t)
	idA := comSeed(t, env, env.pid, env.prefix, "rb-many-a")
	idB := comSeed(t, env, env.pid, env.prefix, "rb-many-b")
	beforeA, beforeB := rbRead(t, env, idA), rbRead(t, env, idB)

	// Long enough for row A to reach its commit, short enough to wait out.
	withShortBatchDeadline(t, 2*time.Second)
	installAfterCommitHook(t, func(ctx context.Context, op, issueID string) {
		if op == "close" && issueID == idA {
			<-ctx.Done() // the store runs under the batch's loop context
		}
	})

	body := comCloseMany(t, env, []any{
		map[string]any{"id": idA, "expectedVersion": float64(beforeA.Version),
			"reason": "batch", "closeOutcome": "wont_do"},
		map[string]any{"id": idB, "expectedVersion": float64(beforeB.Version),
			"reason": "batch", "closeOutcome": "wont_do"},
	})

	afterA := rbRead(t, env, idA)
	if afterA.Status != issues.StatusClosed || afterA.Outcome != "wont_do" || afterA.Version != beforeA.Version+1 {
		t.Fatalf("row A in db = %+v, want closed/wont_do at version %d", afterA, beforeA.Version+1)
	}
	rowA := comRow(t, body, 0)
	if rowA["ok"] != true {
		t.Fatalf("row A committed but answered %+v", rowA)
	}
	if got := jsonInt(t, rowA, "version"); got != afterA.Version {
		t.Errorf("row A version = %d, want the committed %d", got, afterA.Version)
	}
	if rowA["closeOutcome"] != "wont_do" {
		t.Errorf("row A closeOutcome = %v, want wont_do", rowA["closeOutcome"])
	}

	rowB := comRow(t, body, 1)
	if code := comRowErr(t, rowB); code != ErrCodeBatchDeadlineExceeded {
		t.Errorf("row B code = %q, want %q", code, ErrCodeBatchDeadlineExceeded)
	}
	if afterB := rbRead(t, env, idB); afterB != beforeB {
		t.Errorf("row B never ran but changed: before %+v, after %+v", beforeB, afterB)
	}
	summary, _ := body["summary"].(map[string]any)
	if summary["succeeded"] != float64(1) || summary["failed"] != float64(1) {
		t.Errorf("summary = %+v, want 1 succeeded / 1 failed", summary)
	}
}

// TestEpicCreateMCP_CallerContextEndsAfterCommit_ServesTheCards: the
// second production incident shape — every epic was written and every call
// answered internal_error, so the agent created them all again. verbose, so
// the cards themselves prove the read-back ran detached (see the close test).
func TestEpicCreateMCP_CallerContextEndsAfterCommit_ServesTheCards(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "rbe")
	handler := handleEpicCreate(pool, issues.NewStore(pool), projects.NewStore(pool))

	ctx, cancel := context.WithCancel(withCtx(t, userID, "admin"))
	defer cancel()
	installAfterCommitHook(t, func(_ context.Context, op, _ string) {
		if op == "epic_create" {
			cancel()
		}
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "read-back probe epic",
		"kind":    "directed",
		"verbose": true,
		"childTemplates": []any{
			map[string]any{"title": "child one"},
			map[string]any{"title": "child two"},
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("epic committed but the tool answered with an error: %+v", body)
	}
	if _, marked := body["cardUnavailable"]; marked {
		t.Fatalf("cards were not read back (cardUnavailable set): %+v", body)
	}

	epic, ok := body["epic"].(map[string]any)
	if !ok {
		t.Fatalf("epic missing: %+v", body)
	}
	epicID, _ := epic["id"].(string)
	rbAssertCardMatchesDB(t, pool, epic, epicID)
	if epic["title"] != "read-back probe epic" {
		t.Errorf("epic title = %v, want the requested title", epic["title"])
	}

	children, ok := body["children"].([]any)
	if !ok || len(children) != 2 {
		t.Fatalf("children = %+v, want 2 rows", body["children"])
	}
	for i, c := range children {
		card := c.(map[string]any)
		want := fmt.Sprintf("%s.%d", epicID, i+1)
		rbAssertCardMatchesDB(t, pool, card, want)
		if wantTitle := []string{"child one", "child two"}[i]; card["title"] != wantTitle {
			t.Errorf("children[%d].title = %v, want %q", i, card["title"], wantTitle)
		}
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1`, pid).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if n != 3 {
		t.Errorf("issues in project = %d, want 3 (epic + 2 children, written once)", n)
	}
}

// TestCreateMCP_CallerContextEndsAfterCommit_ServesTheCard: the single
// create, same window, verbose for the same reason as the close test.
func TestCreateMCP_CallerContextEndsAfterCommit_ServesTheCard(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "rbc")
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
		"project": pid.String(),
		"title":   "read-back probe issue",
		"type":    "task",
		"kind":    "directed",
		"verbose": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("issue committed but the tool answered with an error: %+v", body)
	}
	if body["created"] != true {
		t.Fatalf("created = %v, want true", body["created"])
	}
	if _, marked := body["cardUnavailable"]; marked {
		t.Fatalf("card was not read back (cardUnavailable set): %+v", body)
	}
	card, ok := body["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue missing: %+v", body)
	}
	id, _ := card["id"].(string)
	rbAssertCardMatchesDB(t, pool, card, id)
	if card["title"] != "read-back probe issue" {
		t.Errorf("title = %v, want the requested title", card["title"])
	}
}

// rbAssertCardMatchesDB checks a full card (verbose) against the row the
// database holds: same id, version and status, and the card fields an
// acknowledgement never carries (title, description) are present.
func rbAssertCardMatchesDB(t *testing.T, pool *pgxpool.Pool, card map[string]any, id string) {
	t.Helper()
	if id == "" || card["id"] != id {
		t.Fatalf("card id = %v, want %q", card["id"], id)
	}
	var version int
	var status, title string
	if err := pool.QueryRow(context.Background(),
		`SELECT version, status, title FROM issues WHERE id = $1`, id).Scan(&version, &status, &title); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	if got := jsonInt(t, card, "version"); got != version {
		t.Errorf("%s: card version = %d, want the committed %d", id, got, version)
	}
	if card["status"] != status {
		t.Errorf("%s: card status = %v, want %q", id, card["status"], status)
	}
	if card["title"] != title {
		t.Errorf("%s: card title = %v, want %q — not a full card", id, card["title"], title)
	}
	if _, ok := card["description"]; !ok {
		t.Errorf("%s: card has no description key — not a full card: %+v", id, card)
	}
}

// TestClassifyCloseError_ContextEndedBeforeCommit: a close whose context
// ended while the write was in flight is not a server failure. It must not
// be filed under internal_error "close failed" — the code the caller
// retries blindly on — but under the code that names why it stopped.
func TestClassifyCloseError_ContextEndedBeforeCommit(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExp := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExp()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{"caller cancelled", cancelled, fmt.Errorf("closing issue: %w", context.Canceled)},
		{"caller deadline", expired, fmt.Errorf("loading issue in tx: %w", context.DeadlineExceeded)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCloseError(tc.ctx, nil, uuid.Nil, tc.err, "skein_close", "u", "x-1", "")
			if got.Code != ErrCodeRequestCancelled {
				t.Errorf("classifyCloseError(%v) code = %q, want %q", tc.err, got.Code, ErrCodeRequestCancelled)
			}
		})
	}

	// A context error while the caller's context is still alive is not a
	// stop of ours — it stays a failure.
	alive := context.Background()
	got := classifyCloseError(alive, nil, uuid.Nil, errors.Join(errors.New("x"), context.DeadlineExceeded),
		"skein_close", "u", "x-1", "")
	if got.Code != ErrCodeInternalError {
		t.Errorf("context error with a live caller: code = %q, want %q", got.Code, ErrCodeInternalError)
	}
}

// TestMapCreateErrors_ContextEndedBeforeCommit: the same rule for the two
// create surfaces. Their transaction is a single unit, so a context that
// ended inside it is a request that stopped, not a create that broke.
func TestMapCreateErrors_ContextEndedBeforeCommit(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err := fmt.Errorf("inserting issue: %w", context.Canceled)

	t.Run("skein_create", func(t *testing.T) {
		res := mapCreateError(cancelled, err, uuid.Nil, uuid.Nil)
		if got := rbToolErrorCode(t, res); got != ErrCodeRequestCancelled {
			t.Errorf("mapCreateError(%v) = %q, want %q", err, got, ErrCodeRequestCancelled)
		}
	})
	t.Run("skein_epic_create, epic itself", func(t *testing.T) {
		res := mapEpicCreateError(cancelled, err, uuid.Nil, uuid.Nil)
		if got := rbToolErrorCode(t, res); got != ErrCodeRequestCancelled {
			t.Errorf("mapEpicCreateError(%v) = %q, want %q", err, got, ErrCodeRequestCancelled)
		}
	})
	t.Run("skein_epic_create, inside a child insert", func(t *testing.T) {
		childErr := &issues.ErrEpicChildFailed{Index: 1, Err: err}
		res := mapEpicCreateError(cancelled, childErr, uuid.Nil, uuid.Nil)
		if got := rbToolErrorCode(t, res); got != ErrCodeRequestCancelled {
			t.Errorf("mapEpicCreateError(%v) = %q, want %q", childErr, got, ErrCodeRequestCancelled)
		}
	})
}

// rbToolErrorCode reads the `error` code off a tool error envelope and
// fails the test when the result is not an error at all.
func rbToolErrorCode(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatalf("want a tool error, got %+v", res)
	}
	return toolErrorCode(res)
}

// ---- the read-back itself fails: acknowledged from the committed state ----
//
// The tests above end the CALLER'S context; the read-back runs detached
// from it and succeeds. These make the read-back fail too, by deleting the
// row in the post-commit window (a leaf row — issues.parent_id is ON
// DELETE RESTRICT). The oracle is the row as it stood right after the
// commit, read by the hook before the delete: the acknowledgement must
// carry exactly that version and status, built from what the committing
// transaction knew.

// rbCommitted is one row as the hook saw it right after the commit.
type rbCommitted struct {
	Version  int
	Status   string
	Assignee *string
	Closed   bool // closed_at is set
}

// rbSnapshotAndDelete records each id's committed version, status, assignee
// and close stamp, then deletes `victim` so the store's read-back of it fails.
func rbSnapshotAndDelete(t *testing.T, pool *pgxpool.Pool, victim string, ids ...string) map[string]rbCommitted {
	t.Helper()
	snap := make(map[string]rbCommitted, len(ids))
	for _, id := range ids {
		var c rbCommitted
		if err := pool.QueryRow(context.Background(),
			`SELECT version, status, assignee, closed_at IS NOT NULL FROM issues WHERE id = $1`, id).
			Scan(&c.Version, &c.Status, &c.Assignee, &c.Closed); err != nil {
			t.Errorf("snapshot %s: %v", id, err)
			continue
		}
		snap[id] = c
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, victim); err != nil {
		t.Errorf("delete %s: %v", victim, err)
	}
	return snap
}

func rbAssertAckMatches(t *testing.T, row map[string]any, id string, want rbCommitted) {
	t.Helper()
	if row["id"] != id {
		t.Errorf("acknowledged id = %v, want %s", row["id"], id)
	}
	if row["ok"] != true {
		t.Errorf("%s: ok = %v, want true", id, row["ok"])
	}
	if got := jsonInt(t, row, "version"); got != want.Version {
		t.Errorf("%s: acknowledged version = %d, want the committed %d", id, got, want.Version)
	}
	if row["status"] != want.Status {
		t.Errorf("%s: acknowledged status = %v, want the committed %q", id, row["status"], want.Status)
	}
	// The fields the close warning and the claim discipline read: losing
	// one would turn a committed acknowledgement into a false nudge.
	gotAssignee, _ := row["assignee"].(string)
	wantAssignee := ""
	if want.Assignee != nil {
		wantAssignee = *want.Assignee
	}
	if gotAssignee != wantAssignee {
		t.Errorf("%s: acknowledged assignee = %q, want the committed %q", id, gotAssignee, wantAssignee)
	}
	if _, has := row["closedAt"]; has != want.Closed {
		t.Errorf("%s: acknowledgement closedAt present = %v, want %v", id, has, want.Closed)
	}
}

func TestCloseMCP_ReadBackFails_AcknowledgedFromCommittedState(t *testing.T) {
	env := comSetup(t)
	id := comSeed(t, env, env.pid, env.prefix, "rbf-close")
	before := rbRead(t, env, id)

	var snap map[string]rbCommitted
	installAfterCommitHook(t, func(_ context.Context, op, issueID string) {
		if op == "close" && issueID == id {
			snap = rbSnapshotAndDelete(t, env.pool, id, id)
		}
	})

	body := comClose(t, env, map[string]any{
		"id": id, "expectedVersion": float64(before.Version),
		"reason": "shipped", "closeOutcome": "done",
	})
	if comErr(body) != "" {
		t.Fatalf("close committed but answered %+v", body)
	}
	if snap[id].Status != issues.StatusClosed || snap[id].Version != before.Version+1 {
		t.Fatalf("committed row = %+v, want closed at version %d", snap[id], before.Version+1)
	}
	rbAssertAckMatches(t, body, id, snap[id])
	if body["closeOutcome"] != "done" {
		t.Errorf("acknowledged closeOutcome = %v, want done", body["closeOutcome"])
	}
}

// Under verbose the caller asked for the full card and there is none: the
// write is still reported as applied — the acknowledgement plus
// cardUnavailable — and no hollow card is passed off as a real one.
func TestCloseMCP_ReadBackFails_VerboseAcknowledgesWithCardUnavailable(t *testing.T) {
	env := comSetup(t)
	id := comSeed(t, env, env.pid, env.prefix, "rbf-verbose")
	before := rbRead(t, env, id)
	var snap map[string]rbCommitted
	installAfterCommitHook(t, func(_ context.Context, op, issueID string) {
		if op == "close" && issueID == id {
			snap = rbSnapshotAndDelete(t, env.pool, id, id)
		}
	})

	body := comClose(t, env, map[string]any{
		"id": id, "expectedVersion": float64(before.Version),
		"reason": "shipped", "closeOutcome": "done", "verbose": true,
	})
	if comErr(body) != "" {
		t.Fatalf("close committed but answered %+v", body)
	}
	rbAssertAckMatches(t, body, id, snap[id])
	if body["cardUnavailable"] != true {
		t.Errorf("cardUnavailable = %v, want true", body["cardUnavailable"])
	}
	if body["closeOutcome"] != "done" {
		t.Errorf("closeOutcome = %v, want done", body["closeOutcome"])
	}
	if _, hasTitle := body["title"]; hasTitle {
		t.Errorf("answer carries card fields it could not have loaded: %+v", body)
	}
}

func TestEpicCreateMCP_ReadBackFails_AcknowledgedFromCommittedState(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "rbfe")
	handler := handleEpicCreate(pool, issues.NewStore(pool), projects.NewStore(pool))

	var snap map[string]rbCommitted
	installAfterCommitHook(t, func(_ context.Context, op, epicID string) {
		if op == "epic_create" {
			snap = rbSnapshotAndDelete(t, pool, epicID+".2", epicID, epicID+".1", epicID+".2")
		}
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "read-back failure probe epic",
		"kind":    "directed",
		"childTemplates": []any{
			map[string]any{"title": "child one"},
			map[string]any{"title": "child two"},
		},
	}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("epic committed but the tool answered with an error: %+v", body)
	}
	epic, _ := body["epic"].(map[string]any)
	epicID, _ := epic["id"].(string)
	if len(snap) != 3 {
		t.Fatalf("hook snapshot = %+v, want epic and two children", snap)
	}
	assertAcknowledgementShape(t, epic)
	rbAssertAckMatches(t, epic, epicID, snap[epicID])
	children, _ := body["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("children = %+v, want 2 rows", body["children"])
	}
	for i, c := range children {
		row := c.(map[string]any)
		assertAcknowledgementShape(t, row)
		want := fmt.Sprintf("%s.%d", epicID, i+1)
		rbAssertAckMatches(t, row, want, snap[want])
	}
}

func TestCreateMCP_ReadBackFails_AcknowledgedFromCommittedState(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "rbfc")
	handler := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))

	var snap map[string]rbCommitted
	installAfterCommitHook(t, func(_ context.Context, op, id string) {
		if op == "create" {
			snap = rbSnapshotAndDelete(t, pool, id, id)
		}
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "read-back failure probe issue",
		"type":    "task",
		"kind":    "directed",
	}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("issue committed but the tool answered with an error: %+v", body)
	}
	issue, _ := body["issue"].(map[string]any)
	id, _ := issue["id"].(string)
	if _, ok := snap[id]; !ok {
		t.Fatalf("hook snapshot %+v has no row for the acknowledged id %q", snap, id)
	}
	assertAcknowledgementShape(t, issue)
	rbAssertAckMatches(t, issue, id, snap[id])
}

// TestClassifyCloseRowError_LoopDeadline: a skein_close_many row cut by the
// batch's own deadline while its close was in flight answers the code the
// caller already resends on, not internal_error.
func TestClassifyCloseRowError_LoopDeadline(t *testing.T) {
	parent := context.Background()
	loop, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
	defer cancel()
	err := fmt.Errorf("loading components: %w", context.DeadlineExceeded)

	got := classifyCloseRowError(parent, loop, nil, uuid.Nil, err, "u", "x-1", "")
	if got.Code != ErrCodeBatchDeadlineExceeded {
		t.Errorf("classifyCloseRowError(%v) code = %q, want %q", err, got.Code, ErrCodeBatchDeadlineExceeded)
	}
}
