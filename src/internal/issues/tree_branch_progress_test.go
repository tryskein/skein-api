package issues

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
)

// sk-461 — branch progress on tree rows.
//
// The defect these tests pin: the shared list/tree projection gated
// epicProgress on `type = 'epic'`, so in a tree rooted at an epic the field
// reached exactly one row — the root, which the epic section renders as the
// HEADER and never as a child. Every row the section actually draws is
// typically a task, so the branch indicator could not appear on any of them,
// on any data, ever.
//
// Two things are asserted here and nowhere else:
//
//   - a row with children carries the triple whatever its TYPE, and a row
//     without children does not (the fix itself);
//   - the triple reads "unavailable" exactly as the epic rollup does
//     (sk-291, sk-243/F4). The branch bar and the header ring are on one
//     screen. If one of them counted an invisible child and the other did
//     not, the same epic would show two different denominators — which is
//     why the parity legs below are part of THIS fix and not a follow-up.

// branchRows runs the tree rooted at root as a plain member of pids and
// returns the rows by issue id.
func branchRows(t *testing.T, store *Store, ctx context.Context,
	caller uuid.UUID, pids []uuid.UUID, root string) map[string]IssueTreeNode {
	t.Helper()
	res, err := store.IssueTree(ctx, IssueTreeArgs{
		Scope:  authz.ForUser(caller),
		Caller: caller,
		RootID: &root,
		Types:  []string{"parent-child"},
	})
	if err != nil {
		t.Fatalf("IssueTree(%s): %v", root, err)
	}
	out := make(map[string]IssueTreeNode, len(res.Items))
	for _, row := range res.Items {
		out[row.Item.ID] = row
	}
	return out
}

// wantProgress asserts the row's triple, naming the row so a failure says
// which one moved.
func wantProgress(t *testing.T, rows map[string]IssueTreeNode, id string, closed, total, unavailable int) {
	t.Helper()
	row, ok := rows[id]
	if !ok {
		t.Fatalf("row %s missing from the tree", id)
	}
	got := row.Item.EpicProgress
	if got == nil {
		t.Fatalf("row %s carries no epicProgress; want {closed:%d total:%d unavailable:%d}. "+
			"This is the sk-461 defect itself: the row has children and the surface draws a "+
			"progress indicator for it, but the server sent no numbers to draw.",
			id, closed, total, unavailable)
	}
	if got.Closed != closed || got.Total != total || got.Unavailable != unavailable {
		t.Errorf("row %s epicProgress = {closed:%d total:%d unavailable:%d}, want {closed:%d total:%d unavailable:%d}",
			id, got.Closed, got.Total, got.Unavailable, closed, total, unavailable)
	}
}

// TestTreeBranchProgress_NonEpicParentCarriesTheTriple is the fix: a
// task-typed row with children gets the numbers, at every depth, and a leaf
// does not. Before the fix only the epic root had them.
func TestTreeBranchProgress_NonEpicParentCarriesTheTriple(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	// epic ─ task(P) ─ {closed, open, task(G) ─ open}
	epicID := prefix + "-1"
	parent := prefix + "-1.1"
	grand := prefix + "-1.1.3"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, parent, "a task with children", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1.1", "done", TypeTask, &parent)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1.2", "todo", TypeTask, &parent)
	seedTypedIssue(t, pool, pid, owner, grand, "a task with a child", TypeTask, &parent)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1.3.1", "leaf", TypeTask, &grand)
	setIssueStatus(t, pool, prefix+"-1.1.1", "closed")

	rows := branchRows(t, NewStore(pool), callerCtx(t, owner, "user"), owner, []uuid.UUID{pid}, epicID)

	// The row the epic section draws as a child, and the one the old code
	// could never populate.
	wantProgress(t, rows, parent, 1, 3, 0)
	// Depth is not the rule either — a grandchild parent counts too.
	wantProgress(t, rows, grand, 0, 1, 0)
	// The root keeps what it always had.
	wantProgress(t, rows, epicID, 0, 1, 0)

	// A leaf has nothing to report, and must not report a zero: "no children"
	// and "no children closed" are different statements.
	if leaf := rows[prefix+"-1.1.3.1"]; leaf.Item.EpicProgress != nil {
		t.Errorf("leaf row carries epicProgress %+v; want absent", *leaf.Item.EpicProgress)
	}
}

// TestTreeBranchProgress_ChildlessEpicUnchanged — the one row that already
// worked must keep working: an epic with no children still answers {0,0,0}
// rather than falling off the wire with the leaves.
func TestTreeBranchProgress_ChildlessEpicUnchanged(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "empty epic", TypeEpic, nil)

	rows := branchRows(t, NewStore(pool), callerCtx(t, owner, "user"), owner, []uuid.UUID{pid}, epicID)
	wantProgress(t, rows, epicID, 0, 0, 0)
}

