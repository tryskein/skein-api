package notifications

// issue_create_prefs_test.go — sk-565, T025, test matrix rows 3–6 (INV-8,
// INV-10, SC-008). Reference — the sk-565 spec: "A missing row of personal
// settings = the setting is on"; "Reading a setting does not create a row.
// A write changes only this flag … Parallel writes of different personal
// settings keep both. Of two parallel writes of the flag itself
// the last committed one wins".
//
// HOW THE OVERLAP IS GUARANTEED (rows 5, 6b). A lost update shows only when
// both writes have started and read the row before either commits; run one
// after the other, a read-modify-write is green. So a third transaction
// takes FOR UPDATE on the row first, both writes are started, and the test
// waits until the database reports BOTH backends blocked (directly or
// through the other waiter) by that transaction (pg_blocking_pids) before
// releasing it. Whatever a write reads before it reaches the row lock, it
// has read by then. Postgres hands
// a contended row lock to waiters in arrival order, so starting the two
// writes one at a time — each confirmed blocked before the next starts —
// fixes their commit order.

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func countIssueCreatePrefsRows(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_notification_prefs WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatalf("count prefs rows: %v", err)
	}
	return n
}

func getAssignFlag(t *testing.T, store *Store, user uuid.UUID) bool {
	t.Helper()
	dto, err := store.GetIssueCreatePrefs(context.Background(), user)
	if err != nil {
		t.Fatalf("GetIssueCreatePrefs: %v", err)
	}
	return dto.AssignAgentIssuesToMe
}

func updateAssignFlag(t *testing.T, store *Store, user uuid.UUID, on bool) IssueCreatePrefsDTO {
	t.Helper()
	dto, err := store.UpdateIssueCreatePrefs(context.Background(), user,
		UpdateIssueCreatePrefsInput{AssignAgentIssuesToMe: &on})
	if err != nil {
		t.Fatalf("UpdateIssueCreatePrefs(%v): %v", on, err)
	}
	return dto
}

// otherPersonalSettings is what the other personal-settings readers answer
// — the effective values the flag write must not move.
type otherPersonalSettings struct {
	review  ReviewPrefsDTO
	reasons ReasonPrefsDTO
}

func readOtherPersonalSettings(t *testing.T, store *Store, user uuid.UUID) otherPersonalSettings {
	t.Helper()
	ctx := context.Background()
	rv, err := store.GetReviewPrefs(ctx, user)
	if err != nil {
		t.Fatalf("GetReviewPrefs: %v", err)
	}
	// ReasonPrefsDTO carries the reason×channel table AND the two flags
	// suppress_agent_actions and unwatch_on_close.
	rs, err := store.GetReasonPrefs(ctx, user)
	if err != nil {
		t.Fatalf("GetReasonPrefs: %v", err)
	}
	return otherPersonalSettings{review: rv, reasons: rs}
}

// lockPrefsRow takes FOR UPDATE on the user's prefs row in a transaction of
// its own and returns that transaction and its backend pid.
func lockPrefsRow(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) (pgx.Tx, int32) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}
	var one int
	if err := tx.QueryRow(ctx,
		`SELECT 1 FROM user_notification_prefs WHERE user_id = $1 FOR UPDATE`, user).Scan(&one); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}
	return tx, pid
}

// waitBlockedBy waits until exactly n backends wait, directly or through
// another waiter, on the backend pid. Transitively, because the second
// writer on a row waits for the first one's tuple lock, not for the holder
// of the row. It is a wait for a database condition, not a timing
// assumption; the deadline only turns a hang into a failure.
func waitBlockedBy(t *testing.T, pool *pgxpool.Pool, pid int32, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked int
		if err := pool.QueryRow(ctx, `
WITH RECURSIVE chain(pid) AS (
    SELECT $1::int
    UNION
    SELECT a.pid FROM pg_stat_activity a, chain c
     WHERE c.pid = ANY(pg_blocking_pids(a.pid))
)
SELECT count(*) - 1 FROM chain`,
			pid).Scan(&blocked); err != nil {
			t.Fatalf("count blocked backends: %v", err)
		}
		if blocked == n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d backends blocked by %d: last seen %d", n, pid, blocked)
		case <-tick.C:
		}
	}
}

// TestIssueCreatePrefs_ReadWithoutRowIsTrueAndCreatesNoRow — row 3.
func TestIssueCreatePrefs_ReadWithoutRowIsTrueAndCreatesNoRow(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	user := seedTestUser(t, pool, "user")

	if !getAssignFlag(t, store, user) {
		t.Errorf("GetIssueCreatePrefs without a prefs row = false, want true")
	}
	if n := countIssueCreatePrefsRows(t, pool, user); n != 0 {
		t.Errorf("after GetIssueCreatePrefs the user has %d prefs rows, want 0", n)
	}
}

