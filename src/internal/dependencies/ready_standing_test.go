package dependencies

import (
	"context"
	"testing"
)

// sk-118.11 (F3) — invariant §6.3 (Ready/inbox) leg A: a STANDING issue is not
// itself listed by skein_ready. `ListReady` gates on `i.status = 'open'`
// (store.go ~945), so a standing issue — which is status='standing', never
// 'open' — can never surface as ready work. A standing task is a perpetual
// obligation acted on by spawning parent.N children, NOT a unit of work to be
// claimed off the ready queue.
//
// This is distinct from TestListReady_NotBlockedByStandingTarget, which covers
// the BLOCKER angle (a standing TARGET of a blocks-edge must not gate the
// blocked issue). Here the standing issue stands alone with no edges — the
// point is purely "status='standing' ⇒ never ready", locking in the per-status
// equality that R12 (the static guard) deliberately leaves untouched.
func TestListReady_StandingIssueNotListed(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)

	openID := prefix + "-1"     // a plain open issue — must be ready
	standingID := prefix + "-2" // promoted to standing — must NOT be ready
	seedOpenIssue(t, pool, pid, uid, openID, "ordinary open work")
	seedOpenIssue(t, pool, pid, uid, standingID, "perpetual obligation")
	setStanding(t, pool, standingID) // no dependencies — stands alone

	store := NewStore(pool)
	res, err := store.ListReady(context.Background(), pid, ReadyFilters{}, 25)
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}

	got := map[string]bool{}
	for _, it := range res {
		got[it.ID] = true
	}
	if !got[openID] {
		t.Errorf("open issue %s missing from ListReady (got %v)", openID, idsOf(res))
	}
	if got[standingID] {
		t.Errorf("standing issue %s leaked into ListReady — status='open' filter must exclude it (got %v)", standingID, idsOf(res))
	}
}
