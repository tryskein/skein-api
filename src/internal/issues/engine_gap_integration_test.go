package issues

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"skein-api/internal/testsupport"
)

// sk-105.4 (S3 engine gaps) — issues-store invariants:
//   - pinned-close protection (ErrPinnedNoForce; force=true bypass)
//   - unclaim idempotency on an already-unlocked issue
//   - concurrent child-id allocation (two distinct parent.N, no PK collision)
//   - commits_attach dedup (one row, one event) + DismissDelegated /
//     ResumeDelegated second-call no-op (idempotency matrix, m14)
//
// All assertions are project/entity-scoped; DB-backed, skip without
// TEST_DATABASE_URL (via the in-package newTestPool helper).

// TestClose_PinnedProtection verifies a pinned issue cannot be closed
// without force, and that force=true (admin) bypasses the guard.
func TestClose_PinnedProtection(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	uname := testsupport.Username(t, pool, owner)
	actor := Actor{UserID: owner, Username: uname, Role: "human"}

	pinnedID := prefix + "-1"
	seedIssue(t, pool, pid, owner, pinnedID, "pinned issue")
	testsupport.SetPinned(t, pool, pinnedID, true) // bumps version 1 → 2

	store := NewStore(pool)

	// Close without force → rejected with ErrPinnedNoForce.
	_, err := store.Close(ctx, CloseInput{
		ID:              pinnedID,
		ExpectedVersion: 2,
		Reason:          "done",
		Actor:           actor,
	})
	if !errors.Is(err, ErrPinnedNoForce) {
		t.Fatalf("expected ErrPinnedNoForce closing pinned issue, got %v", err)
	}
	// Still open — the rejected close must not have mutated status.
	if got := testsupport.CountIssuesByStatus(t, pool, pid, "closed"); got != 0 {
		t.Fatalf("pinned issue should remain non-closed, but project has %d closed", got)
	}

	// Close with force=true → succeeds (admin-only at the tool layer; the
	// store honours the force flag directly).
	detail, err := store.Close(ctx, CloseInput{
		ID:              pinnedID,
		ExpectedVersion: 2,
		Reason:          "force-done",
		Force:           true,
		Actor:           actor,
	})
	if err != nil {
		t.Fatalf("force close of pinned issue should succeed, got %v", err)
	}
	if detail.Status != "closed" {
		t.Fatalf("after force close, status = %q, want closed", detail.Status)
	}
	if got := testsupport.CountIssuesByStatus(t, pool, pid, "closed"); got != 1 {
		t.Fatalf("expected 1 closed issue in project after force close, got %d", got)
	}
}

// TestUnclaim_Idempotent_OnUnlockedIssue verifies Unclaim on an issue that
// is already unlocked is a silent no-op: no error, status untouched, no
// new unclaim event written.
func TestUnclaim_Idempotent_OnUnlockedIssue(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	uname := testsupport.Username(t, pool, owner)
	actor := Actor{UserID: owner, Username: uname, Role: "human"}

	id := prefix + "-1"
	seedIssue(t, pool, pid, owner, id, "never claimed") // status=open, locked_by=NULL

	store := NewStore(pool)

	// Capture pre-state.
	before, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue before: %v", err)
	}
	unclaimedBefore := testsupport.CountEventsForProject(t, pool, pid, EventIssueUnclaimed)

	// Unclaim an unlocked issue — must be a no-op (no error).
	after, err := store.Unclaim(ctx, UnclaimInput{
		ID:         id,
		SessionID:  "sess-noop",
		NextStatus: "open",
		Reason:     "give up",
		Actor:      actor,
	})
	if err != nil {
		t.Fatalf("Unclaim on unlocked issue should be a no-op, got error %v", err)
	}

	// Status and version untouched.
	if after.Status != before.Status {
		t.Fatalf("status changed by no-op unclaim: %q → %q", before.Status, after.Status)
	}
	if after.Version != before.Version {
		t.Fatalf("version bumped by no-op unclaim: %d → %d", before.Version, after.Version)
	}
	// No new issue.unclaimed event for this project.
	unclaimedAfter := testsupport.CountEventsForProject(t, pool, pid, EventIssueUnclaimed)
	if unclaimedAfter != unclaimedBefore {
		t.Fatalf("no-op unclaim wrote an event: %d → %d issue.unclaimed events",
			unclaimedBefore, unclaimedAfter)
	}
}

