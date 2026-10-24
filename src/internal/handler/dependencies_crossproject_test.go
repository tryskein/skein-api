package handler

// dependencies_crossproject_test.go — sk-194.6.51.
//
// # What is under test
//
// Every route on DependenciesHandler: Create, Delete, List and Reparent, all
// of them shaped /projects/{projectID}/issues/{issueID}/... Before the fix
// each one ran projects.EnsureProjectVisible on the path project and then
// used the path issue id unconditionally, so a caller could operate on any
// issue in the installation through a project they happened to be able to
// see. Same class as sk-194.6.50, one file over.
//
// # The part that makes this test file interesting: the FROM/TO asymmetry
//
// A dependency edge has two ends and they are NOT governed by the same rule.
//
//   - The FROM side is the path issue. It owns the edge (the primary key is
//     (from_id, to_id, type) and CRUD lives under the source issue), and it
//     is the side the path project must match. That is what these tests
//     assert 404 on.
//   - The TO side may live in a DIFFERENT project, on purpose. Cross-project
//     edges are a supported Skein feature, gated on the caller being able to
//     SEE the target's project, not on the target sharing a project with the
//     source.
//
// So the naive "make everything same-project" hardening would have been a
// functional regression dressed up as a security fix.
// TestDeps_LegitimateCrossProjectEdgeStillWorks is the guard against that
// and is as load-bearing as the 404 tests: if it ever goes red, the fix has
// broken a feature rather than closed a hole.
//
// Reparent is the one place where BOTH ends are same-project, and that is a
// pre-existing rule of the hierarchy (reparent_cross_project), not something
// this change introduced.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// ---------------- harness ----------------

func newXPDepsHandler(t *testing.T, pool *pgxpool.Pool) *DependenciesHandler {
	t.Helper()
	return NewDependenciesHandler(pool, dependencies.NewStore(pool), issues.NewStore(pool))
}

// countDeps returns how many edges have issueID on either end. Used to prove
// a rejected call wrote (or removed) nothing, not merely that it answered
// 404 — the distinction sk-194.6.50 showed matters.
func countDeps(t *testing.T, pool *pgxpool.Pool, issueID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM dependencies WHERE from_id = $1 OR to_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("count deps for %s: %v", issueID, err)
	}
	return n
}

func issueVersion(t *testing.T, pool *pgxpool.Pool, issueID string) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(t.Context(),
		`SELECT version FROM issues WHERE id = $1`, issueID).Scan(&v); err != nil {
		t.Fatalf("read version %s: %v", issueID, err)
	}
	return v
}

func parentOf(t *testing.T, pool *pgxpool.Pool, issueID string) string {
	t.Helper()
	var parent string
	err := pool.QueryRow(t.Context(), `
SELECT to_id FROM dependencies WHERE from_id = $1 AND type = 'parent-child'`, issueID).Scan(&parent)
	if err != nil {
		return "" // no parent edge
	}
	return parent
}

// ---------------- Create ----------------

// TestDepsCreate_CrossProjectFromSideRejected: the FROM side is the path
// issue, so naming a foreign issue there must 404 with no edge written.
func TestDepsCreate_CrossProjectFromSideRejected(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)
	h := newXPDepsHandler(t, pool)

	fromInB := prefixB + "-1"
	targetInA := prefixA + "-1"
	seedCmtIssue(t, pool, pidB, user, fromInB, "human")
	seedCmtIssue(t, pool, pidA, user, targetInA, "human")

	before := countDeps(t, pool, fromInB)

	req := xpRequest(t, http.MethodPost, "",
		map[string]any{"toId": targetInA, "type": "related"}, user, "user",
		"projectID", pidA.String(), "issueID", fromInB)
	status, body := xpCall(t, h.Create, req)

	if status != http.StatusNotFound {
		t.Fatalf("dependency create with a foreign FROM issue = %d, want 404 (body=%v)", status, body)
	}
	if code, _ := body["error"].(string); code != "issue_not_found" {
		t.Errorf("error code = %q, want issue_not_found", code)
	}
	if after := countDeps(t, pool, fromInB); after != before {
		t.Fatalf("edge count on the foreign issue changed %d → %d: the 404 was reported "+
			"but the edge was written anyway", before, after)
	}
}

