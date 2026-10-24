package tools

// update_close_outcome_test.go — sk-489, T009, matrix rows 34, 35, 36.
//
// # Three surfaces, TWO writers on the first of them
//
//	skein_update       → update.go#handleUpdate            (row 34)
//	skein_update_many  → update_run.go#buildUpdateOneInput (row 35)
//	skein_move         → move.go, the close template       (row 36)
//
// In `skein_update` the outcome reaches TWO different store writers, and
// which one is decided by the status:
//
//	status=closed → Store.CloseWithResult      (the close step)
//	no status     → Store.PatchIssueWithResult (the field edit step)
//
// The target cells are checked on BOTH, by the same table: the target helper on the edit
// path is wired through its own input (T004 requires DependenciesCaller on
// PatchIssueInput too), and a half proved on one writer is green when the
// other diverges.
//
// The third writer the surface reaches — `Store.UpdateStatus`
// at a status ≠ `closed` — has no target cells of its own, and this is
// ASSERTED, not inferred: a separate subtest sends both the outcome and the target there and
// requires a refusal before the first step of the chain.
//
// The main caller is not an administrator, for the same reason as in the two
// previous files of the wave: the admin's horizon does not tell a visible target from an
// invisible one. But a non-admin has a blind spot of its own, and it is exactly the opposite:
// substituting the SYSTEM role (issues.DependenciesCaller) with Actor.Role on him
// is INVISIBLE — "user" and "agent" are both non-admin, and membership in
// projects/access.go#EnsureProjectVisible is counted the same for them.
// It is told apart only by an administrator with a target outside his membership, and for that
// the file has a separate probe #TestUpdateMCP_TargetVisibilityUsesTheSystemRole
// (more in the header of close_outcome_mcp_test.go).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// ---------------- helpers on top of the close_outcome_mcp_test.go fixture ----------------

// ucoUpdate runs one skein_update and returns the parsed body.
func ucoUpdate(t *testing.T, env *comEnv, args map[string]any) map[string]any {
	t.Helper()
	return ucoUpdateAs(t, env, env.ctx, args)
}

// ucoUpdateAs — the same call from an arbitrary caller. Needed by the system-role
// probe: it is the only one in the file that runs under "admin".
func ucoUpdateAs(t *testing.T, env *comEnv, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handleUpdate(env.pool, env.store)(ctx, req)
	if err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
	return extractToolJSON(t, res)
}

