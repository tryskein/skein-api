package issues

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-118 (F2i) — create-as-standing store branch + nested-standing ban on the
// CREATE path (R7). CreateIssue with input.Standing=true must:
//   - insert status='standing' (not 'open');
//   - leave started_at NULL (a standing task is never "in progress");
//   - leave accepted_at / accepted_by NULL even on creator self-assign (a
//     standing task is never auto-accepted / worked in place);
//   - leave locked_by / locked_at NULL (never claimed);
//   - record status=standing in the issue.created event snapshot.
// And: a standing child under a standing parentId is rejected with
// nested_standing_forbidden BEFORE any insert; a NON-standing child under a
// standing parent is allowed.

// readStandingRow reads the create-relevant nullable stamps for an issue.
func readStandingRow(t *testing.T, pool *pgxpool.Pool, id string) (status string, startedAtNull, acceptedAtNull, acceptedByNull, lockedByNull bool) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), `
SELECT status,
       started_at  IS NULL,
       accepted_at IS NULL,
       accepted_by IS NULL,
       locked_by   IS NULL
  FROM issues WHERE id = $1`, id).
		Scan(&status, &startedAtNull, &acceptedAtNull, &acceptedByNull, &lockedByNull); err != nil {
		t.Fatalf("read standing row %s: %v", id, err)
	}
	return
}

func TestCreate_Standing_StatusStandingNoStampsNoLock(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	// Self-assign on purpose: the open path would auto-accept (accepted_at set);
	// the standing path must NOT.
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "standing obligation",
			Type:     "task",
			Priority: 2,
			Assignee: &name,
			Standing: true,
		},
		creator, HumanActor(creator, name))
	if err != nil {
		t.Fatalf("CreateIssue(standing): %v", err)
	}
	if dto.Status != StatusStanding {
		t.Fatalf("dto.Status = %q, want standing", dto.Status)
	}

	status, startedNull, acceptedAtNull, acceptedByNull, lockedNull := readStandingRow(t, pool, dto.ID)
	if status != StatusStanding {
		t.Fatalf("row status = %q, want standing", status)
	}
	if !startedNull {
		t.Fatalf("started_at must be NULL on a standing create")
	}
	if !acceptedAtNull || !acceptedByNull {
		t.Fatalf("standing create must NOT auto-accept even on self-assign: accepted_at NULL=%v accepted_by NULL=%v", acceptedAtNull, acceptedByNull)
	}
	if !lockedNull {
		t.Fatalf("locked_by must be NULL on a standing create (never claimed)")
	}

	// Audit snapshot must show status=standing, not open.
	var snapStatus string
	if err := pool.QueryRow(t.Context(), `
SELECT new_data->>'status' FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.created'
 ORDER BY id DESC LIMIT 1`, dto.ID).Scan(&snapStatus); err != nil {
		t.Fatalf("read issue.created snapshot: %v", err)
	}
	if snapStatus != StatusStanding {
		t.Fatalf("issue.created snapshot status = %q, want standing", snapStatus)
	}
}

// TestCreate_Open_ControlSelfAssignAccepts pins the byte-identical control: a
// NON-standing self-assigned create DOES stamp accepted_at (the existing
// auto-accept behaviour). This is the regression backstop that the standing
// branch did not break the default path.
func TestCreate_Open_ControlSelfAssignAccepts(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "ordinary self-assigned",
			Type:     "task",
			Priority: 2,
			Assignee: &name,
		},
		creator, HumanActor(creator, name))
	if err != nil {
		t.Fatalf("CreateIssue(open): %v", err)
	}
	if dto.Status != StatusOpen {
		t.Fatalf("dto.Status = %q, want open", dto.Status)
	}
	status, startedNull, acceptedAtNull, _, lockedNull := readStandingRow(t, pool, dto.ID)
	if status != StatusOpen {
		t.Fatalf("row status = %q, want open", status)
	}
	if acceptedAtNull {
		t.Fatalf("open self-assign control should auto-accept (accepted_at set), but it was NULL")
	}
	// Open self-assigned create still does not auto-claim / start.
	if !startedNull {
		t.Fatalf("started_at should be NULL on an open create")
	}
	if !lockedNull {
		t.Fatalf("locked_by should be NULL on an open create")
	}
}

func TestCreate_StandingUnderStandingParent_Rejected(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	parent := stcreate(t, store, pid, prefix, creator, name, "standing parent")
	setIssueStatus(t, pool, parent.ID, StatusStanding)

	_, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "standing child under standing parent",
			Type:     "task",
			Priority: 2,
			ParentID: &parent.ID,
			Standing: true,
			DependenciesCaller: &DependenciesCaller{
				UserID: creator,
				Role:   "user",
			},
		},
		creator, HumanActor(creator, name))
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "nested_standing_forbidden" {
		t.Fatalf("err = %v, want nested_standing_forbidden validation error", err)
	}

	// Issue NOT inserted: only the parent exists in the project.
	var cnt int
	if err := pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM issues WHERE project_id = $1`, pid).Scan(&cnt); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected 1 issue (parent only, child rolled back), got %d", cnt)
	}
}

func TestCreate_NonStandingUnderStandingParent_Allowed(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	parent := stcreate(t, store, pid, prefix, creator, name, "standing parent")
	setIssueStatus(t, pool, parent.ID, StatusStanding)

	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "ordinary child under standing parent",
			Type:     "task",
			Priority: 2,
			ParentID: &parent.ID,
			DependenciesCaller: &DependenciesCaller{
				UserID: creator,
				Role:   "user",
			},
		},
		creator, HumanActor(creator, name))
	if err != nil {
		t.Fatalf("non-standing child under standing parent should be allowed, got: %v", err)
	}
	if dto.Status != StatusOpen {
		t.Fatalf("child status = %q, want open", dto.Status)
	}
	// The canonical parent-child dep edge is wired to the standing parent.
	var edgeCnt int
	if err := pool.QueryRow(t.Context(), `
SELECT COUNT(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = 'parent-child'`, dto.ID, parent.ID).Scan(&edgeCnt); err != nil {
		t.Fatalf("count parent-child edge: %v", err)
	}
	if edgeCnt != 1 {
		t.Fatalf("expected 1 parent-child edge to the standing parent, got %d", edgeCnt)
	}
}

// compile guards (mirrors the package convention of asserting imports stay used
// when individual tests are commented during local debugging).
var _ = uuid.Nil