// TestDepsCreate_SameProjectWorks is the positive control for the above: the
// identical call through the issue's own project must succeed.
func TestDepsCreate_SameProjectWorks(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	h := newXPDepsHandler(t, pool)

	from := prefix + "-1"
	to := prefix + "-2"
	seedCmtIssue(t, pool, pid, user, from, "human")
	seedCmtIssue(t, pool, pid, user, to, "human")

	req := xpRequest(t, http.MethodPost, "",
		map[string]any{"toId": to, "type": "related"}, user, "user",
		"projectID", pid.String(), "issueID", from)
	status, body := xpCall(t, h.Create, req)

	if status != http.StatusCreated {
		t.Fatalf("same-project dependency create = %d, want 201 (body=%v)", status, body)
	}
	if countDeps(t, pool, from) != 1 {
		t.Fatalf("edge was not persisted")
	}
}

// TestDeps_LegitimateCrossProjectEdgeStillWorks is the functional-regression
// guard described in the file header.
//
// The edge goes FROM an issue in project A (addressed through project A's
// own URL — correct) TO an issue in project B. That is a legitimate,
// documented Skein scenario: "cross-project edges are allowed, but you must
// already see both projects." The fix scopes the FROM side only; if someone
// later "hardens" the TO side to the path project as well, this test goes
// red and tells them they broke a feature.
func TestDeps_LegitimateCrossProjectEdgeStillWorks(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user) // caller sees BOTH projects
	h := newXPDepsHandler(t, pool)

	fromInA := prefixA + "-1"
	toInB := prefixB + "-1"
	seedCmtIssue(t, pool, pidA, user, fromInA, "human")
	seedCmtIssue(t, pool, pidB, user, toInB, "human")

	t.Run("create", func(t *testing.T) {
		req := xpRequest(t, http.MethodPost, "",
			map[string]any{"toId": toInB, "type": "related"}, user, "user",
			"projectID", pidA.String(), "issueID", fromInA)
		status, body := xpCall(t, h.Create, req)
		if status != http.StatusCreated {
			t.Fatalf("cross-project edge create = %d, want 201 — cross-project edges are a "+
				"supported feature and the FROM-side scoping must not have broken it "+
				"(body=%v)", status, body)
		}
	})

	t.Run("list shows it", func(t *testing.T) {
		req := xpRequest(t, http.MethodGet, "", nil, user, "user",
			"projectID", pidA.String(), "issueID", fromInA)
		status, _ := xpCall(t, h.List, req)
		if status != http.StatusOK {
			t.Fatalf("list on the source issue = %d, want 200", status)
		}
	})

	t.Run("delete", func(t *testing.T) {
		req := xpRequest(t, http.MethodDelete, "", nil, user, "user",
			"projectID", pidA.String(), "issueID", fromInA, "toID", toInB, "type", "related")
		status, body := xpCall(t, h.Delete, req)
		if status != http.StatusNoContent {
			t.Fatalf("cross-project edge delete = %d, want 204 (body=%v)", status, body)
		}
		if countDeps(t, pool, fromInA) != 0 {
			t.Fatalf("edge survived the delete")
		}
	})
}

// ---------------- Delete ----------------

func TestDepsDelete_CrossProjectFromSideRejected(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, _ := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)
	h := newXPDepsHandler(t, pool)

	from := prefixB + "-1"
	to := prefixB + "-2"
	seedCmtIssue(t, pool, pidB, user, from, "human")
	seedCmtIssue(t, pool, pidB, user, to, "human")
	seedDepRow(t, pool, from, to, "related", user)

	before := countDeps(t, pool, from)

	req := xpRequest(t, http.MethodDelete, "", nil, user, "user",
		"projectID", pidA.String(), "issueID", from, "toID", to, "type", "related")
	status, body := xpCall(t, h.Delete, req)

	if status != http.StatusNotFound {
		t.Fatalf("dependency delete through a foreign project = %d, want 404 (body=%v)", status, body)
	}
	if code, _ := body["error"].(string); code != "issue_not_found" {
		t.Errorf("error code = %q, want issue_not_found", code)
	}
	if after := countDeps(t, pool, from); after != before {
		t.Fatalf("edge count changed %d → %d: the 404 was reported but the edge was "+
			"deleted anyway", before, after)
	}

	// Positive control on the same fixture: through project B it deletes.
	req = xpRequest(t, http.MethodDelete, "", nil, user, "user",
		"projectID", pidB.String(), "issueID", from, "toID", to, "type", "related")
	if status, body = xpCall(t, h.Delete, req); status != http.StatusNoContent {
		t.Fatalf("same-project dependency delete = %d, want 204 (body=%v)", status, body)
	}
	if countDeps(t, pool, from) != before-1 {
		t.Fatalf("same-project delete did not remove the edge")
	}
}

