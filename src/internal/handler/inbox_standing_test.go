package handler

import (
	"testing"

	"skein-api/internal/testsupport"
)

// sk-118.11 (F3) — invariant §6.3 (Ready/inbox) leg B: a STANDING issue
// assigned to a user and not yet accepted must NOT appear in that user's
// inbox-needs-acceptance surface. The Inbox is "work delegated to me by others
// that I haven't acknowledged yet" — a standing task is a perpetual obligation,
// not a unit of work to accept, so it has no place in the unaccepted queue.
//
// The production query is MeHandler.countInbox (me.go ~181), gated on
// `i.status = 'open'`. Because that is a per-status equality (not a broad
// `<> 'closed'`), a standing issue — status='standing' — is structurally
// excluded already; this test LOCKS THAT IN against future widening of the
// inbox predicate. It is NOT a fix for a leak: the equality was already correct
// (R12 deliberately leaves `status = 'open'` untouched in the static guard).
//
// Surface tested: NotificationCounts → countInbox (the header Inbox badge and
// the left-column Inbox tab share this exact SQL, me.go doc-comment).
func TestInbox_StandingExcludedFromUnaccepted(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsinbstd")
	h := NewMeHandler(pool, nil, nil)

	// A normal inbox row: other → caller, open, UNACCEPTED. This is the
	// control that must stay visible throughout.
	normalID := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, other, normalID, "normal unaccepted", "open", 2)
	testsupport.SetAssignee(t, pool, normalID, callerName)

	// A second row that we will promote to standing: other → caller, open,
	// UNACCEPTED — initially indistinguishable from the normal one.
	standingID := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, other, standingID, "to-be-standing unaccepted", "open", 2)
	testsupport.SetAssignee(t, pool, standingID, callerName)

	inbox := func() int {
		t.Helper()
		_, body := callCounts(t, h, caller)
		return mustInt(t, body, "inbox")
	}

	// Snapshot: both unaccepted, assigned-by-other rows are in the inbox.
	if got := inbox(); got != 2 {
		t.Fatalf("snapshot inbox = %d, want 2 (two unaccepted delegated rows)", got)
	}

	// Promote the second row to standing → it must drop out of the inbox,
	// leaving only the normal control row.
	testsupport.SetStatus(t, pool, standingID, "standing")
	if got := inbox(); got != 1 {
		t.Fatalf("inbox after promote-to-standing = %d, want 1 (standing excluded from needs-acceptance; normal row remains)", got)
	}

	// Cross-check against the canonical testsupport inbox predicate
	// (status='open' AND accepted_at IS NULL AND created_by<>caller): it must
	// agree that exactly one row (the normal one) sits in the surface.
	if got := testsupport.CountUnacceptedInbox(t, pool, pid, caller, callerName); got != 1 {
		t.Fatalf("CountUnacceptedInbox = %d, want 1 (standing must not count)", got)
	}
}
