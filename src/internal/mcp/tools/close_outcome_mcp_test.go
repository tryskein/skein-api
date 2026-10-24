package tools

// close_outcome_mcp_test.go — sk-489, T008, matrix rows 28 and 65.
//
// # Two surfaces, two input builders, two rows
//
//	skein_close       → close.go#handleClose         (row 28)
//	skein_close_many  → close_many.go#parseCloseManyItem (row 65)
//
// `handleCloseMany` does not call `handleClose` EVEN ONCE — the calls to `handleClose` in
// the package are the tool registration and the tests of the single surface — so
// the row 28 assertions do not cover a batch position. The rows cannot be merged
// for a second reason too: two implementations, each green on
// row 28, are caught only by row 65.
//
//	(1) the target checks live only in handleClose — on a batch position
//	    the formal refusal is raised by the writer, and this is caught by the detail
//	    `details.id`, NOT by the code (see below);
//	(2) the target checks are moved BEFORE the loop — one self-target would fail the whole call,
//	    which violates US-1 "in batches — the refusal of this position".
//
// ⚠ Item (1) used to read "the refusal reaches
// close_errors.go#classifyCloseError and becomes internal_error". This is WRONG
// for the four formal cells, and it is measured (review A/4): they are raised by
// issues.DecideCloseOutcome as *ValidationError, and classifyCloseError
// catches any ValidationError with its first branch — before the fall-through. The full
// explanation and how these two places still differ on each of the two
// surfaces are in the header of close_outcome_arg.go.
//
// The contrast with the REST batch (row 26, handler/bulk_close_outcome_test.go)
// is deliberate and checked by both rows: there the target refusal is a BATCH one,
// here it is a PER-POSITION one. Surfaces with a different batch contract, not one
// rule.
//
// # Why the main caller is not an administrator, and why ONE probe
// needs exactly an administrator
//
// The target refusal cells run under the role "user", for the same reason as in REST
// (handler/issues_close_outcome_test.go): for an administrator the horizon is all
// projects, and "a target in another visible project" merges with "a target in a foreign one"
// on him. The fixture therefore holds THREE projects.
//
// But a non-admin has a blind spot of its own, and it is measured (wave A/4 review, F-2).
// Target visibility is checked by the SYSTEM role (issues.DependenciesCaller),
// not by Actor.Role ("agent" on MCP, "human" on REST). For a caller with the
// system role "user" both keys give ONE AND THE SAME answer on any target:
// projects/access.go#EnsureProjectVisible counts membership for any
// non-admin role. So on a non-admin, substituting the system role with Actor.Role
// is INVISIBLE, and the previous edition of this header claimed the opposite.
//
// It is told apart exactly by an administrator: his system role opens all
// projects, Actor.Role — only those he is a member of. Therefore
// #TestCloseMCP_TargetVisibilityUsesTheSystemRole runs under the role "admin" and
// aims at a project where the admin is NOT, and the reference is taken by running a manual link
// with the same caller (US-2: "the refusal code is the same as for this same surface
// on manual link creation").

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// ---------------- fixture ----------------

type comEnv struct {
	pool   *pgxpool.Pool
	store  *issues.Store
	ctx    context.Context
	userID uuid.UUID

	pid    uuid.UUID
	prefix string

	pidOther    uuid.UUID // the second VISIBLE project — a cross-project target
	prefixOther string

	pidHidden    uuid.UUID // a foreign project — an invisible target
	prefixHidden string

	// An administrator who is NOT a member of any of the three projects, and his
	// context. Needed for exactly one probe — see
	// #TestCloseMCP_TargetVisibilityUsesTheSystemRole: substituting the system
	// role with Actor.Role is visible ONLY on him.
	adminID  uuid.UUID
	adminCtx context.Context
}

func comSetup(t *testing.T) *comEnv {
	t.Helper()
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	adminID, _ := seedUser(t, pool, "admin")

	pid, prefix := seedProject(t, pool, userID, "como")
	pidOther, prefixOther := seedProject(t, pool, userID, "comox")
	pidHidden, prefixHidden := seedProject(t, pool, stranger, "comoh")

	return &comEnv{
		pool:         pool,
		store:        issues.NewStore(pool),
		ctx:          withCtxSession(t, userID, "user", "sess-como"),
		userID:       userID,
		pid:          pid,
		prefix:       prefix,
		pidOther:     pidOther,
		prefixOther:  prefixOther,
		pidHidden:    pidHidden,
		prefixHidden: prefixHidden,
		adminID:      adminID,
		adminCtx:     withCtxSession(t, adminID, "admin", "sess-como-adm"),
	}
}

