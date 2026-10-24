package issues

// close_outcome_target_test.go — sk-489, T003, matrix rows 9 and 10.
//
// The link by target (US-2, R-5, INV-9, SC-007) — as a table over BOTH close
// writers, by the same law as row 8: the target helper is one, and half of
// the table run over one writer is green under any divergence of
// the other.
//
// Seven cells per point: the edge itself and six refusal cells. There are no empty
// cells: the link flag (`closeLink`) is returned only by Store.Close, and this is
// recorded in closeWriter.reportsLink, not left to silence.
//
// The mutation the refusal table was written for: allow the outcome default
// before the check "a target only with an EXPLICIT outcome". Such an implementation
// is green on a transition and on archiving (there the default gives done / wont_do, and
// the target hits the white list anyway) and goes red exactly on the third
// input — a repeated close of an issue already closed as duplicate, where the
// default would return the ALLOWED previous outcome and the link would be created from a call
// that said nothing about the outcome.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
)

// edgeRow — a dependencies row in the form in which it is compared with one
// created by hand (INV-9).
type edgeRow struct {
	Version   int
	CreatedBy uuid.UUID
	Metadata  []byte
}

func readEdge(t *testing.T, pool *pgxpool.Pool, fromID, toID, depType string) (edgeRow, bool) {
	t.Helper()
	var r edgeRow
	err := pool.QueryRow(context.Background(), `
SELECT version, created_by, metadata FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = $3`, fromID, toID, depType).
		Scan(&r.Version, &r.CreatedBy, &r.Metadata)
	if err != nil {
		return edgeRow{}, false
	}
	return r, true
}

