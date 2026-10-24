package dependencies

// ready_close_outcome_test.go — sk-489, T003, matrix row 13
// (the "readiness" half).
//
// INV-7: readiness does NOT depend on the close outcome. A closed blocker
// stops gating the dependent under ANY of the four outcomes - "done" has no
// special privilege here.
//
// Why this is a separate test and not a cell in issues. There are two "blocked" flags
// in the product, and different predicates compute them: readiness
// (Store.ListReady, without the reader's horizon) and the row/card flag
// (issues/store.go#isBlockedExprFor, with the horizon). An assertion about one
// says nothing about the other; the second half of row 13 is in
// issues/close_outcome_writers_test.go#TestCloseOutcome_INV7_BlockerReleasesAtAnyOutcome.
//
// The mutation this test turns red: teaching the readiness predicate to read the outcome -
// for example to count a blocker closed as `duplicate` as gating (the common
// "guess about the meaning": a duplicate is not done, after all). Three cells of four
// would go red; the `done` cell would stay green, which is exactly why the check
// is a table and not a single value.

import (
	"context"
	"testing"

	"skein-api/internal/testsupport"
)

// The blocker is closed by a raw write, not by the writer: the writer lives in
// internal/issues, and that package imports this one. For row 13 this is not
// needed either - the assertion is about the readiness PREDICATE, not about the close path:
// the column value, however it got there, must not
// affect readiness. Trigger 000050 on a close without an outcome would set `done`,
// so the outcome is written explicitly in the same UPDATE.
func TestListReady_ClosedBlockerReleasesAtAnyOutcome(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	store := NewStore(pool)

	for _, outcome := range []string{"done", "wont_do", "duplicate", "superseded"} {
		outcome := outcome
		t.Run(outcome, func(t *testing.T) {
			pid, prefix := testsupport.SeedProject(t, pool, owner, "rdy"+outcome[:3])
			blocked := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "blocked", "open", 2)
			blocker := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-2", "blocker", "open", 2)
			testsupport.SeedDep(t, pool, blocked, blocker, DepBlocks, owner)

			// Precondition: while the blocker is open, the dependent is not in readiness.
			// Without it "appeared after the close" would be proven by it
			// having been there all along.
			rows, err := store.ListReady(ctx, pid, ReadyFilters{}, 50)
			if err != nil {
				t.Fatalf("ListReady (before the close): %v", err)
			}
			if containsID(rows, blocked) {
				t.Fatalf("precondition: %s is in readiness while the blocker is open", blocked)
			}

			if _, err := pool.Exec(ctx, `
UPDATE issues
   SET status = 'closed', closed_at = now(), closed_reason = 'outcome '||$2,
       close_outcome = $2, version = version + 1
 WHERE id = $1`, blocker, outcome); err != nil {
				t.Fatalf("closing the blocker with outcome %s: %v", outcome, err)
			}

			rows, err = store.ListReady(ctx, pid, ReadyFilters{}, 50)
			if err != nil {
				t.Fatalf("ListReady (after the close): %v", err)
			}
			if !containsID(rows, blocked) {
				t.Fatalf("%s not in readiness with a blocker closed with outcome %s — "+
					"INV-7: a closed blocker releases the block under ANY outcome", blocked, outcome)
			}
		})
	}
}

func containsID(rows []IssueSummary, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
