package tools

// close_target_required_mcp_test.go — sk-540.2 on the MCP surfaces:
// duplicate/superseded without closeTarget is refused on a transition into
// closed (validation_error, code close_target_required). On the update
// surfaces the refusal must come before the field patch commits;
// skein_update_many, which takes no target, can therefore never close an
// open issue as a duplicate, but may change a closed issue's outcome when the
// matching edge already exists.

import (
	"context"
	"testing"
)

const codeCloseTargetRequired = "close_target_required"

func TestCloseTargetRequiredMCP_TransitionRefused(t *testing.T) {
	env := comSetup(t)

	t.Run("skein_close", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "treq-close")
		before := comRead(t, env, id)
		body := comClose(t, env, map[string]any{
			"id": id, "expectedVersion": before.Version, "reason": "r", "closeOutcome": "duplicate",
		})
		if comErr(body) != ErrCodeValidationError || corDetailCode(body) != codeCloseTargetRequired {
			t.Fatalf("skein_close duplicate without target = %+v, want validation_error/%s",
				body, codeCloseTargetRequired)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("refused close changed the issue: %+v → %+v", before, after)
		}
	})

	t.Run("skein_update title + status=closed superseded", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "treq-update")
		before, title := comRead(t, env, id), corTitle(t, env, id)
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": before.Version, "title": "renamed",
			"status": "closed", "reason": "r", "closeOutcome": "superseded",
		})
		if comErr(body) != ErrCodeValidationError || corDetailCode(body) != codeCloseTargetRequired {
			t.Fatalf("skein_update superseded without target = %+v, want validation_error/%s",
				body, codeCloseTargetRequired)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("refused update changed the issue: %+v → %+v", before, after)
		}
		if got := corTitle(t, env, id); got != title {
			t.Errorf("title = %q after refusal, want unchanged %q — refused after the patch step", got, title)
		}
	})

	t.Run("skein_update_many cannot close an open issue as duplicate", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "treq-many-open")
		before, title := comRead(t, env, id), corTitle(t, env, id)
		body := ucoUpdateMany(t, env, []any{
			map[string]any{"id": id, "expectedVersion": before.Version, "title": "renamed",
				"status": "closed", "reason": "r", "closeOutcome": "duplicate"},
		})
		row := comRow(t, body, 0)
		if row["ok"] != false || comRowErr(t, row) != ErrCodeValidationError ||
			comRowErrDetails(t, row)["code"] != codeCloseTargetRequired {
			t.Fatalf("row = %+v, want failed validation_error/%s", row, codeCloseTargetRequired)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("refused row changed the issue: %+v → %+v", before, after)
		}
		if got := corTitle(t, env, id); got != title {
			t.Errorf("title = %q after refusal, want unchanged %q", got, title)
		}
	})
}

// TestCloseTargetRequiredMCP_UpdateManyOutcomeOfClosed — skein_update_many
// on already closed issues: status=closed with an outcome change passes with
// the matching edge and fails without it; the pre-check sees the difference
// before any write.
func TestCloseTargetRequiredMCP_UpdateManyOutcomeOfClosed(t *testing.T) {
	env := comSetup(t)
	original := comSeed(t, env, env.pid, env.prefix, "treq-orig")

	withEdge := comSeed(t, env, env.pid, env.prefix, "treq-with-edge")
	corCloseWith(t, env, withEdge, "done")
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'duplicates', '{}'::jsonb, 1, $3, now())`, withEdge, original, env.userID); err != nil {
		t.Fatalf("seed duplicates edge: %v", err)
	}
	noEdge := comSeed(t, env, env.pid, env.prefix, "treq-no-edge")
	corCloseWith(t, env, noEdge, "done")
	before, title := comRead(t, env, noEdge), corTitle(t, env, noEdge)

	body := ucoUpdateMany(t, env, []any{
		map[string]any{"id": withEdge, "expectedVersion": comRead(t, env, withEdge).Version,
			"status": "closed", "reason": "r", "closeOutcome": "duplicate"},
		map[string]any{"id": noEdge, "expectedVersion": before.Version, "title": "renamed",
			"status": "closed", "reason": "r", "closeOutcome": "duplicate"},
	})
	if row := comRow(t, body, 0); row["ok"] != true {
		t.Errorf("row with the matching edge = %+v, want ok", row)
	}
	if got := comRead(t, env, withEdge); got.Outcome != "duplicate" {
		t.Errorf("row with edge: outcome %q, want duplicate", got.Outcome)
	}
	row := comRow(t, body, 1)
	if row["ok"] != false || comRowErrDetails(t, row)["code"] != codeCloseTargetRequired {
		t.Errorf("row without edge = %+v, want failed %s", row, codeCloseTargetRequired)
	}
	if after := comRead(t, env, noEdge); after != before {
		t.Errorf("refused row changed: %+v → %+v", before, after)
	}
	if got := corTitle(t, env, noEdge); got != title {
		t.Errorf("title = %q after refusal, want unchanged %q", got, title)
	}
}
