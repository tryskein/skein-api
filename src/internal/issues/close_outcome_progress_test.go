package issues

// close_outcome_progress_test.go — sk-489, T005, matrix row 17.
//
// The row's assertion: "one number rule for all selection places" (US-6,
// INV-5, INV-3 on any data). The rule is one, but there are SEVEN selection places —
// six renderings of the shared projection (`ListIssues`, three builders of
// `list_all.go`, `ListByUser`, `tree.go#summariesForIDs`) plus the branch's own
// reader (`tree.go#branchProgressFor`) — and each is a separate
// SQL with a separate column list and a separate Scan. Therefore the assertion
// is checked by a TABLE over the places, not by one lucky call: an implementation
// that put the "no work" leg into six places out of seven gives
// `noWork:0` at the seventh with a non-empty `closed` — i.e. an epic with cancelled children is again
// read as "100 % done", exactly where nobody looks.
//
// SIX cells out of seven live here; the seventh — `/me/watching` — is in package
// `handler` (`me_watching_progress_test.go`), because that is the only
// surface that prints the `summariesForIDs` numbers without a batch overwrite.
//
// # Why the fixture is exactly like this (review of wave A/3, findings M-1 and M-2)
//
// The first edition of the fixture was "3 done + 1 won't do", and both halves of the
// rule turned out under-observed. Measured by mutations, not assumed:
//
//   - THE HORIZON. EVERY selection place binds the progress legs to the reader's
//     horizon itself (`epicProgressProjection` takes two
//     placeholders, and the caller binds them), and the compiler guard
//     checks only the NUMBER of legs, not what they are bound to:
//     substituting "everything is visible" for the horizon compiles. The mutants
//     "this place binds an UNBOUNDED horizon" in `list_by_user.go`,
//     `list_all.go` and `tree.go#summariesForIDs` passed the whole T005
//     gate green, because the fixture had not a single INVISIBLE
//     child. Now it has one — and it is closed WITHOUT WORK, so that exactly
//     the new leg moves, not only `unavailable`.
//
//   - THE OUTCOME VALUE. The INV-5 rule is "any outcome other than
//     "done" drops out", and in SQL it is written as a BLACK list (`<> 'done'`), and in Go
//     as a white one (`close_outcome.go#IsNoWorkOutcome`). While the only
//     "no work" in the fixture was `wont_do`, an implementation rewritten to an
//     INCOMPLETE white list (`IN ('wont_do','duplicate')`) was
//     indistinguishable from the correct one for the whole gate. Therefore here we seed
//     one closed child per EACH value of `ValidCloseOutcomes()`,
//     and the expected "no work" number is COMPUTED via `IsNoWorkOutcome` —
//     so the seam between the two halves of the predicate gets an owner:
//     a fifth value added to the set and forgotten in Go will pull the
//     expectation apart from the fact and turn this table red instead of
//     diverging silently.
//
// The extra "done" ones (progressExtraDoneKids) are not decoration: with them all
// six numbers of the row are pairwise distinct, and swapping the legs cannot
// go unnoticed.

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
)

// progressExtraDoneKids — how many ADDITIONAL "done" children are hung
// on the parent on top of one child per outcome value. Three is the
// minimum at which {closed, total, unavailable, done, noWork, counted}
// of the fixture are pairwise distinct (7, 8, 1, 4, 3, 5).
const progressExtraDoneKids = 3

// progressHiddenOutcome — the outcome of the INVISIBLE child. "No work", not
// "done", on purpose: an invisible child must be absent from the outcome legs
// with ANY outcome (INV-4, R-6), and it is on "no work" that a horizon-binding
// error moves the new leg, not only the old ones.
const progressHiddenOutcome = CloseOutcomeWontDo

// progressChildren — the children of one parent: one visible closed
// child per outcome value, a few extra "done" ones and one
// child in a project the reader is not a member of.
type progressChildren struct {
	byOutcome map[string]string
	extraDone []string
	hidden    string
}

// progressFixture — an epic with the full set of outcomes, a childless issue next to it and
// a foreign project for the invisible child. Returns the owner, both projects and
// the identifiers.
//
// The titles carry a common token so that the full-text selection (the third cell
// of the table below) finds the same rows by `Q`, not by identifier.
type progressFixture struct {
	owner        uuid.UUID
	outsider     uuid.UUID
	projectID    uuid.UUID
	hiddenPID    uuid.UUID
	prefix       string
	hiddenPrefix string
	epicID       string
	kids         progressChildren
	childless    string
	searchQ      string
}