// countDependencyCreatedEvents counts link-creation events by pair and
// type. The entity_id of link events is "from::to::type" (see
// dependencies/events.go and the parsing in issues/store.go#ListIssueActivity).
func countDependencyCreatedEvents(t *testing.T, pool *pgxpool.Pool, fromID, toID, depType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE type = 'dependency.created' AND entity_type = 'dep' AND entity_id = $1`,
		fromID+"::"+toID+"::"+depType).Scan(&n); err != nil {
		t.Fatalf("count dependency.created: %v", err)
	}
	return n
}

// closeTargetCase — one cell of the target table.
type closeTargetCase struct {
	name    string
	outcome string
	// wantFrom / wantTo / wantType — the expected edge.
	wantFrom func(issueID, targetID string) (string, string, string)
}

func duplicateEdge(issueID, targetID string) (string, string, string) {
	return issueID, targetID, dependencies.DepDuplicates
}

func supersedesEdge(issueID, targetID string) (string, string, string) {
	// The direction is reverse: the target SUPERSEDES the closing issue.
	return targetID, issueID, dependencies.DepSupersedes
}

// TestCloseTarget_LinkTable — row 9: the edge and the link flag over both
// writers, for both outcomes with a target.
func TestCloseTarget_LinkTable(t *testing.T) {
	cases := []closeTargetCase{
		{name: "duplicate → duplicates A→B", outcome: CloseOutcomeDuplicate, wantFrom: duplicateEdge},
		{name: "superseded → supersedes B→A", outcome: CloseOutcomeSuperseded, wantFrom: supersedesEdge},
	}

	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			env := newOutcomeEnv(t)
			for _, c := range cases {
				c := c
				t.Run(c.name, func(t *testing.T) {
					target := env.issue(t, "target")
					iss := env.issue(t, "closing")
					from, to, depType := c.wantFrom(iss.ID, target.ID)

					attempt := closeAttempt{
						Version: iss.Version, Actor: env.actor,
						Outcome: &c.outcome, Target: target.ID, Caller: &env.caller,
					}
					if w.reportsLink {
						state, err := w.closeLink(t, env.store, iss.ID, attempt)
						if err != nil {
							t.Fatalf("close with a target: %v", err)
						}
						if state != CloseLinkCreated {
							t.Fatalf("link flag %q, want %q", state, CloseLinkCreated)
						}
					} else if err := w.closeIssue(t, env.store, iss.ID, attempt); err != nil {
						t.Fatalf("close with a target: %v", err)
					}

					wantOutcome(t, env.pool, iss.ID, c.outcome)
					if n := countEdges(t, env.pool, from, to, depType); n != 1 {
						t.Fatalf("edges %s %s→%s: %d, want 1", depType, from, to, n)
					}
					// There must be no reverse edge: the direction is part of
					// the assertion, not a detail.
					if n := countEdges(t, env.pool, to, from, depType); n != 0 {
						t.Fatalf("an edge of the reverse direction %s→%s was created", to, from)
					}

					// A repeat of the same operation: the link already exists, a second is not
					// created, the close goes through.
					reread := readCloseStamps(t, env.pool, iss.ID)
					repeat := attempt
					repeat.Version = reread.Version
					if w.reportsLink {
						state, err := w.closeLink(t, env.store, iss.ID, repeat)
						if err != nil {
							t.Fatalf("repeat with a target: %v", err)
						}
						if state != CloseLinkExisted {
							t.Fatalf("link flag on the repeat %q, want %q", state, CloseLinkExisted)
						}
					} else if err := w.closeIssue(t, env.store, iss.ID, repeat); err != nil {
						t.Fatalf("repeat with a target: %v", err)
					}
					if n := countEdges(t, env.pool, from, to, depType); n != 1 {
						t.Fatalf("edges after the repeat %d, want 1", n)
					}
					if n := countDependencyCreatedEvents(t, env.pool, from, to, depType); n != 1 {
						t.Fatalf("dependency.created events: %d, want exactly 1", n)
					}
				})
			}

			t.Run("the edge is indistinguishable from one created by hand", func(t *testing.T) {
				// The same pair of types, two ways: a close with a target and
				// dependencies.Store.Create. The rows must match in
				// everything that distinguishes a link (INV-9).
				byClose := env.issue(t, "close with a target")
				closeTarget := env.issue(t, "close target")
				outcome := CloseOutcomeDuplicate
				if err := w.closeIssue(t, env.store, byClose.ID, closeAttempt{
					Version: byClose.Version, Actor: env.actor,
					Outcome: &outcome, Target: closeTarget.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("close with a target: %v", err)
				}

				manualFrom := env.issue(t, "manual link")
				manualTo := env.issue(t, "manual link target")
				depStore := dependencies.NewStore(env.pool)
				if _, err := depStore.Create(t.Context(), manualFrom.ID,
					dependencies.CreateDepInput{ToID: manualTo.ID, Type: dependencies.DepDuplicates},
					dependencies.CallerAccess{UserID: env.author, Role: "user"},
					dependencies.Actor{UserID: env.author, Username: env.actor.Username, Role: env.actor.Role},
				); err != nil {
					t.Fatalf("manual link: %v", err)
				}

				fromClose, okClose := readEdge(t, env.pool, byClose.ID, closeTarget.ID, dependencies.DepDuplicates)
				fromManual, okManual := readEdge(t, env.pool, manualFrom.ID, manualTo.ID, dependencies.DepDuplicates)
				if !okClose || !okManual {
					t.Fatalf("edge not found: close=%v manual=%v", okClose, okManual)
				}
				if fromClose.Version != fromManual.Version ||
					fromClose.CreatedBy != fromManual.CreatedBy ||
					string(fromClose.Metadata) != string(fromManual.Metadata) {
					t.Fatalf("the close edge differs from the manual one: %+v vs %+v", fromClose, fromManual)
				}
				if n := countDependencyCreatedEvents(t, env.pool,
					byClose.ID, closeTarget.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("the close edge has no dependency.created event")
				}

				// The reverse half of INV-9: creating a link by hand does NOT change
				// the status or outcome of the issues.
				stamps := readCloseStamps(t, env.pool, manualFrom.ID)
				if stamps.Status == StatusClosed || stamps.Outcome != nil {
					t.Fatalf("the manual link closed the issue: %+v", stamps)
				}
			})
		})
	}
}

// TestCloseTarget_AllowedEdges — row 9, allowed edge cases: the target's state
// is not checked at all, and a cross-project visible target for a non-admin passes.
func TestCloseTarget_AllowedEdges(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			env := newOutcomeEnv(t)

			t.Run("target closed without work", func(t *testing.T) {
				target := env.issue(t, "target closed as wont_do")
				wontDo := CloseOutcomeWontDo
				if err := w.closeIssue(t, env.store, target.ID, closeAttempt{
					Version: target.Version, Actor: env.actor, Outcome: &wontDo,
				}); err != nil {
					t.Fatalf("closing the target: %v", err)
				}
				iss := env.issue(t, "duplicate of a closed target")
				dup := CloseOutcomeDuplicate
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &dup, Target: target.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("close with a closed target: %v", err)
				}
				if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("edges %d, want 1", n)
				}
			})

			t.Run("target is the parent of the closing issue", func(t *testing.T) {
				parent := env.issue(t, "parent")
				child, err := env.store.CreateIssue(t.Context(), env.pid, env.prefix, "counter",
					CreateIssueInput{
						Title: "child", Type: "task", Priority: 2,
						Dependencies:       []DependencyEdgeInput{{ToID: parent.ID, Type: dependencies.DepParentChild}},
						DependenciesCaller: &env.caller,
					}, env.author, env.actor)
				if err != nil {
					t.Fatalf("CreateIssue(child): %v", err)
				}
				dup := CloseOutcomeDuplicate
				if err := w.closeIssue(t, env.store, child.ID, closeAttempt{
					Version: child.Version, Actor: env.actor,
					Outcome: &dup, Target: parent.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("closing the child as a duplicate of the parent: %v", err)
				}
				if n := countEdges(t, env.pool, child.ID, parent.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("edges %d, want 1", n)
				}
			})

			t.Run("mutual duplicate", func(t *testing.T) {
				a := env.issue(t, "A")
				b := env.issue(t, "B")
				dup := CloseOutcomeDuplicate
				if err := w.closeIssue(t, env.store, b.ID, closeAttempt{
					Version: b.Version, Actor: env.actor,
					Outcome: &dup, Target: a.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("B is a duplicate of A: %v", err)
				}
				if err := w.closeIssue(t, env.store, a.ID, closeAttempt{
					Version: a.Version, Actor: env.actor,
					Outcome: &dup, Target: b.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("A is a duplicate of B: %v", err)
				}
				if n := countEdges(t, env.pool, a.ID, b.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("A→B edges %d", n)
				}
				if n := countEdges(t, env.pool, b.ID, a.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("B→A edges %d", n)
				}
			})

			// A cell of row 9 "a reopen and an outcome change do NOT
			// remove the edge". The reverse half ("manual link creation does not change the status or
			// outcome") lives in the subcase "the edge is indistinguishable from one
			// created by hand"; this one is the direct one. R-5 and US-4: a link is an
			// independent fact of the graph, a close only CREATES it and
			// never removes it. The mutation the cell turns red:
			// "reopened — so the duplicate is cancelled, tear down the edge" (and the same
			// cleanup when the outcome leaves duplicate).
			t.Run("a reopen and an outcome change do not remove the edge", func(t *testing.T) {
				target := env.issue(t, "target surviving the reopen")
				iss := env.issue(t, "duplicate that will be reopened")
				dup := CloseOutcomeDuplicate
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &dup, Target: target.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("closing as a duplicate: %v", err)
				}
				if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("edges after the close %d, want 1", n)
				}

				closed := readCloseStamps(t, env.pool, iss.ID)
				if _, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
					ExpectedVersion: closed.Version, Status: StatusOpen,
				}, env.actor); err != nil {
					t.Fatalf("reopen: %v", err)
				}
				// The reopen erases the outcome (INV-1) — it does not touch the edge.
				if out := readOutcome(t, env.pool, iss.ID); out != nil {
					t.Fatalf("outcome after the reopen %q", *out)
				}
				if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("the reopen removed the edge: edges %d, want 1", n)
				}

				reopened := readCloseStamps(t, env.pool, iss.ID)
				wontDo := CloseOutcomeWontDo
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: reopened.Version, Actor: env.actor, Outcome: &wontDo,
				}); err != nil {
					t.Fatalf("repeated close with another outcome: %v", err)
				}
				wantOutcome(t, env.pool, iss.ID, CloseOutcomeWontDo)
				if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("the outcome change removed the edge: edges %d, want 1", n)
				}
			})

			t.Run("visible cross-project target for a non-admin", func(t *testing.T) {
				otherPID, otherPrefix := seedProject(t, env.pool, env.author)
				target, err := env.store.CreateIssue(t.Context(), otherPID, otherPrefix, "counter",
					CreateIssueInput{Title: "target in a neighbouring project", Type: "task", Priority: 2},
					env.author, env.actor)
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				iss := env.issue(t, "duplicate of a cross-project target")
				sup := CloseOutcomeSuperseded
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &sup, Target: target.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("closing with a cross-project target: %v", err)
				}
				if n := countEdges(t, env.pool, target.ID, iss.ID, dependencies.DepSupersedes); n != 1 {
					t.Fatalf("supersedes edges %s→%s: %d", target.ID, iss.ID, n)
				}
			})
		})
	}
}

// TestCloseTarget_RefusalCells — row 9: six refusal cells. In each one the
// issue is unchanged and there is no edge.
func TestCloseTarget_RefusalCells(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			env := newOutcomeEnv(t)

			t.Run("invisible target → ErrDependencyTargetNotVisible", func(t *testing.T) {
				stranger := seedUser(t, env.pool)
				strangerActor := HumanActor(stranger, readUsername(t, env.pool, stranger))
				hiddenPID, hiddenPrefix := seedProject(t, env.pool, stranger)
				hidden, err := env.store.CreateIssue(t.Context(), hiddenPID, hiddenPrefix, "counter",
					CreateIssueInput{Title: "foreign target", Type: "task", Priority: 2},
					stranger, strangerActor)
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				iss := env.issue(t, "duplicate of an invisible one")
				dup := CloseOutcomeDuplicate
				err = w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &dup, Target: hidden.ID, Caller: &env.caller,
				})
				if !errors.Is(err, dependencies.ErrDependencyTargetNotVisible) {
					t.Fatalf("want ErrDependencyTargetNotVisible, got %v", err)
				}
				assertUntouched(t, env, iss.ID, iss.Version)
				if n := countEdges(t, env.pool, iss.ID, hidden.ID, dependencies.DepDuplicates); n != 0 {
					t.Fatalf("an edge to an invisible target was created")
				}
			})

			t.Run("nonexistent target → ErrTargetNotFound", func(t *testing.T) {
				iss := env.issue(t, "duplicate of a nonexistent one")
				dup := CloseOutcomeDuplicate
				err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &dup, Target: env.prefix + "-999999", Caller: &env.caller,
				})
				if !errors.Is(err, dependencies.ErrTargetNotFound) {
					t.Fatalf("want ErrTargetNotFound, got %v", err)
				}
				assertUntouched(t, env, iss.ID, iss.Version)
			})

			t.Run("target is the issue itself → check refusal", func(t *testing.T) {
				iss := env.issue(t, "duplicate of itself")
				dup := CloseOutcomeDuplicate
				err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &dup, Target: iss.ID, Caller: &env.caller,
				})
				var verr *ValidationError
				if !errors.As(err, &verr) || verr.Code != "close_target_self" {
					t.Fatalf("want close_target_self, got %v", err)
				}
				assertUntouched(t, env, iss.ID, iss.Version)
			})

			for _, outcome := range []string{CloseOutcomeDone, CloseOutcomeWontDo} {
				outcome := outcome
				t.Run("target with an explicit "+outcome+" → check refusal", func(t *testing.T) {
					target := env.issue(t, "target")
					iss := env.issue(t, "closing with "+outcome)
					err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: iss.Version, Actor: env.actor,
						Outcome: &outcome, Target: target.ID, Caller: &env.caller,
					})
					var verr *ValidationError
					if !errors.As(err, &verr) || verr.Code != "close_target_not_allowed" {
						t.Fatalf("want close_target_not_allowed, got %v", err)
					}
					assertUntouched(t, env, iss.ID, iss.Version)
					if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 0 {
						t.Fatalf("an edge was created with outcome %s", outcome)
					}
				})
			}

			// The cell "target without an explicit outcome" — ONE result on three inputs.
			t.Run("target without an explicit outcome → refusal on all inputs", func(t *testing.T) {
				assertRefused := func(t *testing.T, issueID string, version int, in closeAttempt) {
					t.Helper()
					err := w.closeIssue(t, env.store, issueID, in)
					var verr *ValidationError
					if !errors.As(err, &verr) || verr.Code != "close_target_requires_outcome" {
						t.Fatalf("want close_target_requires_outcome, got %v", err)
					}
				}

				t.Run("transition (the default would give done)", func(t *testing.T) {
					target := env.issue(t, "transition target")
					iss := env.issue(t, "transition with a target without an outcome")
					assertRefused(t, iss.ID, iss.Version, closeAttempt{
						Version: iss.Version, Actor: env.actor,
						Target: target.ID, Caller: &env.caller,
					})
					assertUntouched(t, env, iss.ID, iss.Version)
					if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 0 {
						t.Fatalf("an edge was created without an explicit outcome")
					}
				})

				t.Run("repeated close of an already-duplicate (the default would give duplicate)", func(t *testing.T) {
					first := env.issue(t, "first target")
					iss := env.issue(t, "already a duplicate")
					dup := CloseOutcomeDuplicate
					if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: iss.Version, Actor: env.actor,
						Outcome: &dup, Target: first.ID, Caller: &env.caller,
					}); err != nil {
						t.Fatalf("first close: %v", err)
					}
					closed := readCloseStamps(t, env.pool, iss.ID)
					second := env.issue(t, "second target")
					assertRefused(t, iss.ID, closed.Version, closeAttempt{
						Version: closed.Version, Actor: env.actor,
						Target: second.ID, Caller: &env.caller,
					})
					if n := countEdges(t, env.pool, iss.ID, second.ID, dependencies.DepDuplicates); n != 0 {
						t.Fatalf("an edge to the second target was created without an explicit outcome")
					}
					// The outcome is as before, the issue is still closed with the same one.
					wantOutcome(t, env.pool, iss.ID, CloseOutcomeDuplicate)
				})

				if !w.supportsArchve {
					return
				}
				t.Run("archiving with a target without an outcome", func(t *testing.T) {
					target := env.issue(t, "archiving target")
					iss := env.issue(t, "archiving with a target without an outcome")
					assertRefused(t, iss.ID, iss.Version, closeAttempt{
						Version: iss.Version, Actor: env.actor,
						Target: target.ID, Caller: &env.caller, Archive: true,
					})
					assertUntouched(t, env, iss.ID, iss.Version)
				})
			})

			t.Run("version conflict with a correct target → neither a close nor an edge", func(t *testing.T) {
				target := env.issue(t, "correct target")
				iss := env.issue(t, "stale version")
				dup := CloseOutcomeDuplicate
				err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version + 7, Actor: env.actor,
					Outcome: &dup, Target: target.ID, Caller: &env.caller,
				})
				var vc *ErrVersionConflict
				if !errors.As(err, &vc) {
					t.Fatalf("want a version conflict, got %v", err)
				}
				assertUntouched(t, env, iss.ID, iss.Version)
				if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 0 {
					t.Fatalf("an edge was created on a version conflict — atomicity violated")
				}
			})

			t.Run("the visibility key is the system role, not Actor.Role", func(t *testing.T) {
				stranger := seedUser(t, env.pool)
				strangerActor := HumanActor(stranger, readUsername(t, env.pool, stranger))
				hiddenPID, hiddenPrefix := seedProject(t, env.pool, stranger)
				hidden, err := env.store.CreateIssue(t.Context(), hiddenPID, hiddenPrefix, "counter",
					CreateIssueInput{Title: "target in an invisible project", Type: "task", Priority: 2},
					stranger, strangerActor)
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				sup := CloseOutcomeSuperseded

				// A non-admin: refusal. The Actor is the same as below.
				denied := env.issue(t, "non-admin")
				if err := w.closeIssue(t, env.store, denied.ID, closeAttempt{
					Version: denied.Version, Actor: env.actor,
					Outcome: &sup, Target: hidden.ID,
					Caller: &DependenciesCaller{UserID: env.author, Role: "user"},
				}); !errors.Is(err, dependencies.ErrDependencyTargetNotVisible) {
					t.Fatalf("non-admin: want a visibility refusal, got %v", err)
				}

				// An admin (SYSTEM role) with THE SAME Actor: passes. Since sk-674.18
				// T008 the role is read from users by the access relation, not from
				// DependenciesCaller.Role (D-2): the author becomes an admin in the DB
				// for the duration of this attempt.
				setRole := func(role string) {
					t.Helper()
					if _, err := env.pool.Exec(t.Context(), `UPDATE users SET role = $2 WHERE id = $1`, env.author, role); err != nil {
						t.Fatalf("author's role %s: %v", role, err)
					}
				}
				setRole("admin")
				defer setRole("user")
				allowed := env.issue(t, "admin")
				if err := w.closeIssue(t, env.store, allowed.ID, closeAttempt{
					Version: allowed.Version, Actor: env.actor,
					Outcome: &sup, Target: hidden.ID,
					Caller: &DependenciesCaller{UserID: env.author, Role: "admin"},
				}); err != nil {
					t.Fatalf("admin: closing with a target invisible to a non-admin: %v", err)
				}
				if n := countEdges(t, env.pool, hidden.ID, allowed.ID, dependencies.DepSupersedes); n != 1 {
					t.Fatalf("admin: edges %d, want 1", n)
				}
			})
		})
	}
}

// assertUntouched — the issue is not closed, there is no outcome, the version is as before.
func assertUntouched(t *testing.T, env *outcomeEnv, issueID string, version int) {
	t.Helper()
	got := readCloseStamps(t, env.pool, issueID)
	if got.Status == StatusClosed {
		t.Fatalf("%s: the issue is closed, and the refusal was supposed not to touch it", issueID)
	}
	if got.Outcome != nil {
		t.Fatalf("%s: outcome %q after the refusal", issueID, *got.Outcome)
	}
	if got.Version != version {
		t.Fatalf("%s: version %d, want the previous %d", issueID, got.Version, version)
	}
}

// TestCloseTarget_RaceWithManualDependency — row 10: a link race.
//
// A close with a target runs in parallel with manual creation of THE SAME edge.
// There are exactly two allowed close outcomes: it went through (`created` / `existed`)
// or got a version conflict — and then the issue is not closed and has no outcome
// (INV-1). Three are not allowed: `internal_error`, ErrDependencyExists
// out of the close and "closed without an edge".
func TestCloseTarget_RaceWithManualDependency(t *testing.T) {
	directions := []struct {
		name    string
		outcome string
		edge    func(issueID, targetID string) (string, string, string)
	}{
		{"duplicate", CloseOutcomeDuplicate, duplicateEdge},
		{"superseded", CloseOutcomeSuperseded, supersedesEdge},
	}

	for _, d := range directions {
		d := d
		t.Run(d.name, func(t *testing.T) {
			env := newOutcomeEnv(t)
			depStore := dependencies.NewStore(env.pool)

			target := env.issue(t, "race target")
			iss := env.issue(t, "closing in a race")
			from, to, depType := d.edge(iss.ID, target.ID)

			var (
				wg           sync.WaitGroup
				closeErr     atomic.Value
				closeState   atomic.Value
				manualErr    atomic.Value
				closeSucceed atomic.Bool
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, state, err := env.store.CloseWithResult(context.Background(), CloseInput{
					ID: iss.ID, ExpectedVersion: iss.Version, Reason: "race",
					Actor: env.actor, CloseOutcome: &d.outcome,
					CloseTarget: target.ID, DependenciesCaller: &env.caller,
				})
				if err != nil {
					closeErr.Store(err)
					return
				}
				closeSucceed.Store(true)
				closeState.Store(state)
			}()
			go func() {
				defer wg.Done()
				_, err := depStore.Create(context.Background(), from,
					dependencies.CreateDepInput{ToID: to, Type: depType},
					dependencies.CallerAccess{UserID: env.author, Role: "user"},
					dependencies.Actor{UserID: env.author, Username: env.actor.Username, Role: env.actor.Role},
				)
				if err != nil {
					manualErr.Store(err)
				}
			}()
			wg.Wait()

			// Manual link: either went through or "already exists" (it was created by
			// the close). Any other outcome is a separate breakage.
			if v := manualErr.Load(); v != nil {
				err := v.(error)
				if !errors.Is(err, dependencies.ErrDependencyExists) &&
					!errors.Is(err, dependencies.ErrSerializationRetry) {
					t.Fatalf("manual link: unexpected error %v", err)
				}
			}

			if closeSucceed.Load() {
				state := closeState.Load().(CloseLinkState)
				if state != CloseLinkCreated && state != CloseLinkExisted {
					t.Fatalf("the close went through with link flag %q", state)
				}
				wantOutcome(t, env.pool, iss.ID, d.outcome)
			} else {
				err := closeErr.Load().(error)
				var vc *ErrVersionConflict
				if !errors.As(err, &vc) {
					t.Fatalf("the close lost the race with error %v — want a version conflict "+
						"(ErrDependencyExists and internal_error must not come out of the close)", err)
				}
				stamps := readCloseStamps(t, env.pool, iss.ID)
				if stamps.Status == StatusClosed || stamps.Outcome != nil {
					t.Fatalf("the close returned a version conflict, but the issue is closed: %+v", stamps)
				}
				// A repeat of the close after the lost race sees the edge and
				// answers `existed` — there is no closed duplicate without a link
				// (R-5).
				fresh := readCloseStamps(t, env.pool, iss.ID)
				_, state, rerr := env.store.CloseWithResult(t.Context(), CloseInput{
					ID: iss.ID, ExpectedVersion: fresh.Version, Reason: "repeat after the race",
					Actor: env.actor, CloseOutcome: &d.outcome,
					CloseTarget: target.ID, DependenciesCaller: &env.caller,
				})
				if rerr != nil {
					t.Fatalf("repeat after the lost race: %v", rerr)
				}
				if state != CloseLinkExisted {
					t.Fatalf("repeat after the race: link flag %q, want existed", state)
				}
			}

			// Exactly one edge under any outcome.
			if n := countEdges(t, env.pool, from, to, depType); n != 1 {
				t.Fatalf("edges %s %s→%s: %d, want exactly 1", depType, from, to, n)
			}
		})
	}
}

// TestCloseTarget_RaceLostAtTheSeam — row 10, the branch "the close lost the
// race", deterministically.
//
// Why a separate test next to TestCloseTarget_RaceWithManualDependency.
// That one puts real goroutines and accepts BOTH outcomes — row 10
// requires it — but on natural timing the manual link gets in first
// always, and the losing branch is NEVER executed. Measured by two
// critics of the wave independently: a mutant with the translation of
// errCloseLinkRaceLost into a version conflict removed gave a green run. A guard
// that is checked by nothing is exactly the class of defect this
// feature already caught in wave A/1.
//
// Here the window is opened explicitly: the closeLinkRaceSeam seam stands between "the edge is not
// there yet" and the insert, and the test hangs on it an insert of THE SAME edge from another
// connection. Then the close transaction's insert must run into
// uniqueness (dependencies.classifyInsertError → ErrDependencyExists),
// the writer must translate that into a version conflict, and the state must stay
// untouched (INV-1: no close, so no outcome). Exactly
// three outcomes of row 10 are forbidden: internal_error out, ErrDependencyExists out
// and "closed without an edge".
//
// A repeat after the lost race must see the edge and answer
// `existed` — R-5: there is no closed duplicate without a link.
func TestCloseTarget_RaceLostAtTheSeam(t *testing.T) {
	// seamCreatesEdge returns a hook that ONCE creates the edge
	// from another connection and removes itself: the repeat of the close inside the same
	// subcase must go without the window.
	seamCreatesEdge := func(t *testing.T, env *outcomeEnv) func(context.Context, string, string, string) {
		t.Helper()
		var fired bool
		return func(ctx context.Context, fromID, toID, depType string) {
			if fired {
				return
			}
			fired = true
			// An insert by raw SQL, not through dependencies.Store.Create:
			// the competitor is needed here exactly as a row in the table. Create
			// would also read issues, which the close transaction has already
			// changed, and the predictable uniqueness would turn into a
			// serialization race — i.e. into another path, already covered by
			// degradeSerializationAbort.
			if _, err := env.pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, created_by)
VALUES ($1, $2, $3, $4)`, fromID, toID, depType, env.author); err != nil {
				t.Errorf("the competitor could not insert the edge %s %s→%s: %v", depType, fromID, toID, err)
			}
		}
	}

	withSeam := func(t *testing.T, hook func(context.Context, string, string, string), body func()) {
		t.Helper()
		prev := closeLinkRaceSeam
		closeLinkRaceSeam = hook
		defer func() { closeLinkRaceSeam = prev }()
		body()
	}

	t.Run("close writers", func(t *testing.T) {
		for _, w := range closeWriters() {
			t.Run(w.name, func(t *testing.T) {
				env := newOutcomeEnv(t)
				target := env.issue(t, "seam target")
				iss := env.issue(t, "closing at the seam")
				outcome := CloseOutcomeDuplicate
				from, to, depType := duplicateEdge(iss.ID, target.ID)

				var err error
				withSeam(t, seamCreatesEdge(t, env), func() {
					err = w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: iss.Version, Actor: env.actor,
						Outcome: &outcome, Target: target.ID, Caller: &env.caller,
					})
				})

				var vc *ErrVersionConflict
				if !errors.As(err, &vc) {
					t.Fatalf("the close lost the race with error %v — want a version conflict "+
						"(ErrDependencyExists and internal_error must not come out of the close)", err)
				}
				if errors.Is(err, dependencies.ErrDependencyExists) {
					t.Fatalf("ErrDependencyExists came out: %v", err)
				}
				assertUntouched(t, env, iss.ID, iss.Version)
				if n := countEdges(t, env.pool, from, to, depType); n != 1 {
					t.Fatalf("edges %s→%s: %d, want exactly one (the one inserted by the competitor)", from, to, n)
				}

				// A repeat without the window: the edge already exists, the close must go through
				// and say `existed`.
				fresh := readCloseStamps(t, env.pool, iss.ID)
				if w.reportsLink {
					state, rerr := w.closeLink(t, env.store, iss.ID, closeAttempt{
						Version: fresh.Version, Actor: env.actor,
						Outcome: &outcome, Target: target.ID, Caller: &env.caller,
					})
					if rerr != nil {
						t.Fatalf("repeat after the lost race: %v", rerr)
					}
					if state != CloseLinkExisted {
						t.Fatalf("repeat after the race: link flag %q, want existed", state)
					}
				} else if rerr := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: fresh.Version, Actor: env.actor,
					Outcome: &outcome, Target: target.ID, Caller: &env.caller,
				}); rerr != nil {
					t.Fatalf("repeat after the lost race: %v", rerr)
				}
				wantOutcome(t, env.pool, iss.ID, outcome)
				if n := countEdges(t, env.pool, from, to, depType); n != 1 {
					t.Fatalf("after the repeat edges %s→%s: %d, want one", from, to, n)
				}
			})
		}
	})

	// The third target writer is the outcome edit of a closed issue (T004). The translation
	// there is the same and lives in the same place, so it is checked by the same
	// cell: a divergence of writers is exactly what this whole file looks for.
	t.Run("Store.PatchIssue", func(t *testing.T) {
		env := newOutcomeEnv(t)
		target := env.issue(t, "seam target (edit)")
		iss := closedDoneIssue(t, env)
		before := readCloseStamps(t, env.pool, iss.ID)
		from, to, depType := duplicateEdge(iss.ID, target.ID)

		var err error
		withSeam(t, seamCreatesEdge(t, env), func() {
			err = patchOutcome(t, env, iss.ID, before.Version, CloseOutcomeDuplicate, target.ID, env.actor)
		})

		var vc *ErrVersionConflict
		if !errors.As(err, &vc) {
			t.Fatalf("the edit lost the race with error %v — want a version conflict", err)
		}
		if errors.Is(err, dependencies.ErrDependencyExists) {
			t.Fatalf("ErrDependencyExists came out: %v", err)
		}
		after := readCloseStamps(t, env.pool, iss.ID)
		if after.Version != before.Version || after.Outcome == nil || *after.Outcome != CloseOutcomeDone {
			t.Fatalf("the row changed after the refusal: %+v (want version %d and outcome done)",
				after, before.Version)
		}
		if n := countEdges(t, env.pool, from, to, depType); n != 1 {
			t.Fatalf("edges %s→%s: %d, want exactly one", from, to, n)
		}

		// A repeat without the window: the link already exists, the edit must go through.
		fresh := readCloseStamps(t, env.pool, iss.ID)
		if rerr := patchOutcome(t, env, iss.ID, fresh.Version,
			CloseOutcomeDuplicate, target.ID, env.actor); rerr != nil {
			t.Fatalf("repeat of the edit after the lost race: %v", rerr)
		}
		wantOutcome(t, env.pool, iss.ID, CloseOutcomeDuplicate)
	})
}
