package tools

// close_outcome_required_mcp_test.go — sk-540.1: closeOutcome is mandatory
// on a TRANSITION into closed on all four MCP close paths
//
//	skein_close        → Store.CloseWithResult
//	skein_close_many   → Store.CloseWithResult, per row
//	skein_update       → pre-check, PatchIssueWithResult, CloseWithResult
//	skein_update_many  → same chain, per row
//
// while a re-close without it keeps the previous outcome. Expectations come
// from the task's acceptance criteria, not from the current output.

import (
	"context"
	"strings"
	"testing"
)

const codeCloseOutcomeRequired = "close_outcome_required"

// corDetailCode reads details.code of a single-tool error body (ToolError
// merges the details map into the top level of the envelope).
func corDetailCode(body map[string]any) string {
	code, _ := body["code"].(string)
	return code
}

func corTitle(t *testing.T, env *comEnv, id string) string {
	t.Helper()
	var title string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT title FROM issues WHERE id = $1`, id).Scan(&title); err != nil {
		t.Fatalf("read title %s: %v", id, err)
	}
	return title
}

// corCloseWith closes id through skein_close with an explicit outcome —
// the arrangement of an "already closed with a non-default outcome" issue.
func corCloseWith(t *testing.T, env *comEnv, id, outcome string) {
	t.Helper()
	args := map[string]any{
		"id": id, "expectedVersion": comRead(t, env, id).Version,
		"reason": "arrange", "closeOutcome": outcome,
	}
	// sk-540.2: duplicate / superseded name a target on a transition.
	if outcome == "duplicate" || outcome == "superseded" {
		args["closeTarget"] = comSeed(t, env, env.pid, env.prefix,
			"target-"+strings.TrimPrefix(id, env.prefix+"-"))
	}
	body := comClose(t, env, args)
	if body["ok"] != true {
		t.Fatalf("arrange: closing %s with %s failed: %+v", id, outcome, body)
	}
}

func corSetStatus(t *testing.T, env *comEnv, id, status string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE issues SET status = $2 WHERE id = $1`, id, status); err != nil {
		t.Fatalf("set status %s=%s: %v", id, status, err)
	}
}

func TestCloseMCP_OutcomeRequiredOnTransition(t *testing.T) {
	env := comSetup(t)

	for _, from := range []string{"open", "in_progress", "standing"} {
		t.Run("transition from "+from+" without outcome is refused, nothing written", func(t *testing.T) {
			id := comSeed(t, env, env.pid, env.prefix, "req-"+from)
			if from != "open" {
				corSetStatus(t, env, id, from)
			}
			before := comRead(t, env, id)
			body := comClose(t, env, map[string]any{
				"id": id, "expectedVersion": before.Version, "reason": "r",
			})
			if comErr(body) != ErrCodeValidationError || corDetailCode(body) != codeCloseOutcomeRequired {
				t.Fatalf("skein_close without outcome from %s = %+v, want validation_error/%s",
					from, body, codeCloseOutcomeRequired)
			}
			if after := comRead(t, env, id); after != before {
				t.Errorf("refused close changed the issue: before %+v, after %+v", before, after)
			}
		})
	}

	t.Run("force=true does not lift the requirement", func(t *testing.T) {
		// The forcing admin edits by his own role in env.pid (US-6, T066).
		comAdminWritesInPid(t, env)
		id := comSeed(t, env, env.pid, env.prefix, "req-force")
		before := comRead(t, env, id)
		body := comCloseAs(t, env, env.adminCtx, map[string]any{
			"id": id, "expectedVersion": before.Version, "reason": "r", "force": true,
		})
		if comErr(body) != ErrCodeValidationError || corDetailCode(body) != codeCloseOutcomeRequired {
			t.Fatalf("forced skein_close without outcome = %+v, want validation_error/%s",
				body, codeCloseOutcomeRequired)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("refused close changed the issue: before %+v, after %+v", before, after)
		}
	})

	t.Run("re-close without outcome keeps the previous non-default outcome", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "req-reclose")
		corCloseWith(t, env, id, "wont_do")
		body := comClose(t, env, map[string]any{
			"id": id, "expectedVersion": comRead(t, env, id).Version, "reason": "attach more",
		})
		if body["ok"] != true {
			t.Fatalf("re-close without outcome = %+v, want ok", body)
		}
		if got := comRead(t, env, id); got.Status != "closed" || got.Outcome != "wont_do" {
			t.Errorf("after re-close: %+v, want closed/wont_do", got)
		}
	})
}

