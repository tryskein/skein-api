package reminders

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/signals"

	"skein-api/internal/authz"
)

// pgSerializationFailure / pgDeadlockDetected — SQLSTATE 40001 / 40P01,
// the SERIALIZABLE losers. Aligned with issues / attachments / lockreap.
const (
	pgSerializationFailure = "40001"
	pgDeadlockDetected     = "40P01"
)

func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected
}

// withSerializableRetry runs fn inside a SERIALIZABLE tx, retrying up to
// 3 times on 40001/40P01 with jittered backoff. Identical shape to
// attachments.withSerializableRetry — every caller re-reads inside fn so
// a retry observes fresh state.
func withSerializableRetry(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	const maxAttempts = 3
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = signals.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		if err == nil {
			return nil
		}
		if !isSerializationFailure(err) {
			return err
		}
		if attempt == maxAttempts-1 {
			break
		}
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

// ErrNotFound is returned when a reminder row does not exist, or exists but
// is not owned by the caller (the owner scope folds "not yours" into "not
// found" so a user can't probe other users' reminders).
var ErrNotFound = errors.New("reminder not found")

// ErrIssueNotFound is returned by Create when the parent issue row is
// missing (the FK insert would fail; we surface a typed error so the
// handler maps it to a 404 instead of a 500).
var ErrIssueNotFound = errors.New("issue not found")

// Store is the reminders repository over a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires a Store around the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// issueSummarySelect is the shared column list + join used by ListForUser
// and the read-after-write helpers. r.* are the reminder columns; the
// issues join hydrates the denormalised summary. now() is bound by the
// caller (passed as a parameter) only where state derivation needs a
// stable clock — the SELECT computes state in Go from remind_at.
const reminderColumns = `
    r.id, r.issue_id, r.user_id, r.remind_at, r.notified_at,
    r.snooze_count, r.last_reason, r.created_at, r.updated_at,
    i.title, i.status, i.project_id, i.type, i.priority,
    (SELECT s_sp.short_id FROM projects p_sp JOIN spaces s_sp ON s_sp.id = p_sp.space_id
      WHERE p_sp.id = i.project_id)`

// scanReminder reads one joined row into a Reminder. now is used to derive
// State. The pgx row scanner interface is satisfied by both pgx.Row and
// pgx.Rows.
func scanReminder(row pgx.Row, now time.Time) (Reminder, error) {
	var (
		r          Reminder
		notifiedAt *time.Time
		lastReason *string
	)
	if err := row.Scan(
		&r.ID, &r.IssueID, &r.UserID, &r.RemindAt, &notifiedAt,
		&r.SnoozeCount, &lastReason, &r.CreatedAt, &r.UpdatedAt,
		&r.IssueTitle, &r.IssueStatus, &r.ProjectID, &r.IssueType, &r.IssuePriority, &r.SpaceID,
	); err != nil {
		return Reminder{}, err
	}
	r.NotifiedAt = notifiedAt
	r.LastReason = lastReason
	r.State = DeriveState(r.RemindAt, now)
	return r, nil
}

// getByID loads one reminder joined to its issue summary, scoped to owner.
// Returns ErrNotFound when the row is missing or owned by someone else.
func (s *Store) getByID(ctx context.Context, q pgx.Tx, id, ownerID uuid.UUID, now time.Time) (Reminder, error) {
	row := q.QueryRow(ctx, `SELECT`+reminderColumns+`
  FROM reminders r
  JOIN issues i ON i.id = r.issue_id
 WHERE r.id = $1 AND r.user_id = $2`, id, ownerID)
	r, err := scanReminder(row, now)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reminder{}, ErrNotFound
	}
	if err != nil {
		return Reminder{}, fmt.Errorf("loading reminder: %w", err)
	}
	return r, nil
}

// Create inserts a new reminder for ownerID on issueID with remind_at =
// remindAt, emits a reminder.set event, and returns the hydrated row.
// remindAt is stored verbatim (callers normalise to UTC). note, when
// non-empty, is recorded as last_reason and in the event payload.
//
// The issue's project_id is resolved inside the tx so the event row is
// project-scoped. A missing issue surfaces as ErrIssueNotFound.
func (s *Store) Create(ctx context.Context, issueID string, ownerID uuid.UUID, remindAt time.Time, note string, actor Actor) (Reminder, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Reminder{}, fmt.Errorf("generating reminder id: %w", err)
	}
	remindAt = remindAt.UTC()

	var out Reminder
	err = withSerializableRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// Resolve project_id + assert the issue exists (FK would catch it
		// too, but a typed error gives the handler a clean 404).
		var projectID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIssueNotFound
		}
		if err != nil {
			return fmt.Errorf("resolving issue project: %w", err)
		}

		var lastReason any
		if note != "" {
			lastReason = note
		}
		// remind_at is read back: the column keeps microseconds, and the
		// event carries the STORED value, not the caller's (possibly
		// nanosecond) one.
		var storedRemindAt time.Time
		err = tx.QueryRow(ctx, `
INSERT INTO reminders (id, issue_id, user_id, remind_at, notified_at,
                       snooze_count, last_reason, created_at, updated_at)
VALUES ($1, $2, $3, $4, NULL, 0, $5, now(), now())
RETURNING remind_at`,
			id, issueID, ownerID, remindAt, lastReason).Scan(&storedRemindAt)
		if err != nil {
			return fmt.Errorf("inserting reminder: %w", err)
		}

		payload := map[string]any{
			"reminderId":  id.String(),
			"remindAt":    formatRemindAt(storedRemindAt),
			"ownerUserId": ownerID.String(),
		}
		if note != "" {
			payload["note"] = note
		}
		if _, err := writeReminderEvent(ctx, tx, EventReminderSet, issueID, projectID, actor, payload); err != nil {
			return err
		}

		out, err = s.getByID(ctx, tx, id, ownerID, time.Now().UTC())
		return err
	})
	if err != nil {
		return Reminder{}, err
	}
	return out, nil
}