// ---------------- List ----------------

// TestDepsList_CrossProjectRejected. List is a read, but the leak is real:
// the per-edge redaction in the store protects the FAR side of each edge
// only. The near side — the fact that this issue exists, its full edge
// topology, and every neighbour whose project the caller CAN see — was
// readable for any issue id through any project the caller could see.
func TestDepsList_CrossProjectRejected(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, _ := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)
	h := newXPDepsHandler(t, pool)

	from := prefixB + "-1"
	to := prefixB + "-2"
	seedCmtIssue(t, pool, pidB, user, from, "human")
	seedCmtIssue(t, pool, pidB, user, to, "human")
	seedDepRow(t, pool, from, to, "related", user)

	req := xpRequest(t, http.MethodGet, "", nil, user, "user",
		"projectID", pidA.String(), "issueID", from)
	status, body := xpCall(t, h.List, req)
	if status != http.StatusNotFound {
		t.Fatalf("dependency list through a foreign project = %d, want 404 (body=%v)", status, body)
	}
	if code, _ := body["error"].(string); code != "issue_not_found" {
		t.Errorf("error code = %q, want issue_not_found", code)
	}

	// Positive control: through its own project the same call returns the graph.
	req = xpRequest(t, http.MethodGet, "", nil, user, "user",
		"projectID", pidB.String(), "issueID", from)
	if status, body = xpCall(t, h.List, req); status != http.StatusOK {
		t.Fatalf("same-project dependency list = %d, want 200 (body=%v)", status, body)
	}
	out, ok := body["outgoing"].([]any)
	if !ok || len(out) != 1 {
		t.Fatalf("same-project list did not return the edge: %v", body["outgoing"])
	}
}

// ---------------- Reparent ----------------

// TestDepsReparent_CrossProjectRejected. Reparent is the highest-value target
// of the four: it rewrites the hierarchy edge and bumps the issue version, so
// an unauthorized success is both a structural change and a version bump that
// invalidates every other writer's optimistic-concurrency token.
func TestDepsReparent_CrossProjectRejected(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, _ := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)
	h := newXPDepsHandler(t, pool)

	child := prefixB + "-1"
	oldParent := prefixB + "-2"
	newParent := prefixB + "-3"
	seedCmtIssue(t, pool, pidB, user, child, "human")
	seedCmtIssue(t, pool, pidB, user, oldParent, "human")
	seedCmtIssue(t, pool, pidB, user, newParent, "human")
	seedDepRow(t, pool, child, oldParent, "parent-child", user)

	versionBefore := issueVersion(t, pool, child)

	req := xpRequest(t, http.MethodPost, "",
		map[string]any{"newParentId": newParent, "expectedVersion": versionBefore},
		user, "user", "projectID", pidA.String(), "issueID", child)
	status, body := xpCall(t, h.Reparent, req)

	if status != http.StatusNotFound {
		t.Fatalf("reparent through a foreign project = %d, want 404 (body=%v)", status, body)
	}
	if code, _ := body["error"].(string); code != "issue_not_found" {
		t.Errorf("error code = %q, want issue_not_found", code)
	}
	if got := parentOf(t, pool, child); got != oldParent {
		t.Fatalf("parent moved to %q despite the 404 — want it still %q", got, oldParent)
	}
	if got := issueVersion(t, pool, child); got != versionBefore {
		t.Fatalf("issue version bumped %d → %d despite the 404", versionBefore, got)
	}

	// Positive control: the identical move through project B succeeds.
	req = xpRequest(t, http.MethodPost, "",
		map[string]any{"newParentId": newParent, "expectedVersion": versionBefore},
		user, "user", "projectID", pidB.String(), "issueID", child)
	if status, body = xpCall(t, h.Reparent, req); status != http.StatusOK {
		t.Fatalf("same-project reparent = %d, want 200 (body=%v)", status, body)
	}
	if got := parentOf(t, pool, child); got != newParent {
		t.Fatalf("same-project reparent did not move the child: parent = %q, want %q", got, newParent)
	}
}

