package notifications

// assignee_resync.go — sk-622.21, T025 (INV-15; found in review): the background re-resolution of issues.assignee_user_id
// that outlives the R1 reconciliation.
//
// R2 removed the R1 reconciliation worker (its steps existed for the old
// version's rows). One of its steps stays, as this worker, because one case
// of INV-15 is not closed by the write path: the triggers of migration 000060
// keep assignee_user_id equal to the resolution of the assignee text in the
// same transaction as every write. The one race they cannot see alone — an
// assignment in a SERIALIZABLE transaction whose snapshot predates a
// concurrent users write — is closed at commit since sk-622.21 R25: every
// users writer that changes the resolution (users.Store, the SSO
// provisioner auth/oidc.go#provisionTx) runs SERIALIZABLE, and SSI aborts one
// side of the race with 40001. This worker is therefore a SAFETY NET, not the
// correctness path of INV-15: it walks issues and re-resolves rows that
// still disagree, and a non-zero fixed count after rollout is the signal of a
// writer defect (metrics.go, skein_issues_assignee_resync_fixed).
//
// A pass walks the whole issues table in id order (keyset), one page per
// READ COMMITTED transaction, replaying the 40001 the 000060 trigger raises
// against a concurrent re-sync; the resolution is computed once per assigned
// issue of a page (resolveAssigneesSQL). Replicas do not run passes
// concurrently: a pass runs under a session advisory lock taken with
// pg_try_advisory_lock, and a replica that does not get it skips the pass.
// The result of a pass is published as metrics (metrics.go).

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// AssigneeResyncInterval is the cadence of passes after the one at
	// start-up.
	AssigneeResyncInterval = time.Hour
	// defaultAssigneeResyncPageSize bounds one page (one transaction).
	defaultAssigneeResyncPageSize = 500
	// assigneeResyncMaxAttempts is the retry budget of one page (sk-354: one
	// constant per path); an exhausted page fails the pass and the next pass
	// retries it.
	assigneeResyncMaxAttempts = 3
	// assigneeResyncLockKey is the session advisory lock key of a pass: a
	// fixed bigint ("skeinar1" in ASCII).
	assigneeResyncLockKey int64 = 0x736b65696e617231
)

// AssigneeResyncOptions configures the worker.
type AssigneeResyncOptions struct {
	// Interval between passes; zero means AssigneeResyncInterval.
	Interval time.Duration
	// PageSize bounds one page; zero means the default.
	PageSize int
}

// AssigneeResyncRunner is the assignee re-resolution worker.
type AssigneeResyncRunner struct {
	pool *pgxpool.Pool
	opts AssigneeResyncOptions
}

// NewAssigneeResyncRunner builds the worker.
func NewAssigneeResyncRunner(pool *pgxpool.Pool, opts AssigneeResyncOptions) *AssigneeResyncRunner {
	if opts.Interval <= 0 {
		opts.Interval = AssigneeResyncInterval
	}
	if opts.PageSize <= 0 {
		opts.PageSize = defaultAssigneeResyncPageSize
	}
	return &AssigneeResyncRunner{pool: pool, opts: opts}
}

// Run blocks until ctx is cancelled: one pass at start-up, then one every
// interval. A failed pass is logged and the next one comes on schedule.
func (r *AssigneeResyncRunner) Run(ctx context.Context) {
	slog.InfoContext(ctx, "assignee resync runner started",
		"intervalMs", r.opts.Interval.Milliseconds(), "pageSize", r.opts.PageSize)
	defer slog.InfoContext(ctx, "assignee resync runner stopped")
	for {
		if ctx.Err() != nil {
			return
		}
		if _, _, err := r.Pass(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.ErrorContext(ctx, "assignee resync pass failed", "error", err.Error())
		}
		if !sleepCtx(ctx, r.opts.Interval) {
			return
		}
	}
}

