package notifications

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// fanout_race_test.go — sk-303.13, coverage-matrix row 12: INV-9 ("a settings
// change is not retroactive") and the race the spec explicitly ALLOWS.
//
// The two halves are different in kind and are tested differently:
//
//   - No retroactivity is a hard, deterministic rule: what was not written
//     while the reason was off never appears once it is switched back on. The
//     decision is "irreversible for that particular moment in time" and that is the
//     price the ADR names out loud.
//
//   - The race is a PERMISSION, not a guarantee: "A race is allowed: some
//     recipients are processed against the previous state… that is not a defect". So the test
//     must not pin a winner — the acceptance says so in as many words. What it
//     pins is that every row that does appear is WHOLLY one snapshot or wholly
//     the other, never a mixture, and that nothing rewrites a row after the
//     fact.

// TestFanOutRace_DisabledReasonIsNotResurrectedByEnablingItBack — INV-9,
// deterministic. Three events across two preference states, then a check that
// the first one is still absent.
func TestFanOutRace_DisabledReasonIsNotResurrectedByEnablingItBack(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)
	addMember(t, pool, projectID, assignee, actor)

	issueID := "frno-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	tx0, _ := pool.Begin(ctx)
	if err := AddIssueWatcher(ctx, tx0, issueID, creator, ReasonCreator); err != nil {
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	_ = tx0.Commit(ctx)

	// Both channels OFF — the event happens and leaves no trace.
	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, false, false)
	missed := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
		ActorUserID: actor, CreatorUserID: creator, AssigneeUserID: assignee,
	})
	if row := readNotifRow(t, pool, creator, missed); row.found {
		t.Fatalf("a switched-off reason produced a row: %+v", row)
	}

	// The user changes their mind and switches the reason back on.
	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, true, true)

	// From now on the signal arrives…
	delivered := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
		ActorUserID: actor, CreatorUserID: creator, AssigneeUserID: assignee,
	})
	if row := readNotifRow(t, pool, creator, delivered); !row.found {
		t.Fatal("switching the reason back on did not start delivering")
	}

	// …and what was missed stays missed. This is the whole of INV-9: the
	// decision was taken at the moment of the event and is not revisited.
	if row := readNotifRow(t, pool, creator, missed); row.found {
		t.Errorf("the missed event reappeared after the reason was switched back on: %+v", row)
	}
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id IN ($2, $3)`,
		creator, missed, delivered).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Errorf("rows across the two events = %d, want 1 (only the one that happened while enabled)", total)
	}
}

// TestFanOutRace_SettingsChangeAfterTheFactDoesNotRewriteTheRow — the row-level
// reading of INV-9. A preference change lands AFTER the fan-out committed; the
// already-written row keeps the channels it was born with, and nothing goes
// back to revise it.
func TestFanOutRace_SettingsChangeAfterTheFactDoesNotRewriteTheRow(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)
	addMember(t, pool, projectID, assignee, actor)

	issueID := "fraf-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	tx0, _ := pool.Begin(ctx)
	if err := AddIssueWatcher(ctx, tx0, issueID, creator, ReasonCreator); err != nil {
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	_ = tx0.Commit(ctx)

	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, true, false)
	eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
		ActorUserID: actor, CreatorUserID: creator, AssigneeUserID: assignee,
	})
	before := readNotifRow(t, pool, creator, eid)
	if !before.found || !before.feedChannel || before.pushState != "none" {
		t.Fatalf("row = %+v, want feed-only", before)
	}

	// Flip both channels the other way round, long after the event.
	enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, false, true)

	after := readNotifRow(t, pool, creator, eid)
	if after.found != before.found ||
		after.feedChannel != before.feedChannel ||
		after.pushState != before.pushState ||
		after.hasDeadline != before.hasDeadline ||
		after.hasNextAt != before.hasNextAt ||
		derefStr(after.reason) != derefStr(before.reason) {
		t.Errorf("the existing row changed after a settings edit: %+v → %+v", before, after)
	}
}

// TestFanOutRace_ConcurrentSettingsChangeNeverTearsARow — the permitted race.
//
// The two snapshots are chosen so that a torn read would be VISIBLE: the old
// state is feed-only and the new one is push-only, so the two legal shapes are
// disjoint on both channels. A row that carried feed AND a pending push, or
// neither, could only come from reading one channel from one snapshot and the
// other channel from the other.
//
// No winner is asserted — the spec permits either — and the loop deliberately
// randomises the moment of the flip instead of engineering an interleaving,
// because the claim under test is «whatever the interleaving», not «this
// interleaving».
func TestFanOutRace_ConcurrentSettingsChangeNeverTearsARow(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	assignee := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)
	addMember(t, pool, projectID, assignee, actor)

	issueID := "frrc-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	tx0, _ := pool.Begin(ctx)
	if err := AddIssueWatcher(ctx, tx0, issueID, creator, ReasonCreator); err != nil {
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	_ = tx0.Commit(ctx)

	const rounds = 24
	feedOnly, pushOnly := 0, 0
	for i := range rounds {
		// Old snapshot: feed only.
		enableReasonChannels(t, pool, creator, ReasonKeyDelegatedStatus, true, false)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Somewhere between "before the fan-out even starts" and "after it
			// has finished" — that whole span is what the spec permits.
			time.Sleep(time.Duration(rand.IntN(400)) * time.Microsecond)
			// New snapshot: push only.
			_, _ = pool.Exec(context.Background(), `
UPDATE user_notification_reason_prefs
   SET feed_enabled = false, push_enabled = true, updated_at = now()
 WHERE user_id = $1 AND reason = $2`, creator, string(ReasonKeyDelegatedStatus))
		}()

		eid := statusChangeFanOut(t, pool, issueID, projectID, FanOutInput{
			ActorUserID: actor, CreatorUserID: creator, AssigneeUserID: assignee,
		})
		wg.Wait()

		row := readNotifRow(t, pool, creator, eid)
		if !row.found {
			t.Fatalf("round %d: no row at all — neither snapshot allows that (old=feed-only, new=push-only)", i)
		}
		if row.reason == nil || *row.reason != string(ReasonKeyDelegatedStatus) {
			t.Fatalf("round %d: reason = %v, want %s", i, row.reason, ReasonKeyDelegatedStatus)
		}
		switch {
		case row.feedChannel && row.pushState == "none" && !row.hasDeadline:
			feedOnly++
		case !row.feedChannel && row.pushState == "pending" && row.hasDeadline:
			pushOnly++
		default:
			t.Fatalf("round %d: torn row %+v — neither the old snapshot (feed only) nor the new one (push only)", i, row)
		}
	}

	if feedOnly+pushOnly != rounds {
		t.Fatalf("accounted %d of %d rounds", feedOnly+pushOnly, rounds)
	}
	t.Logf("permitted race outcomes over %d rounds: old snapshot %d, new snapshot %d (no winner is required)",
		rounds, feedOnly, pushOnly)
	_ = ctx
}

// derefStr renders a nullable text column for comparison; the row structs hold
// pointers, so comparing them directly would compare addresses.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
