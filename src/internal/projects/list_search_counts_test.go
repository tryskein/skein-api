package projects

import (
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

// S2 (sk-105.3): the project-list FTS/search path
// (list_search.go::listProjectsWithSearch) duplicates ALL the per-status
// FILTER pill logic from the alphabetic store.go path, but the existing
// list_counts_test.go only exercises the alphabetic path (empty Query).
// This file drives the FTS branch (non-empty Query) and asserts the
// open/in_progress/blocked pills, then transitions issues and re-asserts
// — catching a leak between the two duplicated SQL blocks.
//
// The FTS path is selected by setting ListProjectsArgs.Query. We search
// by the project's own id_prefix (the score expression gives an exact
// id_prefix match a +0.5 boost) so the seeded project is the FTS hit.
// All asserts pluck the single seeded project out of the result, so a
// concurrent package's projects can never perturb the assertion.

// findItem returns the list row for pid, or fails.
func findItem(t *testing.T, res *ListResult, pid uuid.UUID) *ProjectListItem {
	t.Helper()
	for i := range res.Items {
		if res.Items[i].ID == pid {
			return &res.Items[i]
		}
	}
	t.Fatalf("project %s not present in FTS list result (%d rows)", pid, len(res.Items))
	return nil
}

// TestListProjectsFTS_PillsAndTransitions seeds a mix of statuses, drives
// the FTS path, asserts the pills, then transitions issues and
// re-asserts. Verifies the disjointness invariant: adding a blocks edge
// moves a row from the open pill to the blocked pill (open −1, blocked
// +1); closing removes it from every pill; deferring removes it from the
// open/in_progress/blocked pills (those pills omit deferred).
func TestListProjectsFTS_PillsAndTransitions(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "tsfts")

	// 3 open, 1 in_progress, 1 deferred, 1 closed.
	o1, o2, o3 := prefix+"-1", prefix+"-2", prefix+"-3"
	ip := prefix + "-4"
	df := prefix + "-5"
	cl := prefix + "-6"
	testsupport.SeedIssue(t, pool, pid, owner, o1, "alpha", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, o2, "beta", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, o3, "gamma", "open", 2)
	testsupport.SeedIssue(t, pool, pid, owner, ip, "delta", "in_progress", 2)
	testsupport.SeedIssue(t, pool, pid, owner, df, "epsilon", "deferred", 2)
	testsupport.SeedIssue(t, pool, pid, owner, cl, "zeta", "closed", 2)

	store := NewStore(pool)
	listFTS := func() *ProjectListItem {
		// Query = the project's own id_prefix triggers the FTS path and
		// matches this project (exact id_prefix boost).
		res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
			CallerID: owner,
			Query:    prefix,
		})
		if err != nil {
			t.Fatalf("FTS list: %v", err)
		}
		return findItem(t, res, pid)
	}

	// Snapshot: open=3, in_progress=1, blocked=0 (deferred/closed never
	// appear in these three pills).
	got := listFTS()
	if got.OpenCount != 3 || got.InProgressCount != 1 || got.BlockedCount != 0 {
		t.Fatalf("FTS snapshot pills = open %d / in_progress %d / blocked %d, want 3/1/0",
			got.OpenCount, got.InProgressCount, got.BlockedCount)
	}

	// Transition 1: open o3 → in_progress. open 3→2, in_progress 1→2.
	testsupport.SetStatus(t, pool, o3, "in_progress")
	got = listFTS()
	if got.OpenCount != 2 || got.InProgressCount != 2 || got.BlockedCount != 0 {
		t.Fatalf("after o3→in_progress pills = %d/%d/%d, want 2/2/0",
			got.OpenCount, got.InProgressCount, got.BlockedCount)
	}

	// Transition 2: add a blocks edge to o1 (still open). Disjointness:
	// open 2→1, blocked 0→1.
	testsupport.SeedBlocker(t, pool, o1, owner)
	got = listFTS()
	if got.OpenCount != 1 || got.InProgressCount != 2 || got.BlockedCount != 1 {
		t.Fatalf("after blocking o1 pills = %d/%d/%d, want 1/2/1 (disjoint open/blocked)",
			got.OpenCount, got.InProgressCount, got.BlockedCount)
	}

	// Transition 3: close o2 (open). open 1→0.
	testsupport.SetStatus(t, pool, o2, "closed")
	got = listFTS()
	if got.OpenCount != 0 || got.InProgressCount != 2 || got.BlockedCount != 1 {
		t.Fatalf("after closing o2 pills = %d/%d/%d, want 0/2/1",
			got.OpenCount, got.InProgressCount, got.BlockedCount)
	}

	// Transition 4: defer the in_progress ip. in_progress 2→1 (deferred
	// is not in any of the three pills).
	testsupport.SetStatus(t, pool, ip, "deferred")
	got = listFTS()
	if got.OpenCount != 0 || got.InProgressCount != 1 || got.BlockedCount != 1 {
		t.Fatalf("after deferring ip pills = %d/%d/%d, want 0/1/1",
			got.OpenCount, got.InProgressCount, got.BlockedCount)
	}
}

