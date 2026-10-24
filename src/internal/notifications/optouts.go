package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/signals"
)

// Subscriptions, opt-outs and Watching cards (sk-622.21, T010: US-1, US-6,
// INV-1, INV-2, INV-6, INV-7, D2).
//
// The model this file implements:
//
//   - issue_watchers holds LIVE subscriptions only (INV-1). Unsubscribing
//     deletes the row; nothing is left behind in it.
//   - An EXPLICIT "stop watching" of an issue the user is subscribed to
//     (single unwatch, unwatch of a selection, declining a delegation)
//     deletes the subscription, records an opt-out in issue_watch_optouts
//     and clears the Watching card X on every notification of that user
//     whose card is X (INV-6).
//   - "Remove" of a HISTORICAL card — the user has no subscription to X but
//     still has notifications carded X — clears the card and writes NO
//     opt-out (decision R3): the person is tidying the column, not asking
//     never to be subscribed again.
//   - The opt-out is read by the automatic subscription paths ONLY
//     (AddIssueWatcherScoped with a non-manual reason: creation, assignment,
//     reopen). Delivery, the badge, the feeds and the card computation never
//     read it — an opt-out on a descendant does not mute a subtree
//     subscription of an ancestor.
//   - A manual subscription ("watch", including the one a task monitor
//     places — decision R4) deletes the opt-out.
//   - Auto-unsubscribe on close and the bulk sweep by state delete the
//     subscription WITHOUT an opt-out.
//
// Concurrency (K2). Every operation here reads or writes subscriptions,
// opt-outs or cards that a concurrent fan-out (FanOut, T012) reads and
// writes too, so each runs in a SERIALIZABLE transaction replayed by
// withSerializableRetry. SSI then breaks the cycle "the fan-out read
// subscription X — the unwatch deleted it; the unwatch cleared cards X —
// the fan-out inserted one" with a serialization failure on one side, and
// the replay re-decides against what the winner committed: the outcome is
// that of SOME serial order of the two (INV-7), and a card X written by a
// fan-out racing the unwatch does not survive it (INV-6).
//
// R2 (T025, INV-1): issue_watchers holds live subscriptions only — the
// CHECK of migration 000062 forbids the old version's tombstone rows
// (reasons containing 'unwatched'), so no path here has to recognise one.

// Per-operation retry budgets (sk-354: one constant per path, so that a
// measurement on one path can move its budget alone). The writes here
// contend incidentally — one user's rows of one or a few issues against a
// fan-out that touches the same rows — the same shape as claim, so each
// starts at claim's 3.
const (
	addIssueWatchMaxAttempts    = 3
	removeIssueWatchMaxAttempts = 3
	bulkUnwatchMaxAttempts      = 3
	// updateReasonPrefsMaxAttempts — the settings write that turns «unwatch
	// on close» on rewrites one user's subscriptions on closed issues
	// (decision R23), the same incidental contention as a bulk unwatch.
	updateReasonPrefsMaxAttempts = 3
)

// pgDeadlockDetected is SQLSTATE 40P01. Like 40001 it rolls the whole
// transaction back and is safe to replay.
const pgDeadlockDetected = "40P01"

// isRetryableTxError reports a class-40 abort that a replay of the whole
// SERIALIZABLE transaction resolves: 40001 serialization_failure or 40P01
// deadlock_detected. Anything else is returned to the caller as is.
func isRetryableTxError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected
}

// withSerializableRetry runs fn inside a SERIALIZABLE transaction, replaying
// it on a retryable abort (isRetryableTxError) with a small jittered backoff.
// `attempts` is the TOTAL number of tries; when they run out the last error
// is returned as is. `op` names the operation in the retry log line.
//
// This is the notifications-package sibling of issues.Store's and
// attachments' wrappers (the package cannot import issues: issues imports
// notifications). Replay is safe because a class-40 abort rolls the
// transaction back whole and fn re-reads everything it needs; fn must not
// have side effects outside the transaction, and captured locals it assigns
// must be (re)assigned on every attempt.
func withSerializableRetry(ctx context.Context, pool *pgxpool.Pool, op string, attempts int, fn func(tx pgx.Tx) error) error {
	return withTxRetry(ctx, pool, pgx.Serializable, op, attempts, fn)
}

