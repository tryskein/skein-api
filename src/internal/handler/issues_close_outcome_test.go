package handler

// issues_close_outcome_test.go — sk-489, T007, matrix rows 24 and 25.
//
// # Subject
//
// Two REST write surfaces, each with its own storage writer:
//
//	POST  /projects/{pid}/issues/{id}/transitions  → issues.Store.UpdateStatus
//	PATCH /projects/{pid}/issues/{id}              → issues.Store.PatchIssueWithResult
//
// What is checked is what must be the SAME on both (the outcome value, all
// seven target refusal cells, exactly one edge per target) and what must
// DIFFER (an outcome with a status other than "closed" — 400 on a transition; an outcome edit
// on a non-closed issue — 422 on an edit).
//
// # Why the main caller is NOT an administrator
//
// Target visibility is checked by the system role (issues.DependenciesCaller),
// and for an administrator the horizon is all projects, so a cross-project and
// an invisible target are two different cells only for a non-admin. That is why all
// refusal cells below run under the "user" role, and the fixture holds THREE projects: its own,
// a second visible one (a cross-project target is allowed by US-2) and a foreign one (the target
// is invisible). A non-admin catches, among other things, a role nailed to a literal
// "admin".
//
// # And why ONE probe needs an administrator
//
// The previous edition of this header claimed that an implementation that substituted
// Actor.Role ("human") for the system role "would behave indistinguishably from a correct one
// on an admin". The phrase is inverted, and this is measured (review of wave
// A/4, F-2): projects/access.go#EnsureProjectVisible counts membership
// the same for ANY non-admin role, so for a caller with the system
// role "user" the keys "user" and "human" give ONE AND THE SAME
// answer for any target — the substitution is invisible exactly there. It is visible for an administrator:
// the system role opens all projects to them, Actor.Role only those they
// are a member of.
//
// Hence #TestCloseOutcome_TargetVisibilityUsesTheSystemRole: the caller is
// "admin", the target is in a project where they are NOT a member, the reference is POST …/dependencies by the same
// caller.
//
// # Why each refusal has two assertions, not one
//
// A refusal code proves the request was rejected; it does NOT prove that nothing
// was written. A writer that managed to close the issue and failed on the edge also
// returns a refusal. So every negative cell also checks the state:
// version, status, outcome AND the absence of an edge. The second half is the one that
// goes red on an implementation that "answers correctly but has already written".

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// ---------------- fixture ----------------

// coEnv — three projects and one non-admin living in two of them.
type coEnv struct {
	pool   *pgxpool.Pool
	issues *IssuesHandler
	deps   *DependenciesHandler

	user uuid.UUID

	pid    uuid.UUID // project of the issues being closed
	prefix string

	pidOther    uuid.UUID // second VISIBLE project — cross-project target
	prefixOther string

	pidHidden    uuid.UUID // project the caller is not in — invisible target
	prefixHidden string

	// An administrator who is a member of none of the three projects. Needed by
	// a single probe — see the file header.
	admin uuid.UUID
}

func coSetup(t *testing.T) *coEnv {
	t.Helper()
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	stranger, _ := seedIMUser(t, pool, "user")
	admin, _ := seedIMUser(t, pool, "admin")

	pid, prefix := seedIMProject(t, pool, user)
	pidOther, prefixOther := seedIMProject(t, pool, user)
	pidHidden, prefixHidden := seedIMProject(t, pool, stranger)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	return &coEnv{
		pool:         pool,
		issues:       NewIssuesHandler(pool, projects.NewStore(pool), depsStore),
		deps:         NewDependenciesHandler(pool, depsStore, issuesStore),
		user:         user,
		pid:          pid,
		prefix:       prefix,
		pidOther:     pidOther,
		prefixOther:  prefixOther,
		pidHidden:    pidHidden,
		prefixHidden: prefixHidden,
		admin:        admin,
	}
}

// coAdminWritesInPid makes the administrator a member of env.pid: by the US-6
// action table (sk-674.18 T066) an admin does not write beyond their issue role, and
// a manual link from env.pid requires their own membership there. The target in
// env.pidHidden stays outside their membership — the distinguishing case "target visibility
// by the system role" does not change.
func coAdminWritesInPid(t *testing.T, env *coEnv) {
	t.Helper()
	testsupport.AddProjectMember(t, env.pool, env.pid, env.admin, "member")
}