// seedProgressChildren hangs the full set of children on parentID and returns
// their identifiers. hiddenID is the identifier of the invisible child in a foreign
// project (each parent has its own, so that the fixture also works on a second
// parent in the same run).
func seedProgressChildren(t *testing.T, pool *pgxpool.Pool, f *progressFixture,
	parentID, hiddenID string) progressChildren {
	t.Helper()
	kids := progressChildren{byOutcome: map[string]string{}, hidden: hiddenID}

	n := 0
	next := func() string {
		n++
		return fmt.Sprintf("%s.%d", parentID, n)
	}
	for _, outcome := range ValidCloseOutcomes() {
		id := next()
		seedTypedIssue(t, pool, f.projectID, f.owner, id, "manjeta "+outcome+" child", TypeTask, &parentID)
		setClosedWithOutcome(t, pool, id, outcome)
		kids.byOutcome[outcome] = id
	}
	for range progressExtraDoneKids {
		id := next()
		seedTypedIssue(t, pool, f.projectID, f.owner, id, "manjeta done child", TypeTask, &parentID)
		setClosedWithOutcome(t, pool, id, CloseOutcomeDone)
		kids.extraDone = append(kids.extraDone, id)
	}

	// The invisible child lives in a foreign project and is bound to the parent
	// by a cross-project parent-child edge (sk-35) — the very way
	// this situation arises in prod.
	seedTypedIssue(t, pool, f.hiddenPID, f.outsider, hiddenID, "secret child", TypeTask, nil)
	setClosedWithOutcome(t, pool, hiddenID, progressHiddenOutcome)
	seedParentChild(t, pool, hiddenID, parentID, f.outsider)

	return kids
}

func seedProgressFixture(t *testing.T, pool *pgxpool.Pool) progressFixture {
	t.Helper()
	owner := seedUser(t, pool)
	outsider := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	hiddenPID, hiddenPrefix := seedProject(t, pool, outsider)

	f := progressFixture{
		owner:        owner,
		outsider:     outsider,
		projectID:    pid,
		hiddenPID:    hiddenPID,
		prefix:       prefix,
		hiddenPrefix: hiddenPrefix,
		epicID:       prefix + "-1",
		childless:    prefix + "-2",
		searchQ:      "manjeta",
	}
	seedTypedIssue(t, pool, pid, owner, f.epicID, "manjeta epic", TypeEpic, nil)
	f.kids = seedProgressChildren(t, pool, &f, f.epicID, hiddenPrefix+"-90")
	seedTypedIssue(t, pool, pid, owner, f.childless, "manjeta lonely epic", TypeEpic, nil)

	// A vector on every visible row — for the FIFTH cell of the table,
	// the semantic re-sort: `list_all.go#semanticRerankAllIssues` is the
	// third rendering of the shared projection, and for a query that is not in the text
	// only it brings rows. The vector is the same as `bhEmbedder`'s, so the
	// similarity is one and the threshold is passed.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET embedding = $1::halfvec WHERE project_id = $2`,
		bhUnitVectorLiteral(), pid); err != nil {
		t.Fatalf("seed embeddings: %v", err)
	}
	return f
}

// wantRowProgress — the numbers of the fixture parent's row. One expectation for the whole
// table: a divergence between selection places must read as a
// divergence from ONE expectation, not as two different truths.
//
// The numbers are DERIVED from the outcome set, not written as a literal, and this is a
// substantive choice — see "THE OUTCOME VALUE" in the file header: the expectation
// "how many dropped out" is computed by the Go half of the predicate (`IsNoWorkOutcome`), and
// the fact comes from the SQL half (`<> 'done'`). While the halves agree, the
// numbers match; if they diverge — this table turns red.
func wantRowProgress() EpicProgress {
	outcomes := ValidCloseOutcomes()
	noWork := 0
	for _, o := range outcomes {
		if IsNoWorkOutcome(o) {
			noWork++
		}
	}
	closed := len(outcomes) + progressExtraDoneKids
	total := closed + 1 // + the invisible child: it is in Total but in no outcome leg
	return EpicProgress{
		Closed:      closed,
		Total:       total,
		Unavailable: 1,
		Done:        closed - noWork,
		NoWork:      noWork,
		Counted:     total - noWork,
	}
}