// TestListProjectsFTS_OutboxAndOnMePills exercises the caller-scoped
// MyOutbox* / OnMe* pills on the FTS path (the same FILTER blocks are
// duplicated there from store.go). Seeds a delegated issue and an
// on-me issue, asserts the scoped pills, then transitions and re-asserts.
func TestListProjectsFTS_OutboxAndOnMePills(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsftsx")

	store := NewStore(pool)
	listFTS := func() *ProjectListItem {
		res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
			CallerID: caller,
			Query:    prefix,
		})
		if err != nil {
			t.Fatalf("FTS list: %v", err)
		}
		return findItem(t, res, pid)
	}

	// Delegated: caller-created, assigned to other, open → MyOutboxOpen=1.
	deleg := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, caller, deleg, "delegated", "open", 2)
	testsupport.SetAssignee(t, pool, deleg, otherName)

	// On-me: other-created, assigned to caller, accepted, open → OnMeOpen=1.
	onme := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, other, onme, "on me", "open", 2)
	testsupport.SetAssignee(t, pool, onme, callerName)
	testsupport.Accept(t, pool, onme, caller, true)

	got := listFTS()
	if got.MyOutboxOpenCount != 1 {
		t.Fatalf("MyOutboxOpenCount = %d, want 1", got.MyOutboxOpenCount)
	}
	if got.OnMeOpenCount != 1 {
		t.Fatalf("OnMeOpenCount = %d, want 1", got.OnMeOpenCount)
	}

	// Transition: delegated open → in_progress. MyOutbox shifts pill.
	testsupport.SetStatus(t, pool, deleg, "in_progress")
	got = listFTS()
	if got.MyOutboxOpenCount != 0 || got.MyOutboxInProgressCount != 1 {
		t.Fatalf("after deleg→in_progress MyOutbox open/in_progress = %d/%d, want 0/1",
			got.MyOutboxOpenCount, got.MyOutboxInProgressCount)
	}

	// Transition: on-me open → in_progress. OnMe shifts pill.
	testsupport.SetStatus(t, pool, onme, "in_progress")
	got = listFTS()
	if got.OnMeOpenCount != 0 || got.OnMeInProgressCount != 1 {
		t.Fatalf("after onme→in_progress OnMe open/in_progress = %d/%d, want 0/1",
			got.OnMeOpenCount, got.OnMeInProgressCount)
	}

	// Close both → all scoped pills drop to 0.
	testsupport.SetStatus(t, pool, deleg, "closed")
	testsupport.SetStatus(t, pool, onme, "closed")
	got = listFTS()
	if got.MyOutboxOpenCount != 0 || got.MyOutboxInProgressCount != 0 ||
		got.OnMeOpenCount != 0 || got.OnMeInProgressCount != 0 {
		t.Fatalf("after closing both, scoped pills not all zero: outbox %d/%d onme %d/%d",
			got.MyOutboxOpenCount, got.MyOutboxInProgressCount,
			got.OnMeOpenCount, got.OnMeInProgressCount)
	}
}

