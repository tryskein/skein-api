package dependencies_test

// outcome_link_delete_test.go — sk-540.8.
//
// Contract under test (acceptance of sk-540.8): removing the LAST edge that
// justifies a close outcome is refused and writes nothing — the last
// outgoing `duplicates` edge of an issue closed as "duplicate", the last
// incoming `supersedes` edge of an issue closed as "superseded". With a
// sibling edge of the same type and direction left, the removal goes
// through; after the outcome changes or the issue is reopened it goes
// through too. The count and the DELETE share one transaction, and the two
// races named in the acceptance are proven by the END STATE: a closed
// duplicate / superseded issue never ends up without its edge.
//
// The issues are closed through the real issues writer (CloseWithResult,
// PatchIssue, UpdateStatus), not by raw SQL, so the outcome and the edge in
// these tests are exactly what production writes.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// linkEnv is one author, one project, and the two stores under test.
type linkEnv struct {
	pool   *pgxpool.Pool
	issues *issues.Store
	deps   *dependencies.Store
	author uuid.UUID
	actor  issues.Actor
	dActor dependencies.Actor
	caller issues.DependenciesCaller
	pid    uuid.UUID
	prefix string
}

func newLinkEnv(t *testing.T) *linkEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	author, name := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, author, "olk")
	return &linkEnv{
		pool:   pool,
		issues: issues.NewStore(pool),
		deps:   dependencies.NewStore(pool),
		author: author,
		actor:  issues.HumanActor(author, name),
		dActor: dependencies.Actor{UserID: author, Username: name, Role: "human"},
		caller: issues.DependenciesCaller{UserID: author, Role: "user"},
		pid:    pid,
		prefix: prefix,
	}
}

// callerCtx is a request context of the author, a member of the project:
// the closed issue is inside the caller's horizon, so a refusal names it.
// Built from context.Background so goroutines outliving a subtest can use it.
func (e *linkEnv) callerCtx() context.Context {
	return auth.WithUserRole(auth.WithUserID(context.Background(), e.author), "user")
}

func (e *linkEnv) issue(t *testing.T, title string) *issues.IssueDetail {
	t.Helper()
	dto, err := e.issues.CreateIssue(t.Context(), e.pid, e.prefix, "counter",
		issues.CreateIssueInput{Title: title, Type: "task", Priority: 2},
		e.author, e.actor)
	if err != nil {
		t.Fatalf("CreateIssue(%s): %v", title, err)
	}
	return dto
}

func (e *linkEnv) version(t *testing.T, id string) int {
	t.Helper()
	var v int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version %s: %v", id, err)
	}
	return v
}

func (e *linkEnv) outcome(t *testing.T, id string) *string {
	t.Helper()
	var out *string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT close_outcome FROM issues WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatalf("read close_outcome %s: %v", id, err)
	}
	return out
}

// closeWithTarget closes id with the given outcome and target through the
// production close writer; the writer creates the edge in the same tx.
func (e *linkEnv) closeWithTarget(t *testing.T, id, outcome, target string) {
	t.Helper()
	if _, _, err := e.issues.CloseWithResult(t.Context(), issues.CloseInput{
		ID: id, ExpectedVersion: e.version(t, id), Reason: "closed by test",
		Actor: e.actor, CloseOutcome: &outcome, CloseTarget: target,
		DependenciesCaller: &e.caller,
	}); err != nil {
		t.Fatalf("close %s as %s → %s: %v", id, outcome, target, err)
	}
}

func (e *linkEnv) addEdge(t *testing.T, fromID, toID, depType string) {
	t.Helper()
	if _, err := e.deps.Create(t.Context(), fromID,
		dependencies.CreateDepInput{ToID: toID, Type: depType},
		dependencies.CallerAccess{UserID: e.author, Role: "user"}, e.dActor); err != nil {
		t.Fatalf("add %s %s→%s: %v", depType, fromID, toID, err)
	}
}

func (e *linkEnv) countEdges(t *testing.T, fromID, toID, depType string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = $3`,
		fromID, toID, depType).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	return n
}

func (e *linkEnv) countDeletedEvents(t *testing.T, fromID, toID, depType string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE type = 'dependency.deleted' AND entity_type = 'dep' AND entity_id = $1`,
		fromID+"::"+toID+"::"+depType).Scan(&n); err != nil {
		t.Fatalf("count dependency.deleted: %v", err)
	}
	return n
}