// withTxRetry is withSerializableRetry at a chosen isolation level. A
// READ COMMITTED transaction can still abort with a class-40 error raised by
// a trigger (000060's assignee resolution answers 40001 to a concurrent
// re-resolution), and the replay rule is the same.
func withTxRetry(ctx context.Context, pool *pgxpool.Pool, iso pgx.TxIsoLevel, op string, attempts int, fn func(tx pgx.Tx) error) error {
	var err error
	for attempt := range attempts {
		err = signals.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: iso}, fn)
		if err == nil {
			return nil
		}
		if !isRetryableTxError(err) {
			return err
		}
		if attempt == attempts-1 {
			break
		}
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) {
			slog.WarnContext(ctx, "transaction hit a retryable abort; retrying",
				"isoLevel", string(iso),
				"op", op,
				"attempt", attempt+1,
				"maxAttempts", attempts,
				"sqlstate", pgErr.Code,
				"detail", pgErr.Message)
		}
		// Jittered backoff: 0..25ms × (attempt+1), as in issues.Store.
		base := 25 * time.Millisecond * time.Duration(attempt+1)
		jitter := time.Duration(rand.Int64N(int64(25 * time.Millisecond)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(base + jitter):
		}
	}
	return err
}

// ExplicitUnwatchResult reports what UnwatchExplicitTx did for one issue.
type ExplicitUnwatchResult struct {
	// HadSubscription is true when the user had a subscription on the
	// issue — it was deleted and
	// an opt-out was recorded. False is the R3 branch: nothing to
	// unsubscribe from, only the card was cleared.
	HadSubscription bool
	// CardsCleared is how many of the user's notifications lost the
	// Watching card X, counted in notification rows.
	CardsCleared int64
}

// UnwatchExplicitTx is the explicit "stop watching X" primitive, run inside
// the caller's transaction (the caller owns SERIALIZABLE and the retry —
// K2). It is the single definition behind the single unwatch (REST and
// MCP), and the delegation decline (issues.Store#DismissDelegated, T011):
//
//  1. delete the user's subscription on X;
//  2. if there was one, record the opt-out (U, X) — idempotent;
//  3. clear the Watching card X on every notification of the user carded X,
//     archived ones included (INV-6: after the unwatch U has NO notification
//     carded X; K6) — with or without a subscription, so the R3 branch
//     ("remove" of a historical card) is the same call with step 2 skipped.
//
// One statement: the three data-modifying CTEs see one snapshot and commit
// together. Only the given user's rows are read or written. The opt-out FK
// references issues, and step 1 deletes nothing for an issue that does not
// exist, so an unknown id is a no-op rather than an FK violation.
func UnwatchExplicitTx(ctx context.Context, tx pgx.Tx, issueID string, userID uuid.UUID) (ExplicitUnwatchResult, error) {
	const sql = `
WITH del AS (
    DELETE FROM issue_watchers
     WHERE issue_id = $1 AND user_id = $2
    RETURNING issue_id
),
opt AS (
    INSERT INTO issue_watch_optouts (issue_id, user_id)
    SELECT issue_id, $2 FROM del
    ON CONFLICT (issue_id, user_id) DO NOTHING
),
clr AS (
    UPDATE notifications
       SET card_issue_id = NULL
     WHERE user_id = $2
       AND card_issue_id = $1
    RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM del), (SELECT count(*) FROM clr)`
	var out ExplicitUnwatchResult
	if err := tx.QueryRow(ctx, sql, issueID, userID).Scan(&out.HadSubscription, &out.CardsCleared); err != nil {
		return ExplicitUnwatchResult{}, fmt.Errorf("explicit unwatch of %s: %w", issueID, err)
	}
	// sk-622.21 T016 (US-11): "unsubscribe / 'remove'" — the user's Watching
	// changed when a subscription row or a card went.
	if out.HadSubscription || out.CardsCleared > 0 {
		signals.Add(tx, userID, signals.AreaWatching)
	}
	return out, nil
}
