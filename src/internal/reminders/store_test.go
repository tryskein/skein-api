package reminders

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const testDatabaseURL = "TEST_DATABASE_URL"

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "rmu" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := "rmtest_" + suffix
	prefix := "rm" + suffix[:6]
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err = pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, id, owner)
	if err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0) ON CONFLICT (project_id) DO NOTHING`, id)
	if err != nil {
		t.Fatalf("seed project counter: %v", err)
	}
	t.Cleanup(func() {
		// issues.project_id is ON DELETE RESTRICT (migration 000004), so
		// delete issues leaf-first (children before parents) before the
		// project, else the project DELETE silently fails and rows leak.
		for range 64 {
			ct, err := pool.Exec(context.Background(), `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (SELECT parent_id FROM issues WHERE project_id = $1 AND parent_id IS NOT NULL)`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id, prefix
}

func seedIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		idStr, projectID, title, creator)
	if err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

func testActor(uid uuid.UUID) Actor {
	return Actor{UserID: uid, Username: "tester", Role: "human"}
}

func TestStore_Create_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "remind me")

	store := NewStore(pool)
	at := time.Now().Add(time.Hour).UTC()
	rem, err := store.Create(context.Background(), iid, owner, at, "ping me", testActor(owner))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rem.IssueID != iid {
		t.Fatalf("issueID = %s, want %s", rem.IssueID, iid)
	}
	if rem.State != StateArmed {
		t.Fatalf("state = %s, want armed", rem.State)
	}
	if rem.SnoozeCount != 0 {
		t.Fatalf("snoozeCount = %d, want 0", rem.SnoozeCount)
	}
	if rem.NotifiedAt != nil {
		t.Fatalf("notifiedAt = %v, want nil", rem.NotifiedAt)
	}
	if rem.IssueTitle != "remind me" {
		t.Fatalf("issueTitle = %q, want %q", rem.IssueTitle, "remind me")
	}
	if rem.ProjectID != pid {
		t.Fatalf("projectID = %s, want %s", rem.ProjectID, pid)
	}
	// reminder.set event written.
	assertEventExists(t, pool, iid, EventReminderSet)
}

func TestStore_Create_IssueNotFound(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	store := NewStore(pool)
	_, err := store.Create(context.Background(), "no-such-issue-zzz", owner,
		time.Now().Add(time.Hour), "", testActor(owner))
	if err != ErrIssueNotFound {
		t.Fatalf("err = %v, want ErrIssueNotFound", err)
	}
}

func TestStore_Snooze_BumpsAndResetsNotified(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "t")
	store := NewStore(pool)

	// Create a due reminder and manually stamp notified_at (simulating a
	// prior ring) so we can assert snooze resets it.
	past := time.Now().Add(-time.Hour).UTC()
	rem, err := store.Create(context.Background(), iid, owner, past, "", testActor(owner))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rem.State != StateRinging {
		t.Fatalf("state = %s, want ringing", rem.State)
	}
	_, err = pool.Exec(context.Background(),
		`UPDATE reminders SET notified_at = now() WHERE id = $1`, rem.ID)
	if err != nil {
		t.Fatalf("stamp notified_at: %v", err)
	}

	newAt := time.Now().Add(3 * 24 * time.Hour).UTC()
	updated, err := store.Snooze(context.Background(), rem.ID, owner, newAt, "later", testActor(owner))
	if err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	if updated.SnoozeCount != 1 {
		t.Fatalf("snoozeCount = %d, want 1", updated.SnoozeCount)
	}
	if updated.NotifiedAt != nil {
		t.Fatalf("notifiedAt = %v, want nil after snooze", updated.NotifiedAt)
	}
	if updated.State != StateArmed {
		t.Fatalf("state = %s, want armed after snooze into future", updated.State)
	}
	if updated.RemindAt.Sub(newAt).Abs() > time.Second {
		t.Fatalf("remindAt = %v, want ~%v", updated.RemindAt, newAt)
	}
	if updated.LastReason == nil || *updated.LastReason != "later" {
		t.Fatalf("lastReason = %v, want \"later\"", updated.LastReason)
	}
	assertEventExists(t, pool, iid, EventReminderSnoozed)
}

func TestStore_Snooze_NotOwner(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	other := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "t")
	store := NewStore(pool)

	rem, err := store.Create(context.Background(), iid, owner, time.Now().Add(time.Hour), "", testActor(owner))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = store.Snooze(context.Background(), rem.ID, other,
		time.Now().Add(2*time.Hour), "", testActor(other))
	if err != ErrNotFound {
		t.Fatalf("snooze by non-owner: err = %v, want ErrNotFound", err)
	}
}

func TestStore_Dismiss_Deletes(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "t")
	store := NewStore(pool)

	rem, err := store.Create(context.Background(), iid, owner, time.Now().Add(time.Hour), "", testActor(owner))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Dismiss(context.Background(), rem.ID, owner, "done", testActor(owner)); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	// Row gone.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reminders WHERE id = $1`, rem.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("reminder row still present after dismiss")
	}
	// Second dismiss is a not-found (already deleted).
	if err := store.Dismiss(context.Background(), rem.ID, owner, "", testActor(owner)); err != ErrNotFound {
		t.Fatalf("re-dismiss: err = %v, want ErrNotFound", err)
	}
	assertEventExists(t, pool, iid, EventReminderDismissed)
}

