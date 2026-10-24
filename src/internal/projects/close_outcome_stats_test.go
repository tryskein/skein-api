package projects

// close_outcome_stats_test.go — sk-494: "closed" does not equal "done" in
// project statistics.
//
// What is guarded here. ProjectStats.Closed stays the number of ALL closed
// issues whatever the outcome, and next to it two halves appear: Done
// (outcome "done") and NoWork (the three other outcomes — "won't do",
// "duplicate", "superseded"). The rule is not recomputed here: it is the same as
// in the epic progress (`issues/store.go#epicSummaryNoWorkExpr`, sk-489
// INV-5), and is written in the query `store.go#GetProjectWithStats` as one leg.
//
// Why the fixture is what it is. The three "dropped" issues are taken with DIFFERENT
// outcomes, not three `wont_do`: the leg is written as a blacklist ("everything
// that is not done"), and a fixture of a single value would not tell it from a leg
// that checks exactly that one value. The numbers of the halves are deliberately DIFFERENT
// (2 and 3) — with equal numbers a swapped Scan order would pass green.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedClosedIssueWithOutcome seeds a closed issue carrying an explicit
// close outcome. It goes through seedIssueRaw and then sets the outcome,
// so the row is built exactly like every other fixture row in this
// package and differs from them in one column only.
//
// The two-step is not laziness: the 000050 trigger normalises a closed
// row with no outcome to `done`, so the UPDATE is what makes the row
// distinguishable at all — and going through it proves the column is
// writable to the three no-work values, which a bare INSERT of the
// literal would also prove but with a second spelling of the fixture.
func seedClosedIssueWithOutcome(
	t *testing.T, pool *pgxpool.Pool, project, creator uuid.UUID, idStr, outcome string,
) {
	t.Helper()
	seedIssueRaw(t, pool, project, creator, idStr, "closed", "")
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET close_outcome = $2 WHERE id = $1`, idStr, outcome); err != nil {
		t.Fatalf("set close_outcome=%s on %s: %v", outcome, idStr, err)
	}
}

// TestGetProjectWithStats_CloseOutcomeSplit — the main case: closed
// issues with different outcomes lie in one project, and the statistics tell them apart.
func TestGetProjectWithStats_CloseOutcomeSplit(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "gco", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "gco-" + pid.String()[:8]
	// 2 "done" — one through an explicit outcome, one through the trigger default
	// (a closed row with no outcome → done). Both must land in Done:
	// otherwise the seeded history (US-8) would drop out of the numerator.
	seedClosedIssueWithOutcome(t, pool, pid, owner, prefix+"-1", "done")
	seedIssueRaw(t, pool, pid, owner, prefix+"-2", "closed", "")
	// 3 "no work" — all three values of the set, one each.
	seedClosedIssueWithOutcome(t, pool, pid, owner, prefix+"-3", "wont_do")
	seedClosedIssueWithOutcome(t, pool, pid, owner, prefix+"-4", "duplicate")
	seedClosedIssueWithOutcome(t, pool, pid, owner, prefix+"-5", "superseded")
	// Unclosed ones have no outcome at all (INV-1) and cannot fall into
	// either half.
	seedIssue(t, pool, pid, owner, prefix+"-6", "open", "")
	seedIssue(t, pool, pid, owner, prefix+"-7", "in_progress", "")

	store := NewStore(pool)
	dto, err := store.GetProjectWithStats(storeFixtureCtx(), pid, owner)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := dto.Stats

	if got.Closed != 5 {
		t.Errorf("closed = %d, want 5 — \"closed\" must stay the number of ALL closed", got.Closed)
	}
	if got.Done != 2 {
		t.Errorf("done = %d, want 2", got.Done)
	}
	if got.NoWork != 3 {
		t.Errorf("noWork = %d, want 3", got.NoWork)
	}
	if got.Done+got.NoWork != got.Closed {
		t.Errorf("identity broken: done(%d) + noWork(%d) != closed(%d)",
			got.Done, got.NoWork, got.Closed)
	}
	if got.Total != 7 {
		t.Errorf("total = %d, want 7 — the split has no right to touch the total", got.Total)
	}
	if got.Open != 1 || got.InProgress != 1 {
		t.Errorf("open=%d in_progress=%d, want 1/1 — unclosed ones do not take part in the split",
			got.Open, got.InProgress)
	}
}

// TestGetProjectWithStats_CloseOutcomeSplit_AllDone — the CONTROL to
// the previous one. "noWork = 3" by itself does not prove that the leg can
// answer zero at all: a leg counting ALL closed would give 5, and a leg
// counting anything at all could give 3 here too. This test fixes
// the other end of the scale — on a project where all closed issues are done, "no work"
// must be zero, and "done" must coincide with "closed".
//
// This is also the acceptance of a US-8-like history: before sk-489 nobody had
// outcomes, the migration seeded every closed issue with "done", and the statistics of such a
// project have no right to show a single dropped issue.
func TestGetProjectWithStats_CloseOutcomeSplit_AllDone(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "gcd", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "gcd-" + pid.String()[:8]
	seedIssueRaw(t, pool, pid, owner, prefix+"-1", "closed", "")
	seedIssueRaw(t, pool, pid, owner, prefix+"-2", "closed", "")
	seedIssueRaw(t, pool, pid, owner, prefix+"-3", "closed", "")
	seedIssue(t, pool, pid, owner, prefix+"-4", "open", "")

	store := NewStore(pool)
	dto, err := store.GetProjectWithStats(storeFixtureCtx(), pid, owner)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := dto.Stats
	if got.Closed != 3 || got.Done != 3 || got.NoWork != 0 {
		t.Errorf("closed=%d done=%d noWork=%d, want 3/3/0",
			got.Closed, got.Done, got.NoWork)
	}
}

// TestGetProject_CloseOutcomeSplit_CallerAgnostic — the split arrives also
// on the path WITHOUT a caller (GetProject → callerID = uuid.Nil). The two
// halves are properties of issues, not of the reader's load, and they have no right to
// reset to zero together with MyAssigned / MyCreated.
func TestGetProject_CloseOutcomeSplit_CallerAgnostic(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "gca", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	// The halves are deliberately of different size (2 and 1): with 1:1 numbers
	// swapped places would pass green.
	prefix := "gca-" + pid.String()[:8]
	seedIssueRaw(t, pool, pid, owner, prefix+"-1", "closed", "")
	seedClosedIssueWithOutcome(t, pool, pid, owner, prefix+"-2", "done")
	seedClosedIssueWithOutcome(t, pool, pid, owner, prefix+"-3", "duplicate")

	store := NewStore(pool)
	dto, err := store.GetProject(storeFixtureCtx(), pid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := dto.Stats
	if got.MyAssigned != 0 || got.MyCreated != 0 {
		t.Fatalf("caller-scoped counts leaked: myAssigned=%d myCreated=%d",
			got.MyAssigned, got.MyCreated)
	}
	if got.Closed != 3 || got.Done != 2 || got.NoWork != 1 {
		t.Errorf("closed=%d done=%d noWork=%d, want 3/2/1", got.Closed, got.Done, got.NoWork)
	}
}