// TestDeps_NonMemberOfIssueProjectGets404 is the realistic attacker for all
// four routes: a member of project A only, guessing ids from project B.
// Project visibility does not stop them, because they call THEIR OWN
// project's URL — only the object-level gate does.
func TestDeps_NonMemberOfIssueProjectGets404(t *testing.T) {
	pool := newIMTestPool(t)
	attacker, _ := seedIMUser(t, pool, "user")
	victim, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, attacker)
	pidB, prefixB := seedIMProject(t, pool, victim)
	h := newXPDepsHandler(t, pool)

	victimIssue := prefixB + "-1"
	victimOther := prefixB + "-2"
	attackerIssue := prefixA + "-1"
	seedCmtIssue(t, pool, pidB, victim, victimIssue, "human")
	seedCmtIssue(t, pool, pidB, victim, victimOther, "human")
	seedCmtIssue(t, pool, pidA, attacker, attackerIssue, "human")
	seedDepRow(t, pool, victimIssue, victimOther, "related", victim)

	before := countDeps(t, pool, victimIssue)
	versionBefore := issueVersion(t, pool, victimIssue)

	cases := []struct {
		name string
		run  func() (int, map[string]any)
	}{
		{"POST /dependencies", func() (int, map[string]any) {
			return xpCall(t, h.Create, xpRequest(t, http.MethodPost, "",
				map[string]any{"toId": attackerIssue, "type": "related"}, attacker, "user",
				"projectID", pidA.String(), "issueID", victimIssue))
		}},
		{"DELETE /dependencies/{toID}/{type}", func() (int, map[string]any) {
			return xpCall(t, h.Delete, xpRequest(t, http.MethodDelete, "", nil, attacker, "user",
				"projectID", pidA.String(), "issueID", victimIssue,
				"toID", victimOther, "type", "related"))
		}},
		{"GET /dependencies", func() (int, map[string]any) {
			return xpCall(t, h.List, xpRequest(t, http.MethodGet, "", nil, attacker, "user",
				"projectID", pidA.String(), "issueID", victimIssue))
		}},
		{"POST /reparent", func() (int, map[string]any) {
			return xpCall(t, h.Reparent, xpRequest(t, http.MethodPost, "",
				map[string]any{"newParentId": victimOther, "expectedVersion": versionBefore},
				attacker, "user", "projectID", pidA.String(), "issueID", victimIssue))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := tc.run()
			if status != http.StatusNotFound {
				t.Fatalf("non-member cross-project call = %d, want 404 (body=%v)", status, body)
			}
			// The code matters as much as the status: a distinct code for
			// "exists elsewhere" would be the same oracle one layer down.
			if code, _ := body["error"].(string); code != "issue_not_found" {
				t.Errorf("non-member error code = %q, want issue_not_found (body=%v)", code, body)
			}
			if after := countDeps(t, pool, victimIssue); after != before {
				t.Fatalf("edge count changed %d → %d despite the 404", before, after)
			}
			if v := issueVersion(t, pool, victimIssue); v != versionBefore {
				t.Fatalf("issue version bumped %d → %d despite the 404", versionBefore, v)
			}
		})
	}
}

