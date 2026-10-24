package lockreap

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

	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/signals"
)

// pgSerializationFailure is SQLSTATE 40001 — a SERIALIZABLE transaction
// loser. Aligned with internal/issues/store.go and
// internal/attachments/store.go.
const pgSerializationFailure = "40001"

// pgDeadlockDetected is SQLSTATE 40P01.
const pgDeadlockDetected = "40P01"

// isSerializationFailure reports whether the error wraps a Postgres
// 40001 / 40P01 SQLSTATE — both safe to retry without re-validating
// inputs.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected
}

// MaxRowsPerTick caps the total number of rows the reaper will process
// in a single tick across all internal sweep iterations. Plan §D-3:
// a pathological backlog (one big crash that left thousands of stale
// locks) drains over multiple ticks instead of one runaway sweep that
// could hold the goroutine indefinitely. Next tick picks up the rest.
const MaxRowsPerTick = 200

// ReleaseReasonStaleTimer is written to events.reason on every auto-release
// and ReleasedByStaleTimer to new_data.releasedBy. Exported as constants
// rather than left as literals because they are the ONLY thing separating
// "a person dropped this task" from "a timer took the lock off a task that
// may still be in hand": consumers branch on them, and a value that only
// exists as a string in one function is not something a consumer can branch
// on safely.
//
// events.reason is the field that reaches agents — the slim EventDTO behind
// skein_show(includeEvents) projects type / actor / actorType / field /
// reason and deliberately never old_data / new_data. The new_data marker is
// for the durable audit trail and the REST consumers that read it.
const (
	ReleaseReasonStaleTimer = "auto-released-stale-lock"
	ReleasedByStaleTimer    = "stale-timer"
)

// minMeaningfulAgentIdle is the point below which an AgentIdle window
// starts fighting the agents.last_seen_at write throttle (60 seconds) and
// would report busy agents as silent.
const minMeaningfulAgentIdle = 2 * time.Minute

// Config bounds the reaper goroutine behaviour. Defaults are populated
// in main() from config.Config; this struct lives here so the worker
// stays independent of the config package.
type Config struct {
	// Interval between ticks. 0 disables the reaper entirely (caller
	// decides — main.go does not currently allow this).
	Interval time.Duration

	// Threshold is the minimum age (now() - locked_at) at which a lock
	// is considered stale and eligible for auto-release.
	Threshold time.Duration

	// AgentIdle is how long the LOCK HOLDER's MCP session may be silent
	// before the reaper treats the agent as gone (sk-243).
	//
	// Threshold and AgentIdle answer two different questions and are kept
	// as two knobs on purpose: "how long may a lock sit untouched" and
	// "how long may its holder be silent". Conflating them means an
	// operator shortening the first also declares agents dead sooner —
	// which is precisely the failure this field exists to end. Both
	// default to 30m, so the out-of-the-box behaviour needs no tuning.
	//
	// 0 disables the liveness check entirely and restores the pre-sk-243
	// "age alone" rule. Nothing in main() sets that; it exists so an
	// operator has a way back if the agents registry is ever wrong.
	AgentIdle time.Duration

	// BatchSize bounds the number of locks one sweep judges in a single
	// short transaction. Defaults to 200 when zero. The reaper keeps
	// looping within a tick until a sweep finds fewer than BatchSize
	// candidates, so BatchSize is purely a per-transaction cap — it bounds
	// how many issue rows are row-locked at once — not a per-tick cap.
	BatchSize int
}

// Reaper releases stale issue locks and emits issue.unclaimed events.
// Construction is cheap (just stores deps); call Run to start the
// goroutine. Run blocks until ctx is cancelled.
type Reaper struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New builds a Reaper. Pool is the shared *pgxpool.Pool — the reaper
// uses its own connections (no fork from issues.Store).
func New(pool *pgxpool.Pool, cfg Config) *Reaper {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	return &Reaper{pool: pool, cfg: cfg}
}

