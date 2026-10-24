package lockreap

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
)

// sk-243 marked one claim explicitly UNVERIFIED: "we did not check whether
// skein_claim returns success to another agent on these in_progress-without-
// lock issues — we did not want to run the experiment on a live run".
//
// This test runs it, and the answer is two-sided:
//
//   - ANOTHER SESSION OF THE SAME USER takes the task. That is the hijack
//     vector, and it is the realistic one: when several agents run in parallel every
//     agent authenticates with the SAME api key, so "another
//     agent" and "the same user" are the same thing. Nothing in the claim
//     predicate distinguishes them once the lock is gone — assignee still
//     names that user, and the lock is what "somebody is on it" was
//     supposed to mean.
//   - A DIFFERENT USER is refused with already_claimed / assignee_vs_actor,
//     because the assignee survives the reap even though the lock does not.
//
// So the exposure the ticket suspected is real but is bounded by the
// assignee column. The primary fix (do not reap a live holder) is what
// closes it; this test is the regression pin that says what the state
// MEANS once a lock genuinely does expire.
func TestClaim_AfterStaleRelease_SameUserOtherSessionTakesIt(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	stranger := seedUser(t, pool)
	strangerName := readUsername(t, pool, stranger)
	pid, prefix := seedProject(t, pool, owner)
	ctx := context.Background()

	store := issues.NewStore(pool)
	id := prefix + "-1"
	seedOpenIssue(t, pool, pid, owner, id)

	// The holder claims it for real: status → in_progress, assignee → owner,
	// locked_by → its session.
	if _, err := store.Claim(ctx, issues.ClaimInput{
		ID:        id,
		SessionID: "mcp-session-holder",
		Actor:     issues.HumanActor(owner, ownerName),
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	// The holder then genuinely disappears: no agents row, lock aged past
	// the threshold. This is the state the reaper is FOR.
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET locked_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
		t.Fatalf("age the lock: %v", err)
	}
	newReaper(pool).Tick(ctx)

	var status string
	var locked *string
	if err := pool.QueryRow(ctx,
		`SELECT status, locked_by FROM issues WHERE id = $1`, id).Scan(&status, &locked); err != nil {
		t.Fatalf("read post-reap state: %v", err)
	}
	if status != "in_progress" || locked != nil {
		t.Fatalf("post-reap state = (%s, %v), want (in_progress, NULL)", status, locked)
	}

	// (1) Another session of the SAME user takes it — no error, and the
	// lock is now theirs.
	if _, err := store.Claim(ctx, issues.ClaimInput{
		ID:        id,
		SessionID: "mcp-session-interloper",
		Actor:     issues.HumanActor(owner, ownerName),
	}); err != nil {
		t.Fatalf("second session of the same user was refused (%v) — if this ever "+
			"becomes the behaviour, sk-243's hijack note needs rewriting, not this test", err)
	}
	if got := lockedBy(t, pool, id); got == nil || *got != "mcp-session-interloper" {
		t.Fatalf("locked_by = %v, want the interloper's session", got)
	}

	// (2) A different USER is still refused: the assignee outlives the lock.
	_, err := store.Claim(ctx, issues.ClaimInput{
		ID:        id,
		SessionID: "mcp-session-stranger",
		Actor:     issues.HumanActor(stranger, strangerName),
	})
	var ac *issues.AlreadyClaimedError
	if !errors.As(err, &ac) {
		t.Fatalf("a different user's claim returned %v, want AlreadyClaimedError", err)
	}
}

func readUsername(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("read username: %v", err)
	}
	return name
}

func seedOpenIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, id string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, version, title, description, type, priority,
                    status, pinned, source, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, 1, $3, '', 'task', 2, 'open', false, 'human', $4, now(), now(), '{}'::jsonb)`,
		id, projectID, "claimable "+id, creator)
	if err != nil {
		t.Fatalf("seed open issue %s: %v", id, err)
	}
}