// assertProgressRow — the numbers of one parent and the outcomes on its closed
// rows. Separate from assertProgressSurface because the table below
// also has a parent-ISSUE with its own reader.
func assertProgressRow(t *testing.T, surface, parentID string,
	kids progressChildren, rows map[string]IssueSummary) {
	t.Helper()

	parent, ok := rows[parentID]
	if !ok {
		t.Fatalf("[%s] the parent's row %s is not in the result — the fixture did not reach the surface, "+
			"and there is nothing to check the numbers assertion on", surface, parentID)
	}
	if parent.EpicProgress == nil {
		t.Fatalf("[%s] row %s came without epicProgress", surface, parentID)
	}
	want := wantRowProgress()
	if *parent.EpicProgress != want {
		t.Errorf("[%s] row %s: epicProgress = %+v, want %+v.\n"+
			"Fixture: one closed child per each outcome %v, plus %d extra \"done\", "+
			"plus one INVISIBLE child closed with outcome %q.\n"+
			"`noWork` lower than expected — this selection place computes progress by STATUS or "+
			"does not know all the outcome values (INV-5); `unavailable:0` with the same `total` — it "+
			"bound the legs to an UNBOUNDED horizon and counted a foreign child as its own (INV-4)",
			surface, parentID, *parent.EpicProgress, want,
			ValidCloseOutcomes(), progressExtraDoneKids, progressHiddenOutcome)
	}

	// The outcome on the row is the second half of the same selection: the column
	// `close_outcome` was added to the same six projections.
	for outcome, id := range kids.byOutcome {
		kid, ok := rows[id]
		if !ok {
			t.Fatalf("[%s] closed child %s (outcome %q) is not in the result", surface, id, outcome)
		}
		if kid.CloseOutcome == nil || *kid.CloseOutcome != outcome {
			t.Errorf("[%s] row %s: closeOutcome = %v, want %q",
				surface, id, kid.CloseOutcome, outcome)
		}
	}
	for _, id := range kids.extraDone {
		kid, ok := rows[id]
		if !ok {
			t.Fatalf("[%s] closed child %s is not in the result", surface, id)
		}
		if kid.CloseOutcome == nil || *kid.CloseOutcome != CloseOutcomeDone {
			t.Errorf("[%s] row %s: closeOutcome = %v, want %q",
				surface, id, kid.CloseOutcome, CloseOutcomeDone)
		}
	}

	// The invisible child: in lists its row is absent altogether, in the tree it is there
	// as a "<hidden>" stub. In both cases there can be no outcome on it —
	// the outcome is the same card content as the status (R-6, INV-2).
	if kid, ok := rows[kids.hidden]; ok && kid.CloseOutcome != nil {
		t.Errorf("[%s] the row of the inaccessible child %s carries closeOutcome %q — the outcome "+
			"of an inaccessible issue is published neither as a number nor as a field (R-6)",
			surface, kids.hidden, *kid.CloseOutcome)
	}

	// An unclosed row carries no outcome (INV-1) — the key is absent, not
	// empty, and it is the same assertion as on the card.
	if parent.CloseOutcome != nil {
		t.Errorf("[%s] open row %s carries closeOutcome %q; an unclosed issue has no outcome (INV-1)",
			surface, parentID, *parent.CloseOutcome)
	}
}

// assertProgressSurface — one cell of the table: the epic's numbers, the outcome on
// the closed rows, the absence of numbers on the childless row.
func assertProgressSurface(t *testing.T, surface string, f progressFixture, rows map[string]IssueSummary) {
	t.Helper()

	assertProgressRow(t, surface, f.epicID, f.kids, rows)

	// The population of rows with progress does not change: a childless epic answers
	// with zeros, it neither disappears nor stays silent.
	lonely, ok := rows[f.childless]
	if !ok {
		t.Fatalf("[%s] childless row %s is not in the result", surface, f.childless)
	}
	if lonely.EpicProgress == nil {
		t.Fatalf("[%s] the childless epic lost epicProgress", surface)
	}
	if want := (EpicProgress{}); *lonely.EpicProgress != want {
		t.Errorf("[%s] childless epic: epicProgress = %+v, want %+v (a row without children — "+
			"as today)", surface, *lonely.EpicProgress, want)
	}
}