// outcomeDirection names one of the two edge-backed outcomes and where the
// closed issue sits on its edge.
type outcomeDirection struct {
	name    string
	outcome string
	depType string
	// edge returns (from, to) of the edge between the closed issue and
	// another issue — the closed issue is FROM for duplicates and TO for
	// supersedes (the replacement supersedes it).
	edge func(closed, other string) (string, string)
}

func outcomeDirections() []outcomeDirection {
	return []outcomeDirection{
		{
			name: "duplicate", outcome: issues.CloseOutcomeDuplicate, depType: dependencies.DepDuplicates,
			edge: func(closed, other string) (string, string) { return closed, other },
		},
		{
			name: "superseded", outcome: issues.CloseOutcomeSuperseded, depType: dependencies.DepSupersedes,
			edge: func(closed, other string) (string, string) { return other, closed },
		},
	}
}

// wantRefused asserts err is the outcome-link refusal naming the closed
// issue, and that the edge and the audit trail are untouched.
func wantRefused(t *testing.T, e *linkEnv, err error, d outcomeDirection, closedID, from, to string) {
	t.Helper()
	var linkErr *dependencies.ErrCloseOutcomeLinkRequired
	if !errors.As(err, &linkErr) {
		t.Fatalf("Delete(%s %s→%s) = %v, want ErrCloseOutcomeLinkRequired", d.depType, from, to, err)
	}
	if linkErr.IssueID != closedID || linkErr.CloseOutcome != d.outcome || linkErr.Type != d.depType {
		t.Fatalf("refusal = %+v, want issue %s, outcome %s, type %s",
			*linkErr, closedID, d.outcome, d.depType)
	}
	if n := e.countEdges(t, from, to, d.depType); n != 1 {
		t.Fatalf("refused Delete removed the edge %s→%s: %d rows left, want 1", from, to, n)
	}
	if n := e.countDeletedEvents(t, from, to, d.depType); n != 0 {
		t.Fatalf("refused Delete wrote %d dependency.deleted events, want 0", n)
	}
}

