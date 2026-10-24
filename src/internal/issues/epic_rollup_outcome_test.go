package issues

// epic_rollup_outcome_test.go — sk-489, the epic rollup by OUTCOME.
//
// Matrix row 20: ALL acceptance examples of US-5 as a table, both on-screen
// identities on each of them, the standing child outside the count.
//
// Why the identity is checked rather than "obvious from the code". The rollup is ONE
// partition of one set of children, computed by nine independent
// FILTERs of one statement. Nothing in SQL prevents two legs from overlapping
// or leaving a hole; on screen that looks like a ring that does not
// close — the arcs fill not the whole circle or overflow it. The identity is the
// only claim that catches such a hole without listing all
// its possible forms.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rollupChild — one child of the epic in the example fixture.
type rollupChild struct {
	status string
	// outcome is filled only for a closed child; empty when status != closed.
	outcome string
}

// seedRollupExample builds an epic with the listed children and returns its
// rollup as the project owner sees it.
//
// hidden — children in a PROJECT the reader cannot see; their states
// are listed the same way as the visible ones. They live in a separate parameter rather than
// a flag in rollupChild, because an unavailable child differs from a
// visible one not by state but by project, and storing that in the "status" field would
// mean introducing a fourth value for a three-valued concept.
func seedRollupExample(t *testing.T, pool *pgxpool.Pool, kids, hidden []rollupChild) EpicRollup {
	t.Helper()
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	epicID := prefix + "-1"
	seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
	setKidState := func(id string, kid rollupChild) {
		if kid.status == "closed" {
			// The outcome is written together with the status: trigger 000050 seeds
			// `done` on any raw close, and a two-step write would put the
			// fixture through a state the example did not ask for.
			setClosedWithOutcome(t, pool, id, kid.outcome)
		} else if kid.status != "open" {
			setIssueStatus(t, pool, id, kid.status)
		}
	}
	for i, kid := range kids {
		id := prefix + "-1." + itoa(i+1)
		seedTypedIssue(t, pool, pid, owner, id, "child", TypeTask, &epicID)
		setKidState(id, kid)
	}
	if len(hidden) > 0 {
		outsider := seedUser(t, pool)
		hiddenPID, hiddenPrefix := seedProject(t, pool, outsider)
		for i, kid := range hidden {
			// The identifier must carry a FOREIGN prefix — the issue lives in
			// a foreign project — so the edge is written separately.
			id := hiddenPrefix + "-" + itoa(i+1)
			seedTypedIssue(t, pool, hiddenPID, outsider, id, "secret child", TypeTask, nil)
			setKidState(id, kid)
			seedParentChild(t, pool, id, epicID, outsider)
		}
	}

	dto, err := NewStore(pool).GetIssue(callerCtx(t, owner, "user"), epicID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", epicID, err)
	}
	if dto.Epic == nil {
		t.Fatalf("epic %s has no rollup", epicID)
	}
	return *dto.Epic
}

// assertRingIdentities — the on-screen identity of the ring (US-5, [leading]).
//
//	done + open + in progress + deferred + unavailable = denominator
//	denominator + "no work" = direct non-standing children
//
// Both equalities must hold FOR ANY data: the ring is drawn from
// these numbers, and a broken equality means arcs that do not fill the circle.
func assertRingIdentities(t *testing.T, label string, r EpicRollup) {
	t.Helper()
	parts := r.DoneChildren + r.OpenChildren + r.InProgressChildren +
		r.DeferredChildren + r.UnavailableChildren
	if parts != r.CountedChildren {
		t.Errorf("[%s] the ring parts do not add up to the denominator: "+
			"done(%d)+open(%d)+inProgress(%d)+deferred(%d)+unavailable(%d) = %d, "+
			"but counted = %d. This is a ring whose arcs do not fill the circle",
			label, r.DoneChildren, r.OpenChildren, r.InProgressChildren,
			r.DeferredChildren, r.UnavailableChildren, parts, r.CountedChildren)
	}
	if r.CountedChildren+r.NoWorkChildren != r.TotalChildren {
		t.Errorf("[%s] counted(%d) + noWork(%d) = %d, but direct non-standing children are %d — "+
			"the \"no work\" tail and the denominator must add up to all the ring's children",
			label, r.CountedChildren, r.NoWorkChildren,
			r.CountedChildren+r.NoWorkChildren, r.TotalChildren)
	}
}

