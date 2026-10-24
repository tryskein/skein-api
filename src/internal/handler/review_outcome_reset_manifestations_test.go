package handler

// review_outcome_reset_manifestations_test.go — sk-489, T004, matrix
// row 15.
//
// CONSEQUENCE of resetting the mark on an outcome edit: the issue is again counted as
// unreviewed — in ALL manifestations of the set at once (the selection, the second
// number, the reminder recipients), like any unreviewed issue.
//
// How this file differs from the neighbouring review_unreviewed_manifestations_test.go,
// which it is modelled on. That one reads the manifestations on a fixture
// seeded by a raw pool.Exec and does not call the writer at all — it is about READING
// the set. Here the TRANSITION into unreviewed is checked, so the initial
// "reviewed" is seeded by a raw write (it is a valid state, the product can
// create it), and the reset itself is done only by the writer —
// issues.Store.PatchIssue. Fake the reset with a write, and the test would prove
// agreement of the three readers on an invented state, not that an outcome
// edit leads to that state.
//
// Mutations that this file reddens:
//   - reset on ANY outcome change (without the "leaving done" condition) → the
//     wont_do → duplicate cell goes red;
//   - reset without the actor condition → the "author edits it themselves" cell goes red;
//   - no reset at all → the first cell goes red in all three manifestations.

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
)

// patchOutcomeAs edits the outcome of an issue through the writer, on behalf of the given
// user. The version is re-read from the database: the fixture seeds version=1, but
// the previous cells of the table move it.
func patchOutcomeAs(t *testing.T, pool *pgxpool.Pool, issueID, outcome string,
	userID uuid.UUID, username string,
) {
	t.Helper()
	store := issues.NewStore(pool)
	var version int
	if err := pool.QueryRow(t.Context(),
		`SELECT version FROM issues WHERE id = $1`, issueID).Scan(&version); err != nil {
		t.Fatalf("reading version of %s: %v", issueID, err)
	}
	if _, err := store.PatchIssue(t.Context(), issueID, issues.PatchIssueInput{
		ExpectedVersion: version,
		CloseOutcome:    &outcome,
	}, issues.HumanActor(userID, username)); err != nil {
		t.Fatalf("PatchIssue(%s → %s): %v", issueID, outcome, err)
	}
}

func reviewedAtOf(t *testing.T, pool *pgxpool.Pool, issueID string) *string {
	t.Helper()
	var at *string
	if err := pool.QueryRow(t.Context(),
		`SELECT reviewed_at::text FROM issues WHERE id = $1`, issueID).Scan(&at); err != nil {
		t.Fatalf("reading reviewed_at of %s: %v", issueID, err)
	}
	return at
}

// TestUnreviewed_OutcomeChangeEntersTheSet — row 15.
//
// The reference is taken from the shared fixture: issue "C" (prefix-3) is closed,
// delegated, with the flag on and the mark ALREADY SET,
// so it is not in the set. An outcome edit by a non-author must
// return it there — and at once in all three manifestations.
func TestUnreviewed_OutcomeChangeEntersTheSet(t *testing.T) {
	e := newUnreviewedEnv(t)
	reviewed := e.prefix + "-3"

	// Precondition: the issue is reviewed and not in the set. Without it
	// "entered after the edit" would be proven by the fact that it was always there.
	if reviewedAtOf(t, e.pool, reviewed) == nil {
		t.Fatalf("precondition: %s is not marked reviewed", reviewed)
	}
	if hasID(e.crossPathIDs(t, "unreviewed=true"), reviewed) {
		t.Fatalf("precondition: %s is already in the set before the edit", reviewed)
	}

	outsider, outsiderName := seedIMUser(t, e.pool, "user")
	patchOutcomeAs(t, e.pool, reviewed, issues.CloseOutcomeWontDo, outsider, outsiderName)

	if reviewedAtOf(t, e.pool, reviewed) != nil {
		t.Fatalf("edit done → wont_do by a non-author did not reset the mark")
	}

	cross := e.crossPathIDs(t, "unreviewed=true")
	project := e.projectPathIDs(t, "createdBy=me&unreviewed=true")
	mechanicOn, count := e.secondNumber(t)
	nudge := e.nudgeSetIDs(t)

	want := append(append([]string(nil), e.wantSet...), reviewed)
	if !equalIDs(cross, want) {
		t.Errorf("cross-project selection = %v, want %v", cross, want)
	}
	if !equalIDs(project, want) {
		t.Errorf("project selection = %v, want %v", project, want)
	}
	if !mechanicOn || count == nil {
		t.Fatalf("precondition: the mechanic is off, there is no second number")
	}
	if *count != len(want) {
		t.Errorf("second number %d, set size %d (%v)", *count, len(want), want)
	}
	if !equalIDs(nudge, want) {
		t.Errorf("reminder recipients see %v, set = %v", nudge, want)
	}
}

