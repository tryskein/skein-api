package issues

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// readBackTimeout bounds the read that follows a committed write (sk-607).
//
// That read runs DETACHED from the caller's context (ReadBackContext): the
// write already happened, and a context that ends now must not turn a
// committed write into a reported failure. Where that matters is a deadline
// this service sets itself — a batch loop reaching its own budget — because
// the caller is still waiting for the answer. When the context ends because
// the caller left (skein-web's proxy tearing the connection down), no answer
// reaches it whatever is written here: that case is not cured by this read
// but by the proxy's window (sk-608) and by the write finishing well inside
// it (sk-609). A detached read therefore needs
// a bound of its own, or a sick database would keep it, and the pool
// connection under it, alive long after anyone waits for the answer.
//
// The value is a ceiling, not a measured latency. The read is one primary-
// key lookup plus the hydration queries of GetIssue, all on indexed keys;
// when it has not finished in three seconds the database is in trouble,
// and the caller is better served at once by the post-state the committing
// transaction already knew (ErrReadBackFailed) than by a full card later.
// Three seconds also fits after a batch loop's 20 s budget
// (tools.batchLoopDeadline) inside the proxy's window, which is 120 s by
// default and was 30 s when this bound was chosen.
const readBackTimeout = 3 * time.Second

// ReadBackContext derives the context for a read that follows a committed
// write: the caller's values (request id, logging attributes) survive, its
// cancellation and deadline do not, and readBackTimeout bounds the read.
// Callers must call the returned CancelFunc.
func ReadBackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), readBackTimeout)
}

// CommittedState is what a writing transaction knows about one issue at the
// moment it commits: the issues-table columns a write acknowledgement
// carries, read inside that transaction.
//
// It is NOT a card. Labels, components, commit refs, the parent, comment
// counts, blockers and rollups are absent — they live in other tables and
// are loaded by GetIssue's hydration. It exists so a caller can confirm a
// committed write when the hydrated read-back failed, without reading the
// database again.
type CommittedState struct {
	ID           string
	Version      int
	Status       string
	Assignee     *string
	LockedBy     *string
	LockedAt     *time.Time
	StartedAt    *time.Time
	ClosedAt     *time.Time
	CloseOutcome *string
}

// ErrReadBackFailed reports a write that COMMITTED but whose post-state
// could not be read back (sk-607). The write is not to be retried: it
// happened. State (and Children, for an epic create) carry what the
// committing transaction knew, which is enough to acknowledge the write.
//
// It deliberately has no Unwrap. The cause is usually a context error, and
// callers classify context errors as "the request stopped before the write
// committed" — exactly the wrong reading here. A caller that does not know
// this type gets an opaque error, which is what it got before this type
// existed.
type ErrReadBackFailed struct {
	// Op names the write: "create", "epic_create", "close", "patch",
	// "status" or "update" (the whole skein_update chain).
	Op string
	// State is the issue the write was about (the epic, for epic_create).
	State CommittedState
	// Children are the epic's children in creation order; nil otherwise.
	Children []CommittedState
	// InheritedComponentIDs is CreateIssue's inheritance echo, which the
	// transaction computed; nil when inheritance did not fire.
	InheritedComponentIDs []uuid.UUID
	// Cause is the read failure, kept for the log line only.
	Cause error
}

func (e *ErrReadBackFailed) Error() string {
	return fmt.Sprintf("%s of %s committed at version %d, reading it back failed: %v",
		e.Op, e.State.ID, e.State.Version, e.Cause)
}

// AfterCommitHook is a test seam: when non-nil it runs right after a
// writing transaction of CreateIssue, CreateEpicWithChildren, the close
// writer, PatchIssueWithResult or UpdateStatus has COMMITTED and before anything is read back. It lets a test
// end the caller's context inside that window, which is otherwise a real
// but non-deterministic race (sk-607). Always nil in production; only
// tests assign it, and they must not run in parallel with each other.
//
// It is exported because the surfaces it protects are tested from the MCP
// tools package; an unexported seam would only be reachable from this one.
var AfterCommitHook func(ctx context.Context, op, issueID string)

// afterCommit fires AfterCommitHook when a test installed one.
func afterCommit(ctx context.Context, op, issueID string) {
	if AfterCommitHook != nil {
		AfterCommitHook(ctx, op, issueID)
	}
}

// committedStatesInTx reads the CommittedState of each id inside the
// writing transaction, in the order given. One query for any number of
// ids, so an epic with its children pays one round-trip, not one per row.
// A missing id is an error: the caller just wrote it in this transaction.
func committedStatesInTx(ctx context.Context, tx pgx.Tx, ids ...string) ([]CommittedState, error) {
	rows, err := tx.Query(ctx, `
SELECT id, version, status, assignee, locked_by, locked_at, started_at, closed_at, close_outcome
  FROM issues WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("reading committed state: %w", err)
	}
	defer rows.Close()
	byID := make(map[string]CommittedState, len(ids))
	for rows.Next() {
		var c CommittedState
		if err := rows.Scan(&c.ID, &c.Version, &c.Status, &c.Assignee, &c.LockedBy,
			&c.LockedAt, &c.StartedAt, &c.ClosedAt, &c.CloseOutcome); err != nil {
			return nil, fmt.Errorf("scanning committed state: %w", err)
		}
		byID[c.ID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading committed state: %w", err)
	}
	out := make([]CommittedState, 0, len(ids))
	var missing []string
	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		out = append(out, c)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("reading committed state: rows not found: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// readBack loads the full IssueDetail of every committed state under
// ReadBackContext, in order. On any failure it returns *ErrReadBackFailed
// carrying the states, so the caller can still acknowledge the write.
func (s *Store) readBack(ctx context.Context, op string, states []CommittedState) ([]*IssueDetail, error) {
	rctx, cancel := ReadBackContext(ctx)
	defer cancel()
	out := make([]*IssueDetail, 0, len(states))
	for _, st := range states {
		d, err := s.GetIssue(rctx, st.ID)
		if err != nil {
			rb := &ErrReadBackFailed{
				Op:    op,
				State: states[0],
				Cause: fmt.Errorf("reading back %s: %w", st.ID, err),
			}
			if len(states) > 1 {
				rb.Children = states[1:]
			}
			return nil, rb
		}
		out = append(out, d)
	}
	return out, nil
}

// withCommittedState wraps the body of a writing transaction so that, when
// the body succeeds — whichever of its return paths it took — the post-state
// of id is read as the transaction's last statement into *dst.
func withCommittedState(ctx context.Context, id string, dst *CommittedState, fn func(pgx.Tx) error) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		st, err := committedStatesInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		*dst = st[0]
		return nil
	}
}