// TestEpicRollup_OutcomeNumbers_US5Examples — the T006 gate smoke on
// the acceptance examples of US-5.
func TestEpicRollup_OutcomeNumbers_US5Examples(t *testing.T) {
	done := func(n int) []rollupChild {
		out := make([]rollupChild, n)
		for i := range out {
			out[i] = rollupChild{status: "closed", outcome: CloseOutcomeDone}
		}
		return out
	}
	wontDo := rollupChild{status: "closed", outcome: CloseOutcomeWontDo}
	duplicate := rollupChild{status: "closed", outcome: CloseOutcomeDuplicate}

	cases := []struct {
		name                          string
		kids, hidden                  []rollupChild
		done, noWork, counted         int
		inProgress, open, closedTotal int
		unavailable, standing, total  int
	}{
		{
			name: "3 done + 1 won't do -> ring 100%, \"3 of 3\", \"no work: 1\"",
			kids: append(done(3), wontDo),
			done: 3, noWork: 1, counted: 3,
			closedTotal: 4, total: 4,
		},
		{
			name: "2 done + 1 open + 1 duplicate -> \"2 of 3\", \"no work: 1\"",
			kids: append(done(2), rollupChild{status: "open"}, duplicate),
			done: 2, noWork: 1, counted: 3,
			open: 1, closedTotal: 3, total: 4,
		},
		{
			name: "1 done + 1 in progress + 2 won't do -> \"1 of 2\", \"no work: 2\"",
			kids: []rollupChild{
				{status: "closed", outcome: CloseOutcomeDone},
				{status: "in_progress"},
				wontDo, wontDo,
			},
			done: 1, noWork: 2, counted: 2, inProgress: 1, closedTotal: 3, total: 4,
		},
		{
			name:   "2 done + 1 won't do + 1 unavailable -> \"2 of 3\", unavailable as today",
			kids:   append(done(2), wontDo),
			hidden: []rollupChild{{status: "open"}},
			done:   2, noWork: 1, counted: 3,
			closedTotal: 3, unavailable: 1, total: 4,
		},
		{
			// SC-004 inside the examples table: the same example, but the unavailable
			// child is itself closed WITHOUT WORK. All numbers must match
			// the previous row exactly — the outcome of an unavailable one has no
			// right to move any of them.
			name:   "...and the same example with an unavailable one closed without work — same numbers",
			kids:   append(done(2), wontDo),
			hidden: []rollupChild{wontDo},
			done:   2, noWork: 1, counted: 3,
			closedTotal: 3, unavailable: 1, total: 4,
		},
		{
			name: "all visible dropped out, no unavailable -> denominator 0",
			kids: []rollupChild{wontDo, wontDo},
			done: 0, noWork: 2, counted: 0,
			closedTotal: 2, total: 2,
		},
		{
			name:   "all visible dropped out + 1 unavailable -> \"0 of 1\"",
			kids:   []rollupChild{wontDo, wontDo},
			hidden: []rollupChild{{status: "open"}},
			done:   0, noWork: 2, counted: 1,
			closedTotal: 2, unavailable: 1, total: 3,
		},
		{
			name: "the standing child is not counted by any ring number",
			kids: append(done(1), wontDo, rollupChild{status: "standing"}),
			done: 1, noWork: 1, counted: 1,
			closedTotal: 2, standing: 1, total: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := newTestPool(t)
			got := seedRollupExample(t, pool, c.kids, c.hidden)

			if got.DoneChildren != c.done || got.NoWorkChildren != c.noWork ||
				got.CountedChildren != c.counted {
				t.Errorf("{done:%d noWork:%d counted:%d}, want {%d %d %d}; whole rollup: %+v",
					got.DoneChildren, got.NoWorkChildren, got.CountedChildren,
					c.done, c.noWork, c.counted, got)
			}
			// The BY-STATUS breakdown next to it does not move (INV-3): closed
			// is still all closed ones, whatever their outcome; the standing one is in
			// its own bucket and outside total; the unavailable one is in its own and inside it.
			if got.ClosedChildren != c.closedTotal || got.InProgressChildren != c.inProgress ||
				got.OpenChildren != c.open || got.UnavailableChildren != c.unavailable ||
				got.StandingChildren != c.standing || got.TotalChildren != c.total {
				t.Errorf("the by-status breakdown shifted: {closed:%d inProgress:%d open:%d "+
					"unavailable:%d standing:%d total:%d}, want {%d %d %d %d %d %d} — INV-3 "+
					"requires the old keys to keep their meaning",
					got.ClosedChildren, got.InProgressChildren, got.OpenChildren,
					got.UnavailableChildren, got.StandingChildren, got.TotalChildren,
					c.closedTotal, c.inProgress, c.open, c.unavailable, c.standing, c.total)
			}
			assertRingIdentities(t, c.name, got)
		})
	}
}

// TestEpicRollup_EligibleForCloseIgnoresOutcome — INV-6, as an explicit claim
// next to the numbers it does NOT read: an epic all of whose children are closed, and
// some without work, is closable. A closed-without-work issue is closed, and
// there is no reason for it to hold the epic open.
func TestEpicRollup_EligibleForCloseIgnoresOutcome(t *testing.T) {
	pool := newTestPool(t)
	got := seedRollupExample(t, pool, []rollupChild{
		{status: "closed", outcome: CloseOutcomeDone},
		{status: "closed", outcome: CloseOutcomeWontDo},
		{status: "closed", outcome: CloseOutcomeDuplicate},
		{status: "closed", outcome: CloseOutcomeSuperseded},
	}, nil)
	if !got.EligibleForClose {
		t.Errorf("eligibleForClose = false with all children closed (%+v); the flag is counted "+
			"by STATUS and does not depend on the outcome (INV-6)", got)
	}
	// The denominator here is 1, not 0: three of four dropped out, the fourth is done
	// and did not drop out of the denominator. This epic's ring is filled completely —
	// "1 of 1" — and that is exactly what should be visible: the work that
	// existed is all done.
	if got.CountedChildren != 1 || got.DoneChildren != 1 || got.NoWorkChildren != 3 {
		t.Errorf("{done:%d noWork:%d counted:%d}, want {1 3 1}",
			got.DoneChildren, got.NoWorkChildren, got.CountedChildren)
	}
	assertRingIdentities(t, "all closed, three without work", got)
}