// TestTreeBranchProgress_StandingChildExcludedFromTotal — parity leg 1 with
// the rollup (R11): a standing child is invisible to progress, so an epic
// whose only other child is closed reads 1/1 and not 1/2.
func TestTreeBranchProgress_StandingChildExcludedFromTotal(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	parent := prefix + "-1.1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, parent, "a task with children", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1.1", "done", TypeTask, &parent)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1.2", "perpetual", TypeTask, &parent)
	setIssueStatus(t, pool, prefix+"-1.1.1", "closed")
	setIssueStatus(t, pool, prefix+"-1.1.2", "standing")

	rows := branchRows(t, NewStore(pool), callerCtx(t, owner, "user"), owner, []uuid.UUID{pid}, epicID)
	wantProgress(t, rows, parent, 1, 1, 0)
}

// TestTreeBranchProgress_InvisibleChildIsUnavailableNotClosed — parity leg 2,
// the one that keeps the branch bar and the header ring telling the same
// story. A child in a project the caller cannot see:
//
//   - stays in TOTAL (a vanishing cross-project tail reads as "finished");
//   - is absent from CLOSED whatever its real status (sk-291: its status must
//     not be derivable);
//   - is counted in UNAVAILABLE as an exact number (sk-243/F4).
//
// Asserted across several states of the hidden child, because the numbers
// being IDENTICAL for all of them is the security half of the property.
//
// sk-489, matrix row 18: there are now three states, and the third is `closed` with
// the outcome "won't do". The outcome of an invisible child is the content of its card
// exactly like the status (R-6), so it must not move any
// branch number: neither the "no work" leg nor the denominator.
func TestTreeBranchProgress_InvisibleChildIsUnavailableNotClosed(t *testing.T) {
	var shots []EpicProgress
	for _, hiddenStatus := range []string{"open", "closed", "closed/" + CloseOutcomeWontDo} {
		pool := newTestPool(t)
		caller := seedUser(t, pool)
		outsider := seedUser(t, pool)
		pid, prefix := seedProject(t, pool, caller)
		hiddenPID, hiddenPrefix := seedProject(t, pool, outsider)

		epicID := prefix + "-1"
		parent := prefix + "-1.1"
		seedTypedIssue(t, pool, pid, caller, epicID, "the epic", TypeEpic, nil)
		seedTypedIssue(t, pool, pid, caller, parent, "a task with children", TypeTask, &epicID)
		seedTypedIssue(t, pool, pid, caller, prefix+"-1.1.1", "done", TypeTask, &parent)
		seedTypedIssue(t, pool, pid, caller, prefix+"-1.1.2", "todo", TypeTask, &parent)
		setIssueStatus(t, pool, prefix+"-1.1.1", "closed")

		// The cross-project child hangs off the TASK, so the leg under test is
		// the branch's own, not the epic's.
		hiddenID := hiddenPrefix + "-9"
		seedTypedIssue(t, pool, hiddenPID, outsider, hiddenID, "secret child", TypeTask, nil)
		if status, outcome, ok := strings.Cut(hiddenStatus, "/"); ok {
			if status != "closed" {
				t.Fatalf("an outcome is allowed only on a closed issue (%q)", hiddenStatus)
			}
			setClosedWithOutcome(t, pool, hiddenID, outcome)
		} else {
			setIssueStatus(t, pool, hiddenID, hiddenStatus)
		}
		seedParentChild(t, pool, hiddenID, parent, outsider)

		rows := branchRows(t, NewStore(pool), callerCtx(t, caller, "user"), caller, []uuid.UUID{pid}, epicID)
		wantProgress(t, rows, parent, 1, 3, 1)

		// sk-489, row 18: the outcome legs do not move either. The visible closed
		// child is closed here by a raw UPDATE, that is by trigger 000050, with
		// the outcome "done"; the invisible one lands in neither done nor noWork, but
		// stays in counted together with the whole total.
		if got := rows[parent].Item.EpicProgress; got.Done != 1 || got.NoWork != 0 || got.Counted != 3 {
			t.Errorf("[%s] branch: {done:%d noWork:%d counted:%d}, want {1 0 3}",
				hiddenStatus, got.Done, got.NoWork, got.Counted)
		}

		// The stub standing for the invisible child must carry nothing: INV-2
		// names the nested epic's progress among the things a stub may not
		// publish, and a stub with a triple would also confirm it has children.
		for id, row := range rows {
			if row.Item.Title == "<hidden>" && row.Item.EpicProgress != nil {
				t.Errorf("stub row %s carries epicProgress %+v; want absent", id, *row.Item.EpicProgress)
			}
		}
		shots = append(shots, *rows[parent].Item.EpicProgress)
	}

	for i, shot := range shots[1:] {
		if shot != shots[0] {
			t.Errorf("the hidden child's state is derivable from the branch numbers: "+
				"state #0 -> %+v, state #%d -> %+v", shots[0], i+1, shot)
		}
	}
}

