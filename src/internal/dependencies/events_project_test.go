package dependencies

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-421.2.8, test-plan row 23 (US-5 "the link was set in another tab",
// SC-008 server half, FOR-PLAN round 3). The link event is published under
// the project of the FROM side: a reader of the child's project gets dependency.created
// and dependency.deleted even if the blocker lives in a project he does
// not see. The feature code does not change this behaviour - the test pins it
// (Create -> WriteDependencyEvent(..., from.ProjectID, ...), Delete - the project
// of the issues row by d.from_id).
//
// FROM and TO are in DIFFERENT projects: if published under the TO project, reader P,
// not a member of Q, would not get the events. The absence of the event under Q is not
// asserted - the spec does not require it.

// eventProjectIDs returns project_id of every event of the type for the entity.
func eventProjectIDs(t *testing.T, pool *pgxpool.Pool, entityID, eventType string) []*uuid.UUID {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT project_id FROM events WHERE entity_id = $1 AND type = $2 ORDER BY id`, entityID, eventType)
	if err != nil {
		t.Fatalf("query events %s/%s: %v", entityID, eventType, err)
	}
	defer rows.Close()
	var out []*uuid.UUID
	for rows.Next() {
		var pid *uuid.UUID
		if err := rows.Scan(&pid); err != nil {
			t.Fatalf("scan event project: %v", err)
		}
		out = append(out, pid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return out
}

func assertEventUnderProject(t *testing.T, pool *pgxpool.Pool, entityID, eventType string, want uuid.UUID) {
	t.Helper()
	got := eventProjectIDs(t, pool, entityID, eventType)
	if len(got) != 1 {
		t.Fatalf("events %s for %s: %d, want 1", eventType, entityID, len(got))
	}
	if got[0] == nil || *got[0] != want {
		t.Errorf("%s for %s is recorded under project_id=%v; want the FROM side project %s", eventType, entityID, got[0], want)
	}
}

func TestDependencyEvents_PublishedUnderFromProject(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	pOwner := seedUser(t, pool)
	qOwner := seedUser(t, pool)
	pID, pPrefix := seedProject(t, pool, pOwner)
	qID, qPrefix := seedProject(t, pool, qOwner)
	if pID == qID {
		t.Fatalf("precondition: FROM and TO must be in different projects")
	}

	// The author of the link sees both sides: Create refuses if the TO project is invisible.
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, qID, pOwner, qOwner); err != nil {
		t.Fatalf("add %s to Q: %v", pOwner, err)
	}

	child := pPrefix + "-1"   // child C in P - the FROM side
	blocker := qPrefix + "-1" // blocker B in Q - the TO side
	seedOpenIssue(t, pool, pID, pOwner, child, "child C")
	seedOpenIssue(t, pool, qID, qOwner, blocker, "blocker B")

	store := NewStore(pool)
	actor := HumanActor(pOwner, "tester")
	entity := dependencyEntityID(child, blocker, DepBlocks)

	if _, err := store.Create(ctx, child, CreateDepInput{ToID: blocker, Type: DepBlocks},
		CallerAccess{UserID: pOwner, Role: "user"}, actor); err != nil {
		t.Fatalf("Create %s -blocks-> %s: %v", child, blocker, err)
	}
	assertEventUnderProject(t, pool, entity, EventDependencyCreated, pID)

	if err := store.Delete(ctx, child, blocker, DepBlocks, actor); err != nil {
		t.Fatalf("Delete %s -blocks-> %s: %v", child, blocker, err)
	}
	assertEventUnderProject(t, pool, entity, EventDependencyDeleted, pID)
}

// eventEndProjects reads fromProjectId / toProjectId from the data object of
// the one event of the type for the entity: new_data on created, old_data on
// deleted (the WriteDependencyEvent convention).
func eventEndProjects(t *testing.T, pool *pgxpool.Pool, entityID, eventType string) (from, to *string) {
	t.Helper()
	col := "new_data"
	if eventType == EventDependencyDeleted {
		col = "old_data"
	}
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*), max(`+col+`->>'fromProjectId'), max(`+col+`->>'toProjectId')
  FROM events WHERE entity_id = $1 AND type = $2`, entityID, eventType).Scan(&n, &from, &to); err != nil {
		t.Fatalf("read %s of %s/%s: %v", col, entityID, eventType, err)
	}
	if n != 1 {
		t.Fatalf("events %s for %s: %d, want 1", eventType, entityID, n)
	}
	return from, to
}

// TestDependencyEvents_CarryProjectsOfBothEnds — sk-622.21, test-plan row 5
// (US-12, INV-17, precondition R8, decision K15). The data of the
// dependency.created and dependency.deleted events of a cross-project link carries
// fromProjectId and toProjectId - the REAL projects of the ends. The reference is the projects
// into which the test itself put the issues.
//
// The TO issue lies in Q, but its id carries the prefix of P (this happens after a project
// alias change): deriving the project from the id prefix would give P and go red here.
func TestDependencyEvents_CarryProjectsOfBothEnds(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	pOwner := seedUser(t, pool)
	qOwner := seedUser(t, pool)
	pID, pPrefix := seedProject(t, pool, pOwner)
	qID, _ := seedProject(t, pool, qOwner)
	if pID == qID {
		t.Fatalf("precondition: the ends must be in different projects")
	}
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, qID, pOwner, qOwner); err != nil {
		t.Fatalf("add %s to Q: %v", pOwner, err)
	}

	from := pPrefix + "-1"
	to := pPrefix + "-900" // lives in Q, the prefix is from P
	seedOpenIssue(t, pool, pID, pOwner, from, "from in P")
	seedOpenIssue(t, pool, qID, qOwner, to, "to in Q with P's prefix")

	store := NewStore(pool)
	actor := HumanActor(pOwner, "tester")
	entity := dependencyEntityID(from, to, DepBlocks)

	if _, err := store.Create(ctx, from, CreateDepInput{ToID: to, Type: DepBlocks},
		CallerAccess{UserID: pOwner, Role: "user"}, actor); err != nil {
		t.Fatalf("Create %s -blocks-> %s: %v", from, to, err)
	}
	if err := store.Delete(ctx, from, to, DepBlocks, actor); err != nil {
		t.Fatalf("Delete %s -blocks-> %s: %v", from, to, err)
	}

	for _, eventType := range []string{EventDependencyCreated, EventDependencyDeleted} {
		gotFrom, gotTo := eventEndProjects(t, pool, entity, eventType)
		if gotFrom == nil || *gotFrom != pID.String() {
			t.Errorf("%s: fromProjectId = %v, want %s (the FROM project)", eventType, gotFrom, pID)
		}
		if gotTo == nil || *gotTo != qID.String() {
			t.Errorf("%s: toProjectId = %v, want %s (the TO project, not the id prefix)", eventType, gotTo, qID)
		}
	}
}
