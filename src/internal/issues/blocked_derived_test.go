package issues

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The reads in this file are about the DERIVATION rule — which edge types
// and which target statuses make an issue blocked — so they read as the
// project's owner, a member of the one project every fixture here lives in.
// sk-423 made «blocked» a property of the reader (only blockers the reader
// can see count); an undeclared context sees no project and would read
// nothing as blocked. Per-reader behaviour is covered where the readers are
// seeded: blockers_visibility_test.go, blocked_horizon_test.go.

// seedDep inserts one row into the dependencies table. The blocked_derived
// tests don't need version, metadata, or cycle-detection — they just need
// a typed edge so the EXISTS predicate has something to find. We bypass
// the dependencies.Store on purpose: tests own their fixtures and we want
// the data shape, not the business rules.
func seedDep(t *testing.T, pool *pgxpool.Pool, fromID, toID, depType string, creator uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, '{}'::jsonb, 1, $4, now())`,
		fromID, toID, depType, creator)
	if err != nil {
		t.Fatalf("seed dep %s -%s-> %s: %v", fromID, depType, toID, err)
	}
}

// setIssueStatus is a low-level helper to flip status without going
// through UpdateStatus (which would emit events / enforce locks). Tests
// use it to set up the closed-blocker / open-blocker scenarios.
func setIssueStatus(t *testing.T, pool *pgxpool.Pool, issueID, status string) {
	t.Helper()
	closedAt := "NULL"
	if status == "closed" {
		closedAt = "now()"
	}
	_, err := pool.Exec(context.Background(), `
UPDATE issues SET status = $1, closed_at = `+closedAt+` WHERE id = $2`, status, issueID)
	if err != nil {
		t.Fatalf("set issue %s status=%s: %v", issueID, status, err)
	}
}

// TestIsBlocked_DerivedFromBlocksEdge: A blocks B (A has open blocker B
// pointed to via a 'blocks' typed edge — wait, semantics: in
// dependencies/types.go the 'blocks' edge has from=blocked, to=blocker.
// So issuing a blocks edge from A→B says "A is blocked by B").
func TestIsBlocked_DerivedFromBlocksEdge(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "blocked task")
	seedIssue(t, pool, pid, owner, prefix+"-2", "the blocker")
	// from = the blocked side, to = blocker
	seedDep(t, pool, prefix+"-1", prefix+"-2", "blocks", owner)

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, owner, "user"), prefix+"-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if !dto.IsBlocked {
		t.Fatalf("IsBlocked = false; want true (blocks edge to open issue)")
	}
	if len(dto.Blockers) != 1 || dto.Blockers[0].ID != prefix+"-2" {
		t.Fatalf("Blockers = %+v; want [%s]", dto.Blockers, prefix+"-2")
	}
}

// TestIsBlocked_FalseWhenBlockerClosed: A blocks B, B is closed → A is
// not blocked.
func TestIsBlocked_FalseWhenBlockerClosed(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "blocked task")
	seedIssue(t, pool, pid, owner, prefix+"-2", "closed blocker")
	seedDep(t, pool, prefix+"-1", prefix+"-2", "blocks", owner)
	setIssueStatus(t, pool, prefix+"-2", "closed")

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, owner, "user"), prefix+"-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.IsBlocked {
		t.Fatalf("IsBlocked = true; want false (blocker is closed)")
	}
	if len(dto.Blockers) != 0 {
		t.Fatalf("Blockers = %+v; want []", dto.Blockers)
	}
}

// TestIsBlocked_FromWaitsForEdge: waits-for is also a derived-blocked
// source. Same direction as blocks (from = waiter, to = awaited).
func TestIsBlocked_FromWaitsForEdge(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "waiter")
	seedIssue(t, pool, pid, owner, prefix+"-2", "awaited")
	// waits-for needs a gate in metadata; the migration doesn't enforce
	// that on raw inserts, but the dependencies.Store does. We bypass.
	_, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'waits-for', '{"gate":"all-children"}'::jsonb, 1, $3, now())`,
		prefix+"-1", prefix+"-2", owner)
	if err != nil {
		t.Fatalf("seed waits-for: %v", err)
	}

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, owner, "user"), prefix+"-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if !dto.IsBlocked {
		t.Fatalf("IsBlocked = false; want true (waits-for with open target)")
	}
}