// TestListProjectsSearch_StandingExcludedAndCounted is the MAJOR gap: F2g
// added `standing_count` + `my_outbox_standing_count` to BOTH the FTS pill
// block (list_search.go::listProjectsWithSearch, ~line 171/243) and the
// semantic-rerank pill block (~line 487/557), but the standing tests only
// covered the NON-search path (GetProject + alphabetic ListProjects via
// standing_counts_test.go). This drives the SEARCH/FTS path the same way
// list_search_counts_test.go does (Query = project id_prefix → exact-prefix
// FTS hit) and asserts:
//
//	(a) standing is excluded from the active open/in_progress/blocked pills,
//	(b) standingCount reports it (unscoped),
//	(c) myOutboxStandingCount is correct under the outbox-scoped Delegated
//	    predicate (caller-created, delegated to other).
//
// Promote-to-standing → re-assert is the canonical leak-catching pattern: it
// proves the search-path FILTER blocks key on status='standing' exactly like
// the alphabetic path, so a future refactor of one block can't silently
// re-fold standing into the active counters on the other.
//
// Semantic-path note: the rerank block shares the IDENTICAL standing SQL
// (`COUNT(*) FILTER (WHERE i.status = 'standing')` + the outbox variant), but
// it only fires when args.Embedder is non-nil + Available(). The test harness
// constructs ListProjectsArgs with no Embedder, so the semantic path is never
// reached here — it degrades to FTS-only by design. The FTS assertions below
// therefore cover the live path; the semantic block is verified by code
// inspection to carry the same two FILTER expressions.
func TestListProjectsSearch_StandingExcludedAndCounted(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsftsst")

	store := NewStore(pool)
	listFTS := func() *ProjectListItem {
		t.Helper()
		res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
			CallerID: caller,
			Query:    prefix, // exact id_prefix → FTS path + this project
		})
		if err != nil {
			t.Fatalf("FTS list: %v", err)
		}
		return findItem(t, res, pid)
	}

	// Seed: one caller-created+delegated OPEN issue, plus a normal open issue
	// the caller created (unassigned) — so the active open pill is 2 and the
	// outbox open pill is 1 before any standing exists.
	deleg := prefix + "-1"
	plain := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, caller, deleg, "delegated open", "open", 2)
	testsupport.SetAssignee(t, pool, deleg, otherName)
	testsupport.SeedIssue(t, pool, pid, caller, plain, "plain open", "open", 2)

	got := listFTS()
	if got.OpenCount != 2 {
		t.Fatalf("snapshot openCount = %d, want 2", got.OpenCount)
	}
	if got.MyOutboxOpenCount != 1 {
		t.Fatalf("snapshot myOutboxOpenCount = %d, want 1", got.MyOutboxOpenCount)
	}
	if got.StandingCount != 0 || got.MyOutboxStandingCount != 0 {
		t.Fatalf("snapshot standing pills = %d/%d, want 0/0",
			got.StandingCount, got.MyOutboxStandingCount)
	}

	// Promote the DELEGATED issue to standing. It must drop out of BOTH the
	// active open pill AND the outbox open pill, landing only in the standing
	// aggregates — unscoped standingCount AND outbox-scoped
	// myOutboxStandingCount (caller-created + delegated-to-other still
	// satisfies the Delegated predicate at status='standing').
	testsupport.SetStatus(t, pool, deleg, "standing")
	got = listFTS()
	if got.OpenCount != 1 {
		t.Fatalf("after promote: openCount = %d, want 1 (standing excluded from open)", got.OpenCount)
	}
	if got.InProgressCount != 0 || got.BlockedCount != 0 {
		t.Fatalf("after promote: active pills polluted — in_progress=%d blocked=%d",
			got.InProgressCount, got.BlockedCount)
	}
	if got.MyOutboxOpenCount != 0 {
		t.Fatalf("after promote: myOutboxOpenCount = %d, want 0 (standing must not inflate the outbox open pill)", got.MyOutboxOpenCount)
	}
	if got.StandingCount != 1 {
		t.Fatalf("after promote: standingCount = %d, want 1 (FTS path must report standing)", got.StandingCount)
	}
	if got.MyOutboxStandingCount != 1 {
		t.Fatalf("after promote: myOutboxStandingCount = %d, want 1 (outbox-scoped standing pill on the FTS path)", got.MyOutboxStandingCount)
	}
}

// TestListProjectsSearch_OutboxStandingHonorsDelegatedPredicate pins that the
// FTS-path my_outbox_standing_count obeys the SAME Delegated predicate as the
// other outbox pills — it counts a standing issue the caller created and
// delegated to someone else, but NOT a standing issue assigned to the caller
// themselves (self-assigned is not "delegated out"), and NOT a standing issue
// created by someone else. Without this, the outbox standing pill could
// silently count every standing row in the project.
func TestListProjectsSearch_OutboxStandingHonorsDelegatedPredicate(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsftssd")

	// Caller→other standing: counts in the outbox pill.
	s1 := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, caller, s1, "delegated standing", "standing", 2)
	testsupport.SetAssignee(t, pool, s1, otherName)
	// Caller→self standing: must NOT count (not delegated out).
	s2 := prefix + "-2"
	testsupport.SeedIssue(t, pool, pid, caller, s2, "self standing", "standing", 2)
	testsupport.SetAssignee(t, pool, s2, callerName)
	// Other→other standing: must NOT count (caller did not create it, and is
	// not a previous assignee, so it's not in the caller's Delegated set).
	s3 := prefix + "-3"
	testsupport.SeedIssue(t, pool, pid, other, s3, "foreign standing", "standing", 2)
	testsupport.SetAssignee(t, pool, s3, otherName)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: caller,
		Query:    prefix,
	})
	if err != nil {
		t.Fatalf("FTS list: %v", err)
	}
	got := findItem(t, res, pid)

	// Unscoped: all three standing rows count.
	if got.StandingCount != 3 {
		t.Fatalf("standingCount = %d, want 3 (all three standing rows)", got.StandingCount)
	}
	// Outbox-scoped: only the caller→other one.
	if got.MyOutboxStandingCount != 1 {
		t.Fatalf("myOutboxStandingCount = %d, want 1 (only caller-created + delegated-out standing)", got.MyOutboxStandingCount)
	}
}
