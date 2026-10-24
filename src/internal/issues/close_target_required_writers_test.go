package issues

// close_target_required_writers_test.go — sk-540.2 on the three writers of
// a close outcome: Store.Close (MCP close tools), Store.UpdateStatus (REST
// transition and bulk) and Store.PatchIssue (outcome edit of a closed
// issue). The expected answers come from the task's acceptance:
//
//   - duplicate/superseded on a TRANSITION without closeTarget is refused
//     (close_target_required) and writes nothing;
//   - (a) changing the outcome of a closed issue to duplicate/superseded
//     without a target passes only if the edge of the right TYPE and
//     DIRECTION exists: `duplicates` from the issue, `supersedes` to it;
//   - (b) a re-close keeping the outcome passes without a target, a
//     historical issue without any edge included.

import (
	"context"
	"strings"
	"testing"

	"skein-api/internal/dependencies"
)

func wantTargetRequired(t *testing.T, err error) {
	t.Helper()
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "close_target_required" {
		t.Fatalf("err = %v, want validation code close_target_required", err)
	}
}

func closedWithOutcome(t *testing.T, env *outcomeEnv, title, outcome string) *IssueDetail {
	t.Helper()
	iss := env.issue(t, title)
	done := CloseOutcomeDone
	if _, err := env.store.Close(t.Context(), CloseInput{
		ID: iss.ID, ExpectedVersion: iss.Version, Reason: "closed", Actor: env.actor,
		CloseOutcome: &done,
	}); err != nil {
		t.Fatalf("Close(%s): %v", iss.ID, err)
	}
	if outcome != CloseOutcomeDone {
		// A historical row: closed with this outcome before sk-540.2 made
		// the target mandatory, so it carries no edge.
		if _, err := env.pool.Exec(context.Background(),
			`UPDATE issues SET close_outcome = $2 WHERE id = $1`, iss.ID, outcome); err != nil {
			t.Fatalf("seed historical outcome: %v", err)
		}
	}
	out, err := env.store.GetIssue(t.Context(), iss.ID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", iss.ID, err)
	}
	return out
}

func TestCloseTargetRequired_TransitionRefusedOnBothCloseWriters(t *testing.T) {
	for _, w := range closeWriters() {
		for _, outcome := range []string{CloseOutcomeDuplicate, CloseOutcomeSuperseded} {
			t.Run(w.name+"/"+outcome, func(t *testing.T) {
				env := newOutcomeEnv(t)
				iss := env.issue(t, "to close")
				// An edge of the right type and direction does NOT lift the
				// rule on a transition: exception (a) is for closed issues.
				other := env.issue(t, "other")
				from, to := iss.ID, other.ID
				depType := dependencies.DepDuplicates
				if outcome == CloseOutcomeSuperseded {
					from, to, depType = other.ID, iss.ID, dependencies.DepSupersedes
				}
				seedDep(t, env.pool, from, to, depType, env.author)

				o := outcome
				err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Outcome: &o, Caller: &env.caller, Actor: env.actor,
				})
				wantTargetRequired(t, err)
				assertUntouched(t, env, iss.ID, iss.Version)
			})
		}
	}
}

