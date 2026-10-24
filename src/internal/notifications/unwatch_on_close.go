package notifications

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/signals"
)

// sk-551.10, item 5 — the personal "unwatch on close" setting
// (user_notification_prefs.unwatch_on_close, migration 000053).
//
// On by default since sk-622.21 (US-2; was off — sk-551.10); a person
// without a prefs row reads as on. When it is on, closing an issue removes
// the person's AUTOMATIC subscription reasons on it — `creator` and
// `assignee` — and leaves a `manual` one alone. It writes no opt-out and
// clears no Watching card: the close notification keeps its card and the
// card becomes historical (US-6). A reopen brings the subscription back
// unless the person has an opt-out (issues.reAddAutoWatchersOnReopen).

// autoWatchReasons are the reasons the setting strips. Exactly the two the
// system adds on its own today (issues/store.go Step 7 and the assign
// path); `manual` is the person's own decision and is never touched.
var autoWatchReasons = []string{ReasonCreator, ReasonAssignee}

// UnwatchOnCloseEnabled reports the person's setting. No prefs row (they
// are created lazily) means the default — ON since sk-622.21 (US-2;
// migration 000059 made the column default true, and a missing row must
// read the same as a row created by somebody else's upsert).
func UnwatchOnCloseEnabled(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (bool, error) {
	var on bool
	err := tx.QueryRow(ctx,
		`SELECT unwatch_on_close FROM user_notification_prefs WHERE user_id = $1`, userID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading unwatch_on_close for %s: %w", userID, err)
	}
	return on, nil
}

// StripAutoWatchersOnClose removes, on one just-closed issue, the automatic
// subscription reasons of every watcher whose setting is on — a watcher
// without a prefs row included, because no row reads as on (sk-622.21,
// US-2). It runs inside the closing transaction, AFTER the close has been
// fanned out — the person still gets the notification that the issue was
// closed, with its Watching card; it is the last one the automatic
// subscription brings.
//
// Two statements, because one row can need either outcome:
//   - a row whose live reasons are ONLY automatic ones is deleted — a row
//     with no reason left would be a subscription nobody asked for;
//   - a row that also carries another reason (`manual`, or a legacy one)
//     keeps it and loses the automatic ones.
//
// Neither writes an opt-out nor touches notifications (US-2: no refusal,
// no card cleared).
//
// Returns how many rows were changed or removed.
func StripAutoWatchersOnClose(ctx context.Context, tx pgx.Tx, issueID string) (int64, error) {
	// "Setting on" = no prefs row saying it is off. NOT EXISTS rather than
	// a join, so a person who never saved preferences is stripped too.
	removed, err := collectUserIDs(ctx, tx, `
DELETE FROM issue_watchers iw
 WHERE iw.issue_id = $1
   AND iw.reasons <@ $2::text[]
   AND cardinality(iw.reasons) > 0
   AND NOT EXISTS (SELECT 1 FROM user_notification_prefs p
                    WHERE p.user_id = iw.user_id
                      AND NOT p.unwatch_on_close)
RETURNING iw.user_id`, issueID, autoWatchReasons)
	if err != nil {
		return 0, fmt.Errorf("removing auto-only watchers of closed %s: %w", issueID, err)
	}
	stripped, err := collectUserIDs(ctx, tx, `
UPDATE issue_watchers iw
   SET reasons = array_remove(array_remove(iw.reasons, $2), $3)
 WHERE iw.issue_id = $1
   AND iw.reasons && ARRAY[$2, $3]::text[]
   AND NOT EXISTS (SELECT 1 FROM user_notification_prefs p
                    WHERE p.user_id = iw.user_id
                      AND NOT p.unwatch_on_close)
RETURNING iw.user_id`, issueID, ReasonCreator, ReasonAssignee)
	if err != nil {
		return 0, fmt.Errorf("stripping auto reasons of closed %s: %w", issueID, err)
	}
	// sk-622.21 (K13, US-11): automatic unsubscription changes the Watching
	// of exactly the users whose row was removed or stripped.
	for _, uid := range append(removed, stripped...) {
		signals.Add(tx, uid, signals.AreaWatching)
	}
	return int64(len(removed) + len(stripped)), nil
}

// collectUserIDs runs a data-changing statement whose RETURNING list is one
// user id and answers the ids, one per affected row.
func collectUserIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// stripUserClosedAutoWatchesSQL is StripUserAutoWatchesOnClosed's statement.
// $1 = the user, $2 / $3 = the creator / assignee reasons. The predicates
// are StripAutoWatchersOnClose's, turned from «one issue, every watcher» to
// «one watcher, every closed issue»: an automatic-only row is deleted, a
// mixed row loses the automatic reasons. The setting is not read — the caller is the write that turns it on.
const stripUserClosedAutoWatchesSQL = `
WITH picked AS (
    SELECT w.issue_id, (w.reasons <@ ARRAY[$2, $3]::text[]) AS auto_only
      FROM issue_watchers w
      JOIN issues i ON i.id = w.issue_id
     WHERE w.user_id = $1
       AND i.status = 'closed'
       AND w.reasons && ARRAY[$2, $3]::text[]
       FOR UPDATE OF w
),
del AS (
    DELETE FROM issue_watchers w
     USING picked p
     WHERE p.auto_only AND w.issue_id = p.issue_id AND w.user_id = $1
    RETURNING 1
),
strip AS (
    UPDATE issue_watchers w
       SET reasons = array_remove(array_remove(w.reasons, $2), $3)
      FROM picked p
     WHERE NOT p.auto_only AND w.issue_id = p.issue_id AND w.user_id = $1
    RETURNING 1
)
SELECT (SELECT count(*) FROM del) + (SELECT count(*) FROM strip)`

// StripUserAutoWatchesOnClosed applies «unwatch on close» retroactively to
// one person: on every issue that is already closed, their automatic
// subscription reasons (creator, assignee) are removed — an automatic-only
// row is deleted, a mixed row keeps its manual reason (sk-622.21, decision
// R23: the US-2 acceptance is an invariant of the state, so turning the
// setting on must leave no automatic-only subscription of that person on a
// closed issue, not just act on the next close).
//
// It runs inside the transaction that turns the setting on, as one
// statement. No opt-out is written and no card is cleared, exactly as on a
// close; a reopen gives the subscription back
// (issues.reAddAutoWatchersOnReopen). Turning the setting OFF restores
// nothing — this function is never called for it.
//
// The caller's transaction must be SERIALIZABLE (K2): a close or reopen
// racing the setting change reads the setting and the subscriptions this
// statement writes, and SSI makes one of the two replay.
//
// Returns how many rows were changed or removed.
func StripUserAutoWatchesOnClosed(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int64, error) {
	var n int64
	if err := tx.QueryRow(ctx, stripUserClosedAutoWatchesSQL, userID, ReasonCreator, ReasonAssignee).Scan(&n); err != nil {
		return 0, fmt.Errorf("stripping auto reasons of %s on closed issues: %w", userID, err)
	}
	return n, nil
}