// TestCreateIssue_ConcurrentChildAllocation is the HIGH-VALUE race: two
// goroutines create a child of the SAME parent concurrently. The
// child_counters UPSERT + reconciliation UPDATE runs inside CreateIssue's
// SERIALIZABLE tx, so both children must land with DISTINCT parent.N ids
// and no issues_pkey collision. We assert exactly two distinct children
// shaped parent.1 and parent.2 (some serialization losers may retry via
// SERIALIZABLE, but the public CreateIssue surfaces a clean result or a
// retryable error — we retry the loser deterministically).
func TestCreateIssue_ConcurrentChildAllocation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	uname := testsupport.Username(t, pool, owner)
	actor := Actor{UserID: owner, Username: uname, Role: "human"}

	parentID := prefix + "-1"
	seedIssue(t, pool, pid, owner, parentID, "parent epic")

	store := NewStore(pool)

	// Two goroutines, each creating a child of parentID. createChild retries
	// on a SERIALIZABLE serialization failure (40001) — the legitimate
	// outcome for the losing tx under contention — so each goroutine
	// eventually produces exactly one child.
	createChild := func(title string) (string, error) {
		parent := parentID
		for attempt := 0; attempt < 8; attempt++ {
			detail, err := store.CreateIssue(ctx, pid,
				prefix, "counter",
				CreateIssueInput{
					ParentID: &parent,
					Title:    title,
					Type:     "task",
					Priority: 2,
					Source:   SourceHuman,
				},
				owner, actor)
			if err == nil {
				return detail.ID, nil
			}
			if isSerializationFailure(err) {
				continue // retry the loser
			}
			return "", err
		}
		return "", errors.New("exhausted retries creating child")
	}

	var (
		wg       sync.WaitGroup
		ids      [2]atomic.Value
		errCount int32
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			id, err := createChild("child")
			if err != nil {
				atomic.AddInt32(&errCount, 1)
				return
			}
			ids[i].Store(id)
		}()
	}
	wg.Wait()

	if errCount != 0 {
		t.Fatalf("one or both child creates failed (%d errors)", errCount)
	}

	id0, _ := ids[0].Load().(string)
	id1, _ := ids[1].Load().(string)
	if id0 == "" || id1 == "" {
		t.Fatalf("missing child ids: %q %q", id0, id1)
	}
	if id0 == id1 {
		t.Fatalf("child id collision: both goroutines got %q", id0)
	}

	// Both must be immediate children of the parent: parent.1 and parent.2.
	want := map[string]bool{parentID + ".1": false, parentID + ".2": false}
	for _, id := range []string{id0, id1} {
		if _, ok := want[id]; !ok {
			t.Fatalf("unexpected child id %q (want %s.1 / %s.2)", id, parentID, parentID)
		}
		want[id] = true
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("expected child %q was not produced", id)
		}
	}

	// And exactly 2 immediate children exist in the DB for this parent.
	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM issues
 WHERE project_id = $1 AND id ~ ('^' || $2 || '\.[0-9]+$')`,
		pid, parentID).Scan(&n); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected exactly 2 immediate children of %s, got %d", parentID, n)
	}

	// child_counters must be consistent: last_child = 2.
	var lastChild int
	if err := pool.QueryRow(ctx,
		`SELECT last_child FROM child_counters WHERE parent_id = $1`, parentID).Scan(&lastChild); err != nil {
		t.Fatalf("read child_counters: %v", err)
	}
	if lastChild != 2 {
		t.Fatalf("child_counters.last_child = %d, want 2", lastChild)
	}
}

// TestCommitsAttach_DedupHash_OneRowOneEvent verifies that re-attaching the
// same commit hash results in exactly ONE issue_commits row (ON CONFLICT
// dedup) while STILL emitting an issue.commits_attached event on each call
// (the audit trail records intent even when the row is a no-op). m14.
func TestCommitsAttach_DedupHash_OneRowOneEvent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	testsupport.SeedRepo(t, pool, pid, "repo1", "https://example.com/repo1.git")
	uname := testsupport.Username(t, pool, owner)
	actor := Actor{UserID: owner, Username: uname, Role: "human"}

	id := prefix + "-1"
	seedIssue(t, pool, pid, owner, id, "ship it")

	store := NewStore(pool)
	const hash = "cffd5ab"

	// First attach.
	if _, err := store.AttachCommits(ctx, AttachCommitsInput{
		ID:      id,
		Commits: []CloseCommitInput{{Hash: hash}},
		Actor:   actor,
	}); err != nil {
		t.Fatalf("first AttachCommits: %v", err)
	}
	// Second attach of the same hash — row-level no-op, event still emitted.
	if _, err := store.AttachCommits(ctx, AttachCommitsInput{
		ID:      id,
		Commits: []CloseCommitInput{{Hash: hash}},
		Actor:   actor,
	}); err != nil {
		t.Fatalf("second AttachCommits (dedup): %v", err)
	}

	// Exactly one issue_commits row for this (issue, hash).
	var nRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM issue_commits WHERE issue_id = $1 AND hash = $2`,
		id, hash).Scan(&nRows); err != nil {
		t.Fatalf("count issue_commits: %v", err)
	}
	if nRows != 1 {
		t.Fatalf("expected exactly 1 issue_commits row after dedup, got %d", nRows)
	}

	// Two issue.commits_attached events for this project (one per call).
	nEvents := testsupport.CountEventsForProject(t, pool, pid, EventIssueCommitsAttached)
	if nEvents != 2 {
		t.Fatalf("expected 2 issue.commits_attached events (one per call), got %d", nEvents)
	}
}