func TestCloseManyMCP_OutcomeRequiredFailsOnlyThatRow(t *testing.T) {
	env := comSetup(t)

	missing := comSeed(t, env, env.pid, env.prefix, "reqm-missing")
	explicit := comSeed(t, env, env.pid, env.prefix, "reqm-explicit")
	reclosed := comSeed(t, env, env.pid, env.prefix, "reqm-reclose")
	corCloseWith(t, env, reclosed, "duplicate")
	missingBefore := comRead(t, env, missing)

	body := comCloseMany(t, env, []any{
		map[string]any{"id": missing, "expectedVersion": missingBefore.Version, "reason": "r"},
		map[string]any{"id": explicit, "expectedVersion": comRead(t, env, explicit).Version,
			"reason": "r", "closeOutcome": "done"},
		map[string]any{"id": reclosed, "expectedVersion": comRead(t, env, reclosed).Version,
			"reason": "r"},
	})

	row0 := comRow(t, body, 0)
	if row0["ok"] != false || comRowErr(t, row0) != ErrCodeValidationError ||
		comRowErrDetails(t, row0)["code"] != codeCloseOutcomeRequired {
		t.Errorf("row without outcome = %+v, want failed validation_error/%s", row0, codeCloseOutcomeRequired)
	}
	if after := comRead(t, env, missing); after != missingBefore {
		t.Errorf("refused row changed its issue: before %+v, after %+v", missingBefore, after)
	}
	if row1 := comRow(t, body, 1); row1["ok"] != true {
		t.Errorf("row with explicit outcome = %+v, want ok", row1)
	}
	if got := comRead(t, env, explicit); got.Status != "closed" || got.Outcome != "done" {
		t.Errorf("explicit row: %+v, want closed/done", got)
	}
	if row2 := comRow(t, body, 2); row2["ok"] != true {
		t.Errorf("re-close row without outcome = %+v, want ok", row2)
	}
	if got := comRead(t, env, reclosed); got.Status != "closed" || got.Outcome != "duplicate" {
		t.Errorf("re-closed row: %+v, want closed/duplicate kept", got)
	}
}

