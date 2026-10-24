package issues

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestCreateIssue_InheritsComponentFromParent_WhenOmitted asserts the
// dominant Jira-style behaviour: when a subtask is created via parentId
// AND componentIds is omitted (CreateIssueInput zero-value slice +
// ComponentIDsExplicit=false), the child inherits the parent's
// component set in the same SERIALIZABLE transaction. The returned
// IssueDetail.InheritedComponentIDs echoes the inherited uuid set so
// the caller can surface a warning.
func TestCreateIssue_InheritsComponentFromParent_WhenOmitted(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	compA := seedComponent(t, pool, pid, owner, "alpha")
	compB := seedComponent(t, pool, pid, owner, "beta")

	store := NewStore(pool)

	// Parent: created with both components pinned explicitly.
	parent, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:                "parent epic",
			Type:                 "epic",
			Priority:             2,
			ComponentIDs:         []uuid.UUID{compA, compB},
			ComponentIDsExplicit: true,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Child: parentId set, componentIds OMITTED — explicit=false, nil slice.
	child, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "child task",
			Type:     "task",
			Priority: 3,
			ParentID: &parent.ID,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	// Child must have inherited BOTH components.
	if len(child.ComponentIDs) != 2 {
		t.Fatalf("child.ComponentIDs = %v, want 2 (inherited from parent)", child.ComponentIDs)
	}
	gotSet := map[uuid.UUID]bool{}
	for _, c := range child.ComponentIDs {
		gotSet[c] = true
	}
	if !gotSet[compA] || !gotSet[compB] {
		t.Fatalf("child.ComponentIDs = %v, want both %s and %s", child.ComponentIDs, compA, compB)
	}

	// Signal field — caller surfaces the warning off this.
	if len(child.InheritedComponentIDs) != 2 {
		t.Fatalf("child.InheritedComponentIDs = %v, want 2", child.InheritedComponentIDs)
	}
}

// TestCreateIssue_DoesNotInherit_WhenExplicitEmptyArray asserts that
// passing `componentIds: []` (ComponentIDsExplicit=true, empty slice)
// is interpreted as the explicit "no component" override — even when
// the parent has components attached, the child stays uncomponented
// and InheritedComponentIDs is empty.
func TestCreateIssue_DoesNotInherit_WhenExplicitEmptyArray(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	compA := seedComponent(t, pool, pid, owner, "alpha")

	store := NewStore(pool)

	parent, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:                "parent",
			Type:                 "feature",
			Priority:             2,
			ComponentIDs:         []uuid.UUID{compA},
			ComponentIDsExplicit: true,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	child, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:                "child explicit none",
			Type:                 "task",
			Priority:             3,
			ParentID:             &parent.ID,
			ComponentIDs:         []uuid.UUID{},
			ComponentIDsExplicit: true, // <-- the bit that suppresses inheritance
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	if len(child.ComponentIDs) != 0 {
		t.Fatalf("child.ComponentIDs = %v, want empty (explicit override)", child.ComponentIDs)
	}
	if len(child.InheritedComponentIDs) != 0 {
		t.Fatalf("child.InheritedComponentIDs = %v, want empty", child.InheritedComponentIDs)
	}
}

// TestCreateIssue_DoesNotInherit_WhenComponentsProvided asserts that an
// explicit non-empty componentIds list is honoured as-is — no merge
// with the parent's set, no implicit widening. The Inherited field
// stays empty since the caller chose their own components.
func TestCreateIssue_DoesNotInherit_WhenComponentsProvided(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	compA := seedComponent(t, pool, pid, owner, "alpha")
	compB := seedComponent(t, pool, pid, owner, "beta")

	store := NewStore(pool)

	parent, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:                "parent A only",
			Type:                 "feature",
			Priority:             2,
			ComponentIDs:         []uuid.UUID{compA},
			ComponentIDsExplicit: true,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	child, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:                "child overrides to B",
			Type:                 "task",
			Priority:             3,
			ParentID:             &parent.ID,
			ComponentIDs:         []uuid.UUID{compB},
			ComponentIDsExplicit: true,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	if len(child.ComponentIDs) != 1 || child.ComponentIDs[0] != compB {
		t.Fatalf("child.ComponentIDs = %v, want only %s", child.ComponentIDs, compB)
	}
	if len(child.InheritedComponentIDs) != 0 {
		t.Fatalf("child.InheritedComponentIDs = %v, want empty (explicit override)", child.InheritedComponentIDs)
	}
}

// TestCreateIssue_DoesNotInherit_WhenParentInDifferentProject asserts
// that cross-project parents (allowed since sk-35) do NOT trigger
// component inheritance — component uuids only resolve inside the
// child's own project, so copying the parent's set would either fail
// validation or silently produce a partial-intersect / orphaned-uuid
// mess. The store skips inheritance cleanly and the child lands with
// no components.
func TestCreateIssue_DoesNotInherit_WhenParentInDifferentProject(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)

	// Parent project + component + parent issue.
	parentPID, parentPrefix := seedProject(t, pool, owner)
	parentComp := seedComponent(t, pool, parentPID, owner, "parent-side")

	// Child project (different, separate component namespace).
	childPID, _ := seedProject(t, pool, owner)
	// Add the owner as a member of the parent project too so the
	// cross-project visibility check passes in createIssueInTx (the
	// helper seedProject already added them to the project they
	// created, but the child create needs visibility on BOTH).
	_, err := pool.Exec(context.Background(),
		`WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
		 VALUES ($1, $2, 'member', now(), $2)
		 ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, parentPID, owner)
	if err != nil {
		t.Fatalf("add cross-project membership: %v", err)
	}

	store := NewStore(pool)

	parent, err := store.CreateIssue(t.Context(), parentPID, parentPrefix, "counter",
		CreateIssueInput{
			Title:                "cross-project parent",
			Type:                 "feature",
			Priority:             2,
			ComponentIDs:         []uuid.UUID{parentComp},
			ComponentIDsExplicit: true,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Child in childPID, parentId points into parentPID.
	child, err := store.CreateIssue(t.Context(), childPID, parentPrefix, "counter",
		CreateIssueInput{
			Title:    "child in other project",
			Type:     "task",
			Priority: 3,
			ParentID: &parent.ID,
			DependenciesCaller: &DependenciesCaller{
				UserID: owner,
				Role:   "user",
			},
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create cross-project child: %v", err)
	}

	if len(child.ComponentIDs) != 0 {
		t.Fatalf("child.ComponentIDs = %v, want empty (cross-project parent skip)", child.ComponentIDs)
	}
	if len(child.InheritedComponentIDs) != 0 {
		t.Fatalf("child.InheritedComponentIDs = %v, want empty", child.InheritedComponentIDs)
	}
}

// TestCreateIssue_DoesNotInherit_WhenParentHasNoComponents asserts the
// silent-no-op case: parent in same project but with zero components
// attached → child lands with empty componentIds AND no warning
// signal. InheritedComponentIDs stays empty so the caller doesn't
// surface a misleading "inherited 0 components" note.
func TestCreateIssue_DoesNotInherit_WhenParentHasNoComponents(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)

	store := NewStore(pool)

	parent, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "parent no components",
			Type:     "feature",
			Priority: 2,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	child, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "child",
			Type:     "task",
			Priority: 3,
			ParentID: &parent.ID,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	if len(child.ComponentIDs) != 0 {
		t.Fatalf("child.ComponentIDs = %v, want empty", child.ComponentIDs)
	}
	if len(child.InheritedComponentIDs) != 0 {
		t.Fatalf("child.InheritedComponentIDs = %v, want empty", child.InheritedComponentIDs)
	}
}