// TestTreeBranchProgress_AdminSeesTheCrossProjectChild — the bucket is about
// the CALLER, not about the project boundary: for an admin the same child is
// an ordinary counted child and `unavailable` is empty.
func TestTreeBranchProgress_AdminSeesTheCrossProjectChild(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUser(t, pool)
	outsider := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, caller)
	hiddenPID, hiddenPrefix := seedProject(t, pool, outsider)

	epicID := prefix + "-1"
	parent := prefix + "-1.1"
	seedTypedIssue(t, pool, pid, caller, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, caller, parent, "a task with children", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, caller, prefix+"-1.1.1", "todo", TypeTask, &parent)

	hiddenID := hiddenPrefix + "-9"
	seedTypedIssue(t, pool, hiddenPID, outsider, hiddenID, "secret child", TypeTask, nil)
	setIssueStatus(t, pool, hiddenID, "closed")
	seedParentChild(t, pool, hiddenID, parent, outsider)
	// The admin is an admin in users: since sk-674.18 T007 the horizon reads
	// the role from the access relation, not from the request context (D-2).
	if _, err := pool.Exec(t.Context(), `UPDATE users SET role = 'admin' WHERE id = $1`, caller); err != nil {
		t.Fatalf("make the caller an admin: %v", err)
	}

	res, err := NewStore(pool).IssueTree(callerCtx(t, caller, "admin"), IssueTreeArgs{
		Scope:  authz.Unrestricted(),
		Caller: caller,
		RootID: &epicID,
		Types:  []string{"parent-child"},
	})
	if err != nil {
		t.Fatalf("IssueTree: %v", err)
	}
	rows := make(map[string]IssueTreeNode, len(res.Items))
	for _, row := range res.Items {
		rows[row.Item.ID] = row
	}
	wantProgress(t, rows, parent, 1, 2, 0)
}

