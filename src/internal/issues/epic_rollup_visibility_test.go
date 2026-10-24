package issues

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

// sk-291 — the epic rollup counted children the caller cannot see, and not
// merely into a total: into the per-status breakdown. Parent-child is
// allowed to cross projects (sk-35), so it is enough for somebody to hang a
// task from a closed project under a visible epic, and an outsider watches
// it move open → in_progress → closed.
//
// The product already answers this question next door, and answers it
// differently: the dependency list keeps the EDGE (the user knows there is
// a link) and blanks the other side's card with redacted:true. Status is
// card content. The sk-282 counters made the same call with a named
// `unavailable` bucket that is deliberately not broken down by status.
// This fixes the rollup to the second of those, because the counters are
// what it is — and because one product must not answer "what is behind the
// wall" two different ways.

// callerCtx builds the request context a REST/MCP caller would arrive with.
func callerCtx(t *testing.T, userID uuid.UUID, role string) context.Context {
	t.Helper()
	ctx := auth.WithUserID(t.Context(), userID)
	ctx = auth.WithUserRole(ctx, role)
	return ctx
}

// crossProjectEpicFixture builds: an epic in a project the caller owns,
// two visible children (one open, one closed), and one child in a project
// the caller is not a member of, whose STATE the test can pick.
//
// sk-489 — the state is «status», or «closed/<outcome>» when the test needs
// the invisible child to be closed with a particular outcome. One parameter
// rather than two because every caller is a loop over states, and the
// property under test is «all of these states look the same from here»: a
// second parameter would invite a call that varies the outcome while holding
// the status, which is not a state the invariant distinguishes.
//
// The outcome is written raw, with the status, in ONE update: the 000050
// trigger seeds `done` on any raw close, so setting the status first and the
// outcome second would pass through a state the fixture did not ask for.
func crossProjectEpicFixture(t *testing.T, pool *pgxpool.Pool, hiddenChildState string) (
	caller uuid.UUID, epicID string,
) {
	t.Helper()
	caller = seedUser(t, pool)
	outsider := seedUser(t, pool)

	visiblePID, prefix := seedProject(t, pool, caller)
	hiddenPID, hiddenPrefix := seedProject(t, pool, outsider)

	epicID = prefix + "-1"
	seedTypedIssue(t, pool, visiblePID, caller, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, visiblePID, caller, prefix+"-1.1", "open child", TypeTask, &epicID)
	seedTypedIssue(t, pool, visiblePID, caller, prefix+"-1.2", "closed child", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.2", "closed")

	// The cross-project child. seedTypedIssue would write the parent-child
	// edge for us, but the hierarchical id has to belong to the OTHER
	// project's prefix, so the edge is written explicitly.
	hiddenID := hiddenPrefix + "-9"
	seedTypedIssue(t, pool, hiddenPID, outsider, hiddenID, "secret child", TypeTask, nil)
	if status, outcome, ok := strings.Cut(hiddenChildState, "/"); ok {
		if status != "closed" {
			t.Fatalf("crossProjectEpicFixture: an outcome is allowed only on a closed issue (%q)", hiddenChildState)
		}
		setClosedWithOutcome(t, pool, hiddenID, outcome)
	} else {
		setIssueStatus(t, pool, hiddenID, hiddenChildState)
	}
	seedParentChild(t, pool, hiddenID, epicID, outsider)

	return caller, epicID
}

// TestEpicRollup_InvisibleChildIsNotBucketedByStatus is the regression: the
// observer must not be able to tell the invisible child's status apart —
// the numbers are IDENTICAL whichever status it is in.
//
// sk-489, matrix row 21: `closed/wont_do` is added to the states. The outcome
// of an invisible child is the same card content as the status (R-6),
// and the new "no work" leg is a new channel through which it could leak. Without
// this state the channel would stay unchecked: in all the earlier
// states a closed invisible child is closed by a raw UPDATE and gets
// the outcome "done" from trigger 000050, so it does not land in the "no work" leg
// under ANY implementation — and lifting the horizon off that leg would pass
// the test unnoticed (verified by mutation when T006 was handed in).
func TestEpicRollup_InvisibleChildIsNotBucketedByStatus(t *testing.T) {
	type shot struct {
		status string
		rollup EpicRollup
	}
	var shots []shot

	for _, st := range []string{"open", "in_progress", "closed", "deferred", "standing",
		"closed/" + CloseOutcomeWontDo} {
		pool := newTestPool(t)
		caller, epicID := crossProjectEpicFixture(t, pool, st)

		store := NewStore(pool)
		dto, err := store.GetIssue(callerCtx(t, caller, "user"), epicID)
		if err != nil {
			t.Fatalf("GetIssue (%s): %v", st, err)
		}
		if dto.Epic == nil {
			t.Fatalf("no rollup on the epic (%s)", st)
		}
		shots = append(shots, shot{status: st, rollup: *dto.Epic})
	}

	first := shots[0]
	for _, s := range shots[1:] {
		if s.rollup != first.rollup {
			t.Fatalf("the invisible child's status is derivable from the rollup:\n"+
				" %s -> %+v\n %s -> %+v", first.status, first.rollup, s.status, s.rollup)
		}
	}

	got := first.rollup
	if got.UnavailableChildren != 1 {
		t.Errorf("UnavailableChildren = %d, want 1", got.UnavailableChildren)
	}
	// The two visible children land in their real buckets.
	if got.OpenChildren != 1 || got.ClosedChildren != 1 {
		t.Errorf("visible buckets = open %d / closed %d, want 1 / 1", got.OpenChildren, got.ClosedChildren)
	}
	if got.InProgressChildren != 0 || got.StandingChildren != 0 {
		t.Errorf("invisible child leaked into a status bucket: %+v", got)
	}
	// Total keeps the invisible child — a vanishing cross-project tail is
	// what makes an unfinished wave read as finished.
	if got.TotalChildren != 3 {
		t.Errorf("TotalChildren = %d, want 3 (2 visible + 1 unavailable)", got.TotalChildren)
	}
	if got.EligibleForClose {
		t.Errorf("EligibleForClose = true with an unavailable child; the honest answer is \"cannot say\"")
	}
}

// TestEpicRollup_AllChildrenVisible_NumbersUnchanged is the other half of
// the acceptance: nothing moves for the ordinary case.
func TestEpicRollup_AllChildrenVisible_NumbersUnchanged(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.1", "open", TypeTask, &epicID)
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.2", "wip", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.2", "in_progress")
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.3", "done", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.3", "closed")
	seedTypedIssue(t, pool, pid, owner, prefix+"-1.4", "perpetual", TypeTask, &epicID)
	setIssueStatus(t, pool, prefix+"-1.4", "standing")

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, owner, "user"), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	want := EpicRollup{
		TotalChildren:      3, // standing excluded, as before
		OpenChildren:       1,
		InProgressChildren: 1,
		ClosedChildren:     1,
		StandingChildren:   1,
		// sk-489: the only closed child is closed by a raw UPDATE, that
		// is with the outcome "done" from trigger 000050 — data where all
		// closed ones are "done" (SC-005). The literal is compared IN FULL, so
		// the new keys are named here explicitly: the old ones must stay the same,
		// and the new ones may only be added.
		DoneChildren:    1,
		NoWorkChildren:  0,
		CountedChildren: 3,
	}
	if *dto.Epic != want {
		t.Fatalf("rollup = %+v, want %+v", *dto.Epic, want)
	}

	// SC-005, stated as a RELATION, not a literal. The literal above says "these
	// six numbers", the relation says "the new keys, on data with only "done",
	// are DERIVED from the old ones". The second survives a change of fixture, the first does not, and
	// it is the second that promises the client that moving to the new keys
	// shifts nothing.
	got := *dto.Epic
	if got.DoneChildren != got.ClosedChildren {
		t.Errorf("doneChildren = %d, closedChildren = %d: on data where all closed ones are "+
			"\"done\", these are ONE AND THE SAME number (SC-005)", got.DoneChildren, got.ClosedChildren)
	}
	if got.CountedChildren != got.TotalChildren {
		t.Errorf("countedChildren = %d, totalChildren = %d: nobody dropped out, the denominator equals "+
			"the whole (SC-005)", got.CountedChildren, got.TotalChildren)
	}
	if got.NoWorkChildren != 0 {
		t.Errorf("noWorkChildren = %d, want 0: nobody dropped out", got.NoWorkChildren)
	}
}