// Run starts the reaper loop. The first tick fires immediately at boot
// — same pattern as attachments.RunCleanupWorker — so a backlog at
// startup doesn't wait for the first scheduled interval.
//
// The loop never panics out of the goroutine. Errors are logged and
// the next tick takes another shot.
func (r *Reaper) Run(ctx context.Context) {
	if r.cfg.Interval <= 0 {
		slog.Warn("stale lock reaper disabled: interval <= 0")
		return
	}
	if r.cfg.Threshold <= 0 {
		slog.Warn("stale lock reaper disabled: threshold <= 0")
		return
	}

	slog.Info("stale lock reaper started",
		"interval", r.cfg.Interval,
		"threshold", r.cfg.Threshold,
		"agentIdle", r.cfg.AgentIdle,
		"batchSize", r.cfg.BatchSize)
	if r.cfg.AgentIdle <= 0 {
		// Not an error — the escape hatch is deliberate — but it restores
		// the behaviour sk-243 reported, so an operator must not be able
		// to end up here without the log saying so.
		slog.Warn("stale lock reaper: agent-liveness check disabled; locks are reaped on age alone and an agent that is mid-run will lose its lock (sk-243)")
	} else if r.cfg.AgentIdle < minMeaningfulAgentIdle {
		// agents.last_seen_at is only bumped when the previous value is
		// older than ~60s (internal/agents/store.go), so a window near or
		// below that resolution reports a busy agent as silent.
		slog.Warn("stale lock reaper: agentIdle is close to the agents.last_seen_at write throttle (60s); a live agent may be judged silent",
			"agentIdle", r.cfg.AgentIdle)
	}

	r.tick(ctx)

	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("stale lock reaper stopping")
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick runs one sweep — keeps releasing batches until a batch comes
// back empty (no more stale locks), the per-tick row cap is reached,
// or the context is done. A per-statement cap of BatchSize means we
// don't hold a long SERIALIZABLE tx; the next batch picks up where
// this one left off. The MaxRowsPerTick budget bounds the goroutine's
// per-tick wall-clock so a pathological backlog (huge crash) drains
// over multiple ticks rather than one runaway sweep.
func (r *Reaper) tick(ctx context.Context) {
	rowsThisTick := 0
	for {
		if ctx.Err() != nil {
			return
		}
		released, candidates, err := r.reapOnce(ctx)
		if err != nil {
			slog.WarnContext(ctx, "stale lock reaper: sweep failed",
				"error", err.Error())
			return
		}
		if candidates == 0 {
			return
		}
		// Per-row event write — one tx per row, separate from the
		// UPDATE-RETURNING tx. Best-effort: a per-row failure is
		// logged but does not stop the rest of the batch.
		for _, row := range released {
			if ctx.Err() != nil {
				return
			}
			if err := r.writeReleaseEvent(ctx, row); err != nil {
				slog.WarnContext(ctx, "stale lock reaper: event write failed",
					"issueId", row.IssueID,
					"projectId", row.ProjectID.String(),
					"error", err.Error())
				continue
			}
			staleLocksReleasedTotal.Inc()
			staleLockAgeSeconds.Observe(row.AgeSeconds)
			holderLastSeen := "never"
			if row.HolderLastSeenAt != nil {
				holderLastSeen = row.HolderLastSeenAt.UTC().Format(time.RFC3339)
			}
			slog.InfoContext(ctx, "stale lock released",
				"issueId", row.IssueID,
				"ageSeconds", row.AgeSeconds,
				"previousLockedBy", row.PrevSession,
				// The grounds for the release, on the same line as the
				// release: an operator asking "was that agent actually
				// gone?" must not have to join two log streams to find out.
				"holderLastSeenAt", holderLastSeen)
		}
		// Budget and drain decisions count CANDIDATES, not releases: a
		// candidate whose holder woke up in the window is work this batch
		// did, and counting only releases would both understate the budget
		// and read a full batch of survivors as "the backlog is empty".
		rowsThisTick += candidates
		if rowsThisTick >= MaxRowsPerTick {
			slog.InfoContext(ctx, "stale lock reaper: per-tick cap reached, deferring remainder to next tick",
				"rowsProcessed", rowsThisTick,
				"cap", MaxRowsPerTick)
			return
		}
		if candidates < r.cfg.BatchSize {
			return
		}
	}
}

// releasedRow captures one row freed by the reaper. The pre-update
// locked_by / locked_at are surfaced from the candidate snapshot joined
// into the UPDATE, because PostgreSQL's RETURNING returns post-update
// values for columns named in SET.
type releasedRow struct {
	IssueID      string
	ProjectID    uuid.UUID
	NewVersion   int
	PrevSession  string
	PrevLockedAt time.Time
	AgeSeconds   float64
	// HolderLastSeenAt is when the holding session last spoke to the MCP
	// transport, or nil when it has no agents row at all. It is the
	// EVIDENCE for the release: without it the audit trail records a
	// verdict and not its grounds, and "the agent was working" cannot be
	// told apart from "the agent was gone" after the fact.
	HolderLastSeenAt *time.Time
}

// candidate is one lock the sweep has taken a row lock on and is about to
// judge. Its (locked_by, locked_at, version) triple is the snapshot the
// release is compare-and-swapped against, so the write can only ever land
// on the exact lock that was judged.
type candidate struct {
	IssueID   string
	ProjectID uuid.UUID
	LockedBy  string
	LockedAt  time.Time
	Version   int
}

// reapOnce runs one batch: pick candidates, then release the ones whose
// holder is still silent when the write happens.
//
// sk-243 — what "stale" means. A lock is eligible only when BOTH hold:
// the lock is older than Threshold, AND its holder has not spoken for
// AgentIdle. The second clause is the fix: the old rule read "nothing was
// written to this ISSUE", which in a multi-agent run is true of every
// head task by construction (the agent writes to the channel task, to the
// children and to git, and is forbidden from touching the head), so the
// timer fired on agents that were mid-run. The holder IS an MCP session
// id and `agents.last_seen_at` is bumped by every authenticated /mcp
// request, so "is this agent alive" is a direct join rather than an
// inference from the issue's own history. The scan is served by
// idx_agents_last_seen, the index migration 000010 created for exactly this
// query ("last_seen_at index supports stale-lock auto-release queries").
//
// agents is keyed (user_id, session_id), so one session id can carry more
// than one row. Both phases below treat the session as alive if ANY of its
// rows is recent — phase one as a NOT EXISTS over the recent ones, phase
// two as max(last_seen_at) — which is the same rule stated two ways, and
// neither can fan a candidate out into duplicates the way a plain join to
// agents would.
//
// A holder with NO agents row is released on age alone — the pre-sk-243
// behaviour. That covers cookie-session claims, rows written before the
// registry existed, and pruned agents: absence of evidence of life is not
// evidence of life, and the alternative (never reap what we cannot
// verify) would resurrect the immortal-lock problem the reaper exists to
// solve.
//
// # Why two statements in one transaction, and not one statement
//
// The sweep is a check-then-act on two different tables, and sk-243's whole
// point is that the check must still hold when the act lands. PostgreSQL's
// READ COMMITTED gives exactly two guarantees here, and they pull in
// opposite directions:
//
//   - "The search condition of the command (the WHERE clause) is
//     re-evaluated to see if the updated version of the row still matches"
//     — so a predicate on `issues` in the UPDATE's own WHERE genuinely
//     re-fires against the row as another transaction just left it. That is
//     what makes the compare-and-swap below load-bearing: if the holder
//     unclaimed and re-claimed while we were working, locked_at no longer
//     matches and the row is left alone. Without it, `WHERE issues.id =
//     stale.id` still matches and the reaper clears a lock that is seconds
//     old — the very defect sk-243 exists to prevent, in a narrower window.
//
//   - "…it does not see effects of those commands on other rows in the
//     database." A heartbeat lands in `agents`, not in `issues`, and does
//     not touch the row we are updating, so it can neither block us nor be
//     seen by us: within a single statement the liveness verdict is frozen
//     at that statement's snapshot however the WHERE is written. One
//     statement therefore CANNOT re-check liveness at write time.
//
// Hence: statement one takes the row locks (FOR UPDATE SKIP LOCKED — a
// claim or unclaim mid-flight on that row is skipped rather than waited
// for, and once locked, (locked_by, locked_at) cannot move under us);
// statement two re-reads `agents` and does the UPDATE. Because "Read
// Committed mode starts each command with a new snapshot", statement two
// sees every heartbeat committed since statement one — including the one
// from an agent that woke up in the gap. The liveness predicate lives in
// the UPDATE's own WHERE, not in a read that preceded it.
//
// The pre-filter in statement one is an optimisation only: it keeps live
// holders out of the batch budget. Statement two is the authority.
func (r *Reaper) reapOnce(ctx context.Context) (released []releasedRow, candidates int, err error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, 0, fmt.Errorf("reapOnce begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cands, err := r.selectCandidates(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	if len(cands) == 0 {
		return nil, 0, nil
	}
	released, err = r.releaseCandidates(ctx, tx, cands)
	if err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("reapOnce commit: %w", err)
	}
	return released, len(cands), nil
}