// TestCloseTargetRequired_OutcomeChangeNeedsDirectedEdge — exception (a) on
// every writer that can change a closed issue's outcome, with the edge in
// the right direction (passes) and in the reverse one (refused, nothing
// written).
func TestCloseTargetRequired_OutcomeChangeNeedsDirectedEdge(t *testing.T) {
	type changer struct {
		name   string
		change func(t *testing.T, env *outcomeEnv, iss *IssueDetail, outcome string) error
	}
	changers := []changer{
		{"Store.PatchIssue", func(t *testing.T, env *outcomeEnv, iss *IssueDetail, outcome string) error {
			return patchOutcome(t, env, iss.ID, iss.Version, outcome, "", env.actor)
		}},
	}
	for _, w := range closeWriters() {
		changers = append(changers, changer{w.name, func(t *testing.T, env *outcomeEnv, iss *IssueDetail, outcome string) error {
			o := outcome
			return w.closeIssue(t, env.store, iss.ID, closeAttempt{
				Version: iss.Version, Outcome: &o, Caller: &env.caller, Actor: env.actor,
			})
		}})
	}

	for _, c := range changers {
		for _, outcome := range []string{CloseOutcomeDuplicate, CloseOutcomeSuperseded} {
			for _, rightDirection := range []bool{true, false} {
				name := c.name + "/" + outcome + "/reverse-edge"
				if rightDirection {
					name = c.name + "/" + outcome + "/matching-edge"
				}
				t.Run(name, func(t *testing.T) {
					env := newOutcomeEnv(t)
					iss := closedWithOutcome(t, env, "closed done", CloseOutcomeDone)
					other := env.issue(t, "original or replacement")

					// Matching: duplicates iss→other, supersedes other→iss.
					from, to := iss.ID, other.ID
					depType := dependencies.DepDuplicates
					if outcome == CloseOutcomeSuperseded {
						from, to, depType = other.ID, iss.ID, dependencies.DepSupersedes
					}
					if !rightDirection {
						from, to = to, from
					}
					seedDep(t, env.pool, from, to, depType, env.author)

					err := c.change(t, env, iss, outcome)
					if rightDirection {
						if err != nil {
							t.Fatalf("outcome change to %s with the matching edge: %v", outcome, err)
						}
						wantOutcome(t, env.pool, iss.ID, outcome)
						if n := countEdges(t, env.pool, from, to, depType); n != 1 {
							t.Errorf("%s edges %s→%s = %d, want 1 (no second edge)", depType, from, to, n)
						}
						return
					}
					wantTargetRequired(t, err)
					wantOutcome(t, env.pool, iss.ID, CloseOutcomeDone)
					if got := readCloseStamps(t, env.pool, iss.ID); got.Version != iss.Version {
						t.Errorf("version = %d after refusal, want %d", got.Version, iss.Version)
					}
				})
			}
		}
	}
}

// TestCloseTargetRequired_RecloseKeepsHistoricalOutcome — exception (b): a
// historical duplicate/superseded issue with NO edge may be re-closed, with
// the outcome omitted or repeated, on both close writers.
func TestCloseTargetRequired_RecloseKeepsHistoricalOutcome(t *testing.T) {
	for _, w := range closeWriters() {
		for _, outcome := range []string{CloseOutcomeDuplicate, CloseOutcomeSuperseded} {
			for _, explicit := range []bool{false, true} {
				name := w.name + "/" + outcome + "/omitted"
				if explicit {
					name = w.name + "/" + outcome + "/repeated"
				}
				t.Run(name, func(t *testing.T) {
					env := newOutcomeEnv(t)
					iss := closedWithOutcome(t, env, "historical", outcome)
					in := closeAttempt{Version: iss.Version, Caller: &env.caller, Actor: env.actor}
					if explicit {
						o := outcome
						in.Outcome = &o
					}
					if err := w.closeIssue(t, env.store, iss.ID, in); err != nil {
						t.Fatalf("re-close of a historical %s issue: %v", outcome, err)
					}
					wantOutcome(t, env.pool, iss.ID, outcome)
				})
			}
		}
	}
}

// targetFor returns a fresh target issue and the caller for the outcomes
// that require one since sk-540.2 (duplicate, superseded), and nothing for
// the others — for tests whose subject is the outcome, not the link.
func (e *outcomeEnv) targetFor(t *testing.T, outcome string) (string, *DependenciesCaller) {
	t.Helper()
	if !outcomeNeedsTarget(strings.TrimSpace(outcome)) {
		return "", nil
	}
	return e.issue(t, "target for "+outcome).ID, &e.caller
}
