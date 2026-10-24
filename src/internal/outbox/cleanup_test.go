package outbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// cleanup_test.go — sk-303.13 / T013, coverage-matrix row 23: what the
// retention sweep is allowed to take.
//
// SC-025 says the check is "a query against the data, not the presence of cleanup code", and
// that cuts both ways: the sweep must actually remove what it promises, and it
// must not remove anything else. The dangerous direction is the second one —
// a row deleted by mistake is somebody's history, and there is no undo.
//
// The clock is controlled by writing push_finalized_at rather than by waiting:
// the rule is about an AGE, and an age is a value, not a delay.

// agedRow describes one seeded notification row in a shape the eligibility
// rule can be asked about.
type agedRow struct {
	name        string
	feedChannel bool
	state       string
	outcome     string
	// finalisedAgo is how long ago the delivery ended; zero leaves the stamp
	// NULL, which is the shape of a row that never finished.
	finalisedAgo time.Duration
	wantDeleted  bool
	why          string
}

// seedAgedRow inserts one row per the description and returns its id.
func seedAgedRow(t *testing.T, pool *pgxpool.Pool, f *pushFixture, idx int, r agedRow) int64 {
	t.Helper()
	ctx := context.Background()
	eventID := seedEvent(t, pool, "issue.status_changed", "issue", f.issueID, f.projectID)

	var outcome any
	if r.outcome != "" {
		outcome = r.outcome
	}
	var finalised any
	if r.finalisedAgo > 0 {
		finalised = time.Now().UTC().Add(-r.finalisedAgo)
	}

	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (
    user_id, event_id, type, project_id, reason, feed_channel,
    push_state, push_outcome, push_finalized_at, push_deadline_at, published_at)
VALUES ($1, $2, 'issue.status_changed', $3, 'delegated_status', $4,
        $5, $6, $7, now() + interval '1 hour', NULL)
RETURNING id`,
		f.user, eventID, f.projectID, r.feedChannel, r.state, outcome, finalised).Scan(&id); err != nil {
		t.Fatalf("seed row %d (%s): %v", idx, r.name, err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE id = $1`, id) })
	return id
}

// TestCleanup_EligibilityMatrix — the whole rule in one table.
func TestCleanup_EligibilityMatrix(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "clem")

	const old = 31 * 24 * time.Hour
	rows := []agedRow{
		// Eligible: invisible, finished, past the window — for EVERY final
		// outcome. The two configuration/settings outcomes are listed
		// explicitly because they are the ones that arrive in bulk during an
		// incident, and exempting them would rebuild the pile the sweep exists
		// to remove.
		{"success, aged", false, "final", outcomeSuccess, old, true,
			"delivered and invisible — nothing left to look at"},
		{"no_device, aged", false, "final", outcomeNoDevice, old, true,
			"a legitimate final outcome is still a finished one"},
		{"access_revoked, aged", false, "final", outcomeAccessRevoked, old, true, ""},
		{"prefs_unavailable, aged", false, "final", outcomePrefsUnavailable, old, true,
			"\"including delivery-prefs-unavailable\""},
		{"configuration_no_send, aged", false, "final", outcomeConfigurationNoSend, old, true,
			"\"including configuration-no-send\""},
		{"transport_exhausted, aged", false, "final", outcomeTransportExhausted, old, true, ""},
		{"superseded, aged", false, "final", outcomeSuperseded, old, true,
			"sk-303.14: a superseded digest is the same finished signal, and the sweep " +
				"must recognise it together with the others: the outcome set grows, and this " +
				"list is hand-written and silent on a miss"},

		// Not eligible.
		{"feed row, aged and final", true, "final", outcomeSuccess, old, false,
			"a feed row is somebody's history — invisibility is the whole eligibility"},
		{"invisible but fresh", false, "final", outcomeSuccess, 24 * time.Hour, false,
			"inside the retention window: an undelivered signal may still be wanted in an incident review"},
		{"invisible, still pending", false, "pending", "", 0, false,
			"still owed a push"},
		{"invisible, leased right now", false, "leased", "", 0, false,
			"a worker is holding it"},
		{"invisible, no push at all", false, "none", "", 0, false,
			"never had a delivery to finish"},
	}

	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = seedAgedRow(t, pool, f, i, r)
	}

	runner := NewCleanupRunner(pool, CleanupOptions{BatchSize: 100, Retention: PushRetention})
	if _, err := runner.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	for i, r := range rows {
		exists := rowExists(t, pool, ids[i])
		if r.wantDeleted && exists {
			t.Errorf("%s: row survived the sweep, want it deleted%s", r.name, because(r.why))
		}
		if !r.wantDeleted && !exists {
			t.Errorf("%s: row was DELETED, want it kept%s", r.name, because(r.why))
		}
	}
}

func because(why string) string {
	if why == "" {
		return ""
	}
	return " — " + why
}

