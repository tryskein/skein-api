package lockreap

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const testDatabaseURL = "TEST_DATABASE_URL"

// newTestPool returns a *pgxpool.Pool against the URL in
// TEST_DATABASE_URL, or skips the test when the env var is unset.
// Mirrors the helper used by sibling packages (attachments,
// mcp/tools).
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
	uname := "lru" + strings.ReplaceAll(id.String()[:18], "-", "")
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
	name := "lrtest_" + suffix
	prefix := "lr" + suffix[:6]
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
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, id)
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

// seedLockedIssue creates an issue row that is already lock-held by the
// supplied session of the creator — locked_by_user_id is written, as every
// claim writes it since sk-424 (the reaper judges the holder by that pair,
// K-27). lockAge is how far in the past locked_at should land
// (positive value subtracted from now()).
func seedLockedIssue(
	t *testing.T,
	pool *pgxpool.Pool,
	projectID, creator uuid.UUID,
	id, sessionID string,
	lockAge time.Duration,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee, locked_by, locked_by_user_id, locked_at,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, 'human', NULL, NULL,
        $4, $6, now() - $5::interval,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $6, now(), now(), '{}'::jsonb)`,
		id, projectID, "locked issue "+id, sessionID, lockAge.String(), creator)
	if err != nil {
		t.Fatalf("seed locked issue %s: %v", id, err)
	}
}

// TestReaper_ReleasesStaleLocksAndEmitsEvents drives the reaper end-to-end:
// seed 5 locked issues with locked_at in the past, run one tick, and
// verify all 5 are unlocked + each got one issue.unclaimed event.
func TestReaper_ReleasesStaleLocksAndEmitsEvents(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	const total = 5
	ids := make([]string, total)
	for i := 0; i < total; i++ {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		// 2-hour-old locks — past any sensible threshold.
		seedLockedIssue(t, pool, pid, owner, ids[i], "sess-stuck", 2*time.Hour)
	}

	r := New(pool, Config{
		Interval:  time.Minute, // unused by direct Tick()
		Threshold: 30 * time.Minute,
		BatchSize: 200,
	})
	r.Tick(ctx)

	for _, id := range ids {
		var lockedBy *string
		var version int
		err := pool.QueryRow(ctx,
			`SELECT locked_by, version FROM issues WHERE id = $1`, id,
		).Scan(&lockedBy, &version)
		if err != nil {
			t.Fatalf("read released issue %s: %v", id, err)
		}
		if lockedBy != nil {
			t.Fatalf("issue %s: locked_by = %v, want NULL", id, *lockedBy)
		}
		if version != 2 {
			t.Fatalf("issue %s: version = %d, want 2 (was 1)", id, version)
		}

		var nUnclaimed int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events
 WHERE entity_type = 'issue'
   AND entity_id   = $1
   AND type        = 'issue.unclaimed'
   AND reason      = 'auto-released-stale-lock'
   AND actor_type  = 'system'`, id).Scan(&nUnclaimed); err != nil {
			t.Fatalf("query event count for %s: %v", id, err)
		}
		if nUnclaimed != 1 {
			t.Fatalf("issue %s: expected 1 issue.unclaimed event, got %d",
				id, nUnclaimed)
		}
	}
}

// TestReaper_LeavesFreshLocksAlone verifies the threshold is honoured —
// a lock acquired 5 minutes ago must NOT be reaped at threshold=30m.
func TestReaper_LeavesFreshLocksAlone(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, "sess-fresh", 5*time.Minute)

	r := New(pool, Config{
		Interval:  time.Minute,
		Threshold: 30 * time.Minute,
		BatchSize: 200,
	})
	r.Tick(ctx)

	var lockedBy *string
	if err := pool.QueryRow(ctx,
		`SELECT locked_by FROM issues WHERE id = $1`, id,
	).Scan(&lockedBy); err != nil {
		t.Fatalf("read fresh-locked issue: %v", err)
	}
	if lockedBy == nil || *lockedBy != "sess-fresh" {
		t.Fatalf("fresh lock got reaped: locked_by = %v", lockedBy)
	}

	var nUnclaimed int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.unclaimed'`,
		id).Scan(&nUnclaimed); err != nil {
		t.Fatalf("query event count: %v", err)
	}
	if nUnclaimed != 0 {
		t.Fatalf("expected 0 events on fresh lock, got %d", nUnclaimed)
	}
}

// TestReaper_NoOpWhenNothingStale exercises the empty-RETURNING branch:
// no locked issues at all → no errors, no events, no metric increment
// at the integration level.
func TestReaper_NoOpWhenNothingStale(t *testing.T) {
	pool := newTestPool(t)
	r := New(pool, Config{
		Interval:  time.Minute,
		Threshold: 30 * time.Minute,
		BatchSize: 200,
	})
	r.Tick(context.Background()) // must not panic
}

// TestReaper_HolderIsTheUserSessionPair — sk-674.18, row 146 (T034, K-27).
// Reference — K-27 as the plan words it, written before the code: the
// liveness of a lock's holder is the PAIR (user, session) — the agents key —
// not the session string alone. Two locks of the same staleness:
//
//   - the holder's own session is alive (agents row of his user and that
//     session) — the lock stays;
//   - the holder has no live session, but a live agents row with the SAME
//     session string belongs to another user — that is not his session, so
//     the lock goes.
//
// Red if liveness is read by session_id alone (the second lock would stay)
// and if every holder is treated as dead (the first lock would go).
func TestReaper_HolderIsTheUserSessionPair(t *testing.T) {
	pool := newTestPool(t)
	holder := seedUser(t, pool)
	impostor := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, holder)
	ctx := context.Background()

	const ownSession = "k27-own-session"
	const sharedSession = "k27-shared-session"
	seedAgent(t, pool, holder, ownSession, 30*time.Second)
	// The live session string of the second lock belongs to somebody else.
	seedAgent(t, pool, impostor, sharedSession, 30*time.Second)

	alive := prefix + "-1"
	seedLockedIssue(t, pool, pid, holder, alive, ownSession, 2*time.Hour)
	stolen := prefix + "-2"
	other := seedUser(t, pool) // the holder of the second lock, with no agents row
	seedLockedIssue(t, pool, pid, other, stolen, sharedSession, 2*time.Hour)

	if _, _, err := newReaper(pool).reapOnce(ctx); err != nil {
		t.Fatalf("reapOnce: %v", err)
	}
	if got := lockedBy(t, pool, alive); got == nil || *got != ownSession {
		t.Errorf("the lock of a live (user, session) pair was released (locked_by %v)", got)
	}
	if got := lockedBy(t, pool, stolen); got != nil {
		t.Errorf("a lock whose session string is alive only for ANOTHER user was kept (locked_by %q)", *got)
	}
}
