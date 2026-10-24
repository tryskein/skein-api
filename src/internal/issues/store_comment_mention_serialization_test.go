package issues

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// sk-303.17 — AddComment ran its INSERT + mention fan-out under
// SERIALIZABLE with plain pgx.BeginTxFunc and no isSerializationFailure
// fallback at all (unlike PatchIssue / ReplaceComponents / etc., which
// already degrade a class-40 abort to ErrVersionConflict). A genuine
// abort travelled straight to the caller as a raw pgError, which
// mapCommentWriteError / the MCP comment tool has no mapping for and
// answers with ErrCodeInternalError — the exact sk-305 defect shape.
//
// The contention is per-RECIPIENT, not per-issue: this test was built by
// measurement, not guesswork. A same-issue run where each concurrent
// comment @mentions a DISTINCT (or no) user showed ZERO serialization
// failures across repeated 24..40-way concurrent runs — the comment INSERT
// itself is conflict-free (each comment is its own new row). Only when
// every concurrent comment mentions the SAME user does contention become
// real: that shared recipient's mention/notification bookkeeping
// (emitCommentMentions / notifications.WriteMentionNotification) is what
// the concurrent transactions actually collide on. Pre-fix, a 40-way
// concurrent run with one shared mention target produced 3-4 raw 40001s
// per run across 3 runs; commentMentionMaxAttempts=3 was picked because
// this test is green at 3 across repeated runs, not copied from a
// neighbouring constant.
//
// sk-303.17 review — this N-way run is a genuinely
// PROBABILISTIC repro, unlike the deterministic single-competitor
// waitForBlocked technique TestSetPinned_SerializationFailure_Retries /
// TestUpdateComment_SerializationFailure_Retries /
// TestCreateEpicWithChildren_SerializationFailure_Retries use. That
// technique does NOT apply here, and it is not a gap left unfixed — it
// is inapplicable to this contention shape on its own terms:
// waitForBlocked works by planting ONE concurrent transaction that holds
// a real row lock on the exact row/predicate the target's own statement
// blocks on (an UPDATE ... WHERE, or a SELECT ... FOR UPDATE), then
// committing it at the moment the target is observably parked in
// pg_locks — a deterministic first-committer-wins conflict. AddComment's
// mention path never does that kind of locking read: loadReasonStates
// (fanout.go) is a plain SELECT with no FOR UPDATE against
// user_notification_reason_prefs, and WriteMentionNotification's INSERT
// into notifications has a distinct (user_id, event_id) key on every
// concurrent comment (event_id is per-comment), so there is no shared row
// two concurrent writers both try to UPDATE. The abort is a genuine
// multi-way Serializable Snapshot Isolation "dangerous structure" —
// Postgres detecting a read/write dependency CYCLE across ≥2 concurrent
// transactions that never blocked on each other at all — which is why it
// needs real N-way concurrency (measured here) to reproduce, not a single
// planted competitor. Confirmed by the SAME measurement this test's own
// comment already cites: a distinct-mention-target run produces ZERO
// aborts even at 40-way concurrency, which a lock-contention theory could
// not explain (a lock would still exist per-recipient) but an SSI
// cross-transaction cycle theory does (no shared predicate to collide on
// when every target is different).
//
// Red without the fix: with plain BeginTxFunc, the losers surface a raw
// *pgconn.PgError (SQLSTATE 40001) straight out of AddComment.
func TestAddComment_ConcurrentMentions_NoSerializationError(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)

	dto, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-303.17 comment-mention repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// The single shared mention target every concurrent comment names —
	// this is what creates the real contention (see comment above); a
	// distinct target per comment does not reproduce it.
	mentionTarget := seedUser(t, pool)
	mentionName := readUsername(t, pool, mentionTarget)
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, mentionTarget, creator); err != nil {
		t.Fatalf("seed mention target membership: %v", err)
	}

	// 40 concurrent commenters is well above what 3 attempts would cover
	// as a naive N-way tie — the budget holds because the contention is a
	// modest per-recipient pile-up, not a guaranteed collision on every
	// attempt (see commentMentionMaxAttempts doc).
	const commenters = 40

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		succeed int
		errs    []error
	)
	start := make(chan struct{})
	for i := 0; i < commenters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together — that is the contention
			_, cerr := store.AddComment(context.Background(), dto.ID,
				fmt.Sprintf("comment %d for @%s", i, mentionName),
				creator, creatorName, CommentAuthorTypeHuman, CommentAuthorKindHuman,
				HumanActor(creator, creatorName))
			mu.Lock()
			defer mu.Unlock()
			if cerr != nil {
				errs = append(errs, cerr)
				return
			}
			succeed++
		}(i)
	}
	close(start)
	wg.Wait()

	for _, e := range errs {
		var pgErr *pgconn.PgError
		if errors.As(e, &pgErr) &&
			(pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected) {
			t.Errorf("AddComment lost a serialization race and was NOT retried: SQLSTATE %s: %v",
				pgErr.Code, e)
			continue
		}
		if errors.Is(e, ErrSerializationRetry) {
			t.Errorf("AddComment exhausted commentMentionMaxAttempts=%d retries under %d-way mention contention: %v",
				commentMentionMaxAttempts, commenters, e)
			continue
		}
		t.Errorf("AddComment failed: %v", e)
	}
	if succeed != commenters {
		t.Fatalf("succeeded %d/%d concurrent comments", succeed, commenters)
	}

	// Every mentioned recipient gets exactly one comment.mention event
	// per comment — a mis-replay that double-applies mentions on retry
	// would inflate this past `commenters`. Mention events carry the
	// comment (not the issue) as entity — scope by project + type +
	// payload recipient instead.
	var mentionEvents int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events
 WHERE project_id = $1 AND type = $2 AND entity_type = 'comment'
   AND payload->>'mentionedUserId' = $3`,
		pid, EventCommentMention, mentionTarget.String()).Scan(&mentionEvents); err != nil {
		t.Fatalf("counting mention events: %v", err)
	}
	if mentionEvents != commenters {
		t.Fatalf("mention events = %d, want %d (a mis-replay would double or skip some)",
			mentionEvents, commenters)
	}
}