// coSeed puts one open issue straight into the database — the fixture does not depend on
// the handler that it itself checks.
func coSeed(t *testing.T, env *coEnv, pid uuid.UUID, prefix, suffix string) string {
	t.Helper()
	id := prefix + "-" + suffix
	seedCmtIssue(t, env.pool, pid, env.user, id, "human")
	return id
}

// coState — everything a closing operation can change in an issue row.
type coState struct {
	Version int
	Status  string
	Outcome string // "" = column is NULL, i.e. the issue is not closed (INV-1)
}

func coRead(t *testing.T, env *coEnv, id string) coState {
	t.Helper()
	var s coState
	var outcome *string
	if err := env.pool.QueryRow(t.Context(),
		`SELECT version, status, close_outcome FROM issues WHERE id = $1`, id).
		Scan(&s.Version, &s.Status, &outcome); err != nil {
		t.Fatalf("read issue %s: %v", id, err)
	}
	if outcome != nil {
		s.Outcome = *outcome
	}
	return s
}

// coEdges returns all edges where either end is one of the named
// issues, as sorted "from|type|to".
//
// ALL of them, not "an edge of the needed type": the assertion "there is no edge" must
// go red both on an edge of the wrong type and on an edge of the wrong direction, otherwise an
// implementation that put supersedes instead of duplicates would pass.
func coEdges(t *testing.T, env *coEnv, ids ...string) []string {
	t.Helper()
	rows, err := env.pool.Query(t.Context(), `
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

func coTransition(t *testing.T, env *coEnv, id string, version int, body map[string]any) xpResponse {
	t.Helper()
	return coTransitionAs(t, env, env.user, "user", id, version, body)
}

// coTransitionAs / coPatchAs / coDepCreateAs — the same calls from an arbitrary
// caller. A separate form exists for the system role probe: all
// other cases of the file run under "user", and the distinguishing case exists only under
// "admin" (see the header).
func coTransitionAs(t *testing.T, env *coEnv, userID uuid.UUID, role, id string,
	version int, body map[string]any) xpResponse {
	t.Helper()
	req := xpRequest(t, http.MethodPost, strconv.Itoa(version), body, userID, role,
		"projectID", env.pid.String(), "issueID", id)
	return xpCallFull(t, env.issues.UpdateStatus, req)
}

func coPatch(t *testing.T, env *coEnv, id string, version int, body map[string]any) xpResponse {
	t.Helper()
	return coPatchAs(t, env, env.user, "user", id, version, body)
}

func coPatchAs(t *testing.T, env *coEnv, userID uuid.UUID, role, id string,
	version int, body map[string]any) xpResponse {
	t.Helper()
	req := xpRequest(t, http.MethodPatch, strconv.Itoa(version), body, userID, role,
		"projectID", env.pid.String(), "issueID", id)
	return xpCallFull(t, env.issues.Patch, req)
}

// coDepCreate — manual creation of a link of the same type to the same target. Needed as the
// REFERENCE of the refusal code: the assertion of rows 24–26 reads "the same code as
// POST …/dependencies", and the reference must be obtained by a run, not written as
// a literal — otherwise the test would agree with any joint drift.
func coDepCreate(t *testing.T, env *coEnv, fromID, toID string) xpResponse {
	t.Helper()
	return coDepCreateAs(t, env, env.user, "user", fromID, toID)
}

func coDepCreateAs(t *testing.T, env *coEnv, userID uuid.UUID, role, fromID, toID string) xpResponse {
	t.Helper()
	req := xpRequest(t, http.MethodPost, "",
		map[string]any{"toId": toID, "type": "duplicates"}, userID, role,
		"projectID", env.pid.String(), "issueID", fromID)
	return xpCallFull(t, env.deps.Create, req)
}

func coErrCode(res xpResponse) string {
	code, _ := res.Body["error"].(string)
	return code
}

// ---------------- row 24: transition ----------------

// TestCloseOutcome_Transition_ValueAndDefault — the outcome value on a transition.
func TestCloseOutcome_Transition_ValueAndDefault(t *testing.T) {
	env := coSetup(t)

	t.Run("explicit wont_do: 200, the card carries the outcome and does NOT carry closeLink", func(t *testing.T) {
		id := coSeed(t, env, env.pid, env.prefix, "t-wontdo")
		res := coTransition(t, env, id, coRead(t, env, id).Version,
			map[string]any{"status": "closed", "closeOutcome": "wont_do"})
		if res.Status != http.StatusOK {
			t.Fatalf("transition = %d, want 200 (body=%s)", res.Status, res.Raw)
		}
		if got, _ := res.Body["closeOutcome"].(string); got != "wont_do" {
			t.Errorf("response card: closeOutcome = %q, want wont_do (body=%s)", got, res.Raw)
		}
		// closeLink is a mark of an ACTION, not a field of the issue: it lives only in
		// MCP confirmations (T008). If it got into the REST card, the reader
		// would take it for a state the issue does not have.
		if _, present := res.Body["closeLink"]; present {
			t.Errorf("REST card carries closeLink — it is a mark of the operation, "+
				"not a field of the issue (body=%s)", res.Raw)
		}
		if got := coRead(t, env, id); got.Status != "closed" || got.Outcome != "wont_do" {
			t.Errorf("in the database %+v, want closed/wont_do", got)
		}
	})

	t.Run("without an outcome: the transition gives done", func(t *testing.T) {
		id := coSeed(t, env, env.pid, env.prefix, "t-default")
		res := coTransition(t, env, id, coRead(t, env, id).Version,
			map[string]any{"status": "closed"})
		if res.Status != http.StatusOK {
			t.Fatalf("transition = %d, want 200 (body=%s)", res.Status, res.Raw)
		}
		if got := coRead(t, env, id); got.Outcome != "done" {
			t.Errorf("outcome = %q, want done", got.Outcome)
		}
	})

	t.Run("bogus: 400 invalid_close_outcome, issue unchanged", func(t *testing.T) {
		id := coSeed(t, env, env.pid, env.prefix, "t-bogus")
		before := coRead(t, env, id)
		res := coTransition(t, env, id, before.Version,
			map[string]any{"status": "closed", "closeOutcome": "bogus"})
		if res.Status != http.StatusBadRequest || coErrCode(res) != "invalid_close_outcome" {
			t.Fatalf("transition = %d/%s, want 400/invalid_close_outcome (body=%s)",
				res.Status, coErrCode(res), res.Raw)
		}
		if after := coRead(t, env, id); after != before {
			t.Errorf("issue changed by a rejected request: was %+v, now %+v", before, after)
		}
	})

	t.Run("outcome with status=in_progress: 400 close_outcome_not_a_close", func(t *testing.T) {
		id := coSeed(t, env, env.pid, env.prefix, "t-notaclose")
		before := coRead(t, env, id)
		res := coTransition(t, env, id, before.Version,
			map[string]any{"status": "in_progress", "closeOutcome": "done"})
		// 400, not 422: the combination of fields is impossible in any state of the
		// issue. The paired edit refusal (close_outcome_requires_closed) is
		// 422, and the difference between the two classes is checked by the neighbouring subtest below.
		if res.Status != http.StatusBadRequest || coErrCode(res) != "close_outcome_not_a_close" {
			t.Fatalf("transition = %d/%s, want 400/close_outcome_not_a_close (body=%s)",
				res.Status, coErrCode(res), res.Raw)
		}
		if after := coRead(t, env, id); after != before {
			t.Errorf("issue changed by a rejected request: was %+v, now %+v", before, after)
		}
	})

	t.Run("typo closedOutcome: strict parsing still gives 400", func(t *testing.T) {
		id := coSeed(t, env, env.pid, env.prefix, "t-typo")
		before := coRead(t, env, id)
		res := coTransition(t, env, id, before.Version,
			map[string]any{"status": "closed", "closedOutcome": "done"})
		// A new field has no right to weaken the strict decodeJSON: a silently
		// swallowed typo would close the issue with outcome done, while the
		// caller would be sure they had recorded their own.
		if res.Status != http.StatusBadRequest || coErrCode(res) != "invalid_json" {
			t.Fatalf("transition = %d/%s, want 400/invalid_json (body=%s)",
				res.Status, coErrCode(res), res.Raw)
		}
		if after := coRead(t, env, id); after != before {
			t.Errorf("issue changed by a rejected request: was %+v, now %+v", before, after)
		}
	})
}

// TestCloseOutcome_Transition_LinkIsExactlyOne — the target edge on the
// UpdateStatus writer: type, direction and UNIQUENESS on repeat.
func TestCloseOutcome_Transition_LinkIsExactlyOne(t *testing.T) {
	env := coSetup(t)

	cases := []struct {
		name       string
		outcome    string
		targetIn   string // "same" | "other"
		wantEdgeFn func(issueID, targetID string) string
	}{
		{
			name:    "duplicate, target in the same project → duplicates A→B",
			outcome: issues.CloseOutcomeDuplicate, targetIn: "same",
			wantEdgeFn: func(a, b string) string { return a + "|duplicates|" + b },
		},
		{
			name:    "duplicate, target in another VISIBLE project (non-admin) → duplicates A→B",
			outcome: issues.CloseOutcomeDuplicate, targetIn: "other",
			wantEdgeFn: func(a, b string) string { return a + "|duplicates|" + b },
		},
		{
			// The direction is reversed, and this is not a typo: "superseded" reads
			// "the target supersedes the issue being closed", exactly like a manual link.
			name:    "superseded → supersedes B→A",
			outcome: issues.CloseOutcomeSuperseded, targetIn: "same",
			wantEdgeFn: func(a, b string) string { return b + "|supersedes|" + a },
		},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			suffix := "t-link" + strconv.Itoa(i)
			id := coSeed(t, env, env.pid, env.prefix, suffix)
			var target string
			if c.targetIn == "other" {
				target = coSeed(t, env, env.pidOther, env.prefixOther, suffix+"x")
			} else {
				target = coSeed(t, env, env.pid, env.prefix, suffix+"x")
			}

			res := coTransition(t, env, id, coRead(t, env, id).Version,
				map[string]any{"status": "closed", "closeOutcome": c.outcome, "closeTarget": target})
			if res.Status != http.StatusOK {
				t.Fatalf("transition with a target = %d, want 200 (body=%s)", res.Status, res.Raw)
			}
			want := []string{c.wantEdgeFn(id, target)}
			if got := coEdges(t, env, id, target); len(got) != 1 || got[0] != want[0] {
				t.Fatalf("edges after close = %v, want exactly %v", got, want)
			}

			// A repeat of the same call is a repeated close. The edge must
			// stay single: link uniqueness is held by US-2, not by luck.
			res = coTransition(t, env, id, coRead(t, env, id).Version,
				map[string]any{"status": "closed", "closeOutcome": c.outcome, "closeTarget": target})
			if res.Status != http.StatusOK {
				t.Fatalf("repeated close = %d, want 200 (body=%s)", res.Status, res.Raw)
			}
			if got := coEdges(t, env, id, target); len(got) != 1 || got[0] != want[0] {
				t.Fatalf("edges after the REPEAT = %v, want still exactly %v", got, want)
			}
		})
	}
}

// coTargetCase — one target refusal cell. A table shared by two surfaces:
// their sets of cells must match, and a difference in the set is a
// defect in itself (US-2: "one semantics on all paths").
type coTargetCase struct {
	name string
	// outcome — nil means "no outcome passed at all".
	outcome *string
	// target — "self" substitutes the id of the issue itself; "" does not occur (there is
	// no cell without a target here); anything else is a literal id.
	target     string
	wantStatus int
	wantCode   string
}

func coStr(s string) *string { return &s }

func coTargetCases(selfID, invisibleID, missingID string) []coTargetCase {
	return []coTargetCase{
		{
			name: "target = the issue itself", outcome: coStr("duplicate"), target: selfID,
			wantStatus: http.StatusBadRequest, wantCode: "close_target_self",
		},
		{
			name: "target without an outcome", outcome: nil, target: invisibleID,
			wantStatus: http.StatusBadRequest, wantCode: "close_target_requires_outcome",
		},
		{
			name: "target with done", outcome: coStr("done"), target: invisibleID,
			wantStatus: http.StatusBadRequest, wantCode: "close_target_not_allowed",
		},
		{
			name: "target with wont_do", outcome: coStr("wont_do"), target: invisibleID,
			wantStatus: http.StatusBadRequest, wantCode: "close_target_not_allowed",
		},
		{
			// Two cells, ONE code — and this is a checked statement, not a
			// tautology: handler/dependencies.go#mapDepErr merges
			// "does not exist" and "not visible" deliberately (see the comment on CWE-203/204),
			// because a difference in codes would be an existence oracle.
			name: "target invisible", outcome: coStr("duplicate"), target: invisibleID,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "target_not_found",
		},
		{
			name: "target does not exist", outcome: coStr("duplicate"), target: missingID,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "target_not_found",
		},
	}
}

// TestCloseOutcome_Transition_TargetRefusals — all target refusal cells on a
// transition, plus a comparison of the code with a manual link to the same target.
func TestCloseOutcome_Transition_TargetRefusals(t *testing.T) {
	env := coSetup(t)
	invisible := coSeed(t, env, env.pidHidden, env.prefixHidden, "t-invisible")
	missing := env.prefix + "-99999"
	scratch := coSeed(t, env, env.pid, env.prefix, "t-scratch")

	for i, c := range coTargetCases("", invisible, missing) {
		t.Run(c.name, func(t *testing.T) {
			id := coSeed(t, env, env.pid, env.prefix, "t-refuse"+strconv.Itoa(i))
			target := c.target
			if target == "" {
				target = id // cell "target = the issue itself"
			}
			before := coRead(t, env, id)

			body := map[string]any{"status": "closed", "closeTarget": target}
			if c.outcome != nil {
				body["closeOutcome"] = *c.outcome
			}
			res := coTransition(t, env, id, before.Version, body)

			if res.Status != c.wantStatus || coErrCode(res) != c.wantCode {
				t.Fatalf("transition = %d/%s, want %d/%s (body=%s)",
					res.Status, coErrCode(res), c.wantStatus, c.wantCode, res.Raw)
			}
			if after := coRead(t, env, id); after != before {
				t.Errorf("rejected request CHANGED the issue: was %+v, now %+v", before, after)
			}
			if edges := coEdges(t, env, id, target); len(edges) != 0 {
				t.Errorf("rejected request created edges: %v", edges)
			}
		})
	}

	// Comparison with the reference: a manual link to the same target answers with the same code.
	// By a run, not a literal — otherwise a joint drift of both paths
	// would stay green.
	for _, target := range []string{invisible, missing} {
		t.Run("same code as POST …/dependencies on "+target, func(t *testing.T) {
			manual := coDepCreate(t, env, scratch, target)
			id := coSeed(t, env, env.pid, env.prefix, "t-oracle-"+target)
			closing := coTransition(t, env, id, coRead(t, env, id).Version,
				map[string]any{"status": "closed", "closeOutcome": "duplicate", "closeTarget": target})
			if manual.Status != closing.Status || coErrCode(manual) != coErrCode(closing) {
				t.Fatalf("the close path tightened or loosened the refusal contract of its surface:\n"+
					"  POST …/dependencies = %d/%s\n  close with a target  = %d/%s",
					manual.Status, coErrCode(manual), closing.Status, coErrCode(closing))
			}
		})
	}
}

// TestCloseOutcome_TargetVisibilityUsesTheSystemRole — the key of target visibility
// is the SYSTEM role (handler/issues.go#closeTargetCaller), not Actor.Role,
// on BOTH single REST surfaces.
//
// # Why no cell above checks this
//
// They all run under the system role "user", and for any non-admin role
// projects/access.go#EnsureProjectVisible counts membership the same way: the keys
// "user" and "human" give one answer for any target. The substitution is invisible there —
// measured by mutation, the full gate of the wave stayed green (review A/4,
// F-2).
//
// There is one distinguishing case: an ADMINISTRATOR and a target in a project they are not in.
// The reference is obtained by running POST …/dependencies by the same caller, not by
// a literal: US-2 promises the close path THE SAME refusal contract as
// a manual link of the same surface, and the assertion must go red on
// a joint drift of both paths too.
func TestCloseOutcome_TargetVisibilityUsesTheSystemRole(t *testing.T) {
	env := coSetup(t)
	// The target is in a project where the administrator is not a member.
	target := coSeed(t, env, env.pidHidden, env.prefixHidden, "adm-target")

	// "Reference accepted" first: if the manual link refused too,
	// there would be nothing to compare, and the probe would stay green with
	// a lost role.
	coAdminWritesInPid(t, env)
	scratch := coSeed(t, env, env.pid, env.prefix, "adm-scratch")
	if manual := coDepCreateAs(t, env, env.admin, "admin", scratch, target); manual.Status != http.StatusCreated &&
		manual.Status != http.StatusOK {
		t.Fatalf("the fixture does not reach the distinguishing case: the admin's manual link "+
			"to a target outside their membership = %d/%s (body=%s)",
			manual.Status, coErrCode(manual), manual.Raw)
	}

	surfaces := []struct {
		name string
		call func(t *testing.T, id string) xpResponse
		// prepare brings the issue to a state from which the writer is reachable.
		prepare func(t *testing.T, id string)
	}{
		{
			name: "transition (Store.UpdateStatus)",
			call: func(t *testing.T, id string) xpResponse {
				return coTransitionAs(t, env, env.admin, "admin", id, coRead(t, env, id).Version,
					map[string]any{"status": "closed", "closeOutcome": "duplicate",
						"closeTarget": target})
			},
			prepare: func(t *testing.T, id string) {},
		},
		{
			name: "edit (Store.PatchIssueWithResult)",
			call: func(t *testing.T, id string) xpResponse {
				return coPatchAs(t, env, env.admin, "admin", id, coRead(t, env, id).Version,
					map[string]any{"closeOutcome": "duplicate", "closeTarget": target})
			},
			prepare: func(t *testing.T, id string) {
				res := coTransitionAs(t, env, env.admin, "admin", id, coRead(t, env, id).Version,
					map[string]any{"status": "closed"})
				if res.Status != http.StatusOK {
					t.Fatalf("setup: close = %d (body=%s)", res.Status, res.Raw)
				}
			},
		},
	}

	for i, sf := range surfaces {
		t.Run(sf.name, func(t *testing.T) {
			id := coSeed(t, env, env.pid, env.prefix, "adm-close"+strconv.Itoa(i))
			sf.prepare(t, id)
			res := sf.call(t, id)
			if res.Status != http.StatusOK {
				t.Fatalf("admin rejected (%d/%s) on a target that the manual link of the same "+
					"caller ACCEPTED (body=%s).\nUS-2 requires the close path to have the same "+
					"refusal contract as POST …/dependencies. Target visibility was computed "+
					"not by the SYSTEM role (closeTargetCaller) but by Actor.Role — for a "+
					"non-admin these two keys are indistinguishable, here they diverge",
					res.Status, coErrCode(res), res.Raw)
			}
			want := id + "|duplicates|" + target
			if got := coEdges(t, env, id); len(got) != 1 || got[0] != want {
				t.Fatalf("edges of the closed issue = %v, want exactly [%s]", got, want)
			}
		})
	}
}

// ---------------- row 25: edit ----------------

// TestCloseOutcome_Patch_ValueAndState — editing the outcome without reopening.
func TestCloseOutcome_Patch_ValueAndState(t *testing.T) {
	env := coSetup(t)

	// closeAsDone closes the issue through a transition and returns its id.
	closeAsDone := func(t *testing.T, suffix string) string {
		t.Helper()
		id := coSeed(t, env, env.pid, env.prefix, suffix)
		res := coTransition(t, env, id, coRead(t, env, id).Version,
			map[string]any{"status": "closed"})
		if res.Status != http.StatusOK {
			t.Fatalf("setup: close %s = %d (body=%s)", id, res.Status, res.Raw)
		}
		return id
	}

	t.Run("outcome edit of a closed issue: 200 and the card with the new outcome", func(t *testing.T) {
		id := closeAsDone(t, "p-ok")
		before := coRead(t, env, id)
		res := coPatch(t, env, id, before.Version, map[string]any{"closeOutcome": "wont_do"})
		if res.Status != http.StatusOK {
			t.Fatalf("edit = %d, want 200 (body=%s)", res.Status, res.Raw)
		}
		if got, _ := res.Body["closeOutcome"].(string); got != "wont_do" {
			t.Errorf("response card: closeOutcome = %q, want wont_do (body=%s)", got, res.Raw)
		}
		after := coRead(t, env, id)
		if after.Outcome != "wont_do" {
			t.Errorf("in the database outcome = %q, want wont_do", after.Outcome)
		}
		if after.Status != "closed" {
			t.Errorf("outcome edit reopened the issue: status = %q", after.Status)
		}
	})

	t.Run("outcome edit of a NOT closed issue: 422 close_outcome_requires_closed", func(t *testing.T) {
		id := coSeed(t, env, env.pid, env.prefix, "p-open")
		before := coRead(t, env, id)
		res := coPatch(t, env, id, before.Version, map[string]any{"closeOutcome": "wont_do"})
		// 422, not 400: the request is correct in shape, the current issue
		// STATE forbids it. The paired transition refusal above is 400. Two boundaries,
		// two classes; they cannot be collapsed into one code.
		if res.Status != http.StatusUnprocessableEntity ||
			coErrCode(res) != "close_outcome_requires_closed" {
			t.Fatalf("edit = %d/%s, want 422/close_outcome_requires_closed (body=%s)",
				res.Status, coErrCode(res), res.Raw)
		}
		if after := coRead(t, env, id); after != before {
			t.Errorf("rejected edit changed the issue: was %+v, now %+v", before, after)
		}
	})

	t.Run("bogus: 400 invalid_close_outcome, issue unchanged", func(t *testing.T) {
		id := closeAsDone(t, "p-bogus")
		before := coRead(t, env, id)
		res := coPatch(t, env, id, before.Version, map[string]any{"closeOutcome": "bogus"})
		if res.Status != http.StatusBadRequest || coErrCode(res) != "invalid_close_outcome" {
			t.Fatalf("edit = %d/%s, want 400/invalid_close_outcome (body=%s)",
				res.Status, coErrCode(res), res.Raw)
		}
		if after := coRead(t, env, id); after != before {
			t.Errorf("rejected edit changed the issue: was %+v, now %+v", before, after)
		}
	})

	t.Run("version conflict: 422 version_conflict, as today", func(t *testing.T) {
		id := closeAsDone(t, "p-conflict")
		before := coRead(t, env, id)
		res := coPatch(t, env, id, before.Version-1, map[string]any{"closeOutcome": "wont_do"})
		if res.Status != http.StatusUnprocessableEntity || coErrCode(res) != "version_conflict" {
			t.Fatalf("edit with a stale version = %d/%s, want 422/version_conflict (body=%s)",
				res.Status, coErrCode(res), res.Raw)
		}
		if after := coRead(t, env, id); after != before {
			t.Errorf("rejected edit changed the issue: was %+v, now %+v", before, after)
		}
	})
}

// TestCloseOutcome_Patch_LinkIsExactlyOne — the target edge on the SECOND writer
// (PatchIssueWithResult), with a target in another visible project, for a non-admin.
func TestCloseOutcome_Patch_LinkIsExactlyOne(t *testing.T) {
	env := coSetup(t)

	id := coSeed(t, env, env.pid, env.prefix, "p-link")
	if res := coTransition(t, env, id, coRead(t, env, id).Version,
		map[string]any{"status": "closed"}); res.Status != http.StatusOK {
		t.Fatalf("setup: close = %d (body=%s)", res.Status, res.Raw)
	}
	target := coSeed(t, env, env.pidOther, env.prefixOther, "p-link-target")

	res := coPatch(t, env, id, coRead(t, env, id).Version,
		map[string]any{"closeOutcome": "duplicate", "closeTarget": target})
	if res.Status != http.StatusOK {
		t.Fatalf("edit with a cross-project target = %d, want 200 (body=%s)", res.Status, res.Raw)
	}
	want := id + "|duplicates|" + target
	if got := coEdges(t, env, id, target); len(got) != 1 || got[0] != want {
		t.Fatalf("edges after the edit = %v, want exactly [%s]", got, want)
	}

	// A repeat of the same edit: the outcome does not change, but by the US-2
	// rules the link must stay single.
	res = coPatch(t, env, id, coRead(t, env, id).Version,
		map[string]any{"closeOutcome": "duplicate", "closeTarget": target})
	if res.Status != http.StatusOK {
		t.Fatalf("repeated edit = %d, want 200 (body=%s)", res.Status, res.Raw)
	}
	if got := coEdges(t, env, id, target); len(got) != 1 || got[0] != want {
		t.Fatalf("edges after the REPEAT = %v, want still exactly [%s]", got, want)
	}
}

// TestCloseOutcome_Patch_TargetRefusals — the same six cells on the edit
// surface. They cannot be merged with the transition: the edit has its own input builder and
// its own writer, and half of them would be green on one while the other drifts.
func TestCloseOutcome_Patch_TargetRefusals(t *testing.T) {
	env := coSetup(t)
	invisible := coSeed(t, env, env.pidHidden, env.prefixHidden, "p-invisible")
	missing := env.prefix + "-99999"
	scratch := coSeed(t, env, env.pid, env.prefix, "p-scratch")

	// The issues to be edited MUST be CLOSED: otherwise
	// close_outcome_requires_closed fires first and no target cell will be reached
	// — the test would be green with the target checks entirely absent.
	closed := func(t *testing.T, suffix string) string {
		t.Helper()
		id := coSeed(t, env, env.pid, env.prefix, suffix)
		if res := coTransition(t, env, id, coRead(t, env, id).Version,
			map[string]any{"status": "closed"}); res.Status != http.StatusOK {
			t.Fatalf("setup: close %s = %d (body=%s)", id, res.Status, res.Raw)
		}
		return id
	}

	for i, c := range coTargetCases("", invisible, missing) {
		t.Run(c.name, func(t *testing.T) {
			id := closed(t, "p-refuse"+strconv.Itoa(i))
			target := c.target
			if target == "" {
				target = id
			}
			before := coRead(t, env, id)

			body := map[string]any{"closeTarget": target}
			if c.outcome != nil {
				body["closeOutcome"] = *c.outcome
			}
			res := coPatch(t, env, id, before.Version, body)

			if res.Status != c.wantStatus || coErrCode(res) != c.wantCode {
				t.Fatalf("edit = %d/%s, want %d/%s (body=%s)",
					res.Status, coErrCode(res), c.wantStatus, c.wantCode, res.Raw)
			}
			if after := coRead(t, env, id); after != before {
				t.Errorf("rejected edit CHANGED the issue: was %+v, now %+v", before, after)
			}
			if edges := coEdges(t, env, id, target); len(edges) != 0 {
				t.Errorf("rejected edit created edges: %v", edges)
			}
		})
	}

	for _, target := range []string{invisible, missing} {
		t.Run("same code as POST …/dependencies on "+target, func(t *testing.T) {
			manual := coDepCreate(t, env, scratch, target)
			id := closed(t, "p-oracle-"+target)
			patching := coPatch(t, env, id, coRead(t, env, id).Version,
				map[string]any{"closeOutcome": "duplicate", "closeTarget": target})
			if manual.Status != patching.Status || coErrCode(manual) != coErrCode(patching) {
				t.Fatalf("the edit path tightened or loosened the refusal contract of its surface:\n"+
					"  POST …/dependencies = %d/%s\n  edit with a target   = %d/%s",
					manual.Status, coErrCode(manual), patching.Status, coErrCode(patching))
			}
		})
	}
}

// TestCloseTargetRequired_RESTSingleSurfaces — sk-540.2 on the two
// single-issue REST routes: a transition as duplicate without closeTarget and
// an outcome edit to superseded of a closed issue without the matching edge
// are business refusals (422 close_target_required) that write nothing.
func TestCloseTargetRequired_RESTSingleSurfaces(t *testing.T) {
	env := coSetup(t)

	t.Run("transition", func(t *testing.T) {
		id := coSeed(t, env, env.pid, env.prefix, "treq-transition")
		before := coRead(t, env, id)
		res := coTransition(t, env, id, before.Version,
			map[string]any{"status": "closed", "closeOutcome": "duplicate"})
		if code, _ := res.Body["error"].(string); res.Status != http.StatusUnprocessableEntity ||
			code != "close_target_required" {
			t.Fatalf("transition = %d %q, want 422 close_target_required (body=%s)", res.Status, code, res.Raw)
		}
		if after := coRead(t, env, id); after != before {
			t.Errorf("refused transition changed the issue: %+v → %+v", before, after)
		}
	})

	t.Run("outcome edit", func(t *testing.T) {
		id := coSeed(t, env, env.pid, env.prefix, "treq-edit")
		bcoClose(t, env, id, "done")
		before := coRead(t, env, id)
		res := coPatch(t, env, id, before.Version, map[string]any{"closeOutcome": "superseded"})
		if code, _ := res.Body["error"].(string); res.Status != http.StatusUnprocessableEntity ||
			code != "close_target_required" {
			t.Fatalf("outcome edit = %d %q, want 422 close_target_required (body=%s)", res.Status, code, res.Raw)
		}
		if after := coRead(t, env, id); after != before {
			t.Errorf("refused edit changed the issue: %+v → %+v", before, after)
		}
	})
}