// TestDelete_OutcomeLink_Table — the non-concurrent cells of the
// acceptance, for both edge-backed outcomes.
func TestDelete_OutcomeLink_Table(t *testing.T) {
	for _, d := range outcomeDirections() {
		t.Run(d.name, func(t *testing.T) {
			e := newLinkEnv(t)

			t.Run("last edge is refused", func(t *testing.T) {
				target := e.issue(t, "target")
				closed := e.issue(t, "closed")
				e.closeWithTarget(t, closed.ID, d.outcome, target.ID)
				from, to := d.edge(closed.ID, target.ID)

				err := e.deps.Delete(e.callerCtx(), from, to, d.depType, e.dActor)
				wantRefused(t, e, err, d, closed.ID, from, to)
			})

			t.Run("one of two edges goes, the remaining one is refused", func(t *testing.T) {
				first := e.issue(t, "first target")
				second := e.issue(t, "second target")
				closed := e.issue(t, "closed")
				e.closeWithTarget(t, closed.ID, d.outcome, first.ID)
				f2, t2 := d.edge(closed.ID, second.ID)
				e.addEdge(t, f2, t2, d.depType)

				f1, t1 := d.edge(closed.ID, first.ID)
				if err := e.deps.Delete(e.callerCtx(), f1, t1, d.depType, e.dActor); err != nil {
					t.Fatalf("Delete of one of two edges: %v", err)
				}
				if n := e.countEdges(t, f1, t1, d.depType); n != 0 {
					t.Fatalf("edge %s→%s still present after Delete", f1, t1)
				}

				err := e.deps.Delete(e.callerCtx(), f2, t2, d.depType, e.dActor)
				wantRefused(t, e, err, d, closed.ID, f2, t2)
			})

			t.Run("after the outcome changes the last edge goes", func(t *testing.T) {
				target := e.issue(t, "target")
				closed := e.issue(t, "closed")
				e.closeWithTarget(t, closed.ID, d.outcome, target.ID)
				wontDo := issues.CloseOutcomeWontDo
				if _, err := e.issues.PatchIssue(t.Context(), closed.ID, issues.PatchIssueInput{
					ExpectedVersion: e.version(t, closed.ID), CloseOutcome: &wontDo,
				}, e.actor); err != nil {
					t.Fatalf("change outcome to wont_do: %v", err)
				}
				from, to := d.edge(closed.ID, target.ID)
				if err := e.deps.Delete(e.callerCtx(), from, to, d.depType, e.dActor); err != nil {
					t.Fatalf("Delete after outcome change: %v", err)
				}
				if n := e.countEdges(t, from, to, d.depType); n != 0 {
					t.Fatalf("edge %s→%s still present after Delete", from, to)
				}
			})

			t.Run("after reopen the last edge goes", func(t *testing.T) {
				target := e.issue(t, "target")
				closed := e.issue(t, "closed")
				e.closeWithTarget(t, closed.ID, d.outcome, target.ID)
				if _, err := e.issues.UpdateStatus(t.Context(), closed.ID, issues.UpdateStatusInput{
					ExpectedVersion: e.version(t, closed.ID), Status: issues.StatusOpen,
				}, e.actor); err != nil {
					t.Fatalf("reopen: %v", err)
				}
				from, to := d.edge(closed.ID, target.ID)
				if err := e.deps.Delete(e.callerCtx(), from, to, d.depType, e.dActor); err != nil {
					t.Fatalf("Delete after reopen: %v", err)
				}
			})

			// The guard reads the CLOSED end of the edge only. An edge of the
			// same type whose other end is closed with the same outcome is
			// not that issue's justification and must go through.
			t.Run("edge at the other end of a closed issue goes", func(t *testing.T) {
				target := e.issue(t, "target")
				closed := e.issue(t, "closed")
				e.closeWithTarget(t, closed.ID, d.outcome, target.ID)
				stranger := e.issue(t, "stranger")
				// The closed issue sits on the "other" side of this edge.
				from, to := d.edge(stranger.ID, closed.ID)
				e.addEdge(t, from, to, d.depType)
				if err := e.deps.Delete(e.callerCtx(), from, to, d.depType, e.dActor); err != nil {
					t.Fatalf("Delete of an edge whose closed end is not the outcome side: %v", err)
				}
			})

			t.Run("edge of another type goes", func(t *testing.T) {
				target := e.issue(t, "target")
				closed := e.issue(t, "closed")
				e.closeWithTarget(t, closed.ID, d.outcome, target.ID)
				e.addEdge(t, closed.ID, target.ID, dependencies.DepRelated)
				if err := e.deps.Delete(e.callerCtx(), closed.ID, target.ID, dependencies.DepRelated, e.dActor); err != nil {
					t.Fatalf("Delete of a related edge: %v", err)
				}
			})
		})
	}
}