// Snooze pushes a reminder's remind_at to newRemindAt, resets notified_at
// to NULL (so the next ring re-notifies), bumps snooze_count, records
// reason as last_reason, and emits reminder.snoozed. Scoped to ownerID —
// a reminder owned by another user returns ErrNotFound. Returns the
// updated, hydrated row.
func (s *Store) Snooze(ctx context.Context, id, ownerID uuid.UUID, newRemindAt time.Time, reason string, actor Actor) (Reminder, error) {
	newRemindAt = newRemindAt.UTC()
	var out Reminder
	err := withSerializableRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// Load current row (FOR UPDATE) — owner-scoped, also gives us the
		// previous remind_at + issue id for the event payload.
		var (
			issueID    string
			prevRemind time.Time
		)
		err := tx.QueryRow(ctx, `
SELECT issue_id, remind_at FROM reminders
 WHERE id = $1 AND user_id = $2
 FOR UPDATE`, id, ownerID).Scan(&issueID, &prevRemind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("loading reminder for snooze: %w", err)
		}

		var projectID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&projectID); err != nil {
			return fmt.Errorf("resolving issue project: %w", err)
		}

		var reasonArg any
		if reason != "" {
			reasonArg = reason
		}
		var (
			newCount       int
			storedRemindAt time.Time
		)
		if err := tx.QueryRow(ctx, `
UPDATE reminders
   SET remind_at    = $1,
       notified_at  = NULL,
       snooze_count = snooze_count + 1,
       last_reason  = $2,
       updated_at   = now()
 WHERE id = $3 AND user_id = $4
RETURNING snooze_count, remind_at`, newRemindAt, reasonArg, id, ownerID).Scan(&newCount, &storedRemindAt); err != nil {
			return fmt.Errorf("updating reminder: %w", err)
		}

		payload := map[string]any{
			"reminderId":       id.String(),
			"remindAt":         formatRemindAt(storedRemindAt),
			"previousRemindAt": formatRemindAt(prevRemind),
			"snoozeCount":      newCount,
			"ownerUserId":      ownerID.String(),
		}
		if reason != "" {
			payload["reason"] = reason
		}
		if _, err := writeReminderEvent(ctx, tx, EventReminderSnoozed, issueID, projectID, actor, payload); err != nil {
			return err
		}

		out, err = s.getByID(ctx, tx, id, ownerID, time.Now().UTC())
		return err
	})
	if err != nil {
		return Reminder{}, err
	}
	return out, nil
}

// Dismiss DELETEs a reminder row, scoped to ownerID, and emits
// reminder.dismissed. note (optional) is recorded on the event payload.
// Returns ErrNotFound when the row is missing or owned by someone else.
func (s *Store) Dismiss(ctx context.Context, id, ownerID uuid.UUID, note string, actor Actor) error {
	return withSerializableRetry(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			issueID  string
			remindAt time.Time
		)
		err := tx.QueryRow(ctx, `
DELETE FROM reminders
 WHERE id = $1 AND user_id = $2
RETURNING issue_id, remind_at`, id, ownerID).Scan(&issueID, &remindAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("deleting reminder: %w", err)
		}

		// The issue may have been hard-deleted between the row's life and
		// now (CASCADE would have removed the reminder, so in practice the
		// issue still exists here). Resolve project_id best-effort; on a
		// miss skip the event rather than fail the dismiss.
		var projectID uuid.UUID
		perr := tx.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&projectID)
		if errors.Is(perr, pgx.ErrNoRows) {
			return nil
		}
		if perr != nil {
			return fmt.Errorf("resolving issue project: %w", perr)
		}

		payload := map[string]any{
			"reminderId": id.String(),
			// sk-622.21 T018 (K14): the owner's personal reminder frame is
			// built from the event — its owner and the due time it cancels.
			"remindAt":    formatRemindAt(remindAt),
			"ownerUserId": ownerID.String(),
		}
		if note != "" {
			payload["note"] = note
		}
		_, err = writeReminderEvent(ctx, tx, EventReminderDismissed, issueID, projectID, actor, payload)
		return err
	})
}

