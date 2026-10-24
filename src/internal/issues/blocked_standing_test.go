package issues

import (
	"testing"
)

// sk-118 (F2f) — a STANDING blocker must NOT derive-block the FROM issue.
//
// status.GatesReadiness was widened to exclude `standing` (alongside `closed`),
// and isBlockedExprFor routes through it. So a `blocks` edge whose target is
// standing behaves exactly like a closed-target edge: it does not gate.
//
// The edge is SEEDED directly (and the target promoted to standing) because the
// F2f source-ban in dependencies.CreateInTx forbids creating such an edge fresh
// — this models a blocker that was promoted to standing AFTER the edge existed.
//
// GetIssue here reads as the project's owner: since sk-423 only blockers the
// reader can see count, and an undeclared context sees none (see
// blocked_derived_test.go).

// TestIsBlocked_StandingTargetNotBlocking: A blocks B, then B is promoted to
// standing → A is NOT derived-blocked, and B does not appear in A's active
// blockers list.
func TestIsBlocked_StandingTargetNotBlocking(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "blocked task")
	seedIssue(t, pool, pid, owner, prefix+"-2", "standing blocker")
	// from = blocked side, to = blocker.
	seedDep(t, pool, prefix+"-1", prefix+"-2", "blocks", owner)
	// Promote the blocker to standing (closed_at stays NULL — standing is not
	// terminal). setIssueStatus only sets closed_at for status="closed".
	setIssueStatus(t, pool, prefix+"-2", "standing")

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, owner, "user"), prefix+"-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if dto.IsBlocked {
		t.Fatalf("IsBlocked = true; want false (blocker is standing → non-gating)")
	}
	if len(dto.Blockers) != 0 {
		t.Fatalf("Blockers = %+v; want [] (standing blocker excluded)", dto.Blockers)
	}
}

// TestIsBlocked_StandingAmongBlockers_OnlyOpenGates: A has two blockers — one
// open, one standing. A stays blocked (by the open one), and the standing one is
// NOT listed as an active blocker.
func TestIsBlocked_StandingAmongBlockers_OnlyOpenGates(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "blocked task")
	seedIssue(t, pool, pid, owner, prefix+"-2", "open blocker")
	seedIssue(t, pool, pid, owner, prefix+"-3", "standing blocker")
	seedDep(t, pool, prefix+"-1", prefix+"-2", "blocks", owner)
	seedDep(t, pool, prefix+"-1", prefix+"-3", "blocks", owner)
	setIssueStatus(t, pool, prefix+"-3", "standing")

	store := NewStore(pool)
	dto, err := store.GetIssue(callerCtx(t, owner, "user"), prefix+"-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if !dto.IsBlocked {
		t.Fatalf("IsBlocked = false; want true (the OPEN blocker still gates)")
	}
	if len(dto.Blockers) != 1 || dto.Blockers[0].ID != prefix+"-2" {
		t.Fatalf("Blockers = %+v; want [%s] (standing blocker excluded)", dto.Blockers, prefix+"-2")
	}
}
