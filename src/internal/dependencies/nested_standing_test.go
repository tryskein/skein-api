package dependencies

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-118 (F2e) — nested-standing ban on the edge-create + reparent paths. A
// standing task must never be the child of a standing task (it reintroduces the
// §11.4 mutual-retire deadlock). CreateInTx (the parent-child edge insert, hit
// by skein_dep_add / dep_add_many / create-inline) and Reparent both reject the
// move when BOTH the would-be child and the would-be parent are standing, with
// the stable `nested_standing_forbidden` ValidationError code.

// setStanding flips an issue's status to 'standing' directly (bypassing the
// issues-store lifecycle, which this package cannot import).
func setStanding(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET status = 'standing' WHERE id = $1`, id); err != nil {
		t.Fatalf("set %s standing: %v", id, err)
	}
}

func assertNestedStandingForbidden(t *testing.T, err error) {
	t.Helper()
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "nested_standing_forbidden" {
		t.Fatalf("err = %v, want validation code nested_standing_forbidden", err)
	}
}

// TestDepAdd_StandingChildOfStanding_Rejected — creating a parent-child edge
// (from = child, to = parent) where both ends are standing is rejected.
func TestDepAdd_StandingChildOfStanding_Rejected(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	parent := prefix + "-2"
	seedOpenIssue(t, pool, pid, uid, child, "standing child")
	seedOpenIssue(t, pool, pid, uid, parent, "standing parent")
	setStanding(t, pool, child)
	setStanding(t, pool, parent)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Create(context.Background(), child,
		CreateDepInput{ToID: parent, Type: DepParentChild},
		caller, HumanActor(uid, "tester"))
	assertNestedStandingForbidden(t, err)

	// No edge must have been written.
	var n int
	if qerr := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'parent-child'`,
		child, parent).Scan(&n); qerr != nil {
		t.Fatalf("count edges: %v", qerr)
	}
	if n != 0 {
		t.Errorf("edge count = %d after rejected dep_add, want 0", n)
	}
}

// TestDepAdd_StandingChildOfNonStandingParent_Allowed — control: a standing
// child under a NON-standing parent is fine (only standing-under-standing is
// banned). Mirrors the create-edge guard's exact predicate.
func TestDepAdd_StandingChildOfNonStandingParent_Allowed(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	parent := prefix + "-2"
	seedOpenIssue(t, pool, pid, uid, child, "standing child")
	seedOpenIssue(t, pool, pid, uid, parent, "open parent")
	setStanding(t, pool, child)
	// parent stays open.

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Create(context.Background(), child,
		CreateDepInput{ToID: parent, Type: DepParentChild},
		caller, HumanActor(uid, "tester"))
	if err != nil {
		t.Fatalf("standing child under open parent should be allowed, got: %v", err)
	}
}

// TestReparent_StandingUnderStanding_Rejected — moving a standing child under a
// standing new parent via Reparent is rejected, and no edge churn happens.
func TestReparent_StandingUnderStanding_Rejected(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	p1 := prefix + "-2"
	p2 := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, child, "standing child")
	seedOpenIssue(t, pool, pid, uid, p1, "old parent (open)")
	seedOpenIssue(t, pool, pid, uid, p2, "new parent (standing)")
	seedParentChildDep(t, pool, child, p1, uid)
	setStanding(t, pool, child)
	setStanding(t, pool, p2)
	v0 := issueVersion(t, pool, child)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Reparent(context.Background(), child, p2, v0,
		caller, HumanActor(uid, "tester"))
	assertNestedStandingForbidden(t, err)

	// The original parent edge must be intact (no DELETE+INSERT churn) and the
	// version must NOT have been bumped.
	if got := parentIdCurrent(t, pool, child); got != p1 {
		t.Errorf("parentIdCurrent = %q after rejected reparent, want %q (unchanged)", got, p1)
	}
	if v1 := issueVersion(t, pool, child); v1 != v0 {
		t.Errorf("version = %d after rejected reparent, want %d (no bump)", v1, v0)
	}
}

// TestReparent_StandingChildUnderOpenParent_Allowed — control: a standing child
// can be moved under a NON-standing parent.
func TestReparent_StandingChildUnderOpenParent_Allowed(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	p1 := prefix + "-2"
	p2 := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, child, "standing child")
	seedOpenIssue(t, pool, pid, uid, p1, "old parent (open)")
	seedOpenIssue(t, pool, pid, uid, p2, "new parent (open)")
	seedParentChildDep(t, pool, child, p1, uid)
	setStanding(t, pool, child)
	// p2 stays open.
	v0 := issueVersion(t, pool, child)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	if _, err := store.Reparent(context.Background(), child, p2, v0,
		caller, HumanActor(uid, "tester")); err != nil {
		t.Fatalf("standing child under open parent should be allowed, got: %v", err)
	}
	if got := parentIdCurrent(t, pool, child); got != p2 {
		t.Errorf("parentIdCurrent = %q, want %q after reparent", got, p2)
	}
}