func TestStore_ListForUser_Ordering(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	now := time.Now().UTC()

	// Two ringing (past), two armed (future). Insert out of order.
	mk := func(suffix string, at time.Time) uuid.UUID {
		iid := prefix + "-" + suffix
		seedIssue(t, pool, pid, owner, iid, "t"+suffix)
		r, err := store.Create(context.Background(), iid, owner, at, "", testActor(owner))
		if err != nil {
			t.Fatalf("Create %s: %v", suffix, err)
		}
		return r.ID
	}
	armedFar := mk("1", now.Add(48*time.Hour))
	ringOld := mk("2", now.Add(-2*time.Hour))
	armedSoon := mk("3", now.Add(1*time.Hour))
	ringNew := mk("4", now.Add(-10*time.Minute))

	all, err := store.ListForUser(context.Background(), owner, FilterAll, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListForUser all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d reminders, want 4", len(all))
	}
	// Expected order: ringOld, ringNew (ringing, remind_at ASC), then
	// armedSoon, armedFar (armed, remind_at ASC).
	wantOrder := []uuid.UUID{ringOld, ringNew, armedSoon, armedFar}
	for i, want := range wantOrder {
		if all[i].ID != want {
			t.Fatalf("order[%d] = %s, want %s", i, all[i].ID, want)
		}
	}
	if all[0].State != StateRinging || all[3].State != StateArmed {
		t.Fatalf("state derivation wrong: [0]=%s [3]=%s", all[0].State, all[3].State)
	}

	// Filtered lists.
	ringing, err := store.ListForUser(context.Background(), owner, FilterRinging, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListForUser ringing: %v", err)
	}
	if len(ringing) != 2 {
		t.Fatalf("ringing count = %d, want 2", len(ringing))
	}
	armed, err := store.ListForUser(context.Background(), owner, FilterArmed, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListForUser armed: %v", err)
	}
	if len(armed) != 2 {
		t.Fatalf("armed count = %d, want 2", len(armed))
	}
}

func TestStore_CountRingingTasks_DistinctByIssue(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	now := time.Now().UTC()

	// Two ringing reminders on the SAME issue → counts as one ringing task.
	iid1 := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid1, "a")
	if _, err := store.Create(context.Background(), iid1, owner, now.Add(-time.Hour), "", testActor(owner)); err != nil {
		t.Fatalf("create 1a: %v", err)
	}
	if _, err := store.Create(context.Background(), iid1, owner, now.Add(-30*time.Minute), "", testActor(owner)); err != nil {
		t.Fatalf("create 1b: %v", err)
	}
	// One ringing reminder on a second issue.
	iid2 := prefix + "-2"
	seedIssue(t, pool, pid, owner, iid2, "b")
	if _, err := store.Create(context.Background(), iid2, owner, now.Add(-5*time.Minute), "", testActor(owner)); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	// One ARMED reminder (future) — must not count.
	iid3 := prefix + "-3"
	seedIssue(t, pool, pid, owner, iid3, "c")
	if _, err := store.Create(context.Background(), iid3, owner, now.Add(time.Hour), "", testActor(owner)); err != nil {
		t.Fatalf("create 3: %v", err)
	}

	count, err := store.CountRingingTasks(context.Background(), owner, time.Now().UTC())
	if err != nil {
		t.Fatalf("CountRingingTasks: %v", err)
	}
	if count != 2 {
		t.Fatalf("ringing task count = %d, want 2 (distinct issues)", count)
	}
}

// assertEventExists fails the test if no event of the given type for the
// issue exists in the events table.
func assertEventExists(t *testing.T, pool *pgxpool.Pool, issueID, eventType string) {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE entity_type = 'issue' AND entity_id = $1 AND type = $2`,
		issueID, eventType).Scan(&n)
	if err != nil {
		t.Fatalf("query event %s: %v", eventType, err)
	}
	if n == 0 {
		t.Fatalf("expected at least one %s event for issue %s, found none", eventType, issueID)
	}
}