// TestTreeBranchProgress_ListRowsUnchanged — the blast-radius guard. The fix
// widens the TREE only; the list projection still answers for epic rows
// alone, because its rows are list cards (and /me/watching shares the very
// helper the tree calls). A change there would be a silent contract shift for
// every MCP and REST list consumer.
func TestTreeBranchProgress_ListRowsUnchanged(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	parent := prefix + "-1.1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, parent, "a task with children", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1.1", "child", TypeTask, &parent)

	res, err := NewStore(pool).ListIssues(callerCtx(t, owner, "user"), ListIssuesArgs{
		ProjectID: pid,
		Caller:    &owner,
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	for _, it := range res.Items {
		if it.ID == parent && it.EpicProgress != nil {
			t.Errorf("list row %s (a task) carries epicProgress %+v; the list projection is "+
				"deliberately NOT widened by sk-461 — /me/watching and every MCP list consumer "+
				"read these rows", it.ID, *it.EpicProgress)
		}
		if it.ID == epicID && it.EpicProgress == nil {
			t.Errorf("list row %s (the epic) lost its epicProgress", it.ID)
		}
	}
}

// guard: branchProgressFor is the only new read, and it must survive an empty
// id list without touching the database (IssueTree calls it before knowing
// whether every row was a stub).
func TestBranchProgressFor_EmptyIDs(t *testing.T) {
	var pool *pgxpool.Pool // deliberately nil: an empty list must not query
	got, err := (&Store{pool: pool}).branchProgressFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("branchProgressFor(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("branchProgressFor(nil) = %v, want empty", got)
	}
}

// setClosedWithOutcome closes an issue RAW, with an explicit outcome.
//
// Raw on purpose: this is a FIXTURE, and the writers that resolve outcomes
// (Store.Close / Store.UpdateStatus, sk-489 T003) are the subject of their own
// tests, not a dependency of these. Going through them here would make a
// progress-number test fail whenever a writer's validation changes.
// The 000050 trigger is what keeps the fixture honest: a raw close without an
// explicit outcome lands `done`, so "closed" never means "no outcome" (INV-1).
func setClosedWithOutcome(t *testing.T, pool *pgxpool.Pool, issueID, outcome string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE issues SET status = 'closed', closed_at = now(), close_outcome = $1 WHERE id = $2`,
		outcome, issueID); err != nil {
		t.Fatalf("close %s with outcome %s: %v", issueID, outcome, err)
	}
}

// TestBranchProgress_LegsMatchTheProjection — the sk-489 T005 gate for the one
// progress reader the compiler cannot guard.
//
// Six SQL sites render the legs through epicProgressProjection, and four Scan
// sites assemble them through epicProgressFrom, so adding a leg to the
// projection breaks those scan lists and the build stops. branchProgressFor is
// the reader outside that pair and it is built differently — its own SQL, its
// own direct Scan — so it would have compiled happily with three legs and
// answered `noWork:0` forever. Its docblock claims
// the legs are spelled to match epicSummary{...}Expr «leg for leg»; this test
// is what makes that claim falsifiable.
//
// The comparison is of RESULTS on one fixture, not of source text: the two
// spellings differ by construction (correlated subquery vs FILTER over a
// GROUP BY), so only the numbers can be compared. The fixture is hostile on
// every axis the two readers could disagree about: two visible done children,
// one visible child per NON-done outcome, a visible open child, a visible
// standing child (excluded from Total), and a child in a project the caller
// cannot see (counted in Total and Unavailable, and in NO outcome leg —
// INV-4).
//
// sk-489, review A/3 (finding F-1): all three "no work" values are present
// by name, not a single `wont_do`. The rule is written as a BLACKLIST
// (`close_outcome <> 'done'`) in three SQL copies, and an implementation rewritten
// to an INCOMPLETE whitelist passed the whole T005 gate green with a lone `wont_do`
// in the fixture. The values are listed here explicitly rather than
// derived from the set: completeness of the set is the subject of the per-read-site table
// (`close_outcome_progress_test.go#wantRowProgress`), while this test is about
// the agreement of TWO READERS, and it helps to have its fixture readable by eye.
func TestBranchProgress_LegsMatchTheProjection(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUser(t, pool)
	outsider := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, caller)
	hiddenPID, hiddenPrefix := seedProject(t, pool, outsider)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, caller, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, caller, prefix+"-1.1", "done", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, caller, prefix+"-1.2", "not doing this", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, caller, prefix+"-1.3", "todo", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, caller, prefix+"-1.4", "perpetual", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, caller, prefix+"-1.5", "already filed", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, caller, prefix+"-1.6", "replaced", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, caller, prefix+"-1.7", "done too", TypeTask, &epicID)
	setClosedWithOutcome(t, pool, prefix+"-1.1", CloseOutcomeDone)
	setClosedWithOutcome(t, pool, prefix+"-1.2", CloseOutcomeWontDo)
	setIssueStatus(t, pool, prefix+"-1.4", "standing")
	setClosedWithOutcome(t, pool, prefix+"-1.5", CloseOutcomeDuplicate)
	setClosedWithOutcome(t, pool, prefix+"-1.6", CloseOutcomeSuperseded)
	setClosedWithOutcome(t, pool, prefix+"-1.7", CloseOutcomeDone)

	hiddenID := hiddenPrefix + "-9"
	seedTypedIssue(t, pool, hiddenPID, outsider, hiddenID, "secret child", TypeTask, nil)
	setClosedWithOutcome(t, pool, hiddenID, CloseOutcomeWontDo)
	seedParentChild(t, pool, hiddenID, epicID, outsider)

	store := NewStore(pool)
	ctx := callerCtx(t, caller, "user")

	// Reader 1 — the shared projection (epicProgressProjection/From).
	res, err := store.ListIssues(ctx, ListIssuesArgs{ProjectID: pid, Caller: &caller, Limit: 50})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	var fromProjection *EpicProgress
	for _, it := range res.Items {
		if it.ID == epicID {
			fromProjection = it.EpicProgress
		}
	}
	if fromProjection == nil {
		t.Fatalf("the epic row carries no epicProgress")
	}

	// Reader 2 — the tree's batch, which overwrites the row it just filled.
	fromBranch := branchRows(t, store, ctx, caller, []uuid.UUID{pid}, epicID)[epicID].Item.EpicProgress
	if fromBranch == nil {
		t.Fatalf("the tree row carries no epicProgress")
	}

	if *fromProjection != *fromBranch {
		t.Errorf("the two progress readers disagree on one epic:\n"+
			"  epicProgressProjection -> %+v\n"+
			"  branchProgressFor      -> %+v\n"+
			"The header ring and the branch bar are on one screen (INV-5), so this is two "+
			"different denominators for the same data. Check that every leg of "+
			"branchProgressFor still matches its epicSummary{...}Expr twin.", *fromProjection, *fromBranch)
	}

	// And they agree on the RIGHT numbers, not merely with each other: two
	// readers sharing one bug would pass the comparison above.
	want := EpicProgress{Closed: 5, Total: 7, Unavailable: 1, Done: 2, NoWork: 3, Counted: 4}
	if *fromProjection != want {
		t.Errorf("epicProgress = %+v, want %+v (2 done + wont_do + duplicate + superseded + "+
			"1 open + 1 invisible; the standing child is outside Total, the invisible one is "+
			"inside it and inside Counted but in no outcome leg). `noWork:1` or `noWork:2` "+
			"here means a leg that knows only SOME of the non-done outcomes", *fromProjection, want)
	}
}