// comAdminWritesInPid makes the administrator a member of env.pid: by the
// action table of US-6 (sk-674.18 T066) an admin beyond his issue role does not write, and
// a manual link from env.pid requires his own membership there. The target in
// env.pidHidden stays outside his membership — the discriminating case "target visibility
// by the system role" does not change.
func comAdminWritesInPid(t *testing.T, env *comEnv) {
	t.Helper()
	testsupport.AddProjectMember(t, env.pool, env.pid, env.adminID, "member")
}

func comSeed(t *testing.T, env *comEnv, pid uuid.UUID, prefix, suffix string) string {
	t.Helper()
	id := prefix + "-" + suffix
	seedIssue(t, env.pool, pid, env.userID, id, "close-outcome probe "+suffix, "open", 2)
	return id
}

// comState — the state of an issue row, everything a close can change.
type comState struct {
	Version int
	Status  string
	Outcome string
}

func comRead(t *testing.T, env *comEnv, id string) comState {
	t.Helper()
	var s comState
	var outcome *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT version, status, close_outcome FROM issues WHERE id = $1`, id).
		Scan(&s.Version, &s.Status, &outcome); err != nil {
		t.Fatalf("read issue %s: %v", id, err)
	}
	if outcome != nil {
		s.Outcome = *outcome
	}
	return s
}

// comEdges — all edges touching the named issues, "from|type|to".
func comEdges(t *testing.T, env *comEnv, ids ...string) []string {
	t.Helper()
	rows, err := env.pool.Query(context.Background(), `
SELECT from_id || '|' || type || '|' || to_id
  FROM dependencies
 WHERE from_id = ANY($1) OR to_id = ANY($1)
 ORDER BY 1`, ids)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func comClose(t *testing.T, env *comEnv, args map[string]any) map[string]any {
	t.Helper()
	return comCloseAs(t, env, env.ctx, args)
}

// comCloseAs — the same call from an arbitrary caller. A separate form
// is needed for the system-role probe: all other cases of the file run under "user", and
// the discriminating case exists only under "admin".
func comCloseAs(t *testing.T, env *comEnv, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handleClose(env.pool, env.store)(ctx, req)
	if err != nil {
		t.Fatalf("handleClose: %v", err)
	}
	return extractToolJSON(t, res)
}

func comCloseMany(t *testing.T, env *comEnv, items []any) map[string]any {
	t.Helper()
	return comCloseManyAs(t, env, env.ctx, items)
}

func comCloseManyAs(t *testing.T, env *comEnv, ctx context.Context, items []any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}
	res, err := handleCloseMany(env.pool, env.store)(ctx, req)
	if err != nil {
		t.Fatalf("handleCloseMany: %v", err)
	}
	if res.IsError {
		t.Fatalf("skein_close_many answered with a TOOL error, not per-row results: %+v",
			extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

// comDepAdd — a manual link of the same type to the same target: the REFERENCE of the refusal code,
// taken by running it, not written as a literal.
func comDepAdd(t *testing.T, env *comEnv, fromID, toID string) map[string]any {
	t.Helper()
	return comDepAddAs(t, env, env.ctx, fromID, toID)
}

func comDepAddAs(t *testing.T, env *comEnv, ctx context.Context, fromID, toID string) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"fromId": fromID, "toId": toID, "type": "duplicates",
	}
	res, err := handleDepAdd(env.pool, dependencies.NewStore(env.pool), env.store)(ctx, req)
	if err != nil {
		t.Fatalf("handleDepAdd: %v", err)
	}
	return extractToolJSON(t, res)
}

func comErr(body map[string]any) string {
	code, _ := body["error"].(string)
	return code
}

func comRow(t *testing.T, body map[string]any, i int) map[string]any {
	t.Helper()
	results, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("results missing or not an array: %+v", body)
	}
	if i >= len(results) {
		t.Fatalf("results has %d rows, wanted index %d: %+v", len(results), i, body)
	}
	row, ok := results[i].(map[string]any)
	if !ok {
		t.Fatalf("results[%d] is %T, want an object", i, results[i])
	}
	return row
}

func comRowErr(t *testing.T, row map[string]any) string {
	t.Helper()
	e, ok := row["error"].(map[string]any)
	if !ok {
		t.Fatalf("row has no structured error: %+v", row)
	}
	code, _ := e["code"].(string)
	return code
}

// comRowErrDetails — the detail of a structured position error.
func comRowErrDetails(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	e, ok := row["error"].(map[string]any)
	if !ok {
		t.Fatalf("row has no structured error: %+v", row)
	}
	d, _ := e["details"].(map[string]any)
	return d
}

// ---------------- row 28: skein_close ----------------

// TestCloseMCP_OutcomeValueAndWarning — the outcome value and US-9 on the single
// surface.
func TestCloseMCP_OutcomeValueAndWarning(t *testing.T) {
	env := comSetup(t)

	// sk-540.1: the MCP transition no longer defaults to done (a close
	// without an outcome is refused — close_outcome_required_mcp_test.go),
	// so this case passes done explicitly; what it guards is unchanged.
	t.Run("explicit done, the assignee warning stays", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "1")
		body := comClose(t, env, map[string]any{
			"id": id, "expectedVersion": comRead(t, env, id).Version, "reason": "r",
			"closeOutcome": "done"})
		if got, _ := body["closeOutcome"].(string); got != "done" {
			t.Errorf("closeOutcome = %q, expected done (%+v)", got, body)
		}
		if _, present := body["closeLink"]; present {
			t.Errorf("without a target there must be no closeLink key: %+v", body)
		}
		// The warning IS there — otherwise the assertion of the subtest below ("with an
		// outcome ≠ done there is none") would prove only that the warning
		// never exists.
		if _, present := body["warning"]; !present {
			t.Errorf("a close WITHOUT an assignee with the done outcome must carry "+
				"a warning: %+v", body)
		}
	})

	t.Run("wont_do → wont_do and WITHOUT the assignee warning", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "2")
		body := comClose(t, env, map[string]any{
			"id": id, "expectedVersion": comRead(t, env, id).Version,
			"reason": "r", "closeOutcome": "wont_do"})
		if got, _ := body["closeOutcome"].(string); got != "wont_do" {
			t.Errorf("closeOutcome = %q, expected wont_do (%+v)", got, body)
		}
		if w, present := body["warning"]; present {
			t.Errorf("US-9: with an outcome ≠ done the warning \"closed with no assignee\" "+
				"is not issued, but one came: %v", w)
		}
	})

	t.Run("bogus → validation_error, the issue is unchanged", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "3")
		before := comRead(t, env, id)
		body := comClose(t, env, map[string]any{
			"id": id, "expectedVersion": before.Version,
			"reason": "r", "closeOutcome": "bogus"})
		if comErr(body) != ErrCodeValidationError {
			t.Fatalf("code = %q, expected validation_error (%+v)", comErr(body), body)
		}
		if after := comRead(t, env, id); after != before {
			t.Errorf("the rejected call changed the issue: was %+v, now %+v", before, after)
		}
	})
}

// TestCloseMCP_FormalRefusalPrecedesTheStore — ORDER: the formal refusal
// on the outcome and target is raised BEFORE the store is accessed.
//
// # Why a separate assertion if the codes are already checked by neighbouring tests
//
// Because the codes do NOT check this, and it is measured (wave A/4 review,
// finding 1). The same four formal refusals are raised by
// issues.DecideCloseOutcome INSIDE the store transaction, and
// close_errors.go#classifyCloseError maps any ValidationError to
// validation_error with its first branch — with the same Details{"code": …}. So
// an implementation that PARSES the arguments but does not check them (the helper
// close_outcome_arg.go#parseCloseOutcomeArgs removed while the parsing is left)
// answers byte for byte the same, and the whole package stays green.
//
// The batch surface has a discriminating sign — `details.id`, which
// only the input builder puts in (see the doc block
// #TestCloseManyMCP_TargetRefusalsAreRowFailures). The single one has nowhere to get it:
// the detail is the same on both sides.
//
// ORDER discriminates. parseCloseOutcomeArgs stands in handleClose BEFORE
// GetIssue and, even more so, before the version and lock predicates inside the writer.
// So a call that is WRONG BOTH in the formal argument AND in something
// only the database knows about must answer about the argument. Remove the
// check — and the answer becomes what the database found: version_conflict or
// issue_not_found. This is exactly what is asserted here.
//
// To row 28 of the matrix this adds a limit, not strength: the single surface
// tells "a check before the store" from "no check" only this way.
func TestCloseMCP_FormalRefusalPrecedesTheStore(t *testing.T) {
	env := comSetup(t)
	other := comSeed(t, env, env.pidOther, env.prefixOther, "orddst")

	cases := []struct {
		name string
		// args builds the call from the issue id and its real version.
		args func(id string, version int) map[string]any
		// beaten — the code the call would answer with if the formal
		// check stood AFTER the store access.
		beaten string
		// hitsSeeded — the call is addressed to the seeded issue. For the cell
		// "nonexistent issue" it is false, and the state there is not
		// compared: comparing it for an issue the call did not name
		// would hold an assertion that is green under any implementation.
		hitsSeeded bool
	}{
		{
			name: "stale version + target with done → about the argument, not the version",
			args: func(id string, v int) map[string]any {
				return map[string]any{"id": id, "expectedVersion": v + 7, "reason": "r",
					"closeOutcome": "done", "closeTarget": other}
			},
			beaten: ErrCodeVersionConflict, hitsSeeded: true,
		},
		{
			name: "stale version + target = the issue itself → about the argument, not the version",
			args: func(id string, v int) map[string]any {
				return map[string]any{"id": id, "expectedVersion": v + 7, "reason": "r",
					"closeOutcome": "duplicate", "closeTarget": id}
			},
			beaten: ErrCodeVersionConflict, hitsSeeded: true,
		},
		{
			name: "nonexistent issue + target without an outcome → about the argument, not the issue",
			args: func(id string, v int) map[string]any {
				return map[string]any{"id": env.prefix + "-99999", "expectedVersion": 1,
					"reason": "r", "closeTarget": other}
			},
			beaten: ErrCodeIssueNotFound,
		},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := comSeed(t, env, env.pid, env.prefix, fmt.Sprintf("ord%d", i))
			before := comRead(t, env, id)

			body := comClose(t, env, c.args(id, before.Version))

			if got := comErr(body); got != ErrCodeValidationError {
				t.Fatalf("code = %q, expected validation_error. This is the STORE's answer "+
					"(expected here only when the check is absent — %s): so "+
					"handleClose PARSED the arguments but did not CHECK them, and the refusal was raised by "+
					"the writer, not the input builder (%+v)", got, c.beaten, body)
			}
			if c.hitsSeeded {
				if after := comRead(t, env, id); after != before {
					t.Errorf("the rejected call changed the issue: was %+v, now %+v",
						before, after)
				}
				if edges := comEdges(t, env, id); len(edges) != 0 {
					t.Errorf("the rejected call created edges: %v", edges)
				}
			}
		})
	}
}

// TestCloseMCP_TargetVisibilityUsesTheSystemRole — the target visibility key is the
// SYSTEM role, not Actor.Role, on both closing MCP surfaces.
//
// # Why this is not checked by any cell above
//
// All of them run under the system role "user", and for a non-admin role
// projects/access.go#EnsureProjectVisible counts membership the same way — both
// "user" and "agent" give one answer on any target. Substituting
// issues.DependenciesCaller.Role with actor.role is INVISIBLE there: measured
// by mutation, the full gate T007+T008+T009 stayed green (review A/4,
// F-2).
//
// There is exactly one discriminating case: an ADMINISTRATOR and a target in a project where he
// is NOT. The system role "admin" opens such a target, Actor.Role "agent" does
// not. The reference is taken by running a manual link with the same caller, not
// as a literal: US-2 promises "the same code as for this same surface on
// manual link creation", and the assertion must go red on a joint
// drift of both paths too.
func TestCloseMCP_TargetVisibilityUsesTheSystemRole(t *testing.T) {
	env := comSetup(t)
	// The target is in a project where the administrator is not.
	target := comSeed(t, env, env.pidHidden, env.prefixHidden, "admtgt")

	// Reference: the admin's manual link to the same target. If it was refused too,
	// the probe would be green even with a lost role — there would be nothing to
	// compare, so "the reference is accepted" is checked FIRST.
	comAdminWritesInPid(t, env)
	scratch := comSeed(t, env, env.pid, env.prefix, "admscr")
	manual := comDepAddAs(t, env, env.adminCtx, scratch, target)
	if code := comErr(manual); code != "" {
		t.Fatalf("the fixture does not reach the discriminating case: the admin's manual link "+
			"to a target outside his membership was refused (%s, %+v)", code, manual)
	}

	assertCreated := func(t *testing.T, id string, body map[string]any, link string) {
		t.Helper()
		if link != string(issues.CloseLinkCreated) {
			t.Fatalf("the admin was rejected on a target that a manual link by the same "+
				"caller ACCEPTED: %+v.\nUS-2 requires the close path to have the same refusal "+
				"contract as skein_dep_add. Target visibility was read "+
				"not by the SYSTEM role (issues.DependenciesCaller.Role) but by "+
				"Actor.Role — on a non-admin these two keys are indistinguishable, here "+
				"they diverge", body)
		}
		want := id + "|duplicates|" + target
		if got := comEdges(t, env, id); len(got) != 1 || got[0] != want {
			t.Fatalf("edges of the closed issue = %v, expected exactly [%s]", got, want)
		}
	}

	t.Run("skein_close", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "adm1")
		body := comCloseAs(t, env, env.adminCtx, map[string]any{
			"id": id, "expectedVersion": comRead(t, env, id).Version, "reason": "r",
			"closeOutcome": "duplicate", "closeTarget": target})
		link, _ := body["closeLink"].(string)
		assertCreated(t, id, body, link)
	})

	t.Run("skein_close_many", func(t *testing.T) {
		id := comSeed(t, env, env.pid, env.prefix, "adm2")
		body := comCloseManyAs(t, env, env.adminCtx, []any{
			map[string]any{"id": id, "expectedVersion": float64(comRead(t, env, id).Version),
				"reason": "r", "closeOutcome": "duplicate", "closeTarget": target}})
		row := comRow(t, body, 0)
		link, _ := row["closeLink"].(string)
		assertCreated(t, id, row, link)
	})
}

// comLinkCases — the table "outcome → expected edge", shared by both
// surfaces: the link rule is one, the surfaces are two.
func comLinkCases() []struct {
	name    string
	outcome string
	edge    func(issueID, targetID string) string
} {
	return []struct {
		name    string
		outcome string
		edge    func(issueID, targetID string) string
	}{
		{"duplicate → duplicates A→B", issues.CloseOutcomeDuplicate,
			func(a, b string) string { return a + "|duplicates|" + b }},
		// The direction is reversed, and this is not a typo: "superseded" reads
		// "the target supersedes the closed one", exactly like a manual link.
		{"superseded → supersedes B→A", issues.CloseOutcomeSuperseded,
			func(a, b string) string { return b + "|supersedes|" + a }},
	}
}

// TestCloseMCP_LinkCreatedThenExisted — the link flag and the uniqueness of the edge.
func TestCloseMCP_LinkCreatedThenExisted(t *testing.T) {
	env := comSetup(t)

	for i, c := range comLinkCases() {
		t.Run(c.name, func(t *testing.T) {
			id := comSeed(t, env, env.pid, env.prefix, fmt.Sprintf("link%d", i))
			// The target is in ANOTHER visible project: a cross-project target is allowed by
			// US-2, and for a non-admin it is a separate cell.
			target := comSeed(t, env, env.pidOther, env.prefixOther, fmt.Sprintf("link%dt", i))
			want := c.edge(id, target)

			body := comClose(t, env, map[string]any{
				"id": id, "expectedVersion": comRead(t, env, id).Version,
				"reason": "r", "closeOutcome": c.outcome, "closeTarget": target})
			if got, _ := body["closeLink"].(string); got != string(issues.CloseLinkCreated) {
				t.Fatalf("first call: closeLink = %q, expected created (%+v)", got, body)
			}
			if got := comEdges(t, env, id, target); len(got) != 1 || got[0] != want {
				t.Fatalf("edges = %v, expected exactly [%s]", got, want)
			}

			body = comClose(t, env, map[string]any{
				"id": id, "expectedVersion": comRead(t, env, id).Version,
				"reason": "r2", "closeOutcome": c.outcome, "closeTarget": target})
			if got, _ := body["closeLink"].(string); got != string(issues.CloseLinkExisted) {
				t.Fatalf("repeat: closeLink = %q, expected existed (%+v)", got, body)
			}
			if got := comEdges(t, env, id, target); len(got) != 1 || got[0] != want {
				t.Fatalf("edges after the repeat = %v, expected still exactly [%s]", got, want)
			}
		})
	}
}

// comTargetCase — one target refusal cell. ONE table for both
// surfaces: the set of cells and their codes must match, and a divergence of the
// set is itself a defect.
type comTargetCase struct {
	name string
	// outcome nil = the outcome is not passed at all.
	outcome *string
	// target: "" substitutes the id of the issue itself.
	target   string
	wantCode string
}

func comStr(s string) *string { return &s }

func comTargetCases(invisible, missing string) []comTargetCase {
	return []comTargetCase{
		{"target = the issue itself", comStr("duplicate"), "", ErrCodeValidationError},
		{"target without an outcome", nil, invisible, ErrCodeValidationError},
		{"target with done", comStr("done"), invisible, ErrCodeValidationError},
		{"target with wont_do", comStr("wont_do"), invisible, ErrCodeValidationError},
		// Two cells, ONE code (INV-3, sk-674.18 T033): an invisible target
		// is indistinguishable from a nonexistent one, as in REST.
		{"target invisible", comStr("duplicate"), invisible, ErrCodeIssueNotFound},
		{"target does not exist", comStr("duplicate"), missing, ErrCodeIssueNotFound},
	}
}

// TestCloseMCP_TargetRefusals — six cells on the single surface.
func TestCloseMCP_TargetRefusals(t *testing.T) {
	env := comSetup(t)
	invisible := comSeed(t, env, env.pidHidden, env.prefixHidden, "inv")
	missing := env.prefix + "-99999"
	scratch := comSeed(t, env, env.pid, env.prefix, "scratch")

	for i, c := range comTargetCases(invisible, missing) {
		t.Run(c.name, func(t *testing.T) {
			id := comSeed(t, env, env.pid, env.prefix, fmt.Sprintf("ref%d", i))
			target := c.target
			if target == "" {
				target = id
			}
			before := comRead(t, env, id)

			args := map[string]any{
				"id": id, "expectedVersion": before.Version, "reason": "r",
				"closeTarget": target,
			}
			if c.outcome != nil {
				args["closeOutcome"] = *c.outcome
			}
			body := comClose(t, env, args)

			if comErr(body) != c.wantCode {
				t.Fatalf("code = %q, expected %s (%+v)", comErr(body), c.wantCode, body)
			}
			// The fall-through guard. It is about the TWO target cells (does not
			// exist / is not visible): the store raises them not as a
			// ValidationError, and without an explicit branch in
			// close_errors.go#classifyCloseTargetError they would fall into
			// internal_error. It proves nothing for the four formal cells —
			// they are ValidationError and give validation_error at any place of the check —
			// but it does not become false: they do not fall into
			// internal_error anyway.
			if comErr(body) == ErrCodeInternalError {
				t.Fatalf("the refusal fell into internal_error — the target error mapping " +
					"is absent")
			}
			if after := comRead(t, env, id); after != before {
				t.Errorf("the rejected call changed the issue: was %+v, now %+v", before, after)
			}
			if edges := comEdges(t, env, id, target); len(edges) != 0 {
				t.Errorf("the rejected call created edges: %v", edges)
			}
		})
	}

	// The same codes as for a manual link to the same target — the reference is taken by
	// running it, otherwise the test would agree with a joint drift of both paths.
	for _, target := range []string{invisible, missing} {
		t.Run("the same code as skein_dep_add on "+target, func(t *testing.T) {
			manual := comDepAdd(t, env, scratch, target)
			id := comSeed(t, env, env.pid, env.prefix, "oracle-"+target)
			closing := comClose(t, env, map[string]any{
				"id": id, "expectedVersion": comRead(t, env, id).Version, "reason": "r",
				"closeOutcome": "duplicate", "closeTarget": target})
			if comErr(manual) != comErr(closing) {
				t.Fatalf("the close path answers differently from a manual link to the same target:\n"+
					"  skein_dep_add = %q\n  skein_close   = %q",
					comErr(manual), comErr(closing))
			}
		})
	}
}

// ---------------- row 65: skein_close_many ----------------

// TestCloseManyMCP_PerRowOutcomeAndLink — the outcome and the link flag PER ROW.
//
// One call, with a position that creates a link and a position whose link
// already exists. A flag per batch instead of per row would have to pick one of two
// answers and lie about the other; here both are observed in one answer.
func TestCloseManyMCP_PerRowOutcomeAndLink(t *testing.T) {
	env := comSetup(t)
	target := comSeed(t, env, env.pidOther, env.prefixOther, "mt")
	// A separate target for the `superseded` position: its direction is reversed,
	// and its own target keeps it from mixing with the `duplicates` edges below.
	targetSup := comSeed(t, env, env.pidOther, env.prefixOther, "mts")

	created := comSeed(t, env, env.pid, env.prefix, "m1")
	existed := comSeed(t, env, env.pid, env.prefix, "m2")
	// existed already carries an edge — set by a manual link, not by a close:
	// the fixture must not depend on the tool it is checking.
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, created_by, created_at)
VALUES ($1, $2, 'duplicates', $3, now())`, existed, target, env.userID); err != nil {
		t.Fatalf("seed existing edge: %v", err)
	}
	wontDo := comSeed(t, env, env.pid, env.prefix, "m3")
	deflt := comSeed(t, env, env.pid, env.prefix, "m4")
	// sk-489, repair after review A/4: the row 65 cell "supersedes B→A on
	// superseded" was not executed by ANYTHING so far. `superseded` in all
	// four files that call handleCloseMany occurred once — in the
	// comLinkCases table, which is read only by the single
	// #TestCloseMCP_LinkCreatedThenExisted. The link rule is one for two
	// surfaces, but it must not be proved on one value of two:
	// exactly the shape of the hole the wave A/3 lesson warned about.
	superseded := comSeed(t, env, env.pid, env.prefix, "m5")

	body := comCloseMany(t, env, []any{
		map[string]any{"id": created, "expectedVersion": float64(comRead(t, env, created).Version),
			"reason": "r", "closeOutcome": "duplicate", "closeTarget": target},
		map[string]any{"id": existed, "expectedVersion": float64(comRead(t, env, existed).Version),
			"reason": "r", "closeOutcome": "duplicate", "closeTarget": target},
		map[string]any{"id": wontDo, "expectedVersion": float64(comRead(t, env, wontDo).Version),
			"reason": "r", "closeOutcome": "wont_do"},
		// sk-540.1: explicit done — an MCP transition has no default any more.
		map[string]any{"id": deflt, "expectedVersion": float64(comRead(t, env, deflt).Version),
			"reason": "r", "closeOutcome": "done"},
		map[string]any{"id": superseded, "expectedVersion": float64(comRead(t, env, superseded).Version),
			"reason": "r", "closeOutcome": "superseded", "closeTarget": targetSup},
	})
	assertCloseManyWireForm(t, body, 5)

	want := []struct {
		id      string
		outcome string
		link    string
	}{
		{created, "duplicate", "created"},
		{existed, "duplicate", "existed"},
		{wontDo, "wont_do", ""},
		{deflt, "done", ""},
		{superseded, "superseded", "created"},
	}
	for i, w := range want {
		row := comRow(t, body, i)
		if row["ok"] != true {
			t.Fatalf("results[%d] is not successful: %+v", i, row)
		}
		if got, _ := row["closeOutcome"].(string); got != w.outcome {
			t.Errorf("results[%d].closeOutcome = %q, expected %q", i, got, w.outcome)
		}
		link, present := row["closeLink"]
		if w.link == "" {
			if present {
				t.Errorf("results[%d] carries closeLink = %v, though it named no target", i, link)
			}
			continue
		}
		if got, _ := link.(string); got != w.link {
			t.Errorf("results[%d].closeLink = %q, expected %q — the link flag must be "+
				"PER ROW: in one call one position creates an edge, another finds "+
				"it in place", i, got, w.link)
		}
	}

	// Exactly one edge per position, both to one target.
	wantEdges := []string{created + "|duplicates|" + target, existed + "|duplicates|" + target}
	sort.Strings(wantEdges)
	got := comEdges(t, env, created, existed, target)
	if len(got) != 2 || got[0] != wantEdges[0] || got[1] != wantEdges[1] {
		t.Fatalf("edges = %v, expected exactly %v", got, wantEdges)
	}

	// The direction of `superseded` is REVERSED, and this is not a typo: "superseded"
	// reads "the target supersedes the closed one", exactly like a manual link. A batch
	// position must put the same edge as the single surface.
	wantSup := targetSup + "|supersedes|" + superseded
	if got := comEdges(t, env, superseded, targetSup); len(got) != 1 || got[0] != wantSup {
		t.Fatalf("edges of the superseded position = %v, expected exactly [%s]", got, wantSup)
	}
}