// seedDepRow inserts a dependency edge directly, bypassing the store, so the
// fixtures for the negative cases do not depend on the code under test.
func seedDepRow(t *testing.T, pool *pgxpool.Pool, fromID, toID, depType string, creator uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, '{}'::jsonb, 1, $4, now())`, fromID, toID, depType, creator); err != nil {
		t.Fatalf("seed dependency %s -%s-> %s: %v", fromID, depType, toID, err)
	}
}

// ---------------- anti-oracle: foreign FROM ≡ nonexistent FROM ----------------

// xpDepFixtures are the ids the four routes need on the far side of a call,
// so that a rejection can only ever be about the FROM side.
type xpDepFixtures struct {
	// visibleTarget is a legitimate TO id in a project the caller can see.
	visibleTarget string
	// existingEdgeTo is the TO id of an edge that really exists under the
	// real FROM issue, so DELETE addresses a live row.
	existingEdgeTo string
	// newParent is a reparent target in the real FROM issue's own project.
	newParent string
	// version is the real FROM issue's live version (reparent's OCC token).
	version int
}

// xpDepRoute is one dependency route expressed so the same description can be
// driven with any FROM id.
type xpDepRoute struct {
	name string
	run  func(t *testing.T, h *DependenciesHandler, pathProject uuid.UUID,
		caller uuid.UUID, from string, fx xpDepFixtures) xpResponse
}

func xpDepRoutes() []xpDepRoute {
	return []xpDepRoute{
		{
			name: "POST /dependencies",
			run: func(t *testing.T, h *DependenciesHandler, pp, caller uuid.UUID,
				from string, fx xpDepFixtures) xpResponse {
				return xpCallFull(t, h.Create, xpRequest(t, http.MethodPost, "",
					map[string]any{"toId": fx.visibleTarget, "type": "related"},
					caller, "user", "projectID", pp.String(), "issueID", from))
			},
		},
		{
			name: "DELETE /dependencies/{toID}/{type}",
			run: func(t *testing.T, h *DependenciesHandler, pp, caller uuid.UUID,
				from string, fx xpDepFixtures) xpResponse {
				return xpCallFull(t, h.Delete, xpRequest(t, http.MethodDelete, "", nil,
					caller, "user", "projectID", pp.String(), "issueID", from,
					"toID", fx.existingEdgeTo, "type", "related"))
			},
		},
		{
			name: "GET /dependencies",
			run: func(t *testing.T, h *DependenciesHandler, pp, caller uuid.UUID,
				from string, _ xpDepFixtures) xpResponse {
				return xpCallFull(t, h.List, xpRequest(t, http.MethodGet, "", nil,
					caller, "user", "projectID", pp.String(), "issueID", from))
			},
		},
		{
			name: "POST /reparent",
			run: func(t *testing.T, h *DependenciesHandler, pp, caller uuid.UUID,
				from string, fx xpDepFixtures) xpResponse {
				return xpCallFull(t, h.Reparent, xpRequest(t, http.MethodPost, "",
					map[string]any{"newParentId": fx.newParent, "expectedVersion": fx.version},
					caller, "user", "projectID", pp.String(), "issueID", from))
			},
		},
	}
}

// TestDeps_ForeignFromIdIndistinguishableFromNonexistent is the dependency
// half of the equality the issue routes already pin
// (TestCrossProjectWrite_404NotDistinguishableFromNonexistent).
//
// The 404-over-403 decision only buys anything if the two answers are the
// same answer. If a future branch ever 403s (or 422s, or merely reworded the
// message) specifically when the FROM issue turns out to EXIST in another
// project, then walking ids through one's own project URL sorts real issues
// from imaginary ones — the enumeration step BOLA exploitation starts with,
// an observable response discrepancy (CWE-203/204). The issue-side test would
// have caught that on the issue routes; nothing caught it here.
//
// The comparison is deliberately on the RAW response — status, Content-Type
// and body bytes — not on a decoded `error` key. writeProblem splices `extra`
// fields into the body, so a code-only comparison would stay green while the
// bodies diverged.
func TestDeps_ForeignFromIdIndistinguishableFromNonexistent(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)
	h := newXPDepsHandler(t, pool)

	// The caller is a member of BOTH projects — visibility is removed from
	// the picture, so only the object-level gate can answer.
	realInB := prefixB + "-1"
	otherInB := prefixB + "-2"
	targetInA := prefixA + "-1"
	seedCmtIssue(t, pool, pidB, user, realInB, "human")
	seedCmtIssue(t, pool, pidB, user, otherInB, "human")
	seedCmtIssue(t, pool, pidA, user, targetInA, "human")
	seedDepRow(t, pool, realInB, otherInB, "related", user)

	fx := xpDepFixtures{
		visibleTarget:  targetInA,
		existingEdgeTo: otherInB,
		newParent:      otherInB,
		version:        issueVersion(t, pool, realInB),
	}
	// Same id shape as a real issue of project A, never created.
	neverCreated := prefixA + "-99999"

	for _, rt := range xpDepRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			real := rt.run(t, h, pidA, user, realInB, fx)
			fake := rt.run(t, h, pidA, user, neverCreated, fx)

			if real.Status != http.StatusNotFound {
				t.Fatalf("real-but-foreign FROM = %d, want 404 (body=%s)", real.Status, real.Raw)
			}
			if code, _ := real.Body["error"].(string); code != "issue_not_found" {
				t.Errorf("error code = %q, want issue_not_found (body=%s)", code, real.Raw)
			}
			if real.Status != fake.Status {
				t.Fatalf("existence oracle: real-but-foreign FROM → %d, nonexistent FROM → %d",
					real.Status, fake.Status)
			}
			if real.ContentType != fake.ContentType {
				t.Fatalf("existence oracle in Content-Type: %q vs %q",
					real.ContentType, fake.ContentType)
			}
			if real.Raw != fake.Raw {
				t.Fatalf("existence oracle in the response body:\n  foreign     = %s\n  nonexistent = %s",
					real.Raw, fake.Raw)
			}
		})
	}
}

// ---------------- anti-oracle: the TO side ----------------

// TestDeps_TargetMissingAndTargetInvisibleAnswerIdentically closes the other
// end of the edge.
//
// The FROM side is the path issue and is scoped to the path project. The TO
// side deliberately is NOT — cross-project edges are a feature — so its gate
// is visibility, evaluated in the store. That left two distinguishable
// answers: 422 target_not_found for "no such id anywhere" and 422
// dependency_target_not_visible for "real id, project you cannot see". A
// caller needs exactly one issue in one project they can see to turn that
// pair into a global id-existence oracle over every project in the
// installation — and Skein ids are short and sequential per project prefix,
// so enumeration is cheap. mapDepErr now answers both with one code, one
// message, one body.
//
// The legitimate cross-project edge (caller CAN see both projects) is
// unaffected and is guarded by TestDeps_LegitimateCrossProjectEdgeStillWorks.
func TestDeps_TargetMissingAndTargetInvisibleAnswerIdentically(t *testing.T) {
	pool := newIMTestPool(t)
	attacker, _ := seedIMUser(t, pool, "user")
	victim, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, attacker)
	pidB, prefixB := seedIMProject(t, pool, victim)
	h := newXPDepsHandler(t, pool)

	fromInA := prefixA + "-1"
	seedCmtIssue(t, pool, pidA, attacker, fromInA, "human")
	// Exists, and the attacker cannot see its project.
	hiddenInB := prefixB + "-1"
	seedCmtIssue(t, pool, pidB, victim, hiddenInB, "human")
	// Never created anywhere.
	neverCreated := prefixB + "-99999"

	version := issueVersion(t, pool, fromInA)

	cases := []struct {
		name string
		run  func(target string) xpResponse
	}{
		{"POST /dependencies", func(target string) xpResponse {
			return xpCallFull(t, h.Create, xpRequest(t, http.MethodPost, "",
				map[string]any{"toId": target, "type": "related"}, attacker, "user",
				"projectID", pidA.String(), "issueID", fromInA))
		}},
		{"POST /reparent", func(target string) xpResponse {
			return xpCallFull(t, h.Reparent, xpRequest(t, http.MethodPost, "",
				map[string]any{"newParentId": target, "expectedVersion": version},
				attacker, "user", "projectID", pidA.String(), "issueID", fromInA))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := tc.run(hiddenInB)
			mis := tc.run(neverCreated)

			if inv.Status != http.StatusUnprocessableEntity {
				t.Fatalf("invisible target = %d, want 422 (body=%s)", inv.Status, inv.Raw)
			}
			if inv.Status != mis.Status {
				t.Fatalf("existence oracle: target in an invisible project → %d, "+
					"nonexistent target → %d", inv.Status, mis.Status)
			}
			if inv.ContentType != mis.ContentType {
				t.Fatalf("existence oracle in Content-Type: %q vs %q",
					inv.ContentType, mis.ContentType)
			}
			if inv.Raw != mis.Raw {
				t.Fatalf("existence oracle on the TO side:\n"+
					"  exists, project invisible = %s\n  does not exist            = %s",
					inv.Raw, mis.Raw)
			}
		})
	}
}

// TestDepsDelete_LastOutcomeLinkRefused — sk-540.8, REST surface: the last
// `duplicates` edge of an issue closed as "duplicate" is not removable; the
// answer is 422 close_outcome_link_required and the edge stays.
func TestDepsDelete_LastOutcomeLinkRefused(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pid, prefix := seedIMProject(t, pool, user)
	h := newXPDepsHandler(t, pool)

	from := prefix + "-1"
	to := prefix + "-2"
	seedCmtIssue(t, pool, pid, user, from, "human")
	seedCmtIssue(t, pool, pid, user, to, "human")
	if _, err := pool.Exec(t.Context(), `