// An admin sees everything, so the cross-project child is counted normally
// and `unavailable` stays empty — the bucket is about the CALLER, not about
// the project boundary.
func TestEpicRollup_AdminSeesTheCrossProjectChild(t *testing.T) {
	pool := newTestPool(t)
	caller, epicID := crossProjectEpicFixture(t, pool, "in_progress")
	// The admin is an admin in users: since sk-674.18 T007 the horizon reads
	// the role from the access relation, not from the request context (D-2).
	if _, err := pool.Exec(t.Context(), `UPDATE users SET role = 'admin' WHERE id = $1`, caller); err != nil {
		t.Fatalf("make the caller an admin: %v", err)
	}

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, caller, "admin"), epicID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.Epic.UnavailableChildren != 0 {
		t.Errorf("UnavailableChildren = %d for an admin, want 0", dto.Epic.UnavailableChildren)
	}
	if dto.Epic.InProgressChildren != 1 {
		t.Errorf("InProgressChildren = %d, want 1 — the admin can see it", dto.Epic.InProgressChildren)
	}
}

// The list row's progress chip is the same leak in a smaller shape: a
// (closed, total) pair still discloses whether the invisible child is done.
func TestEpicProgress_ListRow_InvisibleChildIsUnavailable(t *testing.T) {
	var shots []EpicProgress
	for _, st := range []string{"open", "closed", "closed/" + CloseOutcomeWontDo} {
		pool := newTestPool(t)
		caller, epicID := crossProjectEpicFixture(t, pool, st)

		store := NewStore(pool)
		var projectID uuid.UUID
		if err := pool.QueryRow(t.Context(),
			`SELECT project_id FROM issues WHERE id = $1`, epicID).Scan(&projectID); err != nil {
			t.Fatalf("read project: %v", err)
		}
		res, err := store.ListIssues(callerCtx(t, caller, "user"), ListIssuesArgs{
			ProjectID: projectID,
			Caller:    &caller,
		})
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		var chip *EpicProgress
		for _, it := range res.Items {
			if it.ID == epicID {
				chip = it.EpicProgress
			}
		}
		if chip == nil {
			t.Fatalf("no epicProgress on the epic row")
		}
		if chip.Unavailable != 1 {
			t.Errorf("[%s] unavailable = %d, want 1", st, chip.Unavailable)
		}
		// closed counts ONLY visible closed children, whichever status the
		// invisible one is in — that is what stops the chip disclosing it.
		if chip.Closed != 1 || chip.Total != 3 {
			t.Errorf("[%s] chip = %d/%d, want 1/3", st, chip.Closed, chip.Total)
		}
		// sk-489, matrix row 18 (SC-004, INV-4). The outcome of an invisible child
		// is the same card content as the status, and it must not be derived from the row's
		// numbers. The by-outcome numbers are counted over the VISIBLE
		// children: of the visible ones one is closed "done" (trigger 000050 on a raw
		// close), so done=1 and noWork=0; the invisible one stays in total
		// and therefore in counted — "cannot tell", not "dropped out".
		if chip.Done != 1 || chip.NoWork != 0 || chip.Counted != 3 {
			t.Errorf("[%s] by-outcome numbers = {done:%d noWork:%d counted:%d}, want {1 0 3}: "+
				"an invisible child enters no outcome leg under ANY of its outcomes (INV-4), "+
				"yet does not drop out of the denominator",
				st, chip.Done, chip.NoWork, chip.Counted)
		}
		shots = append(shots, *chip)
	}

	// And the main claim of row 18: the five numbers are IDENTICAL in all three
	// states of the invisible child. The values above could have been obtained
	// by accident; the impossibility of telling the states apart is SC-004.
	for i, shot := range shots[1:] {
		if shot != shots[0] {
			t.Errorf("the outcome/status of an invisible child is derivable from the row's numbers: "+
				"state #0 -> %+v, state #%d -> %+v", shots[0], i+1, shot)
		}
	}
}