// TestCloseManyMCP_BadOutcomeIsARowFailure — "bogus" on a position.
func TestCloseManyMCP_BadOutcomeIsARowFailure(t *testing.T) {
	env := comSetup(t)
	bad := comSeed(t, env, env.pid, env.prefix, "b1")
	good := comSeed(t, env, env.pid, env.prefix, "b2")
	before := comRead(t, env, bad)

	body := comCloseMany(t, env, []any{
		map[string]any{"id": bad, "expectedVersion": float64(before.Version),
			"reason": "r", "closeOutcome": "bogus"},
		map[string]any{"id": good, "expectedVersion": float64(comRead(t, env, good).Version),
			"reason": "r", "closeOutcome": "done"},
	})
	if code := comRowErr(t, comRow(t, body, 0)); code != ErrCodeValidationError {
		t.Errorf("results[0].error.code = %q, expected validation_error", code)
	}
	if after := comRead(t, env, bad); after != before {
		t.Errorf("the rejected position was changed: was %+v, now %+v", before, after)
	}
	if comRow(t, body, 1)["ok"] != true {
		t.Errorf("the other positions must be closed: %+v", comRow(t, body, 1))
	}
}

// TestCloseManyMCP_TargetRefusalsAreRowFailures — THE SAME six cells as the
// single surface, and each is a refusal of a POSITION.
//
// This is the discriminating row of the wave. It goes red on two
// implementations, each of which is green by CODES on row 28:
//
//	(1) the checks only in handleClose → here a refusal without `details.id`;
//	(2) the checks moved before the loop → here the whole call fails.
//
// Item (1) used to read "here internal_error" — wrong, see the file header.
//
// All cells go in ONE call, and in it also a position that creates an edge:
// "the other positions are closed" is not asserted otherwise.
func TestCloseManyMCP_TargetRefusalsAreRowFailures(t *testing.T) {
	env := comSetup(t)
	invisible := comSeed(t, env, env.pidHidden, env.prefixHidden, "binv")
	missing := env.prefix + "-99999"
	target := comSeed(t, env, env.pidOther, env.prefixOther, "bt")

	cases := comTargetCases(invisible, missing)
	items := make([]any, 0, len(cases)+1)
	ids := make([]string, 0, len(cases))
	before := map[string]comState{}
	for i, c := range cases {
		id := comSeed(t, env, env.pid, env.prefix, fmt.Sprintf("bref%d", i))
		ids = append(ids, id)
		before[id] = comRead(t, env, id)
		tgt := c.target
		if tgt == "" {
			tgt = id
		}
		item := map[string]any{
			"id": id, "expectedVersion": float64(before[id].Version),
			"reason": "r", "closeTarget": tgt,
		}
		if c.outcome != nil {
			item["closeOutcome"] = *c.outcome
		}
		items = append(items, item)
	}
	// A valid position IN THE SAME call: it proves a partial success.
	ok := comSeed(t, env, env.pid, env.prefix, "bok")
	items = append(items, map[string]any{
		"id": ok, "expectedVersion": float64(comRead(t, env, ok).Version),
		"reason": "r", "closeOutcome": "duplicate", "closeTarget": target})

	body := comCloseMany(t, env, items)
	assertCloseManyWireForm(t, body, len(items))

	for i, c := range cases {
		row := comRow(t, body, i)
		if row["ok"] != false {
			t.Errorf("%s: the position passed, but had to be refused: %+v", c.name, row)
			continue
		}
		code := comRowErr(t, row)
		if code == ErrCodeInternalError {
			t.Errorf("%s: the position code is internal_error — the refusal reached "+
				"classifyCloseError without an explicit mapping, i.e. the target check "+
				"lives only in the single input builder", c.name)
			continue
		}
		if code != c.wantCode {
			t.Errorf("%s: position code = %q, expected %s", c.name, code, c.wantCode)
		}
		// ⚠ WHERE EXACTLY the position was refused — and why this has to be
		// asserted separately.
		//
		// The code alone does NOT tell the two places of the check apart: the same four
		// formal refusals are raised by issues.DecideCloseOutcome inside the
		// store transaction, and classifyCloseError maps any
		// ValidationError to validation_error. Measured by mutation: removing
		// parseCloseOutcomeArgs from the batch builder entirely — and
		// a check by codes alone stays GREEN.
		//
		// The detail tells them apart. The batch builder attaches the position `id`
		// to the error (close_many.go#parseCloseManyItem), so that in an answer
		// over a hundred positions it is visible which one was refused; the path through
		// the store does not carry this key — classifyCloseError puts only `code` into
		// details. So `details.id` is an observable sign that the refusal
		// happened BEFORE the transaction opened, in the SECOND input builder, and did not
		// reach the writer.
		//
		// On the two target cells (invisible / does not exist) the details are different —
		// they are put by classifyCloseError under the key `closeTarget`, because
		// they cannot be learned before the transaction — and they are checked
		// below by their own assertion.
		if c.wantCode == ErrCodeValidationError {
			details := comRowErrDetails(t, row)
			if got, _ := details["id"].(string); got != ids[i] {
				t.Errorf("%s: details.id = %v, expected %q. A formal refusal must "+
					"be raised by the batch input builder BEFORE the store; without "+
					"`id` it came from the writer, i.e. close_many.go parses "+
					"the outcome and target but does not check them", c.name, details["id"], ids[i])
			}
		} else if details := comRowErrDetails(t, row); details["closeTarget"] == nil {
			t.Errorf("%s: the target refusal does not carry details.closeTarget — the caller "+
				"will not learn WHICH of its arguments did not resolve", c.name)
		}
		if after := comRead(t, env, ids[i]); after != before[ids[i]] {
			t.Errorf("%s: the refused position was changed: was %+v, now %+v",
				c.name, before[ids[i]], after)
		}
		if edges := comEdges(t, env, ids[i]); len(edges) != 0 {
			t.Errorf("%s: the refused position created edges: %v", c.name, edges)
		}
	}

	last := comRow(t, body, len(cases))
	if last["ok"] != true {
		t.Fatalf("the valid position was not executed — so the target refusal failed the WHOLE call, "+
			"while US-1 requires the refusal of this position: %+v", last)
	}
	if got, _ := last["closeLink"].(string); got != "created" {
		t.Errorf("valid position: closeLink = %q, expected created", got)
	}
	wantEdge := ok + "|duplicates|" + target
	if got := comEdges(t, env, ok, target); len(got) != 1 || got[0] != wantEdge {
		t.Fatalf("edges of the valid position = %v, expected exactly [%s]", got, wantEdge)
	}

	summary, isObj := body["summary"].(map[string]any)
	if !isObj {
		t.Fatalf("summary missing: %+v", body)
	}
	if n, _ := summary["succeeded"].(float64); int(n) != 1 {
		t.Errorf("summary.succeeded = %v, expected exactly one executed position", summary["succeeded"])
	}
	if n, _ := summary["failed"].(float64); int(n) != len(cases) {
		t.Errorf("summary.failed = %v, expected %d", summary["failed"], len(cases))
	}
}