UPDATE issues SET status = 'closed', closed_at = now(), closed_reason = 'dup',
                  close_outcome = 'duplicate'
 WHERE id = $1`, from); err != nil {
		t.Fatalf("close %s as duplicate: %v", from, err)
	}
	seedDepRow(t, pool, from, to, dependencies.DepDuplicates, user)

	req := xpRequest(t, http.MethodDelete, "", nil, user, "user",
		"projectID", pid.String(), "issueID", from, "toID", to, "type", dependencies.DepDuplicates)
	status, body := xpCall(t, h.Delete, req)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("DELETE last duplicates edge = %d, want 422 (body=%v)", status, body)
	}
	if code, _ := body["error"].(string); code != "close_outcome_link_required" {
		t.Errorf("error code = %q, want close_outcome_link_required", code)
	}
	// The closed issue is visible to the caller: the refusal names it.
	if body["issueId"] != from || body["closeOutcome"] != "duplicate" ||
		body["type"] != dependencies.DepDuplicates {
		t.Errorf("refusal = %v, want issueId %s, closeOutcome duplicate, type duplicates", body, from)
	}
	if n := countDeps(t, pool, from); n != 1 {
		t.Fatalf("edges of %s after the refusal = %d, want 1", from, n)
	}
}

// TestDepsDelete_LastOutcomeLinkRefused_HiddenClosedIssue — sk-540.8 review,
// REST surface: the last `supersedes` edge into an issue closed as
// "superseded" is still refused when that issue lives in a project the
// caller cannot see, but the refusal says nothing about it (sk-423 horizon):
// no issueId, no closeOutcome, and the message does not name the issue.
func TestDepsDelete_LastOutcomeLinkRefused_HiddenClosedIssue(t *testing.T) {
	pool := newIMTestPool(t)
	caller, _ := seedIMUser(t, pool, "user")
	owner, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, caller)
	pidB, prefixB := seedIMProject(t, pool, owner)
	h := newXPDepsHandler(t, pool)

	replacement := prefixA + "-1"
	hidden := prefixB + "-1"
	seedCmtIssue(t, pool, pidA, caller, replacement, "human")
	seedCmtIssue(t, pool, pidB, owner, hidden, "human")
	if _, err := pool.Exec(t.Context(), `
