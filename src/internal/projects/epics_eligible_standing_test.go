package projects

import (
	"testing"

	"github.com/google/uuid"
)

// projectEligibleCount lists projects as `owner` and returns pid's
// EpicsEligibleCount, failing the test if the project is absent.
func projectEligibleCount(t *testing.T, store *Store, owner, pid uuid.UUID) int {
	t.Helper()
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for i := range res.Items {
		if res.Items[i].ID == pid {
			return res.Items[i].EpicsEligibleCount
		}
	}
	t.Fatalf("project %s not found in list", pid)
	return -1
}

// TestEligibleEpics_StandingOnlyEpic_NotCounted (sk-118.9 F2h / R6, codex
// F2h warning): the project-list EpicsEligibleCount must agree with
// issues.Store.epicRollupFor.EligibleForClose. §11.5 makes `standing`
// children INVISIBLE to the rollup, so a standing-only epic has Total == 0
// and reads as an EMPTY epic → NOT eligible (EligibleForClose = false,
// asserted by TestEpicRollup_EpicWithOnlyStandingChild_NotEligibleButCloseable).
//
// Before the fix, epicsEligibleCountExpr's child-presence EXISTS counted ANY
// child (including standing), so the standing-only epic was counted eligible
// here while the rollup said not-eligible — the two surfaces disagreed. The
// fix narrows the EXISTS to a NON-STANDING child; this test pins that a
// standing-only epic is NOT counted.
func TestEligibleEpics_StandingOnlyEpic_NotCounted(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "eso", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "eso-" + pid.String()[:8]

	// Epic — 1 standing child ONLY. No countable (non-standing) child exists,
	// so the epic reads as empty → NOT eligible (matches epicRollupFor:
	// Total == 0 ⇒ EligibleForClose = false).
	epic := prefix + "-1"
	seedTypedIssueWithParent(t, pool, pid, owner, epic, "open", "epic", nil)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-1.1", "standing", "task", &epic)

	if got := projectEligibleCount(t, NewStore(pool), owner, pid); got != 0 {
		t.Errorf("EpicsEligibleCount = %d, want 0 (standing-only epic is invisible to rollup → reads as empty → not eligible, consistent with epicRollupFor.EligibleForClose=false)", got)
	}
}

// TestEligibleEpics_ClosedPlusStandingChild_Counted: an epic with a real
// closed child PLUS a standing child IS eligible/counted — the standing
// child does not suppress eligibility once a countable (closed) child exists.
// This is the positive flip and matches
// TestEpicRollup_EpicWithStandingPlusAllClosed_Eligible
// (EligibleForClose = true: Total == Closed == 1, standing excluded).
func TestEligibleEpics_ClosedPlusStandingChild_Counted(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "ecs", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "ecs-" + pid.String()[:8]

	// Epic — 1 closed child + 1 standing child. The closed child is a
	// countable child (status <> 'standing') → the child-presence EXISTS
	// passes; there is no active (non-standing, non-closed) child → the
	// NOT EXISTS passes ⇒ eligible.
	epic := prefix + "-1"
	seedTypedIssueWithParent(t, pool, pid, owner, epic, "open", "epic", nil)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-1.1", "closed", "task", &epic)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-1.2", "standing", "task", &epic)

	if got := projectEligibleCount(t, NewStore(pool), owner, pid); got != 1 {
		t.Errorf("EpicsEligibleCount = %d, want 1 (closed child + standing child ⇒ eligible; standing does not suppress, consistent with epicRollupFor.EligibleForClose=true)", got)
	}
}

// TestEligibleEpics_OpenPlusStandingChild_NotCounted: an open child still
// gates the close even when a standing child is present → NOT eligible.
// Confirms the standing exclusion does not over-count (matches
// epicRollupFor: an active non-standing child ⇒ Closed < Total ⇒ false).
func TestEligibleEpics_OpenPlusStandingChild_NotCounted(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "eos", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "eos-" + pid.String()[:8]

	epic := prefix + "-1"
	seedTypedIssueWithParent(t, pool, pid, owner, epic, "open", "epic", nil)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-1.1", "open", "task", &epic)
	seedTypedIssueWithParent(t, pool, pid, owner, prefix+"-1.2", "standing", "task", &epic)

	if got := projectEligibleCount(t, NewStore(pool), owner, pid); got != 0 {
		t.Errorf("EpicsEligibleCount = %d, want 0 (open child gates the close even with a standing sibling)", got)
	}
}

// TestEligibleEpics_EmptyEpic_NotCounted is the control: a childless epic is
// NOT counted (the child-presence EXISTS prunes empties). Same outcome as a
// standing-only epic — the whole point of the F2h fix is that these two
// cases are indistinguishable to both surfaces.
func TestEligibleEpics_EmptyEpic_NotCounted(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "eee", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "eee-" + pid.String()[:8]

	epic := prefix + "-1"
	seedTypedIssueWithParent(t, pool, pid, owner, epic, "open", "epic", nil)

	if got := projectEligibleCount(t, NewStore(pool), owner, pid); got != 0 {
		t.Errorf("EpicsEligibleCount = %d, want 0 (empty epic is never eligible)", got)
	}
}