// ucoUpdateMany runs one skein_update_many.
func ucoUpdateMany(t *testing.T, env *comEnv, items []any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}
	res, err := handleUpdateMany(env.pool, env.store)(env.ctx, req)
	if err != nil {
		t.Fatalf("handleUpdateMany: %v", err)
	}
	if res.IsError {
		t.Fatalf("skein_update_many answered with a TOOL ERROR instead of a per-row "+
			"result: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

// ucoPriority reads the priority — the field that verifies "nothing
// changed" on calls that mixed a field edit with an outcome.
func ucoPriority(t *testing.T, env *comEnv, id string) int {
	t.Helper()
	var p int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT priority FROM issues WHERE id = $1`, id).Scan(&p); err != nil {
		t.Fatalf("read priority %s: %v", id, err)
	}
	return p
}

// ucoCloseVia closes an issue THROUGH the tool under test — the preparation of
// an "already closed" probe for the edit path.
func ucoCloseVia(t *testing.T, env *comEnv, id string) {
	t.Helper()
	ucoCloseViaAs(t, env, env.ctx, id)
}

func ucoCloseViaAs(t *testing.T, env *comEnv, ctx context.Context, id string) {
	t.Helper()
	body := ucoUpdateAs(t, env, ctx, map[string]any{
		"id": id, "expectedVersion": comRead(t, env, id).Version,
		"status": "closed", "closeOutcome": "done", "reason": "preparation"})
	if body["ok"] != true {
		t.Fatalf("preparation: closing %s failed: %+v", id, body)
	}
}

// ---------------- row 34: skein_update ----------------

// TestUpdateMCP_OutcomeRoutes — the chain and the two writers.
func TestUpdateMCP_OutcomeRoutes(t *testing.T) {
	env := comSetup(t)

	t.Run("status=closed + wont_do on an open one → closed in one step", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "u1")
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": comRead(t, env, id).Version,
			"status": "closed", "reason": "r", "closeOutcome": "wont_do"})
		if body["ok"] != true {
			t.Fatalf("the call failed: %+v", body)
		}
		if got, _ := body["closeOutcome"].(string); got != "wont_do" {
			t.Errorf("acknowledgement: closeOutcome = %q, expected wont_do (%+v)", got, body)
		}
		if got := comRead(t, env, id); got.Status != "closed" || got.Outcome != "wont_do" {
			t.Errorf("in the database %+v, expected closed/wont_do", got)
		}
	})

	t.Run("priority + bogus → validation_error, PRIORITY UNCHANGED", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "u2")
		before := comRead(t, env, id)
		beforePrio := ucoPriority(t, env, id)
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": before.Version,
			"priority": 0, "status": "closed", "reason": "r", "closeOutcome": "bogus"})
		if comErr(body) != ErrCodeValidationError {
			t.Fatalf("code = %q, expected validation_error (%+v)", comErr(body), body)
		}
		// Step 1 of the chain is a field edit, and it commits on its own.
		// An outcome check standing AFTER it would give the right code with a
		// changed priority: a refusal with a side effect.
		if got := ucoPriority(t, env, id); got != beforePrio {
			t.Errorf("priority = %d, was %d — the outcome refusal happened AFTER the field edit "+
				"step, so \"nothing changed\" is untrue", got, beforePrio)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("the issue changed: was %+v, now %+v", before, after)
		}
	})

	t.Run("outcome at status=in_progress → validation_error, nothing changed", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "u3")
		before := comRead(t, env, id)
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": before.Version,
			"status": "in_progress", "closeOutcome": "done"})
		if comErr(body) != ErrCodeValidationError {
			t.Fatalf("code = %q, expected validation_error (%+v)", comErr(body), body)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("the issue changed: was %+v, now %+v", before, after)
		}
	})

	t.Run("priority + status=closed + outcome → the field is edited AND the issue is closed", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "u4")
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": comRead(t, env, id).Version,
			"priority": 4, "status": "closed", "reason": "r", "closeOutcome": "wont_do"})
		if body["ok"] != true {
			t.Fatalf("the call failed: %+v", body)
		}
		if got := ucoPriority(t, env, id); got != 4 {
			t.Errorf("priority = %d, expected 4 — the chain must perform BOTH steps", got)
		}
		if got := comRead(t, env, id); got.Status != "closed" || got.Outcome != "wont_do" {
			t.Errorf("in the database %+v, expected closed/wont_do", got)
		}
	})

	t.Run("outcome on a CLOSED one without a status and without other fields → an edit", func(t *testing.T) {
		// The discriminating case of the edit path, and the ONLY one in the file where
		// the call has not a single ordinary edit field. An implementation that did not
		// widen the step 1 gate with the "call carries an outcome" flag would skip the
		// step, not write the outcome and answer ok:true — that is, it would be
		// green on any case where some other field travelled alongside.
		id := comSeed(t, env, env.pid, env.prefix, "u5")
		ucoCloseVia(t, env, id)
		if got := comRead(t, env, id); got.Outcome != "done" {
			t.Fatalf("preparation: outcome = %q, expected done", got.Outcome)
		}
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": comRead(t, env, id).Version,
			"closeOutcome": "wont_do"})
		if body["ok"] != true {
			t.Fatalf("the call failed: %+v", body)
		}
		if got, _ := body["closeOutcome"].(string); got != "wont_do" {
			t.Errorf("acknowledgement: closeOutcome = %q, expected wont_do (%+v)", got, body)
		}
		got := comRead(t, env, id)
		if got.Outcome != "wont_do" {
			t.Errorf("in the database the outcome = %q, expected wont_do — an edit of ONLY the outcome was not "+
				"written: the field edit step was skipped", got.Outcome)
		}
		if got.Status != "closed" {
			t.Errorf("the outcome edit reopened the issue: status = %q", got.Status)
		}
	})

	t.Run("reviewRequired together with closeOutcome → refused as a whole", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "u6")
		ucoCloseVia(t, env, id)
		before := comRead(t, env, id)
		body := ucoUpdate(t, env, map[string]any{
			"id": id, "expectedVersion": before.Version,
			"reviewRequired": true, "closeOutcome": "wont_do"})
		if comErr(body) != ErrCodeValidationError {
			t.Fatalf("code = %q, expected validation_error (%+v)", comErr(body), body)
		}
		// Neither the flag nor the outcome: "rejected AS A WHOLE" — about both.
		if after := comRead(t, env, id); after != before {
			t.Errorf("the mixed request wrote something: was %+v, now %+v", before, after)
		}
	})
}

// TestUpdateMCP_LinkOnBothWriters — the edge on BOTH writers of the surface.
func TestUpdateMCP_LinkOnBothWriters(t *testing.T) {
	env := comSetup(t)

	writers := []struct {
		name string
		// args builds the call that brings the outcome to the needed writer.
		args func(id, target string, version int) map[string]any
		// prepare brings the issue to a state from which the writer is reachable.
		prepare func(t *testing.T, env *comEnv, id string)
	}{
		{
			name: "Store.Close (status=closed)",
			args: func(id, target string, v int) map[string]any {
				return map[string]any{"id": id, "expectedVersion": v,
					"status": "closed", "reason": "r",
					"closeOutcome": "duplicate", "closeTarget": target}
			},
			prepare: func(t *testing.T, env *comEnv, id string) {},
		},
		{
			name: "Store.PatchIssue (without a status)",
			args: func(id, target string, v int) map[string]any {
				return map[string]any{"id": id, "expectedVersion": v,
					"closeOutcome": "duplicate", "closeTarget": target}
			},
			prepare: func(t *testing.T, env *comEnv, id string) { ucoCloseVia(t, env, id) },
		},
	}

	for i, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			id := comSeed(t, env, env.pid, env.prefix, fmt.Sprintf("ulink%d", i))
			w.prepare(t, env, id)
			// The target is in another VISIBLE project: for a non-admin it is a separate
			// cell, and a lost system role is caught on it.
			target := comSeed(t, env, env.pidOther, env.prefixOther, fmt.Sprintf("ulink%dt", i))
			want := id + "|duplicates|" + target

			body := ucoUpdate(t, env, w.args(id, target, comRead(t, env, id).Version))
			if body["ok"] != true {
				t.Fatalf("the call failed: %+v", body)
			}
			if got, _ := body["closeLink"].(string); got != string(issues.CloseLinkCreated) {
				t.Fatalf("closeLink = %q, expected created (%+v)", got, body)
			}
			if got := comEdges(t, env, id, target); len(got) != 1 || got[0] != want {
				t.Fatalf("edges = %v, expected exactly [%s]", got, want)
			}

			body = ucoUpdate(t, env, w.args(id, target, comRead(t, env, id).Version))
			if got, _ := body["closeLink"].(string); got != string(issues.CloseLinkExisted) {
				t.Fatalf("repeat: closeLink = %q, expected existed (%+v)", got, body)
			}
			if got := comEdges(t, env, id, target); len(got) != 1 || got[0] != want {
				t.Fatalf("edges after the repeat = %v, expected still exactly [%s]", got, want)
			}
		})
	}
}

// TestUpdateMCP_TargetRefusalsOnBothWriters — THE SAME table of cells as in
// skein_close, run on BOTH writers of this surface.
func TestUpdateMCP_TargetRefusalsOnBothWriters(t *testing.T) {
	env := comSetup(t)
	invisible := comSeed(t, env, env.pidHidden, env.prefixHidden, "uinv")
	missing := env.prefix + "-99999"

	routes := []struct {
		name    string
		prepare func(t *testing.T, env *comEnv, id string)
		extra   map[string]any
	}{
		{"Store.Close (status=closed)", func(t *testing.T, env *comEnv, id string) {},
			map[string]any{"status": "closed", "reason": "r"}},
		{"Store.PatchIssue (without a status)", func(t *testing.T, env *comEnv, id string) {
			ucoCloseVia(t, env, id)
		}, map[string]any{}},
	}

	for r, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			for i, c := range comTargetCases(invisible, missing) {
				t.Run(c.name, func(t *testing.T) {
					id := comSeed(t, env, env.pid, env.prefix, fmt.Sprintf("uref%d%d", r, i))
					route.prepare(t, env, id)
					target := c.target
					if target == "" {
						target = id
					}
					before := comRead(t, env, id)

					args := map[string]any{
						"id": id, "expectedVersion": before.Version, "closeTarget": target,
					}
					for k, v := range route.extra {
						args[k] = v
					}
					if c.outcome != nil {
						args["closeOutcome"] = *c.outcome
					}
					body := ucoUpdate(t, env, args)

					if comErr(body) == ErrCodeInternalError {
						t.Fatalf("the refusal fell into internal_error — mapUpdateError does not "+
							"map target errors explicitly (%+v)", body)
					}
					if comErr(body) != c.wantCode {
						t.Fatalf("code = %q, expected %s (%+v)", comErr(body), c.wantCode, body)
					}
					if after := comRead(t, env, id); after != before {
						t.Errorf("the rejected call changed the issue: was %+v, now %+v",
							before, after)
					}
					if edges := comEdges(t, env, id, target); len(edges) != 0 {
						t.Errorf("the rejected call created edges: %v", edges)
					}
				})
			}
		})
	}
}

// TestUpdateMCP_FormalRefusalPrecedesTheStore — ORDER on skein_update:
// parsing and the shape check stand BEFORE the store access (sk-516).
//
// # What exactly differs here
//
// Not the code — the ORDER. An input in which BOTH the shape AND the state are wrong (the issue
// is absent or not visible) must answer about the SHAPE. An implementation in which the
// formal block stands AFTER GetIssue and the visibility check answers the same input
// with issue_not_found — exactly how skein_update differed from
// skein_close before sk-516.
//
// The discriminating state is "absent / not visible", NOT a stale version:
// the version predicate lives inside the writer, and the formal block beat it
// under the old order too. That the shape check exists at all is
// a neighbouring assertion of its own (#TestUpdateMCP_OutcomeRoutes, the subtest
// "priority + bogus"); the cells here are chosen to go red on
// MOVING the block, not on its removal.
//
// # Why the control cells are alongside
//
// The assertion "the shape wins" is by itself green for an implementation
// that ALWAYS answers validation_error. The control cells — an input
// where ONLY the state is wrong — require a state refusal and lock out this
// degenerate case: together the pair says "the shape wins when it is
// wrong", not "always the shape".
//
// # Why the cells are not only about the outcome
//
// The whole formal block moved before the store, not one of its checks:
// componentIds, labels, status, the outcome with its target, `reason` and the field edit.
// The cells on componentIds and on the title guard the edges of the block — its first and
// last checks — otherwise "only the outcome was lifted up" would stay
// green.
//
// The order reference is close_outcome_mcp_test.go#
// TestCloseMCP_FormalRefusalPrecedesTheStore: it is about skein_close, was
// not changed, and sk-516 aligned the second surface to it.
func TestUpdateMCP_FormalRefusalPrecedesTheStore(t *testing.T) {
	env := comSetup(t)
	other := comSeed(t, env, env.pidOther, env.prefixOther, "ordtgt")
	invisible := comSeed(t, env, env.pidHidden, env.prefixHidden, "ordinv")
	invisibleBefore := comRead(t, env, invisible)
	missing := env.prefix + "-99999"
	tooLongLabel := strings.Repeat("x", 101)

	// ---- the shape is wrong TOGETHER with the state → the refusal must be about the shape ----
	formCases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "nonexistent issue + outcome not from the set → about the argument, not the issue",
			args: map[string]any{"id": missing, "expectedVersion": 1,
				"status": "closed", "reason": "r", "closeOutcome": "bogus"},
		},
		{
			name: "nonexistent issue + target without an outcome → about the argument, not the issue",
			args: map[string]any{"id": missing, "expectedVersion": 1,
				"status": "closed", "reason": "r", "closeTarget": other},
		},
		{
			name: "nonexistent issue + componentIds not a UUID → about the argument, not the issue",
			args: map[string]any{"id": missing, "expectedVersion": 1,
				"componentIds": []any{"not-a-uuid"}},
		},
		{
			name: "nonexistent issue + empty title → about the argument, not the issue",
			args: map[string]any{"id": missing, "expectedVersion": 1, "title": "   "},
		},
		{
			name: "invisible issue + outcome not from the set → about the argument, not the visibility",
			args: map[string]any{"id": invisible, "expectedVersion": invisibleBefore.Version,
				"status": "closed", "reason": "r", "closeOutcome": "bogus"},
		},
		{
			name: "invisible issue + label longer than the limit → about the argument, not the visibility",
			args: map[string]any{"id": invisible, "expectedVersion": invisibleBefore.Version,
				"labelsAdd": []any{tooLongLabel}},
		},
	}

	for _, c := range formCases {
		t.Run(c.name, func(t *testing.T) {
			body := ucoUpdate(t, env, c.args)

			if got := comErr(body); got != ErrCodeValidationError {
				t.Fatalf("code = %q, expected validation_error. %s is the STORE's answer, "+
					"and it comes exactly when handleUpdate went to the database "+
					"BEFORE checking the shape: the caller gets \"look for the issue\" for "+
					"their own wrong argument and goes looking for the issue instead of "+
					"fixing the call (%+v)", got, ErrCodeIssueNotFound, body)
			}
		})
	}

	// The rejected calls did not touch the invisible issue — they could touch it
	// only by getting past the visibility check.
	if after := comRead(t, env, invisible); after != invisibleBefore {
		t.Errorf("the invisible issue was changed by the rejected calls: was %+v, now %+v",
			invisibleBefore, after)
	}

	// ---- control: ONLY the state is wrong → the refusal must be about it ----
	stateCases := []struct {
		name     string
		args     func(id string, version int) map[string]any
		wantCode string
	}{
		{
			name: "nonexistent issue with a flawless shape → issue_not_found",
			args: func(id string, v int) map[string]any {
				return map[string]any{"id": missing, "expectedVersion": 1,
					"status": "closed", "reason": "r", "closeOutcome": "done"}
			},
			wantCode: ErrCodeIssueNotFound,
		},
		{
			name: "invisible issue with a flawless shape → issue_not_found",
			args: func(id string, v int) map[string]any {
				return map[string]any{"id": invisible,
					"expectedVersion": invisibleBefore.Version,
					"labelsAdd":       []any{"ok-label"}}
			},
			wantCode: ErrCodeIssueNotFound,
		},
		{
			name: "stale version with a flawless shape → version_conflict",
			args: func(id string, v int) map[string]any {
				return map[string]any{"id": id, "expectedVersion": v + 7,
					"status": "closed", "reason": "r", "closeOutcome": "wont_do"}
			},
			wantCode: ErrCodeVersionConflict,
		},
	}

	for i, c := range stateCases {
		t.Run(c.name, func(t *testing.T) {
			id := comSeed(t, env, env.pid, env.prefix, fmt.Sprintf("uctl%d", i))
			before := comRead(t, env, id)

			body := ucoUpdate(t, env, c.args(id, before.Version))

			if got := comErr(body); got != c.wantCode {
				t.Fatalf("code = %q, expected %s: the shape here is flawless, and the refusal must "+
					"be about the state — otherwise \"the shape wins\" would mean \"always "+
					"validation_error» (%+v)", got, c.wantCode, body)
			}
		})
	}
}

// TestUpdateManyMCP_FormalRefusalPrecedesTheStore — the same order on the
// SECOND input builder of the surface (update_run.go#buildUpdateOneInput).
//
// The batch path does NOT call the single one — checked by calls, there are none — and
// an assertion proved on handleUpdate is green here under any order.
// A half of the surface without a cell of its own is exactly the class of defect
// on which the sk-489 wave got three majors.
//
// The order of this half was right even BEFORE sk-516 (the row parsing stood above
// its GetIssue); the cell is added not to change it, but so that it
// stops depending on the fact that nobody reordered the lines.
func TestUpdateManyMCP_FormalRefusalPrecedesTheStore(t *testing.T) {
	env := comSetup(t)
	invisible := comSeed(t, env, env.pidHidden, env.prefixHidden, "mordinv")
	invisibleBefore := comRead(t, env, invisible)
	missing := env.prefix + "-99999"
	tooLongLabel := strings.Repeat("x", 101)

	body := ucoUpdateMany(t, env, []any{
		// 0 — the shape AND the state are wrong: the refusal must be about the shape.
		map[string]any{"id": missing, "expectedVersion": float64(1),
			"status": "closed", "reason": "r", "closeOutcome": "bogus"},
		// 1 — the same on an invisible issue, and with another edge of the formal block.
		map[string]any{"id": invisible, "expectedVersion": float64(invisibleBefore.Version),
			"labelsAdd": []any{tooLongLabel}},
		// 2 — control: the shape is flawless, only the state is wrong.
		map[string]any{"id": missing, "expectedVersion": float64(1),
			"status": "closed", "reason": "r", "closeOutcome": "done"},
		// 3 — control on an invisible issue.
		map[string]any{"id": invisible, "expectedVersion": float64(invisibleBefore.Version),
			"labelsAdd": []any{"ok-label"}},
	})

	for i, want := range []string{
		ErrCodeValidationError, ErrCodeValidationError,
		ErrCodeIssueNotFound, ErrCodeIssueNotFound,
	} {
		row := comRow(t, body, i)
		if row["ok"] != false {
			t.Fatalf("position %d passed, but had to be refused: %+v", i, row)
		}
		if got := comRowErr(t, row); got != want {
			t.Errorf("position %d: code = %q, expected %s. Positions 0–1 require a SHAPE "+
				"refusal on an input where both the shape and the state are wrong; positions 2–3 — "+
				"a STATE refusal where the shape is flawless (%+v)", i, got, want, row)
		}
	}

	if after := comRead(t, env, invisible); after != invisibleBefore {
		t.Errorf("the invisible issue was changed by the refusing positions: was %+v, now %+v",
			invisibleBefore, after)
	}
}

// TestUpdateMCP_TargetVisibilityUsesTheSystemRole — the target visibility key in
// skein_update is the SYSTEM role, not Actor.Role, on BOTH writers.
//
// The discriminating case is an administrator and a target in a project where he is not: see
// #TestCloseMCP_TargetVisibilityUsesTheSystemRole and the header of
// close_outcome_mcp_test.go. Under the role "user", which all the cells of the
// table above run under, the substitution is invisible — measured by mutation, the full gate
// stayed green (review A/4, F-2).
//
// Both writers are run, although the role carrier of the surface is assembled by
// ONE line update.go#handleUpdate: the line is one today, but there are two paths to
// the store, and the target cells go through both in the whole file (see
// #TestUpdateMCP_TargetRefusalsOnBothWriters).
func TestUpdateMCP_TargetVisibilityUsesTheSystemRole(t *testing.T) {
	env := comSetup(t)
	target := comSeed(t, env, env.pidHidden, env.prefixHidden, "uadmtgt")

	// Reference by running: a manual link by the same admin to the same target.
	comAdminWritesInPid(t, env)
	scratch := comSeed(t, env, env.pid, env.prefix, "uadmscr")
	if code := comErr(comDepAddAs(t, env, env.adminCtx, scratch, target)); code != "" {
		t.Fatalf("the fixture does not reach the discriminating case: the admin's manual link "+
			"to a target outside his membership was refused (%s)", code)
	}

	routes := []struct {
		name    string
		prepare func(t *testing.T, id string)
		extra   map[string]any
	}{
		{"Store.Close (status=closed)", func(t *testing.T, id string) {},
			map[string]any{"status": "closed", "reason": "r"}},
		{"Store.PatchIssue (without a status)", func(t *testing.T, id string) {
			ucoCloseViaAs(t, env, env.adminCtx, id)
		}, map[string]any{}},
	}

	for i, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			id := comSeed(t, env, env.pid, env.prefix, fmt.Sprintf("uadm%d", i))
			route.prepare(t, id)
			args := map[string]any{
				"id": id, "expectedVersion": comRead(t, env, id).Version,
				"closeOutcome": "duplicate", "closeTarget": target,
			}
			for k, v := range route.extra {
				args[k] = v
			}
			body := ucoUpdateAs(t, env, env.adminCtx, args)

			if got, _ := body["closeLink"].(string); got != string(issues.CloseLinkCreated) {
				t.Fatalf("the admin was rejected on a target that a manual link by the same "+
					"caller ACCEPTED: %+v.\nUS-2 requires the close path to have the same "+
					"refusal contract as skein_dep_add. Target visibility was read "+
					"not by the SYSTEM role (issues.DependenciesCaller.Role) but by "+
					"Actor.Role — on a non-admin these two keys are indistinguishable, here "+
					"they diverge", body)
			}
			want := id + "|duplicates|" + target
			if got := comEdges(t, env, id); len(got) != 1 || got[0] != want {
				t.Fatalf("edges = %v, expected exactly [%s]", got, want)
			}
		})
	}
}

// TestUpdateMCP_ThirdWriterNeverSeesTheOutcome — an ASSERTION about the third
// writer, not an inference from its absence in the table above.
//
// `Store.UpdateStatus` (status ≠ `closed`) has no target cells. This is proved
// not by silence but by the call that would go to it: both the outcome and the target with
// status `in_progress` must be rejected BEFORE the first step of the chain —
// the issue is unchanged, no edge, i.e. neither the outcome nor the target
// reached the writer.
func TestUpdateMCP_ThirdWriterNeverSeesTheOutcome(t *testing.T) {
	env := comSetup(t)
	id := comSeed(t, env, env.pid, env.prefix, "u3rd")
	target := comSeed(t, env, env.pidOther, env.prefixOther, "u3rdt")
	before := comRead(t, env, id)

	body := ucoUpdate(t, env, map[string]any{
		"id": id, "expectedVersion": before.Version,
		"status": "in_progress", "closeOutcome": "duplicate", "closeTarget": target})

	if comErr(body) != ErrCodeValidationError {
		t.Fatalf("code = %q, expected validation_error (%+v)", comErr(body), body)
	}
	if after := comRead(t, env, id); after != before {
		t.Errorf("the issue changed: was %+v, now %+v", before, after)
	}
	if edges := comEdges(t, env, id, target); len(edges) != 0 {
		t.Errorf("the target reached the third writer and created edges: %v", edges)
	}
}

// ---------------- row 35: skein_update_many ----------------

// TestUpdateManyMCP_OutcomePerRow — the outcome on a position, both writers, and refusals.
func TestUpdateManyMCP_OutcomePerRow(t *testing.T) {
	env := comSetup(t)
	target := comSeed(t, env, env.pidOther, env.prefixOther, "umt")

	closing := comSeed(t, env, env.pid, env.prefix, "um1")  // Store.Close
	patching := comSeed(t, env, env.pid, env.prefix, "um2") // Store.PatchIssue
	ucoCloseVia(t, env, patching)
	// sk-540.2: update_many takes no closeTarget, so changing a closed
	// issue's outcome to duplicate needs the `duplicates` edge to exist
	// already (exception (a)).
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'duplicates', '{}'::jsonb, 1, $3, now())`,
		patching, comSeed(t, env, env.pid, env.prefix, "um2-original"), env.userID); err != nil {
		t.Fatalf("seed duplicates edge: %v", err)
	}
	withTarget := comSeed(t, env, env.pid, env.prefix, "um3") // the target is a row refusal
	bogus := comSeed(t, env, env.pid, env.prefix, "um4")
	notAClose := comSeed(t, env, env.pid, env.prefix, "um5")
	untouched := map[string]comState{
		withTarget: comRead(t, env, withTarget),
		bogus:      comRead(t, env, bogus),
		notAClose:  comRead(t, env, notAClose),
	}
	// The priority BEFORE the call — per each refusing position. Why it is here is
	// explained at the positions themselves below.
	prioBefore := map[string]int{
		withTarget: ucoPriority(t, env, withTarget),
		bogus:      ucoPriority(t, env, bogus),
		notAClose:  ucoPriority(t, env, notAClose),
	}

	body := ucoUpdateMany(t, env, []any{
		map[string]any{"id": closing, "expectedVersion": float64(comRead(t, env, closing).Version),
			"status": "closed", "reason": "r", "closeOutcome": "wont_do"},
		map[string]any{"id": patching, "expectedVersion": float64(comRead(t, env, patching).Version),
			"closeOutcome": "duplicate"},
		// ⚠ In the THREE refusing positions an ORDINARY FIELD (`priority`) travels
		// next to the rejected argument, and this is not a decoration of the fixture.
		//
		// Step 1 of the runUpdateForOne chain is a field edit, and it commits on its own.
		// A position without a single ordinary field proves "unchanged" where there is nothing to
		// change: it is green for an implementation that PARSES the
		// arguments but does not check them — the refusal is then raised by the store, but already
		// AFTER the commit of step 1, and such a position has no step 1.
		// Measured (review A/4, F-1): the mutant "raw parsing in
		// buildUpdateOneInput" passed the narrowed run green on the old fixture, and with this field
		// it goes red with the same
		// message as the single row 34 ("priority + bogus").
		map[string]any{"id": withTarget, "expectedVersion": float64(untouched[withTarget].Version),
			"priority": 4, "status": "closed", "reason": "r",
			"closeOutcome": "duplicate", "closeTarget": target},
		map[string]any{"id": bogus, "expectedVersion": float64(untouched[bogus].Version),
			"priority": 4, "status": "closed", "reason": "r", "closeOutcome": "bogus"},
		map[string]any{"id": notAClose, "expectedVersion": float64(untouched[notAClose].Version),
			"priority": 4, "status": "in_progress", "closeOutcome": "done"},
	})
	// The row shape of THIS batch stayed the reference: US-5 froze it, and
	// sk-489 extended only skein_close_many. The guard is the old one, unchanged.
	assertBatchWireFormUnchanged(t, body, 5)

	// The writer of each executed branch is named, not implied:
	// the first went to Store.Close (status), the second to Store.PatchIssue
	// (no status). The surface has TWO writers, not "no writer".
	if row := comRow(t, body, 0); row["ok"] != true {
		t.Errorf("the Store.Close branch was not executed: %+v", row)
	}
	if got := comRead(t, env, closing); got.Status != "closed" || got.Outcome != "wont_do" {
		t.Errorf("the Store.Close branch: in the database %+v, expected closed/wont_do", got)
	}
	if row := comRow(t, body, 1); row["ok"] != true {
		t.Errorf("the Store.PatchIssue branch was not executed: %+v", row)
	}
	if got := comRead(t, env, patching); got.Status != "closed" || got.Outcome != "duplicate" {
		t.Errorf("the Store.PatchIssue branch: in the database %+v, expected closed/duplicate", got)
	}

	for i, id := range []string{withTarget, bogus, notAClose} {
		row := comRow(t, body, i+2)
		if row["ok"] != false {
			t.Errorf("%s: the position passed, but had to be refused: %+v", id, row)
			continue
		}
		if code := comRowErr(t, row); code != ErrCodeValidationError {
			t.Errorf("%s: position code = %q, expected validation_error", id, code)
		}
		if after := comRead(t, env, id); after != untouched[id] {
			t.Errorf("%s: the refused position was changed: was %+v, now %+v",
				id, untouched[id], after)
		}
		if got := ucoPriority(t, env, id); got != prioBefore[id] {
			t.Errorf("%s: priority = %d, was %d — the refusal happened AFTER the field edit "+
				"step, i.e. the batch input builder parsed the arguments but "+
				"did not check them, and \"the position is unchanged\" is untrue", id, got, prioBefore[id])
		}
	}
	// The edge of the "target" bush: it is rejected as a whole, so the surface
	// never has an edge at all.
	if edges := comEdges(t, env, withTarget, target); len(edges) != 0 {
		t.Errorf("skein_update_many created an edge by a target, though the target is not accepted: %v", edges)
	}
}

// ---------------- row 36: the move template ----------------

// TestMoveMCP_CloseStepCarriesSuperseded — the template and an end-to-end run.
func TestMoveMCP_CloseStepCarriesSuperseded(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	srcPID, srcPrefix := seedProject(t, pool, userID, "mvco253s")
	tgtPID, _ := seedProject(t, pool, userID, "mvco253t")
	issueID := srcPrefix + "-1"
	seedIssue(t, pool, srcPID, userID, issueID, "issue to move", "open", 2)

	iStore := issues.NewStore(pool)
	pStore := projects.NewStore(pool)
	dStore := dependencies.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-mvco")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID, "targetProject": tgtPID.String()}
	res, err := handleMove(pool, iStore, pStore)(ctx, req)
	if err != nil {
		t.Fatalf("handleMove: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)

	steps, _ := body["nextSteps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("nextSteps = %d, expected 2", len(steps))
	}
	closeStep, _ := steps[1].(map[string]any)
	if tool, _ := closeStep["tool"].(string); tool != "skein_close" {
		t.Fatalf("nextSteps[1].tool = %q, expected skein_close", tool)
	}
	closeArgs, _ := closeStep["args"].(map[string]any)
	if got, _ := closeArgs["closeOutcome"].(string); got != issues.CloseOutcomeSuperseded {
		t.Errorf("close step: closeOutcome = %q, expected superseded. The default would have given "+
			"`done`, and the move would count as completed work in every progress "+
			"where the source issue stands", got)
	}
	// sk-540.2: superseded requires closeTarget, so the step names the new
	// issue by the same placeholder as `reason` — its id does not exist at
	// signpost time.
	if got, _ := closeArgs["closeTarget"].(string); got != "<NEW-ID-FROM-skein_create-RESPONSE>" {
		t.Errorf("close step closeTarget = %#v, want the new-id placeholder", closeArgs["closeTarget"])
	}

	// An end-to-end run of the prepared calls: the template must WORK, not
	// only look right.
	createStep, _ := steps[0].(map[string]any)
	createArgs, _ := createStep["args"].(map[string]any)
	createReq := mcpgo.CallToolRequest{}
	createReq.Params.Arguments = createArgs
	createRes, err := handleCreate(pool, iStore, dStore, pStore)(ctx, createReq)
	if err != nil {
		t.Fatalf("handleCreate: %v", err)
	}
	if createRes.IsError {
		t.Fatalf("the prepared skein_create refused: %+v", extractToolJSON(t, createRes))
	}
	createBody := extractToolJSON(t, createRes)
	newIssue, _ := createBody["issue"].(map[string]any)
	newID, _ := newIssue["id"].(string)
	if newID == "" {
		t.Fatalf("skein_create did not return an id: %+v", createBody)
	}

	// The placeholder is replaced exactly as the step comment says.
	for _, key := range []string{"reason", "closeTarget"} {
		if v, _ := closeArgs[key].(string); strings.Contains(v, "<NEW-ID-FROM-skein_create-RESPONSE>") {
			closeArgs[key] = strings.ReplaceAll(v, "<NEW-ID-FROM-skein_create-RESPONSE>", newID)
		}
	}
	closeReq := mcpgo.CallToolRequest{}
	closeReq.Params.Arguments = closeArgs
	closeRes, err := handleClose(pool, iStore)(ctx, closeReq)
	if err != nil {
		t.Fatalf("handleClose: %v", err)
	}
	if closeRes.IsError {
		t.Fatalf("the prepared skein_close refused: %+v", extractToolJSON(t, closeRes))
	}
	closeBody := extractToolJSON(t, closeRes)
	if got, _ := closeBody["closeOutcome"].(string); got != issues.CloseOutcomeSuperseded {
		t.Errorf("after the run: closeOutcome = %q, expected superseded", got)
	}
	// Step 1 already made the supersedes edge, so the close finds it.
	if got := closeBody["closeLink"]; got != "existed" {
		t.Errorf("closeLink = %#v, want \"existed\" — the create step made the edge", got)
	}

	var status, outcome string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, close_outcome FROM issues WHERE id = $1`, issueID).
		Scan(&status, &outcome); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if status != "closed" || outcome != issues.CloseOutcomeSuperseded {
		t.Errorf("source issue: %s/%s, expected closed/superseded", status, outcome)
	}

	// The edge is exactly one, and it was put by the CREATE step.
	rows, err := pool.Query(context.Background(), `
SELECT from_id || '|' || type || '|' || to_id
  FROM dependencies WHERE from_id = $1 OR to_id = $1 ORDER BY 1`, issueID)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	defer rows.Close()
	var edges []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		edges = append(edges, e)
	}
	want := newID + "|supersedes|" + issueID
	if len(edges) != 1 || edges[0] != want {
		t.Fatalf("edges = %v, expected exactly [%s]", edges, want)
	}
}