UPDATE issues SET status = 'closed', closed_at = now(), closed_reason = 'replaced',
                  close_outcome = 'superseded'
 WHERE id = $1`, hidden); err != nil {
		t.Fatalf("close %s as superseded: %v", hidden, err)
	}
	seedDepRow(t, pool, replacement, hidden, dependencies.DepSupersedes, owner)

	req := xpRequest(t, http.MethodDelete, "", nil, caller, "user",
		"projectID", pidA.String(), "issueID", replacement, "toID", hidden,
		"type", dependencies.DepSupersedes)
	res := xpCallFull(t, h.Delete, req)
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("DELETE last supersedes edge into a hidden issue = %d, want 422 (body=%s)",
			res.Status, res.Raw)
	}
	if code, _ := res.Body["error"].(string); code != "close_outcome_link_required" {
		t.Errorf("error code = %q, want close_outcome_link_required", code)
	}
	if got, _ := res.Body["type"].(string); got != dependencies.DepSupersedes {
		t.Errorf("type = %q, want %q", got, dependencies.DepSupersedes)
	}
	for _, key := range []string{"issueId", "closeOutcome"} {
		if v, present := res.Body[key]; present {
			t.Errorf("refusal carries %s=%v about an issue the caller cannot see", key, v)
		}
	}
	if strings.Contains(res.Raw, hidden) {
		t.Errorf("refusal names the hidden issue %s: %s", hidden, res.Raw)
	}
	if strings.Contains(res.Raw, "superseded") {
		t.Errorf("refusal reveals the hidden issue's outcome: %s", res.Raw)
	}
	if n := countDeps(t, pool, hidden); n != 1 {
		t.Fatalf("edges of %s after the refusal = %d, want 1", hidden, n)
	}
}