// TestDelegationActions_Idempotent verifies the second call of
// DismissDelegated and ResumeDelegated is a silent no-op: no duplicate
// event, no version bump. m14.
func TestDelegationActions_Idempotent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	uname := testsupport.Username(t, pool, owner)
	actor := Actor{UserID: owner, Username: uname, Role: "human"}

	id := prefix + "-1"
	seedIssue(t, pool, pid, owner, id, "delegated work")

	store := NewStore(pool)

	// First dismiss → writes one delegation_dropped event.
	if _, err := store.DismissDelegated(ctx, DelegationActionInput{ID: id, Actor: actor}); err != nil {
		t.Fatalf("first DismissDelegated: %v", err)
	}
	droppedAfter1 := testsupport.CountEventsForProject(t, pool, pid, EventIssueDelegationDropped)
	if droppedAfter1 != 1 {
		t.Fatalf("expected 1 delegation_dropped event after first dismiss, got %d", droppedAfter1)
	}

	// Second dismiss → no-op, no new event.
	if _, err := store.DismissDelegated(ctx, DelegationActionInput{ID: id, Actor: actor}); err != nil {
		t.Fatalf("second DismissDelegated (no-op): %v", err)
	}
	droppedAfter2 := testsupport.CountEventsForProject(t, pool, pid, EventIssueDelegationDropped)
	if droppedAfter2 != 1 {
		t.Fatalf("second dismiss wrote an extra event: %d → %d", droppedAfter1, droppedAfter2)
	}

	// First resume → writes one delegation_resumed event.
	if _, err := store.ResumeDelegated(ctx, DelegationActionInput{ID: id, Actor: actor}); err != nil {
		t.Fatalf("first ResumeDelegated: %v", err)
	}
	resumedAfter1 := testsupport.CountEventsForProject(t, pool, pid, EventIssueDelegationResumed)
	if resumedAfter1 != 1 {
		t.Fatalf("expected 1 delegation_resumed event after first resume, got %d", resumedAfter1)
	}

	// Second resume → no-op (caller no longer in dismissed_by).
	if _, err := store.ResumeDelegated(ctx, DelegationActionInput{ID: id, Actor: actor}); err != nil {
		t.Fatalf("second ResumeDelegated (no-op): %v", err)
	}
	resumedAfter2 := testsupport.CountEventsForProject(t, pool, pid, EventIssueDelegationResumed)
	if resumedAfter2 != 1 {
		t.Fatalf("second resume wrote an extra event: %d → %d", resumedAfter1, resumedAfter2)
	}
}