// ListForUser returns the caller's reminders joined to their issue
// summary, filtered by derived state and ordered most-urgent-first:
// ringing rows first (by remind_at ASC — the oldest-due rings loudest),
// then armed rows (by remind_at ASC — the soonest-to-ring next). now is
// the clock used for both the filter and state derivation.
//
// Cross-project: the scope is reminder.user_id = ownerID. A reminder only
// exists because the owner created it on an issue they could see, and the
// member-removal cleanup in projects.RemoveMember deletes a user's reminders
// inline when they lose project membership — so an owner-scoped read never
// leaks an issue the caller can no longer see. No per-read visibility join
// needed.
func (s *Store) ListForUser(ctx context.Context, ownerID uuid.UUID, filter ListFilter, now time.Time) ([]Reminder, error) {
	// The ORDER BY puts ringing (remind_at <= now) before armed, then
	// remind_at ASC within each band. `(r.remind_at > $2)` is false (0)
	// for ringing and true (1) for armed, so ASC on that boolean yields
	// ringing-first.
	where := ""
	switch filter {
	case FilterRinging:
		where = ` AND r.remind_at <= $2`
	case FilterArmed:
		where = ` AND r.remind_at > $2`
	case FilterAll:
		where = ``
	default:
		where = ``
	}

	// sk-674.18 T029 (INV-11): only reminders on issues of a project the
	// caller sees now; a reminder of a lost project stays stored, unshown.
	sql := `SELECT` + reminderColumns + `
  FROM reminders r
  JOIN issues i ON i.id = r.issue_id
 WHERE r.user_id = $1
   AND ` + authz.VisibleNowSetExpr("$1::uuid", "i.project_id") + where + `
 ORDER BY (r.remind_at > $2) ASC, r.remind_at ASC, r.id ASC`

	rows, err := s.pool.Query(ctx, sql, ownerID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("listing reminders: %w", err)
	}
	defer rows.Close()

	out := make([]Reminder, 0, 16)
	for rows.Next() {
		r, err := scanReminder(rows, now)
		if err != nil {
			return nil, fmt.Errorf("scanning reminder: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating reminders: %w", err)
	}
	return out, nil
}

// ListForUserIssue returns every reminder the caller owns on a single
// issue, ordered by remind_at. Backs the MCP targeting rule for
// skein_remind (snooze / dismiss): when the caller passes
// issueId (not reminderId) and has exactly one reminder on that issue, the
// tool targets it; >1 forces the caller to disambiguate with reminderId.
func (s *Store) ListForUserIssue(ctx context.Context, ownerID uuid.UUID, issueID string, now time.Time) ([]Reminder, error) {
	rows, err := s.pool.Query(ctx, `SELECT`+reminderColumns+`
  FROM reminders r
  JOIN issues i ON i.id = r.issue_id
 WHERE r.user_id = $1 AND r.issue_id = $2
   AND `+authz.VisibleNowSetExpr("$1::uuid", "i.project_id")+`
 ORDER BY r.remind_at ASC, r.id ASC`, ownerID, issueID)
	if err != nil {
		return nil, fmt.Errorf("listing reminders for issue: %w", err)
	}
	defer rows.Close()

	out := make([]Reminder, 0, 4)
	for rows.Next() {
		r, err := scanReminder(rows, now)
		if err != nil {
			return nil, fmt.Errorf("scanning reminder: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating reminders: %w", err)
	}
	return out, nil
}

// CountRingingTasks returns the number of DISTINCT issues the caller has at
// least one ringing reminder on (remind_at <= now). Distinct-by-issue
// because the badge counts TASKS that are ringing, not individual
// reminders — two stacked reminders on one task is one ringing task.
func (s *Store) CountRingingTasks(ctx context.Context, ownerID uuid.UUID, now time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(DISTINCT r.issue_id)
  FROM reminders r
  JOIN issues i ON i.id = r.issue_id
 WHERE r.user_id = $1 AND r.remind_at <= $2
   AND `+authz.VisibleNowSetExpr("$1::uuid", "i.project_id"), ownerID, now.UTC()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting ringing tasks: %w", err)
	}
	return n, nil
}

// ListAndCount is the composite read backing GET /reminders + the
// skein_remind(action="list") tool: the filtered list plus the
// ringing-task badge
// count, computed against a single now() so the two never disagree.
func (s *Store) ListAndCount(ctx context.Context, ownerID uuid.UUID, filter ListFilter) (ListResponse, error) {
	now := time.Now().UTC()
	list, err := s.ListForUser(ctx, ownerID, filter, now)
	if err != nil {
		return ListResponse{}, err
	}
	count, err := s.CountRingingTasks(ctx, ownerID, now)
	if err != nil {
		return ListResponse{}, err
	}
	return ListResponse{Reminders: list, RingingTaskCount: count}, nil
}