// TestIssueCreatePrefs_WriteChangesOnlyTheFlag — row 4.
func TestIssueCreatePrefs_WriteChangesOnlyTheFlag(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	t.Run("no row before the write", func(t *testing.T) {
		user := seedTestUser(t, pool, "user")
		before := readOtherPersonalSettings(t, store, user)

		if got := updateAssignFlag(t, store, user, false); got.AssignAgentIssuesToMe {
			t.Errorf("UpdateIssueCreatePrefs(false) answered true")
		}
		if getAssignFlag(t, store, user) {
			t.Errorf("after Update(false) the flag reads true")
		}
		if after := readOtherPersonalSettings(t, store, user); !reflect.DeepEqual(before, after) {
			t.Errorf("the first flag write moved other settings:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("row with non-factory neighbours", func(t *testing.T) {
		user := seedTestUser(t, pool, "user")
		off, on := false, true
		if _, err := store.UpdateReviewPrefs(ctx, user, UpdateReviewPrefsInput{
			MechanicEnabled: &off, ReviewRequiredDefault: &off,
		}); err != nil {
			t.Fatalf("seed review prefs: %v", err)
		}
		if _, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{
			SuppressAgentActions: &off, UnwatchOnClose: &on,
		}); err != nil {
			t.Fatalf("seed reason prefs: %v", err)
		}
		before := readOtherPersonalSettings(t, store, user)
		if before.review.MechanicEnabled || before.review.ReviewRequiredDefault ||
			before.reasons.SuppressAgentActions || !before.reasons.UnwatchOnClose {
			t.Fatalf("seeded neighbours did not stick: %+v", before)
		}

		updateAssignFlag(t, store, user, false)
		if after := readOtherPersonalSettings(t, store, user); !reflect.DeepEqual(before, after) {
			t.Errorf("Update(false) moved other settings:\nbefore %+v\nafter  %+v", before, after)
		}
		updateAssignFlag(t, store, user, true)
		if after := readOtherPersonalSettings(t, store, user); !reflect.DeepEqual(before, after) {
			t.Errorf("Update(true) moved other settings:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}

// TestIssueCreatePrefs_OverlappingWritesOfDifferentSettingsKeepBoth — row 5.
//
// The commit order is fixed: the review-prefs write is queued on the held row
// first and the flag write second, so the flag write commits LAST. That is
// the order in which a read-modify-write of the flag (the whole row read
// before the lock, then written back) overwrites the review write committed
// just before it — so that defect turns this test red in every run, not only
// when the scheduler happens to let the flag write commit second.
func TestIssueCreatePrefs_OverlappingWritesOfDifferentSettingsKeepBoth(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	user := seedTestUser(t, pool, "user")
	// The row exists, so there is something to hold and to read.
	if _, err := pool.Exec(ctx, `INSERT INTO user_notification_prefs (user_id) VALUES ($1)`, user); err != nil {
		t.Fatalf("seed prefs row: %v", err)
	}

	blocker, pid := lockPrefsRow(t, pool, user)

	off := false
	var (
		wg                 sync.WaitGroup
		flagErr, reviewErr error
	)
	// Queue order is commit order (as in row 6): review first, flag last.
	wg.Go(func() {
		_, reviewErr = store.UpdateReviewPrefs(ctx, user,
			UpdateReviewPrefsInput{ReviewRequiredDefault: &off})
	})
	waitBlockedBy(t, pool, pid, 1)
	wg.Go(func() {
		_, flagErr = store.UpdateIssueCreatePrefs(ctx, user,
			UpdateIssueCreatePrefsInput{AssignAgentIssuesToMe: &off})
	})
	waitBlockedBy(t, pool, pid, 2)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	wg.Wait()
	if flagErr != nil {
		t.Fatalf("flag write: %v", flagErr)
	}
	if reviewErr != nil {
		t.Fatalf("review-prefs write: %v", reviewErr)
	}

	if getAssignFlag(t, store, user) {
		t.Errorf("after overlapping writes the flag reads true, want false — the flag write was lost")
	}
	rv, err := store.GetReviewPrefs(ctx, user)
	if err != nil {
		t.Fatalf("GetReviewPrefs: %v", err)
	}
	if rv.ReviewRequiredDefault {
		t.Errorf("after overlapping writes reviewRequiredDefault reads true, want false — the review write was lost")
	}
}

// TestIssueCreatePrefs_LastCommittedFlagWriteWins — row 6.
func TestIssueCreatePrefs_LastCommittedFlagWriteWins(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	t.Run("sequential writes from no row", func(t *testing.T) {
		user := seedTestUser(t, pool, "user")
		updateAssignFlag(t, store, user, false)
		updateAssignFlag(t, store, user, true)
		if !getAssignFlag(t, store, user) {
			t.Errorf("Update(false) then Update(true) reads false, want true")
		}
	})

	for _, tc := range []struct {
		name          string
		first, second bool
	}{
		{"false commits first, true last", false, true},
		{"true commits first, false last", true, false},
	} {
		t.Run("overlapping: "+tc.name, func(t *testing.T) {
			user := seedTestUser(t, pool, "user")
			if _, err := pool.Exec(ctx, `INSERT INTO user_notification_prefs (user_id) VALUES ($1)`, user); err != nil {
				t.Fatalf("seed prefs row: %v", err)
			}
			blocker, pid := lockPrefsRow(t, pool, user)

			var (
				wg   sync.WaitGroup
				errs [2]error
			)
			for i, v := range []bool{tc.first, tc.second} {
				wg.Go(func() {
					_, errs[i] = store.UpdateIssueCreatePrefs(ctx, user,
						UpdateIssueCreatePrefsInput{AssignAgentIssuesToMe: &v})
				})
				// Each write is queued on the row before the next starts:
				// queue order is commit order.
				waitBlockedBy(t, pool, pid, i+1)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatalf("release blocker: %v", err)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("write #%d: %v", i, err)
				}
			}
			if got := getAssignFlag(t, store, user); got != tc.second {
				t.Errorf("flag = %v, want %v (the value committed last)", got, tc.second)
			}
		})
	}
}