// TestProgressNumbers_EveryListSurfaceAgrees — a table over the list
// selection places: `Store.ListIssues`, `ListAllIssues` (plain selection and text
// search), `ListByUser`.
func TestProgressNumbers_EveryListSurfaceAgrees(t *testing.T) {
	pool := newTestPool(t)
	f := seedProgressFixture(t, pool)
	store := NewStore(pool)
	ctx := callerCtx(t, f.owner, "user")

	surfaces := []struct {
		name  string
		fetch func() (*ListResult, error)
	}{
		{"Store.ListIssues", func() (*ListResult, error) {
			return store.ListIssues(ctx, ListIssuesArgs{
				ProjectID: f.projectID, Caller: &f.owner, Limit: 100,
			})
		}},
		{"ListAllIssues (plain selection)", func() (*ListResult, error) {
			return store.ListAllIssues(ctx, ListAllIssuesArgs{
				Scope:  authz.ForUser(f.owner),
				Caller: f.owner,
			})
		}},
		{"ListAllIssues (text search)", func() (*ListResult, error) {
			return store.ListAllIssues(ctx, ListAllIssuesArgs{
				Scope:  authz.ForUser(f.owner),
				Caller: f.owner,
				Q:      f.searchQ, Limit: 100,
			})
		}},
		{"ListAllIssues (semantic re-sort)", func() (*ListResult, error) {
			return store.ListAllIssues(ctx, ListAllIssuesArgs{
				Scope:  authz.ForUser(f.owner),
				Caller: f.owner,
				// Full-text selection will find nothing by it — rows are
				// brought ONLY by the re-sort, i.e. the third rendering of the shared
				// projection. Without this cell it has no observer at all:
				// the mutant "this place binds an UNBOUNDED horizon" in
				// `semanticRerankAllIssues` passed the whole T005 gate
				// green even after the other five had passed it.
				Q:        "zzqxwvunmatchable",
				Embedder: bhEmbedder{},
				Limit:    100,
			})
		}},
		{"ListByUser", func() (*ListResult, error) {
			return store.ListByUser(ctx, ListByUserArgs{
				Scope:  authz.ForUser(f.owner),
				UserID: f.owner, Mode: ModeCreatedBy,
				IncludeClosed: true, Limit: 500,
			})
		}},
	}

	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			res, err := s.fetch()
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			assertProgressSurface(t, s.name, f, rowsByID(res.Items))
		})
	}
}

// TestProgressNumbers_TreeSurfacesAgree — the two remaining places: the tree's summary
// scanner (`tree.go#summariesForIDs`) and the branch batch
// (`tree.go#branchProgressFor`).
//
// The tree overwrites the rows' progress with a batch, so "what exactly the scanner
// answered" is visible not by the epic but by the OUTCOME on the closed rows — the batch does not
// touch it. And the branch is checked where the scanner is silent by construction: on the
// parent-ISSUE, which is not entitled to a progress projection at all.
func TestProgressNumbers_TreeSurfacesAgree(t *testing.T) {
	pool := newTestPool(t)
	f := seedProgressFixture(t, pool)
	store := NewStore(pool)
	ctx := callerCtx(t, f.owner, "user")

	// A branch on a non-epic: a parent issue with the same set of children.
	branchParent := f.prefix + "-3"
	seedTypedIssue(t, pool, f.projectID, f.owner, branchParent, "manjeta branch parent", TypeTask, nil)
	branchKids := seedProgressChildren(t, pool, &f, branchParent, f.hiddenPrefix+"-91")

	res, err := store.IssueTree(ctx, IssueTreeArgs{
		Scope:  authz.ForUser(f.owner),
		Caller: f.owner,
		Types:  []string{"parent-child"},
	})
	if err != nil {
		t.Fatalf("IssueTree: %v", err)
	}
	rows := make(map[string]IssueSummary, len(res.Items))
	for _, row := range res.Items {
		rows[row.Item.ID] = row.Item
	}

	assertProgressSurface(t, "tree.go#summariesForIDs + branchProgressFor", f, rows)

	assertProgressRow(t, "tree.go#branchProgressFor (parent issue)", branchParent, branchKids, rows)
}