// TestUnreviewed_OutcomeChangeByAuthorKeepsTheMark — the second and third
// cells of row 15: an edit by the author themselves and a change BETWEEN the dropped outcomes
// do not touch the mark, and the set does not grow in any manifestation.
func TestUnreviewed_OutcomeChangeByAuthorKeepsTheMark(t *testing.T) {
	e := newUnreviewedEnv(t)
	reviewed := e.prefix + "-3"
	if reviewedAtOf(t, e.pool, reviewed) == nil {
		t.Fatalf("precondition: %s is not marked reviewed", reviewed)
	}

	// The author moves the outcome away from "done" themselves — the mark is intact.
	patchOutcomeAs(t, e.pool, reviewed, issues.CloseOutcomeWontDo, e.caller, e.callerName)
	if reviewedAtOf(t, e.pool, reviewed) == nil {
		t.Fatalf("an edit by the author reset the mark — they know about their own change")
	}

	// A non-author changes the outcome BETWEEN the dropped ones — nothing to invalidate.
	outsider, outsiderName := seedIMUser(t, e.pool, "user")
	// sk-540.2: without a target the change to duplicate needs the
	// `duplicates` edge to exist already (exception (a)); -2 is an issue of
	// the same project that stays outside the set either way.
	if _, err := e.pool.Exec(t.Context(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'duplicates', '{}'::jsonb, 1, $3, now())`, reviewed, e.prefix+"-2", e.caller); err != nil {
		t.Fatalf("seed duplicates edge: %v", err)
	}
	patchOutcomeAs(t, e.pool, reviewed, issues.CloseOutcomeDuplicate, outsider, outsiderName)
	if reviewedAtOf(t, e.pool, reviewed) == nil {
		t.Fatalf("change wont_do → duplicate reset the mark — the move was not away from \"done\"")
	}

	cross := e.crossPathIDs(t, "unreviewed=true")
	project := e.projectPathIDs(t, "createdBy=me&unreviewed=true")
	_, count := e.secondNumber(t)
	nudge := e.nudgeSetIDs(t)

	if !equalIDs(cross, e.wantSet) {
		t.Errorf("cross-project selection = %v, want the previous set %v", cross, e.wantSet)
	}
	if !equalIDs(project, e.wantSet) {
		t.Errorf("project selection = %v, want the previous set %v", project, e.wantSet)
	}
	if count == nil || *count != len(e.wantSet) {
		t.Errorf("second number %v, want %d", count, len(e.wantSet))
	}
	if !equalIDs(nudge, e.wantSet) {
		t.Errorf("reminder recipients see %v, want %v", nudge, e.wantSet)
	}
}

// TestUnreviewed_OutcomeChangeWithoutReviewRequired — the fourth cell
// of row 15: with the "review required" flag off the reset HAPPENS, and
// there is no observable effect.
//
// The cell is not cosmetic: an implementation that put a condition on the flag into
// the reset predicate itself ("do not reset if review is not required")
// would pass all three manifestations and diverge from the product exactly here — in
// the column value that the next person to turn the flag on will see.
func TestUnreviewed_OutcomeChangeWithoutReviewRequired(t *testing.T) {
	e := newUnreviewedEnv(t)

	// A separate issue: closed, delegated, the mark is set, and the
	// "review required" flag is OFF.
	id := e.prefix + "-10"
	worker, workerName := seedIMUser(t, e.pool, "user")
	seedReviewIssue(t, e.pool, e.project, e.caller, id, "closed", workerName, false, &e.caller, nil, nil)
	_ = worker

	before := e.crossPathIDs(t, "unreviewed=true")
	if hasID(before, id) {
		t.Fatalf("precondition: the issue with the flag off got into the set: %v", before)
	}

	outsider, outsiderName := seedIMUser(t, e.pool, "user")
	patchOutcomeAs(t, e.pool, id, issues.CloseOutcomeWontDo, outsider, outsiderName)

	if reviewedAtOf(t, e.pool, id) != nil {
		t.Fatalf("the reset did not happen: the reset predicate has no right to look at the " +
			"\"review required\" flag — the flag decides whether the consequence is VISIBLE, not whether the reset happens")
	}
	after := e.crossPathIDs(t, "unreviewed=true")
	if hasID(after, id) {
		t.Fatalf("the issue with the flag off entered the set after the reset: %v", after)
	}
	if !equalIDs(after, e.wantSet) {
		t.Errorf("the set changed: %v, want %v", after, e.wantSet)
	}
}