// TestIsBlocked_MultipleBlockers_ClosedExcluded: 3 blockers; one closed;
// Blockers list should contain only the two open ones.
func TestIsBlocked_MultipleBlockers_ClosedExcluded(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "blocked")
	seedIssue(t, pool, pid, owner, prefix+"-2", "blocker A")
	seedIssue(t, pool, pid, owner, prefix+"-3", "blocker B (closed)")
	seedIssue(t, pool, pid, owner, prefix+"-4", "blocker C")
	seedDep(t, pool, prefix+"-1", prefix+"-2", "blocks", owner)
	seedDep(t, pool, prefix+"-1", prefix+"-3", "blocks", owner)
	seedDep(t, pool, prefix+"-1", prefix+"-4", "blocks", owner)
	setIssueStatus(t, pool, prefix+"-3", "closed")

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, owner, "user"), prefix+"-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if !dto.IsBlocked {
		t.Fatalf("IsBlocked = false; want true")
	}
	if len(dto.Blockers) != 2 {
		t.Fatalf("Blockers count = %d; want 2 (got %+v)", len(dto.Blockers), dto.Blockers)
	}
	gotIDs := map[string]bool{}
	for _, b := range dto.Blockers {
		gotIDs[b.ID] = true
	}
	if !gotIDs[prefix+"-2"] || !gotIDs[prefix+"-4"] {
		t.Fatalf("Blockers = %+v; want [%s, %s]", dto.Blockers, prefix+"-2", prefix+"-4")
	}
}

// TestIsBlocked_FalseWhenNoEdges: sanity — an issue with no outgoing
// edges has IsBlocked=false and Blockers=[].
func TestIsBlocked_FalseWhenNoEdges(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "lonely")

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, owner, "user"), prefix+"-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.IsBlocked {
		t.Fatalf("IsBlocked = true; want false (no edges)")
	}
	if len(dto.Blockers) != 0 {
		t.Fatalf("Blockers = %+v; want []", dto.Blockers)
	}
}

// TestIsBlocked_ListIssues_WireFilter: ListIssues with IsBlocked=&true
// returns only blocked issues; with IsBlocked=&false returns only
// non-blocked issues.
func TestIsBlocked_ListIssues_WireFilter(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "blocked one")
	seedIssue(t, pool, pid, owner, prefix+"-2", "not blocked")
	seedIssue(t, pool, pid, owner, prefix+"-3", "not blocked either")
	seedIssue(t, pool, pid, owner, prefix+"-4", "open blocker")
	seedDep(t, pool, prefix+"-1", prefix+"-4", "blocks", owner)

	store := NewStore(pool)

	t.Run("IsBlocked=true", func(t *testing.T) {
		yes := true
		res, err := store.ListIssues(callerCtx(t, owner, "user"), ListIssuesArgs{
			ProjectID: pid,
			IsBlocked: &yes,
			Limit:     50,
		})
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		if len(res.Items) != 1 || res.Items[0].ID != prefix+"-1" {
			t.Fatalf("isBlocked=true items = %v; want [%s]", idsOf(res.Items), prefix+"-1")
		}
		if !res.Items[0].IsBlocked {
			t.Fatalf("isBlocked col = false on matched row; want true")
		}
	})

	t.Run("IsBlocked=false", func(t *testing.T) {
		no := false
		res, err := store.ListIssues(callerCtx(t, owner, "user"), ListIssuesArgs{
			ProjectID: pid,
			IsBlocked: &no,
			Limit:     50,
		})
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		if len(res.Items) != 3 {
			t.Fatalf("isBlocked=false items count = %d (%v); want 3", len(res.Items), idsOf(res.Items))
		}
		for _, it := range res.Items {
			if it.IsBlocked {
				t.Fatalf("row %s reports isBlocked=true under isBlocked=false filter", it.ID)
			}
		}
	})
}

// TestUpdateStatus_RejectsBlocked: status='blocked' is no longer a
// writable transition. UpdateStatus must surface the blocked_is_derived
// validation error.
func TestUpdateStatus_RejectsBlocked(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	seedIssue(t, pool, pid, owner, prefix+"-1", "task")

	store := NewStore(pool)
	_, err := store.UpdateStatus(context.Background(), prefix+"-1",
		UpdateStatusInput{
			ExpectedVersion: 1,
			Status:          "blocked",
		},
		HumanActor(owner, ownerName),
	)
	if err == nil {
		t.Fatalf("UpdateStatus(status=blocked) succeeded; want validation error")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("UpdateStatus error = %T %v; want *ValidationError", err, err)
	}
	if ve.Code != "blocked_is_derived" {
		t.Fatalf("validation code = %q; want \"blocked_is_derived\"", ve.Code)
	}
}
