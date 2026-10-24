package lockreap

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-243 — the reaper's definition of "stale" was "nothing was written to
// THIS ISSUE for 30 minutes", which in a multi-agent run is true of every
// head task by construction: the agent writes milestones into the
// channel task, updates into the children, and commits into git, and is
// not supposed to write to the head task at all. Two head tasks were
// observed reaped at 21:21 and 21:24 while both holders were mid-run, which
// manufactures exactly the "in_progress with no holder" zombie the atomic
// unclaim (sk-19) exists to prevent.
//
// The timer is aimed at agents that FELL OFF, so the signal of life has to
// be the agent, not the issue. `agents(user_id, session_id, last_seen_at)`
// already records it: every authenticated /mcp request that carries an
// Mcp-Session-Id upserts the row (internal/mcp/server.go, wrapAgentRegistry),
// last_seen_at bumped at 1-minute granularity. The lock holder IS a session
// id, so the join is direct — and migration 000010 anticipated it in so many
// words ("last_seen_at index supports stale-lock auto-release queries").

// seedAgent records a session as last seen `ago` in the past.
func seedAgent(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, sessionID string, ago time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at)
VALUES ($1, $2, now() - $3::interval, now() - $3::interval)`,
		userID, sessionID, ago.String())
	if err != nil {
		t.Fatalf("seed agent %s: %v", sessionID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM agents WHERE user_id = $1 AND session_id = $2`, userID, sessionID)
	})
}

func lockedBy(t *testing.T, pool *pgxpool.Pool, id string) *string {
	t.Helper()
	var s *string
	if err := pool.QueryRow(context.Background(),
		`SELECT locked_by FROM issues WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read locked_by for %s: %v", id, err)
	}
	return s
}

// TestReaper_KeepsLockOfLiveAgent is the reported defect. The lock is two
// hours old — nothing was written to the issue — but the holding session
// called an MCP tool a minute ago. It is working; the lock stays.
func TestReaper_KeepsLockOfLiveAgent(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	const session = "mcp-session-live-agent"
	seedAgent(t, pool, owner, session, time.Minute)

	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, session, 2*time.Hour)

	newReaper(pool).Tick(context.Background())

	if got := lockedBy(t, pool, id); got == nil {
		t.Fatalf("lock released while its holder was active a minute ago (sk-243)")
	} else if *got != session {
		t.Fatalf("locked_by = %q, want %q", *got, session)
	}
}

// TestReaper_ReleasesLockOfSilentAgent is the case the timer is FOR: the
// session has not spoken in two hours, so the agent is gone.
func TestReaper_ReleasesLockOfSilentAgent(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	const session = "mcp-session-crashed-agent"
	seedAgent(t, pool, owner, session, 2*time.Hour)

	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, session, 2*time.Hour)

	newReaper(pool).Tick(context.Background())

	if got := lockedBy(t, pool, id); got != nil {
		t.Fatalf("locked_by = %q, want NULL — the holder has been silent for 2h", *got)
	}
}

// TestReaper_ReleasesLockOfUnknownSession pins the pre-existing behaviour
// for holders that have no agents row at all (a cookie-session claim, a
// row that predates the registry, a pruned agent). "No evidence of life" is
// not "alive" — the lock is still reaped on age alone, exactly as before.
func TestReaper_ReleasesLockOfUnknownSession(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, "session-with-no-agents-row", 2*time.Hour)

	newReaper(pool).Tick(context.Background())

	if got := lockedBy(t, pool, id); got != nil {
		t.Fatalf("locked_by = %q, want NULL — no agents row means no evidence of life", *got)
	}
}

// TestReaper_LivenessIsPerSession: two issues, two sessions, one alive and
// one gone. The reaper must split them, not treat "some agent of this user
// is alive" as evidence for another session's lock.
func TestReaper_LivenessIsPerSession(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedAgent(t, pool, owner, "mcp-session-alive", 30*time.Second)
	seedAgent(t, pool, owner, "mcp-session-gone", 3*time.Hour)

	alive, gone := prefix+"-1", prefix+"-2"
	seedLockedIssue(t, pool, pid, owner, alive, "mcp-session-alive", 2*time.Hour)
	seedLockedIssue(t, pool, pid, owner, gone, "mcp-session-gone", 2*time.Hour)

	newReaper(pool).Tick(context.Background())

	if got := lockedBy(t, pool, alive); got == nil {
		t.Errorf("the live session's lock was released")
	}
	if got := lockedBy(t, pool, gone); got != nil {
		t.Errorf("the gone session's lock survived: %q", *got)
	}
}

// TestReaper_EventNamesTheTimer: an auto-release must be distinguishable
// from a person's unclaim in the audit trail — "nobody holds this" and
// "somebody is working on it, the lock just expired" are different facts
// and were indistinguishable on the issue row alone.
func TestReaper_EventNamesTheTimer(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	const session = "mcp-session-marker"
	seedAgent(t, pool, owner, session, 90*time.Minute)
	id := prefix + "-1"
	seedLockedIssue(t, pool, pid, owner, id, session, 2*time.Hour)

	newReaper(pool).Tick(ctx)

	var actorType string
	var reason *string
	var releasedBy *string
	var holderLastSeen *time.Time
	err := pool.QueryRow(ctx, `
SELECT actor_type,
       reason,
       new_data ->> 'releasedBy',
       (new_data ->> 'holderLastSeenAt')::timestamptz
  FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.unclaimed'`,
		id).Scan(&actorType, &reason, &releasedBy, &holderLastSeen)
	if err != nil {
		t.Fatalf("read unclaim event: %v", err)
	}
	if actorType != "system" {
		t.Errorf("actor_type = %q, want system", actorType)
	}
	if reason == nil || *reason != ReleaseReasonStaleTimer {
		t.Errorf("reason = %v, want %q", reason, ReleaseReasonStaleTimer)
	}
	if releasedBy == nil || *releasedBy != ReleasedByStaleTimer {
		t.Errorf("new_data.releasedBy = %v, want %q", releasedBy, ReleasedByStaleTimer)
	}
	// The evidence for the judgement: when the holder was last heard from.
	// Without it the audit trail records a verdict and not its grounds.
	if holderLastSeen == nil {
		t.Errorf("new_data.holderLastSeenAt is absent — the release records no evidence")
	}
}

// newReaper builds the reaper with the production defaults the tests are
// written against: a 30-minute lock age and a 30-minute silence window.
func newReaper(pool *pgxpool.Pool) *Reaper {
	return New(pool, Config{
		Interval:  time.Minute, // unused by direct Tick()
		Threshold: 30 * time.Minute,
		AgentIdle: 30 * time.Minute,
		BatchSize: 200,
	})
}