// selectCandidates is phase one: age-stale locks, pre-filtered by the
// liveness rule — the holder is the (user, session) pair, as in phase two
// (K-27) — and pinned with a row lock for the rest of the transaction.
//
// SKIP LOCKED rather than a plain FOR UPDATE: a row another transaction is
// already claiming or unclaiming is somebody else's business this tick, and
// waiting on it would hold the sweep behind an unrelated write. Ordering by
// locked_at spends the batch budget on the oldest locks first.
func (r *Reaper) selectCandidates(ctx context.Context, tx pgx.Tx) ([]candidate, error) {
	livenessOff, agentIdle := r.livenessArgs()
	rows, err := tx.Query(ctx, `
SELECT i.id, i.project_id, i.locked_by, i.locked_at, i.version
  FROM issues i
 WHERE i.locked_by IS NOT NULL
   AND i.locked_at < now() - $1::interval
   AND ($3::boolean OR NOT EXISTS (
           SELECT 1 FROM agents a
            WHERE a.user_id = i.locked_by_user_id
              AND a.session_id = i.locked_by
              AND a.last_seen_at >= now() - $4::interval))
 ORDER BY i.locked_at
 LIMIT $2
   FOR UPDATE OF i SKIP LOCKED`,
		r.cfg.Threshold, r.cfg.BatchSize, livenessOff, agentIdle)
	if err != nil {
		return nil, fmt.Errorf("reapOnce candidates: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.IssueID, &c.ProjectID, &c.LockedBy, &c.LockedAt, &c.Version); err != nil {
			return nil, fmt.Errorf("reapOnce candidate scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reapOnce candidate iterate: %w", err)
	}
	return out, nil
}

// releaseCandidates is phase two: a fresh snapshot of `agents`, the
// liveness verdict, and the write, in one statement.
//
// Three things guard the write, and each answers a different way of being
// wrong:
//
//   - the liveness leg, re-read HERE rather than inherited from phase one,
//     so a holder that woke up in the gap keeps its lock. The holder is the
//     PAIR (locked_by_user_id, locked_by) — the agents key — not the
//     session string alone (K-27, sk-674.18 T034): another user's agent
//     presenting the same session string keeps no one else's lock alive,
//     and a lock without its user (written before sk-424) has no live
//     holder, so it goes at its term as every lock does;
//   - the (locked_by, locked_at) compare-and-swap, so the release can only
//     land on the exact lock that was judged;
//   - the version compare-and-swap, which catches ANY mutation of the row
//     — claim and unclaim both bump it — including a re-claim that happened
//     to restore the same session id.
//
// Phase one's row locks already make all three unreachable in a normal
// sweep. They are written anyway because the correctness of this statement
// must not depend on a caller having taken a lock first: dropping the
// FOR UPDATE would then silently reopen the race instead of failing a test.
func (r *Reaper) releaseCandidates(ctx context.Context, tx pgx.Tx, cands []candidate) ([]releasedRow, error) {
	ids := make([]string, len(cands))
	sessions := make([]string, len(cands))
	lockedAt := make([]time.Time, len(cands))
	versions := make([]int32, len(cands))
	for i, c := range cands {
		ids[i] = c.IssueID
		sessions[i] = c.LockedBy
		lockedAt[i] = c.LockedAt
		versions[i] = int32(c.Version)
	}

	livenessOff, agentIdle := r.livenessArgs()
	rows, err := tx.Query(ctx, `
WITH judged AS (
    SELECT c.id, c.locked_by, c.locked_at, c.version,
           (SELECT a.last_seen_at
              FROM issues ih
              JOIN agents a ON a.user_id = ih.locked_by_user_id
                           AND a.session_id = c.locked_by
             WHERE ih.id = c.id) AS holder_last_seen_at
      FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::int[])
             AS c(id, locked_by, locked_at, version)
)
UPDATE issues
   SET locked_by  = NULL,
       locked_at  = NULL,
       locked_by_user_id = NULL,
       version    = issues.version + 1,
       updated_at = now()
  FROM judged j
 WHERE issues.id        = j.id
   AND issues.locked_by = j.locked_by
   AND issues.locked_at = j.locked_at
   AND issues.version   = j.version
   AND ($5::boolean
        OR j.holder_last_seen_at IS NULL
        OR j.holder_last_seen_at < now() - $6::interval)
RETURNING issues.id,
          issues.project_id,
          issues.version,
          j.locked_by  AS prev_session,
          j.locked_at  AS prev_locked_at,
          EXTRACT(EPOCH FROM (now() - j.locked_at)) AS age_seconds,
          j.holder_last_seen_at`,
		ids, sessions, lockedAt, versions, livenessOff, agentIdle)
	if err != nil {
		return nil, fmt.Errorf("reapOnce release: %w", err)
	}
	defer rows.Close()

	var out []releasedRow
	for rows.Next() {
		var row releasedRow
		if err := rows.Scan(
			&row.IssueID,
			&row.ProjectID,
			&row.NewVersion,
			&row.PrevSession,
			&row.PrevLockedAt,
			&row.AgeSeconds,
			&row.HolderLastSeenAt,
		); err != nil {
			return nil, fmt.Errorf("reapOnce scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reapOnce iterate: %w", err)
	}
	return out, nil
}

// livenessArgs renders the two bind values the liveness leg takes. A zero
// interval is still bound (Postgres would reject a NULL cast) but the
// boolean short-circuits the comparison before it is reached.
func (r *Reaper) livenessArgs() (off bool, idle time.Duration) {
	if r.cfg.AgentIdle <= 0 {
		return true, 0
	}
	return false, r.cfg.AgentIdle
}

// systemActor is the synthetic actor for the reaper. UserID = uuid.Nil
// makes notifications.FanOut treat the actor as "no user" — the
// self-action exclusion branch is short-circuited (the lock-holder
// whose lock was reaped DOES want a ping that their session got
// released, see plan §D-4). actor_type = 'system' is allowed by the
// events.actor_type CHECK constraint.
var systemActor = issues.Actor{
	UserID:   uuid.Nil,
	Username: "system",
	Role:     "system",
}

// writeReleaseEvent inserts the issue.unclaimed event row plus the
// notifications fan-out for one released lock. Runs in its own
// SERIALIZABLE tx — a per-row failure here does not affect the
// already-committed UPDATE-RETURNING. See plan §D-4 for the rationale
// of the split-tx pattern.
//
// SERIALIZABLE serialization failures (SQLSTATE 40001/40P01) are
// retried up to 3 times with a small jittered backoff. A reaper event
// tx that races a live unclaim/close on a sibling issue can hit 40001
// — same retry shape used by issues.Store + attachments.Store. Without
// retry the released lock would land without an audit/notification
// row, defeating the reaper's whole purpose (telling the previous
// holder their session was reaped).
func (r *Reaper) writeReleaseEvent(ctx context.Context, row releasedRow) error {
	const maxAttempts = 3
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = signals.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
			oldData := map[string]any{
				"lockedBy": row.PrevSession,
				"lockedAt": row.PrevLockedAt,
			}
			newData := map[string]any{
				"lockedBy": nil,
				"lockedAt": nil,
				// sk-243: name the timer. "Nobody holds this" and "somebody
				// is on it, the lock just expired" are different facts and
				// were indistinguishable once the lock was gone.
				"releasedBy": ReleasedByStaleTimer,
				// …and record what the timer judged on.
				"holderLastSeenAt": row.HolderLastSeenAt,
			}
			eventID, err := issues.WriteIssueEvent(
				ctx, tx,
				issues.EventIssueUnclaimed,
				row.IssueID,
				row.ProjectID,
				systemActor,
				"lockedBy",
				oldData,
				newData,
				ReleaseReasonStaleTimer,
			)
			if err != nil {
				return err
			}
			// FanOut with ActorUserID=uuid.Nil disables self-action
			// exclusion. That's the intended behaviour: the agent whose
			// session was reaped gets a notification.
			//
			// StaleLockRelease is what makes this issue.unclaimed carry a
			// reason at all. A HUMAN unclaim is journal-only — its sibling
			// issue.status_changed already delivers the signal, and INV-6
			// forbids two notifications for one action. The reaper's release
			// has no such sibling, so it maps onto "Automatic lock
			// release".
			if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
				EventID:          eventID,
				EventType:        issues.EventIssueUnclaimed,
				EntityType:       "issue",
				IssueID:          row.IssueID,
				ProjectID:        row.ProjectID,
				ActorUserID:      systemActor.UserID,
				ActorType:        systemActor.Role,
				StaleLockRelease: true,
			}); err != nil {
				return err
			}
			// sk-622.21 T015 (US-11): personal signal of the release,
			// published after this transaction commits.
			return issues.SignalStaleLockReleased(ctx, tx, row.IssueID)
		})
		if err == nil {
			return nil
		}
		if !isSerializationFailure(err) {
			return err
		}
		// Last attempt — surface the failure verbatim.
		if attempt == maxAttempts-1 {
			break
		}
		// Jittered backoff: 0..25ms × (attempt+1). Tiny so retries don't
		// stack into a noticeable per-row latency.
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

// Tick runs one full reaper sweep against the underlying DB. Exposed
// for tests so they can drive the reaper deterministically without
// waiting for the ticker. Errors are not surfaced (they go through the
// same logging path as the production loop) — tests assert on
// observable side effects (events written, locks cleared).
func (r *Reaper) Tick(ctx context.Context) {
	r.tick(ctx)
}
