package issues

import (
	"context"
	"testing"

	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

// sk-118.8 (F2g) — the cross-project default list (ModeCreatedBy / ModeAssignedTo
// with no explicit Statuses filter) is an EXCLUSION-ONLY surface for standing:
// the default clause routes through status.ActiveWork, now widened to exclude
// standing. A standing issue must not appear in the default "On me / Delegated"
// cross-project tab unless the caller explicitly asks for status='standing'.

// TestCrossProjectDefault_ExcludesStanding promotes a caller-created issue to
// standing and asserts it drops out of the ModeCreatedBy default list, then
// reappears when standing is explicitly requested via the Statuses filter
// (proving the exclusion is the default-clause widening, not a hard ban).
func TestCrossProjectDefault_ExcludesStanding(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tscpstd")
	store := NewStore(pool)

	// Caller-created, delegated to other (so the Delegated chain predicate
	// matches), open.
	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, caller, id, "delegated standing", "open", 2)
	testsupport.SetAssignee(t, pool, id, otherName)

	listIDs := func(statuses []string) map[string]struct{} {
		t.Helper()
		res, err := store.ListByUser(context.Background(), ListByUserArgs{
			Scope:          authz.ForUser(caller),
			UserID:         caller,
			Mode:           ModeCreatedBy,
			CallerUsername: callerName,
			Statuses:       statuses,
		})
		if err != nil {
			t.Fatalf("ListByUser: %v", err)
		}
		out := make(map[string]struct{}, len(res.Items))
		for _, it := range res.Items {
			out[it.ID] = struct{}{}
		}
		return out
	}

	// Snapshot: open issue is in the default list.
	if _, ok := listIDs(nil)[id]; !ok {
		t.Fatalf("open issue %s missing from default ModeCreatedBy list", id)
	}

	// Promote to standing → drops out of the default list.
	testsupport.SetStatus(t, pool, id, "standing")
	if _, ok := listIDs(nil)[id]; ok {
		t.Fatalf("standing issue %s present in default ModeCreatedBy list — must be excluded", id)
	}

	// Explicit status='standing' filter → reappears (exclusion is default-only).
	if _, ok := listIDs([]string{"standing"})[id]; !ok {
		t.Fatalf("standing issue %s missing when explicitly filtered by status='standing'", id)
	}
}