// Regression for the update paths: the field patch commits in its own
// transaction before the close, so the refusal must come before it.
func TestUpdateMCP_OutcomeRequiredRefusesBeforeAnyWrite(t *testing.T) {
	env := comSetup(t)

	t.Run("skein_update title + status=closed", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "requ-single")
		before, title := comRead(t, env, id), corTitle(t, env, id)
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": before.Version,
			"title": "renamed", "status": "closed", "reason": "r",
		})
		if comErr(body) != ErrCodeValidationError || corDetailCode(body) != codeCloseOutcomeRequired {
			t.Fatalf("skein_update without outcome = %+v, want validation_error/%s", body, codeCloseOutcomeRequired)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("status/version changed: before %+v, after %+v", before, after)
		}
		if got := corTitle(t, env, id); got != title {
			t.Errorf("title = %q after refusal, want unchanged %q", got, title)
		}
	})

	t.Run("skein_update_many title + status=closed, sibling row unaffected", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "requ-many")
		sibling := comSeed(t, env, env.pid, env.prefix, "requ-many-sib")
		before, title := comRead(t, env, id), corTitle(t, env, id)
		body := ucoUpdateMany(t, env, []any{
			map[string]any{"id": id, "expectedVersion": before.Version,
				"title": "renamed", "status": "closed", "reason": "r"},
			map[string]any{"id": sibling, "expectedVersion": comRead(t, env, sibling).Version,
				"status": "closed", "reason": "r", "closeOutcome": "wont_do"},
		})
		row0 := comRow(t, body, 0)
		if row0["ok"] != false || comRowErr(t, row0) != ErrCodeValidationError ||
			comRowErrDetails(t, row0)["code"] != codeCloseOutcomeRequired {
			t.Errorf("row without outcome = %+v, want failed validation_error/%s", row0, codeCloseOutcomeRequired)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("status/version changed: before %+v, after %+v", before, after)
		}
		if got := corTitle(t, env, id); got != title {
			t.Errorf("title = %q after refusal, want unchanged %q", got, title)
		}
		if row1 := comRow(t, body, 1); row1["ok"] != true {
			t.Errorf("sibling row = %+v, want ok", row1)
		}
		if got := comRead(t, env, sibling); got.Status != "closed" || got.Outcome != "wont_do" {
			t.Errorf("sibling: %+v, want closed/wont_do", got)
		}
	})

	t.Run("skein_update standing → closed without outcome is refused", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "requ-standing")
		corSetStatus(t, env, id, "standing")
		before := comRead(t, env, id)
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": before.Version, "status": "closed", "reason": "r",
		})
		if comErr(body) != ErrCodeValidationError || corDetailCode(body) != codeCloseOutcomeRequired {
			t.Fatalf("skein_update standing→closed without outcome = %+v, want validation_error/%s",
				body, codeCloseOutcomeRequired)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("refused call changed the issue: before %+v, after %+v", before, after)
		}
	})

	t.Run("skein_update re-close without outcome keeps the previous outcome", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "requ-reclose")
		corCloseWith(t, env, id, "superseded")
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": comRead(t, env, id).Version,
			"title": "renamed after close", "status": "closed", "reason": "r2",
		})
		if body["ok"] != true {
			t.Fatalf("re-close via skein_update = %+v, want ok", body)
		}
		if got := comRead(t, env, id); got.Status != "closed" || got.Outcome != "superseded" {
			t.Errorf("after re-close: %+v, want closed/superseded", got)
		}
	})
}

// The race the in-transaction check exists for: with force=true the version
// predicate is bypassed, so an issue that was closed when the pre-check read
// it and is reopened before the close step must still be refused — never
// closed with a silent `done`. The field patch between the two points
// commits in its own transaction; the test records that it stays written
// (the documented partial-state semantics of skein_update).
func TestUpdateMCP_OutcomeRequiredSurvivesReopenRaceUnderForce(t *testing.T) {
	env := comSetup(t)

	// The forcing admin edits by his own role in env.pid (US-6, T066).
	comAdminWritesInPid(t, env)
	id := comSeed(t, env, env.pid, env.prefix, "requ-race")
	corCloseWith(t, env, id, "wont_do")

	afterClosePrecheckHook = func(ctx context.Context, issueID string) {
		if issueID != id {
			return
		}
		// Reopen outside the tool, as a concurrent writer would.
		if _, err := env.pool.Exec(ctx,
			`UPDATE issues SET status = 'open', closed_at = NULL, closed_reason = NULL,
			        close_outcome = NULL, version = version + 1
			  WHERE id = $1`, issueID); err != nil {
			t.Errorf("reopen inside the race window: %v", err)
		}
	}
	t.Cleanup(func() { afterClosePrecheckHook = nil })

	body := ucoUpdateAs(t, env, env.adminCtx, map[string]any{
		"id": id, "expectedVersion": comRead(t, env, id).Version, "force": true,
		"title": "renamed in race", "status": "closed", "reason": "r",
	})
	if comErr(body) != ErrCodeValidationError || corDetailCode(body) != codeCloseOutcomeRequired {
		t.Fatalf("close after reopen race = %+v, want validation_error/%s", body, codeCloseOutcomeRequired)
	}
	got := comRead(t, env, id)
	if got.Status != "open" || got.Outcome != "" {
		t.Errorf("after refused close: %+v, want status open and no outcome", got)
	}
	if title := corTitle(t, env, id); title != "renamed in race" {
		t.Errorf("title = %q, want the field patch (step 1) to stay committed", title)
	}
}
