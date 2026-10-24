package issues

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-118 (F2e) — retire-block on Close(). Closing a `standing` issue is a
// RETIRE, and a standing task's children are its reason to exist. The store
// rejects the retire (ErrStandingHasOpenChildren, code
// standing_has_open_children) while ANY non-closed child remains, in EITHER
// hierarchy representation (the canonical parent-child dep edge OR the legacy
// issues.parent_id column). "Non-closed" = status <> 'closed', so a deferred
// child still blocks (§11.4). The guard is standing-specific: a normal epic
// with open children stays closeable (§11.9 regression). An empty retire and
// an all-children-closed retire both succeed.

// linkParentIDColumn points the legacy issues.parent_id column of child at
// parent (the pre-sk-35 hierarchy representation).
func linkParentIDColumn(t *testing.T, pool *pgxpool.Pool, childID, parentID string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET parent_id = $1 WHERE id = $2`, parentID, childID); err != nil {
		t.Fatalf("set parent_id %s -> %s: %v", childID, parentID, err)
	}
}

// assertStandingHasOpenChildren asserts err is the F2e retire-block error,
// flows through IsValidationError with the stable code, and (optionally) lists
// the expected offending child id.
func assertStandingHasOpenChildren(t *testing.T, err error, wantChild string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Close should have been rejected, got nil")
	}
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "standing_has_open_children" {
		t.Fatalf("err = %v, want validation code standing_has_open_children", err)
	}
	var soc *ErrStandingHasOpenChildren
	if !errors.As(err, &soc) {
		t.Fatalf("err = %v, want *ErrStandingHasOpenChildren", err)
	}
	if wantChild != "" {
		found := false
		for _, id := range soc.ChildIDs {
			if id == wantChild {
				found = true
			}
		}
		if !found {
			t.Errorf("childIDs = %v, want it to contain %q", soc.ChildIDs, wantChild)
		}
	}
}

func standingCloseArrange(t *testing.T) (*Store, *pgxpool.Pool, uuid.UUID, string, uuid.UUID, string) {
	t.Helper()
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	return NewStore(pool), pool, pid, prefix, creator, name
}

// TestClose_StandingWithOpenChild_Blocked — a standing parent with an OPEN
// child (via parent-child dep edge) cannot be retired.
func TestClose_StandingWithOpenChild_Blocked(t *testing.T) {
	store, pool, pid, prefix, creator, name := standingCloseArrange(t)

	parent := stcreate(t, store, pid, prefix, creator, name, "standing parent (open child)")
	child := stcreate(t, store, pid, prefix, creator, name, "open child")
	seedDep(t, pool, child.ID, parent.ID, "parent-child", creator)
	setIssueStatus(t, pool, parent.ID, StatusStanding)
	_, ver := readStatusVer(t, pool, parent.ID)

	_, err := store.Close(t.Context(), CloseInput{
		ID:              parent.ID,
		ExpectedVersion: ver,
		Reason:          "retire",
		Actor:           HumanActor(creator, name),
	})
	assertStandingHasOpenChildren(t, err, child.ID)

	// The standing parent must remain standing (no partial close).
	if status, _ := readStatusVer(t, pool, parent.ID); status != StatusStanding {
		t.Errorf("parent status = %q after rejected retire, want standing", status)
	}
}

// TestClose_StandingWithDeferredChild_Blocked — a DEFERRED child is real,
// unresolved work and still blocks the retire (§11.4).
func TestClose_StandingWithDeferredChild_Blocked(t *testing.T) {
	store, pool, pid, prefix, creator, name := standingCloseArrange(t)

	parent := stcreate(t, store, pid, prefix, creator, name, "standing parent (deferred child)")
	child := stcreate(t, store, pid, prefix, creator, name, "deferred child")
	seedDep(t, pool, child.ID, parent.ID, "parent-child", creator)
	setIssueStatus(t, pool, child.ID, StatusDeferred)
	setIssueStatus(t, pool, parent.ID, StatusStanding)
	_, ver := readStatusVer(t, pool, parent.ID)

	_, err := store.Close(t.Context(), CloseInput{
		ID:              parent.ID,
		ExpectedVersion: ver,
		Reason:          "retire",
		Actor:           HumanActor(creator, name),
	})
	assertStandingHasOpenChildren(t, err, child.ID)
}

// TestClose_StandingAllChildrenClosed_Succeeds — once every child is closed,
// the retire goes through.
func TestClose_StandingAllChildrenClosed_Succeeds(t *testing.T) {
	store, pool, pid, prefix, creator, name := standingCloseArrange(t)

	parent := stcreate(t, store, pid, prefix, creator, name, "standing parent (all closed)")
	c1 := stcreate(t, store, pid, prefix, creator, name, "closed child 1")
	c2 := stcreate(t, store, pid, prefix, creator, name, "closed child 2")
	seedDep(t, pool, c1.ID, parent.ID, "parent-child", creator)
	linkParentIDColumn(t, pool, c2.ID, parent.ID)
	setIssueStatus(t, pool, c1.ID, StatusClosed)
	setIssueStatus(t, pool, c2.ID, StatusClosed)
	setIssueStatus(t, pool, parent.ID, StatusStanding)
	_, ver := readStatusVer(t, pool, parent.ID)

	closed, err := store.Close(t.Context(), CloseInput{
		ID:              parent.ID,
		ExpectedVersion: ver,
		Reason:          "retire — all children done",
		Actor:           HumanActor(creator, name),
	})
	if err != nil {
		t.Fatalf("retire with all children closed should succeed, got: %v", err)
	}
	if closed.Status != StatusClosed {
		t.Fatalf("status = %q, want closed", closed.Status)
	}
}

// TestClose_StandingNoChildren_Succeeds — a childless standing task retires
// cleanly (empty retire ok).
func TestClose_StandingNoChildren_Succeeds(t *testing.T) {
	store, pool, pid, prefix, creator, name := standingCloseArrange(t)

	iss := stcreate(t, store, pid, prefix, creator, name, "childless standing")
	setIssueStatus(t, pool, iss.ID, StatusStanding)
	_, ver := readStatusVer(t, pool, iss.ID)

	closed, err := store.Close(t.Context(), CloseInput{
		ID:              iss.ID,
		ExpectedVersion: ver,
		Reason:          "retire — no children",
		Actor:           HumanActor(creator, name),
	})
	if err != nil {
		t.Fatalf("empty retire should succeed, got: %v", err)
	}
	if closed.Status != StatusClosed {
		t.Fatalf("status = %q, want closed", closed.Status)
	}
}

// TestClose_NonStandingParentNotBlocked is the §11.9 regression: a NORMAL epic
// (status open) with an OPEN child STILL closes — the retire-block is
// standing-specific and must not become a generic open-children block.
func TestClose_NonStandingParentNotBlocked(t *testing.T) {
	store, pool, pid, prefix, creator, name := standingCloseArrange(t)

	epic := stcreate(t, store, pid, prefix, creator, name, "normal epic (open child)")
	child := stcreate(t, store, pid, prefix, creator, name, "open child of normal epic")
	seedDep(t, pool, child.ID, epic.ID, "parent-child", creator)
	// epic stays at its create-time status (open) — NOT standing.

	closed, err := store.Close(t.Context(), CloseInput{
		ID:              epic.ID,
		ExpectedVersion: epic.Version,
		Reason:          "close normal epic despite open child",
		Actor:           HumanActor(creator, name),
	})
	if err != nil {
		t.Fatalf("normal epic with open child must still close (§11.9), got: %v", err)
	}
	if closed.Status != StatusClosed {
		t.Fatalf("status = %q, want closed", closed.Status)
	}
}

// TestClose_StandingChild_ViaParentId_AndViaEdge_BothBlock — the retire-block
// must fire for a non-closed child reachable via EITHER representation. Two
// sub-scenarios: a child linked only via issues.parent_id, and a child linked
// only via the parent-child dep edge.
func TestClose_StandingChild_ViaParentId_AndViaEdge_BothBlock(t *testing.T) {
	t.Run("parent_id column", func(t *testing.T) {
		store, pool, pid, prefix, creator, name := standingCloseArrange(t)

		parent := stcreate(t, store, pid, prefix, creator, name, "standing parent (col child)")
		child := stcreate(t, store, pid, prefix, creator, name, "open child (col only)")
		linkParentIDColumn(t, pool, child.ID, parent.ID)
		setIssueStatus(t, pool, parent.ID, StatusStanding)
		_, ver := readStatusVer(t, pool, parent.ID)

		_, err := store.Close(t.Context(), CloseInput{
			ID:              parent.ID,
			ExpectedVersion: ver,
			Reason:          "retire",
			Actor:           HumanActor(creator, name),
		})
		assertStandingHasOpenChildren(t, err, child.ID)
	})

	t.Run("parent-child edge", func(t *testing.T) {
		store, pool, pid, prefix, creator, name := standingCloseArrange(t)

		parent := stcreate(t, store, pid, prefix, creator, name, "standing parent (edge child)")
		child := stcreate(t, store, pid, prefix, creator, name, "open child (edge only)")
		seedDep(t, pool, child.ID, parent.ID, "parent-child", creator)
		setIssueStatus(t, pool, parent.ID, StatusStanding)
		_, ver := readStatusVer(t, pool, parent.ID)

		_, err := store.Close(t.Context(), CloseInput{
			ID:              parent.ID,
			ExpectedVersion: ver,
			Reason:          "retire",
			Actor:           HumanActor(creator, name),
		})
		assertStandingHasOpenChildren(t, err, child.ID)
	})
}