// TestDelete_OutcomeLink_RaceWithClose — acceptance race (1): "remove the
// edge" against "close as duplicate/superseded with this target".
//
// The delete is held between its guard and its DELETE (the issue is still
// open there, so the guard lets it through) while the close commits and
// finds the edge already present. Whatever the transactions answer, the END
// STATE must not be a closed duplicate/superseded issue without its edge.
// A retry of the losing delete must then be refused by the guard.
func TestDelete_OutcomeLink_RaceWithClose(t *testing.T) {
	for _, d := range outcomeDirections() {
		t.Run(d.name, func(t *testing.T) {
			e := newLinkEnv(t)
			target := e.issue(t, "target")
			iss := e.issue(t, "closed in the race")
			from, to := d.edge(iss.ID, target.ID)
			e.addEdge(t, from, to, d.depType)

			reached := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			dependencies.SetDeleteGuardSeamForTest(t, func(context.Context, string, string, string) {
				once.Do(func() {
					close(reached)
					<-release
				})
			})

			deleteErr := make(chan error, 1)
			go func() {
				deleteErr <- e.deps.Delete(e.callerCtx(), from, to, d.depType, e.dActor)
			}()
			<-reached

			// The close runs to completion while the delete sits between its
			// check and its write.
			_, _, closeErr := e.issues.CloseWithResult(t.Context(), issues.CloseInput{
				ID: iss.ID, ExpectedVersion: iss.Version, Reason: "race",
				Actor: e.actor, CloseOutcome: &d.outcome, CloseTarget: target.ID,
				DependenciesCaller: &e.caller,
			})
			close(release)
			derr := <-deleteErr

			if closeErr != nil {
				t.Fatalf("close in the race: %v", closeErr)
			}
			if derr == nil {
				t.Fatalf("both the close and the delete committed")
			}
			if !errors.Is(derr, dependencies.ErrSerializationRetry) {
				t.Fatalf("delete lost the race with %v, want ErrSerializationRetry", derr)
			}

			// End state: closed with the outcome AND the edge still there.
			if got := e.outcome(t, iss.ID); got == nil || *got != d.outcome {
				t.Fatalf("outcome after the race = %v, want %s", got, d.outcome)
			}
			if n := e.countEdges(t, from, to, d.depType); n != 1 {
				t.Fatalf("closed %s issue left with %d edges %s→%s, want 1", d.outcome, n, from, to)
			}

			// The retry sees the closed issue and is refused.
			err := e.deps.Delete(e.callerCtx(), from, to, d.depType, e.dActor)
			wantRefused(t, e, err, d, iss.ID, from, to)
		})
	}
}

// TestDelete_OutcomeLink_RaceTwoLastEdges — acceptance race (2): the two
// last edges of a closed issue removed in parallel.
//
// Both deletes are held after their guards, so each has counted the other
// edge as a surviving sibling before either writes — the write skew a
// check outside the transaction (or under a weaker isolation) would let
// through. The END STATE must keep at least one edge; the loser's retry is
// refused.
func TestDelete_OutcomeLink_RaceTwoLastEdges(t *testing.T) {
	for _, d := range outcomeDirections() {
		t.Run(d.name, func(t *testing.T) {
			e := newLinkEnv(t)
			first := e.issue(t, "first target")
			second := e.issue(t, "second target")
			closed := e.issue(t, "closed")
			e.closeWithTarget(t, closed.ID, d.outcome, first.ID)
			f2, t2 := d.edge(closed.ID, second.ID)
			e.addEdge(t, f2, t2, d.depType)
			f1, t1 := d.edge(closed.ID, first.ID)

			// Barrier of two: neither delete writes until both passed their
			// guard.
			var arrived sync.WaitGroup
			arrived.Add(2)
			dependencies.SetDeleteGuardSeamForTest(t, func(context.Context, string, string, string) {
				arrived.Done()
				arrived.Wait()
			})

			errs := make(chan error, 2)
			for _, edge := range [][2]string{{f1, t1}, {f2, t2}} {
				go func() {
					errs <- e.deps.Delete(e.callerCtx(), edge[0], edge[1], d.depType, e.dActor)
				}()
			}
			var ok, retry int
			for range 2 {
				err := <-errs
				switch {
				case err == nil:
					ok++
				case errors.Is(err, dependencies.ErrSerializationRetry):
					retry++
				default:
					t.Fatalf("parallel delete: unexpected error %v", err)
				}
			}
			if ok != 1 || retry != 1 {
				t.Fatalf("parallel deletes: %d succeeded, %d lost the race; want exactly one of each", ok, retry)
			}

			left := e.countEdges(t, f1, t1, d.depType) + e.countEdges(t, f2, t2, d.depType)
			if left != 1 {
				t.Fatalf("closed %s issue left with %d edges, want 1", d.outcome, left)
			}
			if got := e.outcome(t, closed.ID); got == nil || *got != d.outcome {
				t.Fatalf("outcome = %v, want %s", got, d.outcome)
			}

			// The retry of the loser is now the last edge — refused.
			dependencies.SetDeleteGuardSeamForTest(t, nil)
			lf, lt := f1, t1
			if e.countEdges(t, f1, t1, d.depType) == 0 {
				lf, lt = f2, t2
			}
			err := e.deps.Delete(e.callerCtx(), lf, lt, d.depType, e.dActor)
			wantRefused(t, e, err, d, closed.ID, lf, lt)
		})
	}
}
