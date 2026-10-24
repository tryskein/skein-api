package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// cleanup_smoke_test.go — sk-303.13 / T013: one sweep, end to end.
//
// The full retention class (coverage-matrix row 23) is a separate task; what this proves is
// that the runner as WIRED does the one thing it exists for — an invisible
// push-only row whose delivery ended long ago is gone, and the row next to it
// that is still owed is not.
func TestCleanupRunner_SweepRemovesExpiredInvisibleRows(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	f := newPushFixture(t, pool, "clsm")

	// Two rows for the same recipient need two events: (user_id, event_id) is
	// unique, which is the invariant that keeps one action from producing two
	// notifications.
	expired := f.owed(t, pushRow{feedChannel: false})
	secondEvent := seedEvent(t, pool, "issue.status_changed", "issue", f.issueID, f.projectID)
	stillOwed := insertPushNotification(t, pool, f.user, secondEvent, "issue.status_changed", f.projectID,
		pushRow{reason: "delegated_status", feedChannel: false, deadline: time.Hour})

	// The first row's delivery ended 31 days ago.
	if _, err := pool.Exec(ctx, `
UPDATE notifications
   SET push_state = 'final', push_outcome = $2, push_finalized_at = now() - interval '31 days',
       push_next_attempt_at = NULL
 WHERE id = $1`, expired, outcomeNoDevice); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	runner := NewCleanupRunner(pool, CleanupOptions{BatchSize: 100, Retention: 30 * 24 * time.Hour})
	n, err := runner.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n < 1 {
		t.Fatalf("swept %d rows, want at least the expired one", n)
	}

	if rowExists(t, pool, expired) {
		t.Error("an invisible push-only row finalised 31 days ago survived the sweep (SC-025)")
	}
	if !rowExists(t, pool, stillOwed) {
		t.Error("a row that is still owed a push was deleted — the sweep must only take finished ones")
	}
}

// rowExists reports whether a notification row is still there.
func rowExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM notifications WHERE id = $1)`, id).Scan(&ok); err != nil {
		t.Fatalf("check row: %v", err)
	}
	return ok
}