// Pass runs one pass under the pass lock and returns how many issues it
// re-resolved. ran is false when another session held the lock and the pass
// was skipped at once, without waiting. Exported so tests drive one pass
// deterministically.
func (r *AssigneeResyncRunner) Pass(ctx context.Context) (fixed int64, ran bool, err error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("assignee resync: acquiring the lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, assigneeResyncLockKey).Scan(&locked); err != nil {
		conn.Release()
		return 0, false, fmt.Errorf("assignee resync: taking the pass lock: %w", err)
	}
	if !locked {
		conn.Release()
		slog.InfoContext(ctx, "assignee resync pass skipped: another session holds the pass lock")
		return 0, false, nil
	}
	defer releaseAssigneeResyncLock(conn)

	start := time.Now()
	fixed, err = r.resolveAssignees(ctx)
	if err != nil {
		return fixed, true, err
	}
	assigneeResyncFixed.Set(float64(fixed))
	assigneeResyncLastPassTimestamp.Set(float64(time.Now().Unix()))
	slog.InfoContext(ctx, "assignee resync pass completed",
		"durationMs", time.Since(start).Milliseconds(), "fixed", fixed)
	return fixed, true, nil
}

// releaseAssigneeResyncLock unlocks the pass lock and returns the connection.
// The unlock runs on its own short context: the pass context may already be
// cancelled (shutdown), and a session lock left on a pooled connection would
// block every later pass. If the unlock cannot be confirmed, the connection
// is closed — closing the session releases its advisory locks.
func releaseAssigneeResyncLock(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlocked bool
	err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, assigneeResyncLockKey).Scan(&unlocked)
	if err != nil || !unlocked {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		slog.Warn("assignee resync: pass lock not released cleanly; closing its connection",
			"unlocked", unlocked, "error", msg)
		_ = conn.Conn().Close(ctx)
	}
	conn.Release()
}

// resolveAssigneesSQL is one page of the walk of issues in id order. $1 =
// the last id of the previous page (the empty string for the first), $2 =
// page size. The resolution is computed ONCE per issue (a MATERIALIZED CTE,
// so the planner cannot copy the call into both the SET and the predicate),
// and not at all for an issue with neither an assignee text nor a cached id:
// resolve(NULL) is NULL, so such a row holds INV-15 trivially. The
// assignment is recomputed by the BEFORE trigger of 000060 anyway; the
// predicate only keeps already-correct rows out of the UPDATE.
const resolveAssigneesSQL = `
WITH page AS (
    SELECT id
      FROM issues
     WHERE id > $1
     ORDER BY id
     LIMIT $2
),
resolved AS MATERIALIZED (
    SELECT i.id, resolve_assignee_user_id(i.assignee) AS now_resolved
      FROM issues i
      JOIN page p ON p.id = i.id
     WHERE i.assignee IS NOT NULL OR i.assignee_user_id IS NOT NULL
),
upd AS (
    UPDATE issues i
       SET assignee_user_id = r.now_resolved
      FROM resolved r
     WHERE i.id = r.id
       AND i.assignee_user_id IS DISTINCT FROM r.now_resolved
    RETURNING 1
)
SELECT (SELECT count(*) FROM page), COALESCE((SELECT max(id) FROM page), ''), (SELECT count(*) FROM upd)`

// resolveAssignees walks the whole issues table in id order, one page per
// transaction, so INV-15 is checked over the entire corpus without one long
// statement.
func (r *AssigneeResyncRunner) resolveAssignees(ctx context.Context) (int64, error) {
	var (
		after string
		total int64
	)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var (
			pageLen int64
			last    string
			fixed   int64
		)
		if err := withTxRetry(ctx, r.pool, pgx.ReadCommitted, "assignee resync", assigneeResyncMaxAttempts, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, resolveAssigneesSQL, after, r.opts.PageSize).Scan(&pageLen, &last, &fixed)
		}); err != nil {
			return total, err
		}
		total += fixed
		if pageLen < int64(r.opts.PageSize) {
			return total, nil
		}
		after = last
	}
}