// TestCleanup_RetentionBoundary — the number is the contract. A row one minute
// past the window goes; a row one minute short of it stays.
func TestCleanup_RetentionBoundary(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "clbd")

	past := seedAgedRow(t, pool, f, 0, agedRow{
		name: "just past the window", state: "final", outcome: outcomeSuccess,
		finalisedAgo: PushRetention + time.Minute,
	})
	short := seedAgedRow(t, pool, f, 1, agedRow{
		name: "just short of the window", state: "final", outcome: outcomeSuccess,
		finalisedAgo: PushRetention - time.Minute,
	})

	runner := NewCleanupRunner(pool, CleanupOptions{BatchSize: 100, Retention: PushRetention})
	if _, err := runner.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if rowExists(t, pool, past) {
		t.Errorf("a row finalised %v ago survived a %v window (SC-025)", PushRetention+time.Minute, PushRetention)
	}
	if !rowExists(t, pool, short) {
		t.Errorf("a row finalised %v ago was deleted by a %v window — the boundary is off",
			PushRetention-time.Minute, PushRetention)
	}
}

// TestCleanup_BoundedBatchesStillDrainEverything — the batch bound must limit
// one statement, not the sweep. A bound that also caps the sweep would leave a
// permanent remainder after any outage, which is the accumulation the feature
// removes.
func TestCleanup_BoundedBatchesStillDrainEverything(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "clbb")

	const eligible = 7
	ids := make([]int64, 0, eligible)
	for i := range eligible {
		ids = append(ids, seedAgedRow(t, pool, f, i, agedRow{
			name: fmt.Sprintf("aged-%d", i), state: "final", outcome: outcomeSuccess,
			finalisedAgo: 40 * 24 * time.Hour,
		}))
	}

	// A batch size far below the backlog: the sweep must loop.
	runner := NewCleanupRunner(pool, CleanupOptions{BatchSize: 2, Retention: PushRetention})
	n, err := runner.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n < eligible {
		t.Errorf("swept %d rows with a batch size of 2, want at least the %d eligible ones", n, eligible)
	}
	for i, id := range ids {
		if rowExists(t, pool, id) {
			t.Errorf("row %d survived a bounded sweep — the batch bound must limit the STATEMENT, not the sweep", i)
		}
	}
}

// TestCleanup_ConcurrentSweepsAreSafe — two replicas sweep at once. Neither
// errors, and the rows are gone exactly once: DELETE is idempotent on a row
// somebody else already removed, which is why no lease or claim is needed here.
func TestCleanup_ConcurrentSweepsAreSafe(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "clcc")

	ids := make([]int64, 0, 6)
	for i := range 6 {
		ids = append(ids, seedAgedRow(t, pool, f, i, agedRow{
			name: fmt.Sprintf("aged-%d", i), state: "final", outcome: outcomeNoDevice,
			finalisedAgo: 40 * 24 * time.Hour,
		}))
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runner := NewCleanupRunner(pool, CleanupOptions{BatchSize: 2, Retention: PushRetention})
			if _, err := runner.Sweep(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent sweep failed: %v", err)
	}
	for i, id := range ids {
		if rowExists(t, pool, id) {
			t.Errorf("row %d survived two concurrent sweeps", i)
		}
	}
}

// TestCleanup_RunStopsOnContext — housekeeping must never hold up shutdown.
func TestCleanup_RunStopsOnContext(t *testing.T) {
	pool := newTestPool(t)
	ctx, stop := context.WithCancel(context.Background())

	runner := NewCleanupRunner(pool, CleanupOptions{
		Interval: 20 * time.Millisecond, BatchSize: 10, Retention: PushRetention,
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = runner.Run(ctx) }()

	// Let it sweep at least once, then stop it.
	time.Sleep(60 * time.Millisecond)
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the cleanup runner did not stop on its context — housekeeping is holding up shutdown")
	}
}

// TestCleanup_LeavesOtherRecipientsAlone — the sweep is global, so the last
// thing to pin is that it keys on the ROW's state and nothing else: a second
// recipient's fresh rows are untouched by a sweep triggered by the first.
func TestCleanup_LeavesOtherRecipientsAlone(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	aged := newPushFixture(t, pool, "clo1")
	fresh := newPushFixture(t, pool, "clo2")

	agedID := seedAgedRow(t, pool, aged, 0, agedRow{
		name: "aged", state: "final", outcome: outcomeSuccess, finalisedAgo: 40 * 24 * time.Hour,
	})
	keptID := fresh.owed(t, pushRow{feedChannel: true})

	runner := NewCleanupRunner(pool, CleanupOptions{BatchSize: 100, Retention: PushRetention})
	if _, err := runner.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rowExists(t, pool, agedID) {
		t.Error("the aged invisible row survived")
	}
	if !rowExists(t, pool, keptID) {
		t.Error("another recipient's live row was swept")
	}
}
