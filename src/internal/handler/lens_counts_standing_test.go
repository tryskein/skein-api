package handler

import (
	"testing"

	"skein-api/internal/testsupport"
)

// sk-118.8 (F2g) — the cross-project "On me" / "Delegated" lens badges are
// EXCLUSION-ONLY surfaces for standing: a standing issue must drop out of both
// badges (they route through status.ActiveWork, now widened to exclude
// standing). No standing count is shown here — dashboard / cross-project lenses
// are exclusion-only per decision #4.

// TestOnMeAndDelegated_ExcludeStanding promotes an On-me issue and a Delegated
// issue to standing and asserts both badges drop to 0.
func TestOnMeAndDelegated_ExcludeStanding(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsstdlen")
	h := NewMeHandler(pool, nil, nil)

	// On-me issue: other → caller, accepted, open.
	onMeID := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, other, onMeID, "on-me", "open", 2)
	testsupport.SetAssignee(t, pool, onMeID, callerName)
	testsupport.Accept(t, pool, onMeID, caller, true)

	// Delegated issue: caller → other, open.
	delegID := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, caller, delegID, "delegated", "open", 2)
	testsupport.SetAssignee(t, pool, delegID, otherName)

	onMe, deleg := lensCounts(t, h, caller)
	if onMe != 1 || deleg != 1 {
		t.Fatalf("snapshot onMe/delegated = %d/%d, want 1/1", onMe, deleg)
	}

	// Promote the On-me issue to standing → drops off the On-me badge.
	testsupport.SetStatus(t, pool, onMeID, "standing")
	onMe, deleg = lensCounts(t, h, caller)
	if onMe != 0 {
		t.Fatalf("On me after standing = %d, want 0 (standing excluded from active lens)", onMe)
	}
	if deleg != 1 {
		t.Fatalf("Delegated after On-me→standing = %d, want 1 (delegated issue untouched)", deleg)
	}

	// Promote the Delegated issue to standing → drops off the Delegated badge.
	testsupport.SetStatus(t, pool, delegID, "standing")
	onMe, deleg = lensCounts(t, h, caller)
	if deleg != 0 {
		t.Fatalf("Delegated after standing = %d, want 0 (standing excluded from active lens)", deleg)
	}
	if onMe != 0 {
		t.Fatalf("On me end-state = %d, want 0", onMe)
	}
}
