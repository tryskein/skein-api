package notifications

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/hierarchy"
	"skein-api/internal/person"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/status"
)

// pgSerializationFailure is SQLSTATE 40001, raised by Postgres when a
// SERIALIZABLE transaction has to abort because its read/write set
// conflicts with a concurrent transaction's commit. We treat it as a
// transient signal that the same invariant the SERIALIZABLE level
// protects (the per-user custom-watcher cap) was violated by the
// loser of the race — same business meaning as an explicit cap hit.
const pgSerializationFailure = "40001"

// Store is the read/write gateway for the watcher + notification tables.
// Goroutine-safe via the underlying *pgxpool.Pool.
type Store struct {
	pool *pgxpool.Pool
	// watchingWindow is the Watching window (sk-622.21, US-5): how far back
	// a notification still counts in the Watching badge and still makes a
	// card in the Watching column. Set by WithWatchingWindow; otherwise
	// DefaultWatchingWindow.
	watchingWindow time.Duration
}

// StoreOption configures a Store at construction.
type StoreOption func(*Store)

// DefaultWatchingWindow is the Watching window a Store reads with when
// WithWatchingWindow is not given: 21 days, the default the spec sets for
// WATCHING_FEED_WINDOW_DAYS (sk-622.21, US-5). The process reads the window
// from its loaded configuration instead (cmd/api composes the store with
// WithWatchingWindow); this default serves stores built without
// configuration, such as in tests.
const DefaultWatchingWindow = 21 * 24 * time.Hour

// WithWatchingWindow sets the Watching window the store's Watching reads use
// (sk-622.21, US-5): the badge (CountWatchingUnread, WatchingBadgeSQL) and
// the column (ListWatchingFeed). A notification is in the window when it was
// written no earlier than d ago; unread does not extend it. d must be
// positive — config.Load refuses a non-positive WATCHING_FEED_WINDOW_DAYS,
// so a non-positive value here is a wiring bug and panics.
func WithWatchingWindow(d time.Duration) StoreOption {
	if d <= 0 {
		panic(fmt.Sprintf("notifications: watching window must be positive, got %s", d))
	}
	return func(s *Store) { s.watchingWindow = d }
}

// NewStore wraps a pgxpool.Pool. One Store per process is sufficient.
func NewStore(pool *pgxpool.Pool, opts ...StoreOption) *Store {
	s := &Store{pool: pool, watchingWindow: DefaultWatchingWindow}
	for _, o := range opts {
		o(s)
	}
	return s
}

// WatchingWindow reports the Watching window this store reads with, so a
// reader that runs the badge statement on its own connection
// (internal/handler's header counts) uses the same window as the column.
func (s *Store) WatchingWindow() time.Duration {
	return s.watchingWindow
}

// ErrNotFound is returned by single-row mark/archive operations when the
// notification doesn't belong to the caller (or doesn't exist at all —
// we don't distinguish to avoid leaking ids of other users).
var ErrNotFound = errors.New("notification not found")

// ----------------- Issue watchers -----------------

// AddIssueWatch is the caller's own "watch this issue" (REST POST
// /issues/{id}/watch, MCP skein_watchers watch, the task monitor's
// subscription — R4): a MANUAL subscription. It deletes the caller's
// opt-out on the issue and subscribes (see AddIssueWatcherScoped,
// sk-622.21 US-1).
//
// subtree=true requests subtree scope (the issue plus every hierarchical
// descendant); subtree=false requests issue scope but never DOWNGRADES an
// existing subtree row — see AddIssueWatcherScoped for the monotonic rule.
//
// Runs SERIALIZABLE with a replay (K2): the write races the fan-out's read
// of the same subscription, and a 40001 is resolved here rather than
// returned to the caller.
//
// The handler verifies project visibility before calling this — we don't
// re-check membership here so admins can subscribe to issues in any
// project they want.
func (s *Store) AddIssueWatch(ctx context.Context, issueID string, userID uuid.UUID, subtree bool) error {
	scope := ScopeIssue
	if subtree {
		scope = ScopeSubtree
	}
	return withSerializableRetry(ctx, s.pool, "addIssueWatch", addIssueWatchMaxAttempts, func(tx pgx.Tx) error {
		return AddIssueWatcherScoped(ctx, tx, issueID, userID, ReasonManual, scope)
	})
}

// RemoveIssueWatch is the caller's own explicit "stop watching this issue"
// (REST DELETE /issues/{id}/watch, MCP skein_watchers unwatch) — sk-622.21,
// US-1, US-6:
//
//   - with a subscription: the row is deleted, the opt-out (caller, issue)
//     is recorded so the automatic paths do not re-subscribe the caller,
//     and the Watching card of this issue is cleared on all of the
//     caller's notifications;
//   - without one (a historical card — decision R3): only the card is
//     cleared, no opt-out.
//
// Idempotent: a repeated call finds no row, records nothing and clears
// nothing. The work is UnwatchExplicitTx, run SERIALIZABLE with a replay
// (K2) so that a fan-out writing card X concurrently cannot leave it behind
// (INV-6).
func (s *Store) RemoveIssueWatch(ctx context.Context, issueID string, userID uuid.UUID) error {
	err := withSerializableRetry(ctx, s.pool, "removeIssueWatch", removeIssueWatchMaxAttempts, func(tx pgx.Tx) error {
		_, err := UnwatchExplicitTx(ctx, tx, issueID, userID)
		return err
	})
	if err != nil {
		return fmt.Errorf("removing issue watch: %w", err)
	}
	return nil
}

// ListIssueWatchers returns the watchers of one issue, joined to users for
// rendering. Sorted by since ASC so the original subscriber appears first.
// Every row is a live subscription (R2's CHECK, INV-1).
func (s *Store) ListIssueWatchers(ctx context.Context, issueID string) ([]WatcherDTO, error) {
	sql := `
SELECT iw.user_id, u.username, u.name, iw.reasons, iw.since,
       ` + person.StatusInProjectExpr("u", "i.project_id") + `
  FROM issue_watchers iw
  JOIN users u ON u.id = iw.user_id
  JOIN issues i ON i.id = iw.issue_id
 WHERE iw.issue_id = $1
 ORDER BY iw.since ASC
`
	rows, err := s.pool.Query(ctx, sql, issueID)
	if err != nil {
		return nil, fmt.Errorf("listing issue watchers: %w", err)
	}
	defer rows.Close()

	out := make([]WatcherDTO, 0, 16)
	for rows.Next() {
		var w WatcherDTO
		var status *person.Status
		if err := rows.Scan(&w.UserID, &w.Username, &w.Name, &w.Reasons, &w.Since, &status); err != nil {
			return nil, fmt.Errorf("scanning watcher row: %w", err)
		}
		if status != nil {
			w.Status = *status
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ----------------- Project watchers -----------------

// AddProjectWatch idempotently inserts a project_watchers row with
// reasons={manual}. Sticky-unwatch is cleared (mirrors AddIssueWatch).
func (s *Store) AddProjectWatch(ctx context.Context, projectID, userID uuid.UUID) error {
	const sql = `
INSERT INTO project_watchers (project_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual']::text[], now())
ON CONFLICT (project_id, user_id) DO UPDATE
SET reasons = (
    SELECT array_agg(DISTINCT r)
      FROM unnest(array_append(array_remove(project_watchers.reasons, 'unwatched'), 'manual')) r
)
`
	_, err := s.pool.Exec(ctx, sql, projectID, userID)
	if err != nil {
		return fmt.Errorf("adding project watch: %w", err)
	}
	return nil
}

// RemoveProjectWatch sticky-unwatches the project. Same pattern as
// RemoveIssueWatch but on project_watchers.
func (s *Store) RemoveProjectWatch(ctx context.Context, projectID, userID uuid.UUID) error {
	const sql = `
INSERT INTO project_watchers (project_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['unwatched']::text[], now())
ON CONFLICT (project_id, user_id) DO UPDATE
SET reasons = (
    SELECT array_agg(DISTINCT r)
      FROM unnest(array_append(array_remove(project_watchers.reasons, 'manual'), 'unwatched')) r
)
`
	_, err := s.pool.Exec(ctx, sql, projectID, userID)
	if err != nil {
		return fmt.Errorf("removing project watch: %w", err)
	}
	return nil
}

// ListProjectWatchers returns watchers for one project; sticky-unwatched
// rows are excluded.
func (s *Store) ListProjectWatchers(ctx context.Context, projectID uuid.UUID) ([]WatcherDTO, error) {
	sql := `
SELECT pw.user_id, u.username, u.name, pw.reasons, pw.since,
       ` + person.StatusInProjectExpr("u", "pw.project_id") + `
  FROM project_watchers pw
  JOIN users u ON u.id = pw.user_id
 WHERE pw.project_id = $1
   AND NOT 'unwatched' = ANY(pw.reasons)
 ORDER BY pw.since ASC
`
	rows, err := s.pool.Query(ctx, sql, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing project watchers: %w", err)
	}
	defer rows.Close()

	out := make([]WatcherDTO, 0, 16)
	for rows.Next() {
		var w WatcherDTO
		var status *person.Status
		if err := rows.Scan(&w.UserID, &w.Username, &w.Name, &w.Reasons, &w.Since, &status); err != nil {
			return nil, fmt.Errorf("scanning watcher row: %w", err)
		}
		if status != nil {
			w.Status = *status
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ----------------- Notifications inbox -----------------

// ListInboxArgs captures filters for GET /notifications. Scope drives
// the WHERE on read_at/archived_at; ProjectID narrows further.
//
// sk-303.13 / T017 — Scope='outbox' is GONE. It was the third,
// event-shaped view of "tasks I delegated", and it disagreed by
// construction with the Delegated task counter it duplicated: one counted
// events and went dark on read, the other counted tasks. The
// notifications themselves are untouched and keep feeding Mentions and
// Watching — what was removed is the way to look at them as an Outbox.
// The global My-assigned lens with the similar internal name is a
// different thing and is not affected.
//
// sk-38 — Scope='mentions' returns the unread mention-typed
// notifications (type IN ('comment.mention','issue.mention')). Drives
// the shell's Mentions tab; each row carries a MentionContext snippet
// extracted from the underlying comment/issue field body.
type ListInboxArgs struct {
	UserID    uuid.UUID
	Scope     string // 'unread' | 'all' | 'archived' | 'mentions'
	ProjectID *uuid.UUID
	Cursor    *int64 // last seen id (paging in id DESC)
	Limit     int
}

// notifRowExtraCols are the two columns the human notification rows add
// after comment_project_prefix (sk-674.18 T029): the short id of the space
// of the event's project, and the actor's person marks there (T027).
// notifSpaceCol is the space column alone — for the in-card timelines of
// Watching (HydrateWatchingRecentEvents), whose plan gate (SC-002) a users
// read per event would trip on; the card's own rows carry the actor marks
// through the Inbox, not here.
var notifSpaceCol = `,
       (SELECT s_sp.short_id FROM projects p_sp JOIN spaces s_sp ON s_sp.id = p_sp.space_id
         WHERE p_sp.id = e.project_id) AS space_short`

var notifRowExtraCols = `,
       (SELECT s_sp.short_id FROM projects p_sp JOIN spaces s_sp ON s_sp.id = p_sp.space_id
         WHERE p_sp.id = e.project_id) AS space_short,
       (SELECT ` + person.StatusInProjectExpr("u_act", "e.project_id") + `
          FROM users u_act WHERE u_act.id = e.actor_user_id) AS actor_status`

// ListInbox runs the paginated inbox query.
//
// Reason filtering happens at WRITE time (sk-303.13 / INV-1, INV-2): a row
// exists only because some channel of its reason was on for this recipient.
// What the read side still has to answer is WHICH channel — a row minted
// for push alone is not part of anybody's feed. `n.feed_channel` is that
// membership flag and it is the whole of the read filter now: no surface
// carries a reason filter of its own any more (the old
// user_notification_prefs.notify_event_types whitelist predicate lived
// here and is gone), and no surface may omit feed_channel — omitting it
// leaks push-only rows (INV-11).
//
// We hydrate actor + entity in a single SQL via JOINs to events / users /
// issues / projects so the response is ready-to-render.
func (s *Store) ListInbox(ctx context.Context, args ListInboxArgs) (*NotificationListResult, error) {
	if args.Limit < 1 {
		args.Limit = 50
	}
	if args.Limit > 100 {
		args.Limit = 100
	}

	clauses := []string{"n.user_id = $1"}
	params := []any{args.UserID}
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}

	switch args.Scope {
	case "unread":
		clauses = append(clauses, "n.read_at IS NULL", "n.archived_at IS NULL")
	case "archived":
		clauses = append(clauses, "n.archived_at IS NOT NULL")
	case "all", "":
		clauses = append(clauses, "n.archived_at IS NULL")
	case "mentions":
		// sk-38 — unread mention-typed notifications: rows where the
		// caller was @-mentioned. The frontend Mentions tab consumes
		// this; per-row MentionContext snippets are populated below in a
		// second pass.
		clauses = append(clauses,
			"n.read_at IS NULL",
			"n.archived_at IS NULL",
			"n.type IN ('comment.mention', 'issue.mention')",
		)
	default:
		return nil, fmt.Errorf("invalid scope %q", args.Scope)
	}

	if args.ProjectID != nil {
		clauses = append(clauses, "n.project_id = "+addParam(*args.ProjectID))
	}
	if args.Cursor != nil {
		clauses = append(clauses, "n.id < "+addParam(*args.Cursor))
	}

	// Feed-channel membership — the only reason-derived predicate the read
	// side has left (INV-2/INV-11). A push-only row belongs to no feed.
	clauses = append(clauses, "n.feed_channel")
	// sk-674.18 T029 (INV-11): only rows of a project the caller sees now.
	clauses = append(clauses, authz.VisibleNowSetExpr("$1::uuid", "n.project_id"))

	limitParam := addParam(args.Limit + 1)

	sql := `
SELECT n.id, n.type, n.created_at, n.read_at, n.archived_at,
       e.actor, e.actor_user_id, e.actor_type,
       e.entity_type, e.entity_id, n.project_id,
       i.title, pr.id_prefix,
       e.field, e.old_data, e.new_data, e.payload,
       ci.id   AS comment_issue_id,
       ci.title AS comment_issue_title,
       cpr.id_prefix AS comment_project_prefix` + notifRowExtraCols + `
  FROM notifications n
  JOIN events e ON e.id = n.event_id
  LEFT JOIN issues i  ON e.entity_type = 'issue' AND i.id = e.entity_id
  LEFT JOIN projects pr ON pr.id = n.project_id
  -- Comment notifications: e.entity_id is the comment uuid; jump through
  -- comments to the parent issue so we can show its title and the
  -- project's id_prefix in the inbox row. We cast e.entity_id (text) to
  -- uuid so the join uses comments.id PK index — casting the indexed
  -- column to text would force a seq scan on every inbox page load.
  --
  -- sk-37 — the cast is wrapped in CASE so non-comment entity_types
  -- (which carry non-UUID hierarchical issue ids like "demo-42") never
  -- hit the cast. Earlier the planner sometimes evaluated the cast
  -- eagerly under nested-loop plans and crashed with "invalid input
  -- syntax for type uuid". Mirrors the guard used in
  -- me.go::countNotificationBuckets.
  LEFT JOIN comments c ON e.entity_type = 'comment'
                       AND c.id = (CASE WHEN e.entity_type = 'comment'
                                        THEN e.entity_id::uuid
                                        ELSE NULL END)
  LEFT JOIN issues ci ON ci.id = c.issue_id
  LEFT JOIN projects cpr ON cpr.id = ci.project_id
 WHERE ` + joinAnd(clauses) + `
 ORDER BY n.id DESC
 LIMIT ` + limitParam

	rows, err := s.pool.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("listing notifications: %w", err)
	}
	defer rows.Close()

	// sk-38 — track mention-typed items so we can hydrate MentionContext
	// snippets in a second SELECT pass.
	var mentionItems []mentionMeta

	items := make([]NotificationDTO, 0, args.Limit)
	for rows.Next() {
		var (
			n                                 NotificationDTO
			actorName                         string
			actorUserID                       *uuid.UUID
			actorType                         string
			entityType, entityID              string
			projectID                         *uuid.UUID
			issueTitle, projectIDPrefix       *string
			fieldName                         *string
			oldData, newData, payload         []byte
			commentIssueID, commentIssueTitle *string
			commentProjectPrefix              *string
			spaceShort                        *string
			actorStatus                       *person.Status
		)
		if err := rows.Scan(
			&n.ID, &n.Type, &n.CreatedAt, &n.ReadAt, &n.ArchivedAt,
			&actorName, &actorUserID, &actorType,
			&entityType, &entityID, &projectID,
			&issueTitle, &projectIDPrefix,
			&fieldName, &oldData, &newData, &payload,
			&commentIssueID, &commentIssueTitle, &commentProjectPrefix,
			&spaceShort, &actorStatus,
		); err != nil {
			return nil, fmt.Errorf("scanning notification row: %w", err)
		}

		hydrateNotificationDTO(&n,
			actorName, actorUserID, actorType,
			entityType, entityID, projectID,
			issueTitle, projectIDPrefix,
			fieldName, oldData, newData, payload,
			commentIssueID, commentIssueTitle, commentProjectPrefix)
		if spaceShort != nil {
			n.SpaceID = *spaceShort
		}
		if actorStatus != nil {
			n.Actor.Status = *actorStatus
		}

		// sk-38 — register mention rows for second-pass snippet hydration.
		// Use the ORIGINAL event entity_type/entity_id (not n.Entity, which
		// has been re-pointed to the parent issue for comment events).
		if n.Type == "comment.mention" || n.Type == "issue.mention" {
			mentionItems = append(mentionItems, mentionMeta{
				idx:          len(items),
				evtType:      n.Type,
				eventEntType: entityType,
				eventEntID:   entityID,
				payload:      append([]byte(nil), payload...),
			})
		}

		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// sk-38 — hydrate MentionContext for mention-typed rows. Resolves the
	// caller's username once (cheap PK lookup), then issues two batched
	// SELECTs (comments + per-field issues) over the collected mention
	// metadata. Failures in any of these are non-fatal — the row simply
	// keeps MentionContext empty and the UI falls back to a plain card.
	if len(mentionItems) > 0 {
		callerUsername, err := s.lookupUsername(ctx, args.UserID)
		if err == nil && callerUsername != "" {
			s.hydrateMentionContext(ctx, items, mentionItems, callerUsername)
		}
	}

	hasMore := false
	if len(items) > args.Limit {
		hasMore = true
		items = items[:args.Limit]
	}
	res := &NotificationListResult{Items: items, HasMore: hasMore}
	if hasMore && len(items) > 0 {
		c := encodeIDCursor(items[len(items)-1].ID)
		res.NextCursor = &c
	}
	return res, nil
}

// hydrateNotificationDTO fills the actor / entity / field / summary
// portions of a NotificationDTO from the raw column values scanned out of
// the shared inbox projection (column list + JOINs in ListInbox /
// the removed per-issue listing). Centralises the issue-vs-comment re-pointing and
// the old/new JSON decode so the two list paths can't drift apart.
//
// The caller is responsible for scanning n.ID / n.Type / n.CreatedAt /
// n.ReadAt / n.ArchivedAt before calling — those are assigned directly
// during Scan and left untouched here.
func hydrateNotificationDTO(
	n *NotificationDTO,
	actorName string, actorUserID *uuid.UUID, actorType string,
	entityType, entityID string, projectID *uuid.UUID,
	issueTitle, projectIDPrefix *string,
	fieldName *string, oldData, newData, payload []byte,
	commentIssueID, commentIssueTitle, commentProjectPrefix *string,
) {
	n.Actor = ActorDTO{
		Username: actorName,
		UserID:   actorUserID,
		Type:     actorType,
	}

	// For issue events the entity is the issue itself; for comment
	// events the inbox row should still link to the parent issue
	// (more useful UX than linking to a transient comment id).
	switch entityType {
	case "issue":
		n.Entity = EntityDTO{
			Type:      "issue",
			ID:        entityID,
			ProjectID: projectID,
		}
		if issueTitle != nil {
			n.Entity.Title = *issueTitle
		}
		if projectIDPrefix != nil {
			n.Entity.ProjectIDPrefix = *projectIDPrefix
		}
	case "comment":
		n.Entity = EntityDTO{
			Type:      "issue",
			ProjectID: projectID,
		}
		if commentIssueID != nil {
			n.Entity.ID = *commentIssueID
		}
		if commentIssueTitle != nil {
			n.Entity.Title = *commentIssueTitle
		}
		if commentProjectPrefix != nil {
			n.Entity.ProjectIDPrefix = *commentProjectPrefix
		}
	default:
		n.Entity = EntityDTO{
			Type:      entityType,
			ID:        entityID,
			ProjectID: projectID,
		}
	}

	n.Field = fieldName
	if len(oldData) > 0 {
		var v any
		if err := json.Unmarshal(oldData, &v); err == nil {
			n.OldValue = v
		}
	}
	if len(newData) > 0 {
		var v any
		if err := json.Unmarshal(newData, &v); err == nil {
			n.NewValue = v
		}
	}

	// The close outcome (sk-489, US-3) — ONLY on status-change rows, and only
	// when the event payload carries it. There is no column in the notifications table:
	// the value is materialised here from events.payload, because the outcome is
	// a property of the event, not of the notification row, and a second copy in the table
	// would diverge from the first on every outcome edit.
	//
	// Why not on issue.outcome_changed: there the change is carried by the regular
	// Field / OldValue / NewValue (field = "closeOutcome"), and a single value
	// next to the "was -> became" pair would be a third, incomplete copy of the same
	// news (see the doc block of NotificationDTO.CloseOutcome).
	if n.Type == "issue.status_changed" {
		if outcome := closeOutcomeFromPayload(payload); outcome != "" {
			n.CloseOutcome = &outcome
		}
	}

	entityRef := n.Entity.ID
	if n.Entity.Title != "" {
		entityRef = n.Entity.ID + " (" + n.Entity.Title + ")"
	}
	n.Summary = FormatSummary(n.Type, actorName, entityRef, payload)
}

// ----------------- sk-142.4 — agent monitor poll -----------------

// PollParams is the input to PollForMonitor. Scope (issue + subtree),
// EventTypes and UserID all come from the validated monitor-token claims
// (never from the request query) so an agent cannot widen its own scope.
// SinceTS/SinceID are the optional watermark cursor echoed back by the
// runner; SafetyLag is the config-driven commit-order fence.
type PollParams struct {
	// UserID is the credential owner — the recipient whose notifications
	// the poll returns.
	UserID uuid.UUID

	// ProjectID is the credential's project and the project-isolation
	// predicate (e.project_id = ProjectID, INV-6). Hierarchical issue ids are
	// globally unique so scope-by-issue already isolates projects, but under
	// the events source this equality is load-bearing: it drops any event
	// whose project_id is a foreign project OR NULL even when its entity_id
	// happens to match the scoped issue, so the monitor never leaks a row
	// outside the token's project subtree.
	ProjectID uuid.UUID

	// ScopeIssueID is the hierarchical issue the monitor targets. Always
	// set (monitor_credentials.scope_issue_id is NOT NULL in v1).
	ScopeIssueID string

	// ScopeSubtree widens matching from the exact issue to the issue plus
	// every issue that has it in its chain of canonical parents at poll
	// time (sk-622.21 T013, US-3 — the hierarchy, not the id prefix).
	ScopeSubtree bool

	// EventTypes is the notification-type whitelist (n.type = ANY). Empty
	// matches nothing (the mint path guarantees at least {comment.created}).
	EventTypes []string

	// SinceTS/SinceID are the watermark cursor lower bound. Both nil =
	// live-start: the server anchors the cursor at DB-side now() so the
	// first poll returns no historical backlog. Both must be set together
	// (the handler enforces this); the store treats "SinceTS == nil" as
	// live-start.
	SinceTS *time.Time
	SinceID *int64

	// Floor is the server-owned lower-bound ("not-before") for the cursor —
	// the credential's mint time (claims.CreatedAt). The effective lower
	// bound is the LATER of (client cursor, Floor): a forged old cursor
	// (e.g. since_ts=epoch, since_id=0) is clamped up to Floor so a monitor
	// token can never backfill notifications created before it was minted.
	// The zero value disables the floor (no clamp) — used by callers that
	// have no mint-time concept.
	Floor time.Time

	// Limit caps the page size. Caller clamps to a sane ceiling.
	Limit int

	// SafetyLag holds rows back until they are at least this old (DB-side
	// now() - SafetyLag), bounding the bigserial commit-order skip race.
	SafetyLag time.Duration

	// ExcludeSession is the agent-declared current MCP session (sk-164 FR-2,
	// the `exclude_session` rebind query-param). When non-empty the poll
	// suppresses events whose actor_session_id equals it — server best-effort
	// echo suppression that only ever NARROWS the owner's own output (INV-9).
	// Empty = no suppression (the default; reader-side self-skip remains
	// authoritative). The consuming filter predicate is wired in T021; this
	// field is the plumbing added in T002.
	ExcludeSession string
}

// PollResult is the output of PollForMonitor: the page of notifications in
// ascending (created_at, id) order plus the cursor the runner echoes on the
// next poll. The cursor is the LAST emitted (created_at, id) tuple in sort
// order; when the page is empty it is the lower bound that was used (the
// live-start anchor on the first poll, or the unchanged incoming cursor on
// a quiet subsequent poll) so the runner always advances correctly.
type PollResult struct {
	Items    []NotificationDTO
	CursorTS time.Time
	CursorID int64
}

// PollForMonitor backs GET /agent/v1/notifications/poll (the agent
// task-monitor data plane, spec §6). It returns the fresh, in-scope,
// in-type events (sk-164: source is `events`, not `notifications`) in
// ascending (occurred_at, id) order, newest cursor last.
//
// Scoping is comment-aware. A notification's event is normalised to an
// issue id under a guard (mirrors MarkReadByEntity):
//   - issue events  → e.entity_id is the hierarchical issue id directly;
//   - comment events → join comments c (with e.entity_id::uuid cast guarded
//     by entity_type='comment' so non-comment hierarchical ids never hit the
//     cast) → c.issue_id.
//
// The normalised id must equal ScopeIssueID, or (subtree) have ScopeIssueID
// in its chain of canonical parents at poll time (sk-622.21 T013, US-3,
// INV-3 — hierarchy.IsAncestorExpr; not an id prefix). Any other entity_type matches neither branch and is silently
// dropped — the poll only delivers what it can normalise, which is exactly
// the set the mint path constrains EventTypes to.
//
// Cursor / live-start. The lower bound is a row-value comparison
// (occurred_at, id) > (boundTS, boundID):
//   - cursor present  → bound = the supplied (SinceTS, SinceID);
//   - cursor absent   → live-start: bound = DB-side now() with id 0, so the
//     first poll returns NO backlog (every already-committed eligible row
//     has occurred_at <= now() < the watermark, and all are <= the anchor so
//     the strict ">" excludes them). The anchor is returned as the cursor so
//     the next poll continues from it.
//
// Floor (server-owned not-before). The effective lower bound is clamped up
// to (Floor, 0) whenever the resolved bound is at or before Floor — the
// credential's mint time. A monitor token is live-start by contract, so a
// presenter that forges an old cursor (since_ts=epoch, since_id=0) must not
// be able to backfill in-scope notifications committed before the token was
// minted. The clamp enforces this without widening any other predicate; a
// zero Floor disables it. The clamp runs on occurred_at (the events source),
// so INV-3 live-start/floor holds unchanged after the cutover.
//
// Watermark. Only rows with occurred_at <= now() - SafetyLag are eligible
// (DB-side now(), so no app/DB clock skew). This bounds the commit-order
// race a plain id-cursor would suffer. Read goes to the primary via the
// shared pool — never a lagging replica (apply-lag > SafetyLag would
// reproduce the very skip the fence prevents).
//
// The wire DTO is hydrated identically to ListInbox via
// hydrateNotificationDTO so /agent/v1 rows match /api/v1/notifications.
// pollForMonitorSQL is the single source of truth for the monitor poll
// query, extracted to package scope so the index-coverage test
// (poll_for_monitor_test.go) can EXPLAIN the byte-for-byte identical
// statement the store executes — the plan it asserts cannot drift from the
// query that actually runs.
//
// The driving table is `events e` (sk-164): the monitor is an event-tail
// over the token's scope, NOT a projection of the personal `notifications`
// inbox. Reading the primary audit log removes the dependency on fan-out
// materialisation, whose user_id self-exclusion is exactly what muted the
// monitor between two sessions of one owner (ADR-DRAFT, INV-1). Eligibility
// is therefore scope + type (minus the mention family, T012) + the owner's
// current visibility (checked fail-closed in the handler, INV-8) — NEVER a
// user_id match. The WHERE is the project isolation equality (INV-6), the
// type whitelist, the SafetyLag watermark and the row-value cursor lower
// bound over (occurred_at, id), with the comment-aware scope predicate and
// ORDER BY (occurred_at, id). The selective driver empirically is the
// occurred_at watermark window served by idx_events_occurred (000001:70);
// scope predicates apply as bounded row-filters over that narrow window (no
// Seq Scan) — confirmed by EXPLAIN on the poll query, so no new
// index is required. The dead idx_notifications_monitor_poll (000032) is no
// longer used by the monitor and is left in place (dropping it needs a
// migration) — human inbox has its own index.
// monitorCommentJoinSQL and monitorEventAdmissionSQL are the shared
// admission predicate of the monitor (sk-579 T012): which events a monitor
// delivers — project, type whitelist minus the mention family, the
// exclude_session rebind, and the comment-aware scope. pollForMonitorSQL (the
// page) and finishMonitorPollSQL (the debt of the root-closed rule, plan
// "Root closure and debt") are glued from these very constants, so "what
// the poll delivers» and «what the monitor still owes» cannot drift apart.
// Both statements share the parameter layout: $1 event types, $5 scope issue,
// $6 subtree, $8 project, $9 exclude_session; $7 is the page LIMIT in the
// poll and the credential id in the finish statement.
const monitorCommentJoinSQL = `
  -- sk-242.1 T011: non-throwing comment join. PostgreSQL may reorder AND
  -- terms in JOIN ON, so a bare entity_id::uuid can cast hierarchical
  -- issue ids (sk-42) and abort the page. CASE short-circuits: only the
  -- THEN branch casts, and only when entity_type is comment AND entity_id
  -- looks like a UUID (regex). Corrupt comment entity_ids yield NULL join
  -- key, page continues (US-2). Soft-deleted comments still join (row
  -- present, deleted_at set) — scope + commentId work; body/deleted_at
  -- projected for Go-side preview rules (INV-4).
  LEFT JOIN comments c ON e.entity_type = 'comment'
                       AND c.id = (CASE
                           WHEN e.entity_type = 'comment'
                            AND e.entity_id ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
                           THEN e.entity_id::uuid
                           ELSE NULL
                       END)`

// monitorSubtreeIssueExpr / monitorSubtreeCommentExpr: "the scope root $5 is
// a proper ancestor of the event's issue" for an issue event and for a
// comment event — hierarchy.IsAncestorExpr, the shared definition of INV-3.
// The expression cannot log a loop; finishMonitorPollSQL reports it (US-3).
var (
	monitorSubtreeIssueExpr   = hierarchy.IsAncestorExpr("$5::text", "e.entity_id")
	monitorSubtreeCommentExpr = hierarchy.IsAncestorExpr("$5::text", "c.issue_id")
)

var monitorEventAdmissionSQL = `
 WHERE e.project_id = $8
   AND e.type = ANY($1)
   -- INV-7 / US-6: the monitor is a BROADCAST channel, so it must never
   -- deliver the mention family — directed point-to-point pings whose payload
   -- names a single recipient (mentionedUserId), fanned out one row per
   -- addressee by WriteMentionNotification, not to a watcher pool. Under the
   -- events source an opt-in "issue." whitelist would otherwise sweep in
   -- issue.mention; excluding the whole closed set (not just the one type the
   -- mint whitelist can reach today) keeps this forward-compatible with any
   -- future directed type. The literals mirror mentionEventTypes (fanout.go),
   -- the source of truth; init()'s drift guard fails fast if they diverge.
   AND e.type NOT IN ('issue.mention', 'comment.mention')
   -- FR-2 / INV-9 (rebind, server best-effort echo suppression). $9 is the
   -- agent's re-declared current MCP session (exclude_session query-param).
   -- This predicate can ONLY narrow: with $9='' (the default, no rebind) the
   -- first disjunct is TRUE for every row so nothing is dropped; a session-less
   -- actor (actor_session_id IS NULL — a human owner in the Web UI, or system)
   -- is NEVER cut (US-5, INV-2), because echo is strict-equality of two
   -- NON-empty sessions; only a row whose non-null actor_session_id EQUALS $9
   -- is suppressed. So for any value (valid / empty / forged / foreign) the
   -- result is a subset of the no-rebind result — never a superset (INV-9).
   -- Placed as its own AND line, deliberately AFTER the mention NOT-IN list so
   -- the init() drift-guard's NOT-IN slice is unaffected.
   AND ($9::text = '' OR e.actor_session_id IS NULL OR e.actor_session_id <> $9::text)
   -- sk-622.21 T013 (US-3, INV-3): the subtree is the canonical hierarchy
   -- AT POLL TIME — the event's issue is in scope when it IS the root, or
   -- (subtree) when the root is in its chain of canonical parents
   -- (hierarchy.IsAncestorExpr, the one definition shared with the fan-out
   -- and the Watching card). Not an id prefix: after a reparent the old
   -- root stops seeing the moved issue's new events and the new root starts.
   AND (
       (e.entity_type = 'issue'
            AND (e.entity_id = $5 OR ($6 AND ` + monitorSubtreeIssueExpr + `)))
       OR
       (e.entity_type = 'comment'
            AND (c.issue_id = $5 OR ($6 AND ` + monitorSubtreeCommentExpr + `)))
   )`

var pollForMonitorSQL = `
SELECT e.id, e.type, e.occurred_at,
       e.actor, e.actor_user_id, e.actor_type,
       e.entity_type, e.entity_id, e.project_id,
       e.actor_session_id,
       i.title, pr.id_prefix,
       e.field, e.old_data, e.new_data, e.payload,
       ci.id   AS comment_issue_id,
       ci.title AS comment_issue_title,
       cpr.id_prefix AS comment_project_prefix,
       -- Soft-deleted body never crosses PG→process (INV-4); live body
       -- still full text — Go rune-truncates to BodyPreviewMaxRunes.
       CASE WHEN c.deleted_at IS NOT NULL THEN NULL ELSE c.body END AS comment_body,
       c.deleted_at AS comment_deleted_at
  FROM events e
  LEFT JOIN issues i  ON e.entity_type = 'issue' AND i.id = e.entity_id
  LEFT JOIN projects pr ON pr.id = e.project_id` + monitorCommentJoinSQL + `
  LEFT JOIN issues ci ON ci.id = c.issue_id
  LEFT JOIN projects cpr ON cpr.id = ci.project_id` + monitorEventAdmissionSQL + `
   AND e.occurred_at <= now() - $2::interval
   AND (e.occurred_at, e.id) > ($3::timestamptz, $4::bigint)
 ORDER BY e.occurred_at, e.id
 LIMIT $7`

// finishMonitorPollSQL is the one extra statement of a successful poll
// (sk-579 T012, NFR "no more than one call"). For the credential $7 it
// stamps the successful poll and decides the root-closed rule of INV-12 by
// the predicates "debt repaid" and "closure recorded" of the
// root-closure design; they are not restated here. Inputs beyond the shared admission layout: $2 SafetyLag, ($3, $4)
// the cursor pair this poll RETURNS. The decision is sticky: once
// root_closed_at is set it is never cleared (a reopen does not revive the
// monitor). issued_session_id and token_file_path are not touched, so a
// credential minted before sk-579 keeps both NULL.
// A credential revoked (monitor_stop) after the door accepted this request is
// not updated: the poll is not recorded and does not decide root_closed, so
// a stopped monitor ends on the next poll's 401, not on root_closed (US-6).
//
// RETURNING: rootClosed — the flag is stored; newly — this very statement
// stored it (root_closed_at equals this statement's now()); cycle_at /
// cycle_chain — the hierarchy-loop report of the scope root (US-3, edge 7).
//
// Why the loop report rides here, and why on the ROOT's chain. With
// subtree=true the admission predicate walks the canonical-parent chain of
// each event's issue through hierarchy.IsAncestorExpr, which cannot log. A
// chain is a functional path: once it enters a loop it never leaves it. So a
// walk that reaches the root and meets a loop has the loop in the root's own
// chain (the root sits before the loop or on it) — walking the root's
// ancestors finds exactly the loops that bear on what this monitor admits. A
// chain that loops without reaching the root admits nothing here; the
// fan-out of that same event walks it and reports it
// (fanout.go#selectFanOutCandidateSQL). The root walk is seeded only when
// $6 (subtree) is set, and it is part of this one statement, so the poll keeps
// its fixed number of statements (SC-013). This statement answers exactly one
// row per successful poll, so the report never depends on the page.
var finishMonitorPollSQL = `
WITH RECURSIVE ` + hierarchy.AncestorsCTE("root_anc", "SELECT $5::text AS issue_id WHERE $6::boolean") + `,
root AS (
    SELECT i.closed_at
      FROM issues i
     WHERE i.id = $5
       AND i.status = 'closed'
       AND i.closed_at IS NOT NULL
), owed AS (
    SELECT EXISTS (
        SELECT 1
          FROM events e` + monitorCommentJoinSQL + monitorEventAdmissionSQL + `
           AND (e.occurred_at, e.id) > ($3::timestamptz, $4::bigint)
           AND e.occurred_at <= (SELECT closed_at FROM root)
    ) AS debt
), verdict AS (
    SELECT EXISTS (SELECT 1 FROM root)
       AND NOT (SELECT debt FROM owed)
       AND ((SELECT closed_at FROM root) <= $3::timestamptz
            OR (SELECT closed_at FROM root) <= now() - $2::interval) AS fixed
)
UPDATE monitor_credentials mc
   SET last_poll_succeeded_at = now(),
       root_closed_at = COALESCE(mc.root_closed_at,
                                 CASE WHEN (SELECT fixed FROM verdict) THEN now() END)
 WHERE mc.id = $7
   AND mc.revoked_at IS NULL
RETURNING mc.root_closed_at IS NOT NULL,
          COALESCE(mc.root_closed_at = now(), false),
          (SELECT a.ancestor_id FROM root_anc a WHERE a.is_cycle LIMIT 1),
          CASE WHEN EXISTS (SELECT 1 FROM root_anc a WHERE a.is_cycle)
               THEN (SELECT array_agg(a.ancestor_id ORDER BY a.depth)
                       FROM root_anc a WHERE NOT a.is_cycle)
          END`

// init is the drift guard for the mention-exclusion predicate (T012, INV-7):
// mentionEventTypes (fanout.go) is the single source of truth for what the
// product treats as a directed mention. pollForMonitorSQL hardcodes those
// literals in its `e.type NOT IN (...)` clause rather than interpolating the
// map — the const must stay byte-stable so the index-coverage EXPLAIN test can
// assert it verbatim, and a var built from a Go map would reorder
// nondeterministically. To make the reuse real instead of a comment, this
// guard fails fast at startup if any mention type is NOT covered by the SQL,
// so a future directed type added to mentionEventTypes can't silently start
// broadcasting through the monitor.
func init() {
	// Check each mentionEventTypes key against ONLY the `e.type NOT IN (...)`
	// predicate, not the whole const. The const embeds SQL comments that name
	// the mention types in prose (and any future comment could quote one), so
	// a whole-string strings.Contains would false-pass even if a real literal
	// were dropped from the predicate. Slice out the exact IN-list — from the
	// `e.type NOT IN (` anchor to its closing `)` — and require each key there.
	const anchor = "e.type NOT IN ("
	start := strings.Index(pollForMonitorSQL, anchor)
	if start < 0 {
		panic("notifications: pollForMonitorSQL has no `e.type NOT IN (` mention-exclusion predicate — " +
			"the mention family is no longer excluded from the monitor broadcast (T012, INV-7)")
	}
	listStart := start + len(anchor)
	rel := strings.Index(pollForMonitorSQL[listStart:], ")")
	if rel < 0 {
		panic("notifications: pollForMonitorSQL `e.type NOT IN (` predicate is unterminated (T012, INV-7)")
	}
	inList := pollForMonitorSQL[listStart : listStart+rel]
	for t := range mentionEventTypes {
		if !strings.Contains(inList, "'"+t+"'") {
			panic("notifications: pollForMonitorSQL NOT IN predicate is missing mention-exclusion literal '" +
				t + "' — mentionEventTypes drifted from it (T012, INV-7)")
		}
	}
}

// ErrMonitorCredentialGone is returned by FinishMonitorPoll when the
// credential row no longer exists or was revoked between authentication and
// the end of the poll (cleanup, key cascade, monitor_stop): the poll cannot be
// recorded, so it is not a success. An expected race, not a fault.
var ErrMonitorCredentialGone = errors.New("monitor credential gone")

// FinishResult is what FinishMonitorPoll decided for one successful poll.
type FinishResult struct {
	// RootClosed is the stored root-closed flag (sticky): the runner stops
	// with root_closed when it sees it.
	RootClosed bool
	// NewlyClosed is true only on the poll that stored the flag.
	NewlyClosed bool
}

// FinishMonitorPoll runs finishMonitorPollSQL for the credential credentialID
// after a successful PollForMonitor (sk-579 T012): it records the successful
// poll and applies the root-closed rule of INV-12 with the same admission
// predicate as the page (p supplies scope, types, project, exclude_session
// and SafetyLag; cursorTS/cursorID is the cursor the poll returns). One
// statement, one round trip.
func (s *Store) FinishMonitorPoll(ctx context.Context, p PollParams, credentialID uuid.UUID,
	cursorTS time.Time, cursorID int64) (FinishResult, error) {
	var (
		res        FinishResult
		cycleAt    *string
		cycleChain []string
	)
	err := s.pool.QueryRow(ctx, finishMonitorPollSQL,
		p.EventTypes, p.SafetyLag, cursorTS, cursorID,
		p.ScopeIssueID, p.ScopeSubtree, credentialID, p.ProjectID,
		p.ExcludeSession,
	).Scan(&res.RootClosed, &res.NewlyClosed, &cycleAt, &cycleChain)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FinishResult{}, ErrMonitorCredentialGone
		}
		return FinishResult{}, fmt.Errorf("finishing monitor poll: %w", err)
	}
	if cycleAt != nil {
		// The subtree walks stopped at the repetition, so the poll is still
		// correct; the loop is a data defect to report (US-3, INV-3).
		hierarchy.LogCycle(ctx, p.ScopeIssueID, *cycleAt, cycleChain)
	}
	return res, nil
}

func (s *Store) PollForMonitor(ctx context.Context, p PollParams) (PollResult, error) {
	limit := p.Limit
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	// Resolve the lower bound. Live-start anchors at DB-side now() so the
	// first poll surfaces no historical backlog and hands the runner a
	// cursor to continue from. now() here is a separate statement from the
	// poll query's now(); the multi-second SafetyLag dwarfs the microsecond
	// gap between them, so the first poll is reliably empty.
	var boundTS time.Time
	var boundID int64
	if p.SinceTS != nil && p.SinceID != nil {
		boundTS = *p.SinceTS
		boundID = *p.SinceID
	} else {
		if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&boundTS); err != nil {
			return PollResult{}, fmt.Errorf("monitor poll anchor: %w", err)
		}
		boundID = 0
	}

	// Server-owned floor: the effective lower bound is the LATER of (client
	// cursor, Floor). When the resolved bound is at or before the credential's
	// mint time, clamp it up to (Floor, 0). This defeats a forged old cursor
	// (since_ts=epoch, since_id=0) that would otherwise backfill in-scope
	// notifications committed before the monitor token existed, while leaving
	// a legitimately-advanced cursor untouched. Live-start already anchors at
	// now() (>= Floor in practice), so the clamp is a no-op there. A zero
	// Floor disables the clamp (no mint-time concept). Row-value ">" semantics
	// downstream are preserved.
	if !boundTS.After(p.Floor) {
		boundTS = p.Floor
		boundID = 0
	}

	rows, err := s.pool.Query(ctx, pollForMonitorSQL,
		p.EventTypes, p.SafetyLag, boundTS, boundID,
		p.ScopeIssueID, p.ScopeSubtree, limit, p.ProjectID,
		p.ExcludeSession,
	)
	if err != nil {
		return PollResult{}, fmt.Errorf("polling monitor notifications: %w", err)
	}
	defer rows.Close()

	items := make([]NotificationDTO, 0, limit)
	for rows.Next() {
		var (
			n                                 NotificationDTO
			actorName                         string
			actorUserID                       *uuid.UUID
			actorType                         string
			actorSessionID                    *string
			entityType, entityID              string
			projectID                         *uuid.UUID
			issueTitle, projectIDPrefix       *string
			fieldName                         *string
			oldData, newData, payload         []byte
			commentIssueID, commentIssueTitle *string
			commentProjectPrefix              *string
			// sk-242.1 monitor-only locals — projected solely by PollForMonitor
			// (not shared with the human ListInbox Scan).
			commentBody      *string
			commentDeletedAt *time.Time
		)
		// Column order is locked to pollForMonitorSQL's SELECT. read_at /
		// archived_at are gone (events has no such columns): n.ReadAt /
		// n.ArchivedAt stay nil → omitempty → absent, so the agent-poll JSON
		// carries no inbox-only fields. CreatedAt now scans e.occurred_at
		// (the referent changed from notifications.created_at, cutover).
		// Trailing comment_body / comment_deleted_at are monitor enrichment
		// (sk-242.1); human Scan paths do not select them.
		if err := rows.Scan(
			&n.ID, &n.Type, &n.CreatedAt,
			&actorName, &actorUserID, &actorType,
			&entityType, &entityID, &projectID,
			&actorSessionID,
			&issueTitle, &projectIDPrefix,
			&fieldName, &oldData, &newData, &payload,
			&commentIssueID, &commentIssueTitle, &commentProjectPrefix,
			&commentBody, &commentDeletedAt,
		); err != nil {
			return PollResult{}, fmt.Errorf("scanning monitor notification row: %w", err)
		}

		// Reader-side echo marker (INV-2, US-2): expose the event's
		// actor_session_id ONLY when non-empty. A NULL or empty string means
		// a session-less actor (a human owner in the Web UI, or system) —
		// which is NEVER the agent's own echo (US-5), so it must stay absent
		// (nil → omitempty) rather than surface as a spurious "" the reader
		// could mis-compare against its current session.
		if actorSessionID != nil && *actorSessionID != "" {
			n.ActorSessionID = actorSessionID
		}

		hydrateNotificationDTO(&n,
			actorName, actorUserID, actorType,
			entityType, entityID, projectID,
			issueTitle, projectIDPrefix,
			fieldName, oldData, newData, payload,
			commentIssueID, commentIssueTitle, commentProjectPrefix)

		// sk-242.1 US-1/2 — monitor-only comment enrichment (INV-5: only here).
		// commentId authority = event entity_id when parseable (FR-007), NOT
		// join success. Preview/truncated ONLY from live comments.body
		// (FR-001 / INV-3); soft-delete → text absent (INV-4); Entity still
		// re-points comment→issue (INV-9).
		if entityType == "comment" {
			if parsed, err := uuid.Parse(entityID); err == nil {
				cid := parsed.String()
				n.CommentID = &cid
			}
			switch {
			case commentDeletedAt != nil:
				// Soft-deleted live row: commentId may be set; bodyPreview
				// and truncated stay nil (deleted text never on poll).
			case commentBody != nil:
				// Live row (including empty body → present "").
				preview, wasTrunc := truncateRunesFlag(*commentBody, BodyPreviewMaxRunes)
				n.BodyPreview = &preview
				if wasTrunc {
					tru := true
					n.Truncated = &tru
				}
			case n.CommentID != nil:
				// Parseable entity id but no comments row joined (orphan if
				// the event was selected — residual; scope usually needs the
				// live row). Do not fail the page; log for observability.
				slog.WarnContext(ctx, "monitor poll: comment event without live comment row",
					"eventId", n.ID,
					"commentId", *n.CommentID,
					"projectId", projectID,
				)
			}
			// Unparseable entity_id: CommentID/BodyPreview/Truncated all nil
			// (degraded item); page continues.
		}

		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return PollResult{}, fmt.Errorf("iterating monitor notification rows: %w", err)
	}

	// Cursor = last emitted (created_at, id) tuple in sort order. On an
	// empty page fall back to the lower bound we used: the live-start anchor
	// (so the next poll continues from now()) or the unchanged incoming
	// cursor (so a quiet poll doesn't rewind).
	res := PollResult{Items: items, CursorTS: boundTS, CursorID: boundID}
	if n := len(items); n > 0 {
		res.CursorTS = items[n-1].CreatedAt
		res.CursorID = items[n-1].ID
	}
	return res, nil
}

// Counts returns unread + total (non-archived) notification counts for
// the caller. Both are restricted to feed-channel rows (n.feed_channel) —
// the one read-side predicate left after sk-303.13 / T014. A push-only
// row is in nobody's feed, so it must not move a feed counter (INV-11),
// and the counter carries no reason filter of its own (INV-2).
//
// projectID is optional — pass nil for a global count (used by the
// header NotificationBadge), pass a non-nil pointer to restrict the
// count to one project (used by the per-project NotificationBadge in
// the project detail header). The SQL uses `($2::uuid IS NULL OR
// n.project_id = $2)` so a single statement covers both cases without
// branching at the Go layer.
func (s *Store) Counts(ctx context.Context, userID uuid.UUID, projectID *uuid.UUID) (CountsDTO, error) {
	sql := `
SELECT
    count(*) FILTER (WHERE n.read_at IS NULL AND n.archived_at IS NULL) AS unread,
    count(*) FILTER (WHERE n.archived_at IS NULL)                       AS total
  FROM notifications n
 WHERE n.user_id = $1
   AND n.feed_channel
   AND ($2::uuid IS NULL OR n.project_id = $2)
   AND ` + authz.VisibleNowSetExpr("$1::uuid", "n.project_id") + `
`
	var (
		c        CountsDTO
		pidParam any // nil → SQL NULL → predicate degenerates to TRUE
	)
	if projectID != nil {
		pidParam = *projectID
	}
	if err := s.pool.QueryRow(ctx, sql, userID, pidParam).Scan(&c.Unread, &c.Total); err != nil {
		return CountsDTO{}, fmt.Errorf("counting notifications: %w", err)
	}
	return c, nil
}

// MarkRead sets read_at = now() if currently NULL. Returns ErrNotFound
// when the row doesn't exist, doesn't belong to the caller, or is not a
// feed row.
//
// The feed_channel predicate here is not bookkeeping: without it a
// push-only row stays ADDRESSABLE, and the 204-vs-404 split is an
// existence oracle for a row the caller must not be able to observe at
// all (INV-11). Marking such a row read would also be a state change on
// something no surface can ever show back.
func (s *Store) MarkRead(ctx context.Context, notificationID int64, userID uuid.UUID) error {
	n, err := s.updateOwnerRows(ctx, userID, `
UPDATE notifications n
   SET read_at = COALESCE(n.read_at, now())
  FROM notifications o
 WHERE n.id = $2 AND n.user_id = $1 AND n.feed_channel AND o.id = n.id
RETURNING n.type, o.read_at IS NULL AS changed`, notificationID)
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkUnread clears read_at. Idempotent. Feed rows only — see MarkRead
// for why a push-only row must not be addressable.
func (s *Store) MarkUnread(ctx context.Context, notificationID int64, userID uuid.UUID) error {
	n, err := s.updateOwnerRows(ctx, userID, `
UPDATE notifications n
   SET read_at = NULL
  FROM notifications o
 WHERE n.id = $2 AND n.user_id = $1 AND n.feed_channel AND o.id = n.id
RETURNING n.type, o.read_at IS NOT NULL AS changed`, notificationID)
	if err != nil {
		return fmt.Errorf("mark unread: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Archive moves a notification out of the inbox into the archive view
// (archived_at IS NOT NULL). Idempotent — re-archiving doesn't bump
// the timestamp. Feed rows only — see MarkRead.
func (s *Store) Archive(ctx context.Context, notificationID int64, userID uuid.UUID) error {
	n, err := s.updateOwnerRows(ctx, userID, `
UPDATE notifications n
   SET archived_at = COALESCE(n.archived_at, now()),
       read_at     = COALESCE(n.read_at, now())
  FROM notifications o
 WHERE n.id = $2 AND n.user_id = $1 AND n.feed_channel AND o.id = n.id
RETURNING n.type, (o.archived_at IS NULL OR o.read_at IS NULL) AS changed`, notificationID)
	if err != nil {
		return fmt.Errorf("archive notification: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllReadArgs is the optional scope filter for mark-all-read. Empty
// scope means "all unread non-archived rows for the user".
type MarkAllReadArgs struct {
	UserID    uuid.UUID
	Scope     string // unused for mark-all-read in R1.6 — accepted for forward compatibility
	ProjectID *uuid.UUID
}

// MarkAllRead bulk-flips read_at on the inbox; honours optional project
// filter. Returns the number of rows updated.
//
// feed_channel is part of the predicate because the returned count is
// itself a read surface: "N marked read" computed over rows the badge
// never counted would contradict the badge in the same gesture (INV-3),
// and it would disclose how many invisible push-only rows the caller has
// (INV-11).
func (s *Store) MarkAllRead(ctx context.Context, args MarkAllReadArgs) (int64, error) {
	clauses := []string{
		"user_id = $1",
		"feed_channel",
		"read_at IS NULL",
		"archived_at IS NULL",
	}
	params := []any{args.UserID}
	if args.ProjectID != nil {
		params = append(params, *args.ProjectID)
		clauses = append(clauses, fmt.Sprintf("project_id = $%d", len(params)))
	}
	sql := "UPDATE notifications SET read_at = now() WHERE " + joinAnd(clauses) + " RETURNING type, true AS changed"
	n, err := s.updateOwnerRows(ctx, args.UserID, sql, params[1:]...)
	if err != nil {
		return 0, fmt.Errorf("mark all read: %w", err)
	}
	return n, nil
}

// MarkReadByEntity flips read_at on every notification for a specific
// entity. Used when the user opens an issue page — we mark all related
// inbox rows as seen in one round-trip.
//
// Special case for entityType='issue': we also mark any comment events
// whose parent issue is the supplied id. UX-wise, opening "issue X"
// implies "I've seen everything on issue X, including its comments";
// not collapsing comment notifications under the same gesture would
// leave stale unread badges that frustrate users.
func (s *Store) MarkReadByEntity(
	ctx context.Context,
	userID uuid.UUID,
	entityType, entityID string,
) (int64, error) {
	if entityType == "" || entityID == "" {
		return 0, fmt.Errorf("entityType and entityId required")
	}

	if entityType == "issue" {
		// Cover the issue itself + every comment on the issue. Two
		// branches unioned via OR (cleaner than two UPDATEs) — the
		// notifications.event_id IN (...) sub-select short-circuits on
		// the index. We reuse the events.entity_type/entity_id index.
		const sql = `
UPDATE notifications
   SET read_at = now()
 WHERE user_id = $1
   AND feed_channel
   AND read_at IS NULL
   AND archived_at IS NULL
   AND event_id IN (
       SELECT e.id
         FROM events e
        WHERE (e.entity_type = 'issue' AND e.entity_id = $2)
           OR (e.entity_type = 'comment'
               AND EXISTS (
                   SELECT 1 FROM comments c
                    WHERE c.id = e.entity_id::uuid AND c.issue_id = $2
               ))
   )
RETURNING type, true AS changed`
		n, err := s.updateOwnerRows(ctx, userID, sql, entityID)
		if err != nil {
			return 0, fmt.Errorf("mark read by entity: %w", err)
		}
		return n, nil
	}

	// Generic path: match the entity directly by (type, id).
	const sql = `
UPDATE notifications
   SET read_at = now()
 WHERE user_id = $1
   AND feed_channel
   AND read_at IS NULL
   AND archived_at IS NULL
   AND event_id IN (
       SELECT id FROM events
        WHERE entity_type = $2
          AND entity_id   = $3
   )
RETURNING type, true AS changed`
	n, err := s.updateOwnerRows(ctx, userID, sql, entityType, entityID)
	if err != nil {
		return 0, fmt.Errorf("mark read by entity: %w", err)
	}
	return n, nil
}

// MarkReadCard is "read card X" (sk-622.21, US-7): mark read every
// unread, non-archived notification of the caller whose Watching card is X —
// the events of X's descendants that the fan-out carded to X included, inside
// the Watching window and outside it. The Watching badge therefore drops by
// exactly the card's unread notifications inside the window; no other card
// is touched. The operation reads the card the notification was given when
// it was written, never the hierarchy, so a card carrying descendants is
// read in one gesture where mark-read-by-entity reads the task itself only.
//
// Only the caller's rows are read or written: the user id is a predicate of
// the statement. A card that is somebody else's, does not exist or has
// nothing unread marks nothing and answers 0 — never an error.
//
// One autocommit statement through updateOwnerRows: when a row changed, the
// owner's personal signal `watching` is published after the commit (T016,
// matrix K13 "read the card"); a repeat that changes nothing signals
// nothing. The rows are found through idx_notif_watching_card
// (user_id, card_issue_id, created_at DESC).
func (s *Store) MarkReadCard(ctx context.Context, userID uuid.UUID, cardIssueID string) (MarkCardReadResult, error) {
	if cardIssueID == "" {
		return MarkCardReadResult{}, fmt.Errorf("mark read card: card issue id required")
	}
	const sql = `
UPDATE notifications
   SET read_at = now()
 WHERE user_id = $1
   AND card_issue_id = $2
   AND read_at IS NULL
   AND archived_at IS NULL
RETURNING type, true AS changed`
	n, err := s.updateOwnerRows(ctx, userID, sql, cardIssueID)
	if err != nil {
		return MarkCardReadResult{}, fmt.Errorf("mark read card: %w", err)
	}
	return MarkCardReadResult{Marked: int(n)}, nil
}

// updateOwnerRows runs one autocommit UPDATE over the caller's notification
// rows and publishes the owner's personal signal (sk-622.21 T016, US-11,
// "Read / unread / archive / read all / by issue"). updateSQL
// must be an UPDATE … RETURNING type, changed whose placeholders are
// $1 = userID, then args in order from $2; the mention-family array is bound
// after them. `changed` is true for a row whose state the UPDATE actually
// moved (found in review): the idempotent single-row marks
// match a row that may already be read / unread / archived, so they compare
// against the pre-update row (a self-join, read from the statement's
// snapshot); the bulk marks select only rows they change and return true.
// Returns the number of rows the UPDATE matched — changed or not, so a
// repeated mark answers exactly as the first one did. Only changed rows
// name signal areas: a repeat that changes nothing signals nothing. One
// statement: the UPDATE runs as a data-modifying CTE and the outer SELECT
// folds it into the count and the two "touched" facts.
func (s *Store) updateOwnerRows(ctx context.Context, userID uuid.UUID, updateSQL string, args ...any) (int64, error) {
	params := append([]any{userID}, args...)
	params = append(params, mentionTypes())
	mentionPh := fmt.Sprintf("$%d", len(params))
	sql := "WITH u AS (" + updateSQL + ")\nSELECT count(*)," +
		" COALESCE(bool_or(u.changed AND u.type = ANY(" + mentionPh + "::text[])), false)," +
		" COALESCE(bool_or(u.changed AND NOT u.type = ANY(" + mentionPh + "::text[])), false)" +
		" FROM u"
	var (
		n                     int64
		mention, otherTouched bool
	)
	if err := s.pool.QueryRow(ctx, sql, params...).Scan(&n, &mention, &otherTouched); err != nil {
		return 0, err
	}
	signalOwnerRows(ctx, userID, mention, otherTouched)
	return n, nil
}

// ----------------- User prefs -----------------

// GetPrefs returns the caller's notification prefs, falling back to
// defaults if no row exists yet (we INSERT lazily on first PUT).
func (s *Store) GetPrefs(ctx context.Context, userID uuid.UUID) (PrefsDTO, error) {
	const sql = `
SELECT notify_event_types, default_inbox_scope, suppress_agent_actions
  FROM user_notification_prefs
 WHERE user_id = $1
`
	var (
		types    []string
		scope    string
		suppress bool
	)
	err := s.pool.QueryRow(ctx, sql, userID).Scan(&types, &scope, &suppress)
	if errors.Is(err, pgx.ErrNoRows) {
		return PrefsDTO{
			NotifyEventTypes:  nil, // nil = all events
			DefaultInboxScope: "all",
			// sk-343: ON for an account that has never written a prefs row.
			// Rows here are lazy, and every upsert that creates one names this
			// column, so the schema DEFAULT (migration 000043) is unreachable
			// in practice — this literal and the fan-out's COALESCE fallback
			// are what "on by default" actually means. All three must
			// move together, or the settings form shows a cleared checkbox for
			// an account the fan-out is already suppressing.
			SuppressAgentActions: true,
		}, nil
	}
	if err != nil {
		return PrefsDTO{}, fmt.Errorf("loading prefs: %w", err)
	}
	return PrefsDTO{
		NotifyEventTypes:     types,
		DefaultInboxScope:    scope,
		SuppressAgentActions: suppress,
	}, nil
}

// UpdatePrefs upserts the caller's prefs. Per UpdatePrefsInput semantics
// we distinguish three states for notify_event_types:
//   - leave alone (input.NotifyEventTypes == nil && !input.SetEventTypesNull)
//   - explicit null (input.SetEventTypesNull == true) → clear filter
//   - explicit list  (input.NotifyEventTypes != nil)  → whitelist
func (s *Store) UpdatePrefs(ctx context.Context, userID uuid.UUID, input UpdatePrefsInput) (PrefsDTO, error) {
	// One round-trip: load current row, merge, upsert. Avoids the
	// "INSERT ON CONFLICT but only update specified fields" clutter.
	cur, err := s.GetPrefs(ctx, userID)
	if err != nil {
		return PrefsDTO{}, err
	}

	next := cur
	if input.SetEventTypesNull {
		next.NotifyEventTypes = nil
	} else if input.NotifyEventTypes != nil {
		next.NotifyEventTypes = *input.NotifyEventTypes
	}
	if input.DefaultInboxScope != nil {
		next.DefaultInboxScope = *input.DefaultInboxScope
	}
	if input.SuppressAgentActions != nil {
		next.SuppressAgentActions = *input.SuppressAgentActions
	}

	const upsert = `
INSERT INTO user_notification_prefs (
    user_id, notify_event_types, default_inbox_scope,
    suppress_agent_actions, updated_at
)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id) DO UPDATE
   SET notify_event_types         = EXCLUDED.notify_event_types,
       default_inbox_scope        = EXCLUDED.default_inbox_scope,
       suppress_agent_actions = EXCLUDED.suppress_agent_actions,
       updated_at                 = now()
`
	var typesArg any
	if next.NotifyEventTypes != nil {
		typesArg = next.NotifyEventTypes
	}
	if _, err := s.pool.Exec(ctx, upsert, userID, typesArg, next.DefaultInboxScope, next.SuppressAgentActions); err != nil {
		return PrefsDTO{}, fmt.Errorf("upserting prefs: %w", err)
	}
	return next, nil
}

// GetPushDisabledCategories returns the caller's opted-out push categories
// (user_notification_prefs.push_disabled_categories). No prefs row → empty
// slice (every category enabled). The returned slice is never nil so the
// handler can JSON-encode it as [] rather than null.
func (s *Store) GetPushDisabledCategories(ctx context.Context, userID uuid.UUID) ([]string, error) {
	var disabled []string
	err := s.pool.QueryRow(ctx, `
SELECT push_disabled_categories
  FROM user_notification_prefs
 WHERE user_id = $1`, userID).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading push prefs: %w", err)
	}
	if disabled == nil {
		disabled = []string{}
	}
	return disabled, nil
}

// SetPushDisabledCategories upserts the caller's push-category opt-out list.
// It writes ONLY push_disabled_categories; the INSERT path seeds the other
// columns with their schema defaults (notify_event_types NULL = all events,
// default_inbox_scope 'all', suppress_agent_actions true since sk-343 —
// matching the GetPrefs fallback), and the conflict path leaves them
// untouched. Seeding the flag with anything else would let a push-category
// edit silently CLEAR a setting the account never touched. Validation of the
// category strings against the registry is the handler's job.
func (s *Store) SetPushDisabledCategories(ctx context.Context, userID uuid.UUID, categories []string) error {
	if categories == nil {
		categories = []string{}
	}
	const upsert = `
INSERT INTO user_notification_prefs (
    user_id, default_inbox_scope, suppress_agent_actions,
    push_disabled_categories, updated_at
)
VALUES ($1, 'all', true, $2, now())
ON CONFLICT (user_id) DO UPDATE
   SET push_disabled_categories = EXCLUDED.push_disabled_categories,
       updated_at                = now()
`
	if _, err := s.pool.Exec(ctx, upsert, userID, categories); err != nil {
		return fmt.Errorf("upserting push prefs: %w", err)
	}
	return nil
}

// ----------------- R3.3 — project watcher event_types -----------------

// ErrTooManyWatchers is returned by AddCustomWatcher when the caller has
// reached the per-user cap. Mapped by the handler to 422 too_many_watchers.
var ErrTooManyWatchers = errors.New("too many custom watchers")

// MaxCustomWatchersPerUser caps the count of custom_watchers per user.
// 10 was chosen by the R3.3 plan (Q-G2) — high enough for a power user
// to slice the inbox by project / priority / event-type, low enough that
// the per-event SQL fan-out stays cheap.
const MaxCustomWatchersPerUser = 10

// SetProjectWatcherEventTypes updates the event_types whitelist for an
// existing project_watchers row. Pass nil to clear the filter (R1.6
// semantics — "all notifiable types"); an empty slice is treated the
// same way to avoid storing an explicit "subscribe to nothing" row.
//
// Returns ErrNotFound when the caller doesn't have a project_watchers
// row for the project (i.e. they're not currently watching). Callers
// must POST /watch first.
func (s *Store) SetProjectWatcherEventTypes(
	ctx context.Context,
	projectID, userID uuid.UUID,
	eventTypes []string,
) error {
	var typesArg any
	if len(eventTypes) > 0 {
		typesArg = eventTypes
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE project_watchers
   SET event_types = $3
 WHERE project_id = $1 AND user_id = $2
   AND NOT 'unwatched' = ANY(reasons)`,
		projectID, userID, typesArg)
	if err != nil {
		return fmt.Errorf("setting project watcher event_types: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ----------------- R3.3 — custom watchers -----------------

// ListCustomWatchers returns every custom_watchers row owned by userID.
// Sort by created_at ASC so the original watcher appears first — matches
// the existing pattern used for issue/project watchers.
func (s *Store) ListCustomWatchers(ctx context.Context, userID uuid.UUID) ([]CustomWatcherDTO, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, name, filter, created_at
  FROM custom_watchers
 WHERE user_id = $1
 ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing custom watchers: %w", err)
	}
	defer rows.Close()

	out := make([]CustomWatcherDTO, 0)
	for rows.Next() {
		var (
			dto         CustomWatcherDTO
			filterBytes []byte
		)
		if err := rows.Scan(&dto.ID, &dto.Name, &filterBytes, &dto.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning custom watcher row: %w", err)
		}
		if len(filterBytes) > 0 {
			if err := json.Unmarshal(filterBytes, &dto.Filter); err != nil {
				return nil, fmt.Errorf("decoding custom watcher filter %s: %w", dto.ID, err)
			}
		}
		out = append(out, dto)
	}
	return out, rows.Err()
}

// AddCustomWatcher inserts a new custom_watchers row for userID. Enforces
// the per-user cap inside a SERIALIZABLE transaction so concurrent POSTs
// can't overshoot. Returns ErrTooManyWatchers when the cap is breached.
//
// Name uniqueness is enforced by the underlying UNIQUE(user_id, name)
// constraint; the handler maps the resulting unique-violation to a 422
// duplicate_name response.
func (s *Store) AddCustomWatcher(
	ctx context.Context,
	userID uuid.UUID,
	input CreateCustomWatcherInput,
) (*CustomWatcherDTO, error) {
	filterBytes, err := json.Marshal(input.Filter)
	if err != nil {
		return nil, fmt.Errorf("encoding filter: %w", err)
	}

	id := uuid.New()
	dto := CustomWatcherDTO{
		ID:     id,
		Name:   input.Name,
		Filter: input.Filter,
	}

	err = signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM custom_watchers WHERE user_id = $1`, userID,
		).Scan(&count); err != nil {
			return fmt.Errorf("counting custom watchers: %w", err)
		}
		if count >= MaxCustomWatchersPerUser {
			return ErrTooManyWatchers
		}
		row := tx.QueryRow(ctx, `
INSERT INTO custom_watchers (id, user_id, name, filter)
VALUES ($1, $2, $3, $4::jsonb)
RETURNING created_at`,
			id, userID, input.Name, filterBytes)
		if err := row.Scan(&dto.CreatedAt); err != nil {
			return fmt.Errorf("inserting custom watcher: %w", err)
		}
		return nil
	})
	if err != nil {
		// R3.3 W2 — concurrent custom-watcher creates: the only
		// cross-row invariant the SERIALIZABLE level protects in this
		// method is the per-user cap recheck. If two POSTs from the
		// same user arrive at count=N-1, one COMMITs and the other
		// surfaces SQLSTATE 40001 (serialization_failure). Map 40001
		// to ErrTooManyWatchers so the caller sees the business-level
		// reason ("too_many_watchers") instead of a generic 500 —
		// matches the pattern used in users.UpdateUser (R3.1) and
		// issues.Claim/Close (R2.3).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgSerializationFailure {
			return nil, ErrTooManyWatchers
		}
		return nil, err
	}
	return &dto, nil
}

// DeleteCustomWatcher removes one custom_watchers row owned by userID.
// Returns ErrNotFound when the row doesn't exist or doesn't belong to
// the caller — same masking semantics as MarkRead/Archive (we don't want
// to leak the existence of other users' watchers).
func (s *Store) DeleteCustomWatcher(ctx context.Context, userID, watcherID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM custom_watchers WHERE id = $1 AND user_id = $2`,
		watcherID, userID)
	if err != nil {
		return fmt.Errorf("deleting custom watcher: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAllWatchers builds the consolidated /me/watchers payload — three
// sections (issues, projects, custom) hydrated with the metadata the
// /watching UI needs (issue title, project name, project prefix). All
// sections exclude sticky-unwatched rows so the page only shows active
// subscriptions.
//
// Caps follow R3.3 plan G16: no paging, hard cap 50 per section and
// flag truncation in the response. The cap is applied SQL-side via
// LIMIT to keep the payload bounded.
func (s *Store) ListAllWatchers(ctx context.Context, userID uuid.UUID) (*AllWatchersDTO, error) {
	const issueSQL = `
SELECT iw.issue_id, COALESCE(i.title, ''), i.project_id, COALESCE(p.id_prefix, ''), iw.reasons, iw.since
  FROM issue_watchers iw
  JOIN issues i   ON i.id = iw.issue_id
  JOIN projects p ON p.id = i.project_id
 WHERE iw.user_id = $1
 ORDER BY iw.since DESC
 LIMIT 50`

	rows, err := s.pool.Query(ctx, issueSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("listing my issue watchers: %w", err)
	}
	issueOut := make([]IssueWatcherForUserDTO, 0)
	for rows.Next() {
		var w IssueWatcherForUserDTO
		if err := rows.Scan(&w.IssueID, &w.IssueTitle, &w.ProjectID, &w.ProjectIDPrefix, &w.Reasons, &w.Since); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning my issue watcher: %w", err)
		}
		issueOut = append(issueOut, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	const projectSQL = `
SELECT pw.project_id, COALESCE(p.name, ''), COALESCE(p.id_prefix, ''),
       pw.reasons, pw.since, pw.event_types
  FROM project_watchers pw
  JOIN projects p ON p.id = pw.project_id
 WHERE pw.user_id = $1
   AND NOT 'unwatched' = ANY(pw.reasons)
 ORDER BY pw.since DESC
 LIMIT 50`

	rows2, err := s.pool.Query(ctx, projectSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("listing my project watchers: %w", err)
	}
	projectOut := make([]ProjectWatcherDTO, 0)
	for rows2.Next() {
		var (
			w     ProjectWatcherDTO
			types []string
		)
		if err := rows2.Scan(&w.ProjectID, &w.ProjectName, &w.ProjectIDPrefix,
			&w.Reasons, &w.Since, &types); err != nil {
			rows2.Close()
			return nil, fmt.Errorf("scanning my project watcher: %w", err)
		}
		w.EventTypes = types
		projectOut = append(projectOut, w)
	}
	rows2.Close()
	if err := rows2.Err(); err != nil {
		return nil, err
	}

	customOut, err := s.ListCustomWatchers(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(customOut) > 50 {
		customOut = customOut[:50]
	}

	return &AllWatchersDTO{
		Issues:   issueOut,
		Projects: projectOut,
		Custom:   customOut,
	}, nil
}

// ----------------- sk-39 — /me/watching feed -----------------

// WatchingFeedLimit caps the number of watched issues returned to the
// shell's Watching tab in one round-trip. The watcher table is
// already capped at 50 in ListAllWatchers; sk-39 doubles that to 100
// so a user with many subscriptions still gets the full set (paging
// can come later — typical user-watched volume stays well under 100).
const WatchingFeedLimit = 100

// WatchingRecentWindow is the time-window for the in-card "recent
// events" timeline. Older events are trimmed; if no events fall in
// the window, the row renders a "no recent activity" placeholder.
const WatchingRecentWindow = 7 * 24 * time.Hour

// WatchingRecentTopN is the per-issue cap on the recent-events list.
// Five lines is enough context to scan ("alice claimed; status →
// in_progress; bob commented; …") without bloating the response.
const WatchingRecentTopN = 5

// ----------------- sk-622.21 T022 — Watching reads by card (R2) -----------------

// Every Watching surface reads the card the notification was given when it
// was written (notifications.card_issue_id, sk-622.21 US-4, D1): the fan-out
// computes it once per recipient and stores it, so a read is a count over an
// index, not a match of the feed against the subscriptions. Three readers,
// one predicate — "the caller's notification, not archived, with a card,
// written inside the Watching window":
//
//   - the badge (WatchingBadgeSQL / CountWatchingUnread) — the unread ones;
//   - the column's per-card counts (ListWatchingFeed) — unread and last
//     notification time per card, and the historical cards;
//   - the badge equals the column's unreadTotal (INV-5) because both count
//     the same rows: every row the badge counts lands on exactly one card
//     (its card_issue_id), and the column has a card for every card id it
//     finds.
//
// The window is the store's (WithWatchingWindow). A notification is in the
// window when created_at >= now() - window; reading it does not extend it.
// Mentions never carry a card (INV-4), so no window applies to them here
// (INV-8): the Mentions badge is counted elsewhere, without a window.

// watchingInWindowPredicate is the shared row predicate of the Watching
// readers over notifications aliased n: user is the placeholder of the
// caller's user id, win the placeholder of the window interval.
//
// sk-674.18 T029 (INV-11): only rows of a project the caller sees now — a
// row of a project he lost stays in the table and is shown nowhere.
//
// visible is the SQL boolean of that condition on n.project_id, in the
// form the composing statement can afford: the point form for the badge's
// single statement (watchingVisibleNow), the bound visible set for the
// header counts; the column (ListWatchingFeed) checks its cards instead
// (WatchingBadgeCountExpr) — one rule, the access relation.
func watchingInWindowPredicate(user, win, visible string) string {
	return `n.user_id = ` + user + `
       AND n.card_issue_id IS NOT NULL
       AND n.archived_at IS NULL
       AND n.created_at >= now() - ` + win + `::interval
       AND ` + visible
}

// watchingVisibleNow is the visibility condition of a Watching statement of
// its own over n.project_id, the caller bound at $1: the point form of the
// relation per row (the rows are bounded by the window), so the statement
// stays one and the relation never enters its plan.
var watchingVisibleNow = "(n.project_id IS NULL OR " + authz.RecipientVisibleExpr("$1::uuid", "n.project_id") + ")"

// watchingVisible is the visibility condition over n.project_id with the
// caller's visible set bound at visiblePh as a uuid array (authz Scope.Bind
// — read once, before the statement, so the relation never enters a
// Watching plan).
func watchingVisible(visiblePh string) string {
	return `(n.project_id IS NULL OR n.project_id = ANY(` + visiblePh + `::uuid[]))`
}

// WatchingBadgeCountExpr is the Watching badge as a scalar subquery over
// the placeholders the composing statement already bound: userPh the
// caller's user id, windowPh the Watching window (an interval — the store's
// WatchingWindow()). The same predicate as WatchingBadgeSQL, for a reader
// that folds the badge into a larger statement (the header counts, one
// statement per computation, sk-622.21 US-16).
//
// visiblePh is the placeholder of the caller's visible set, bound as a uuid
// array from the statement's one read of the relation (authz
// Scope.Bind) — the same rows WatchingBadgeSQL counts (INV-5).
func WatchingBadgeCountExpr(userPh, windowPh, visiblePh string) string {
	return `(SELECT count(*) FROM notifications n WHERE ` +
		watchingInWindowPredicate(userPh, windowPh, watchingVisible(visiblePh)) + ` AND n.read_at IS NULL)`
}

// WatchingBadgeSQL is the statement behind the header's Watching badge
// (sk-622.21, US-5): how many unread, non-archived notifications of the
// caller have a Watching card and were written inside the window. $1 is the
// caller's user id, $2 the window (an interval — pass the store's
// WatchingWindow()); one bigint comes back. sk-674.18 T029 (INV-11): only
// rows of a project the caller sees now — the point form of the relation
// per counted row (one statement; the relation stays out of the plan).
//
// It is served by idx_notif_watching_badge (user_id, created_at) WHERE
// card_issue_id IS NOT NULL AND read_at IS NULL AND archived_at IS NULL —
// a range inside one user, so its cost depends on neither the number of
// the caller's subscriptions nor the rows outside the window (SC-001).
//
// Exported so internal/handler can run it on its own pool for the header
// counts without re-deriving the predicate.
func WatchingBadgeSQL() string {
	return `
SELECT count(*)
  FROM notifications n
 WHERE ` + watchingInWindowPredicate("$1", "$2", watchingVisibleNow) + `
   AND n.read_at IS NULL`
}

// CountWatchingUnread runs WatchingBadgeSQL for one user with the store's
// window.
func (s *Store) CountWatchingUnread(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, WatchingBadgeSQL(), userID, s.watchingWindow).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting watching unread: %w", err)
	}
	return n, nil
}

// ListWatchingFeed builds the cards of the shell's Watching column
// (sk-622.21, US-5). The cards are a set of tasks:
//
//	(the caller's live subscriptions) ∪ (the cards of the caller's
//	non-archived notifications inside the window)
//
// of two kinds:
//
//   - a subscribed card — the caller has a live subscription on the task,
//     with or without notifications: a silent subscription is still
//     returned, sorted last, because "there are subscriptions, but no
//     events" is a different empty state from "there are no subscriptions";
//   - a historical card — no subscription, but notifications in the window
//     carry the task as their card (e.g. after the automatic unwatch on
//     close kept the closing notification, US-6). Its Reasons are empty and
//     Since is nil.
//
// Each card carries the unread notifications in the window
// (UnreadEventsCount) and the time of its latest notification in the window
// (LastActivity, nil for a silent subscription). The activity of a subtree's
// descendants is on the card the fan-out gave it — the ancestor's when the
// subscription was a subtree one.
//
// Order: LastActivity descending, then task id; cards without
// notifications last. Capped at WatchingFeedLimit.
//
// Totals, uncapped:
//
//   - SubscriptionsTotal — live subscriptions;
//   - OpenTotal / ClosedTotal — cards of BOTH kinds by the state of the
//     card's task (closed = status 'closed', every other status is open);
//   - ClosedAutoTotal — closed cards without a manual subscription,
//     historical ones included: what «stop watching closed» would take;
//   - UnreadTotal — the unread notifications over every card, equal to the
//     badge (INV-5) whatever the filter and the cap;
//   - StateTotal — cards matching state; Truncated = StateTotal > rows.
//
// state narrows the CARDS of both kinds by the state of their task.
//
// Cost (SC-011): the subscriptions are read by the caller's user id, the
// notifications through idx_notif_watching_card (user_id, card_issue_id,
// created_at DESC) WHERE card_issue_id IS NOT NULL AND archived_at IS NULL,
// and the tasks of the cards by primary key — linear in the live
// subscriptions and the notifications in the window, independent of
// opt-outs and of other tasks.
func (s *Store) ListWatchingFeed(ctx context.Context, userID uuid.UUID, state WatchingState) (WatchingFeedResult, error) {
	if _, ok := ParseWatchingState(string(state)); !ok {
		return WatchingFeedResult{}, fmt.Errorf("listing watching feed: unknown state %q", state)
	}
	sql := `
WITH subs AS (
    SELECT iw.issue_id, iw.reasons, iw.since
      FROM issue_watchers iw
     WHERE iw.user_id = $1
),
win AS (
    SELECT n.card_issue_id,
           count(*) FILTER (WHERE n.read_at IS NULL)::int AS unread_count,
           max(n.created_at)                              AS last_at
      FROM notifications n
     WHERE ` + watchingInWindowPredicate("$1", "$4", "true") + `
     GROUP BY n.card_issue_id
),
cards AS (
    SELECT i.id AS issue_id,
           s.issue_id IS NOT NULL                    AS subscribed,
           s.reasons, s.since,
           COALESCE(w.unread_count, 0)::int          AS unread_count,
           w.last_at,
           ` + status.IsClosed("i") + `             AS is_closed,
           (SELECT s_sp.short_id FROM projects p_sp JOIN spaces s_sp ON s_sp.id = p_sp.space_id
             WHERE p_sp.id = i.project_id)          AS space_short
      FROM subs s
      FULL JOIN win w ON w.card_issue_id = s.issue_id
      JOIN issues i ON i.id = COALESCE(s.issue_id, w.card_issue_id)
     -- sk-674.18 T029 (INV-11): a card of a project the caller no longer
     -- sees is no card (its subscription stays stored, unshown) — the point
     -- form per card; the window rows need no check of their own: they reach
     -- the totals only through a visible card.
     WHERE ` + authz.RecipientVisibleExpr("$1::uuid", "i.project_id") + `
),
totals AS (
    SELECT count(*) FILTER (WHERE subscribed)::int    AS subscriptions_total,
           count(*) FILTER (WHERE NOT is_closed)::int AS open_total,
           count(*) FILTER (WHERE is_closed)::int     AS closed_total,
           count(*) FILTER (WHERE is_closed
                              AND NOT (subscribed AND 'manual' = ANY(reasons)))::int AS closed_auto_total,
           COALESCE(sum(unread_count), 0)::int        AS unread_total
      FROM cards
),
picked AS (
    SELECT c.issue_id, c.subscribed, c.reasons, c.since, c.unread_count, c.last_at, c.space_short
      FROM cards c
     WHERE $3::text = 'all' OR (($3::text = 'closed') = c.is_closed)
     ORDER BY c.last_at DESC NULLS LAST, c.issue_id
     LIMIT $2
)
SELECT p.issue_id, p.subscribed, p.reasons, p.since, p.unread_count, p.last_at, p.space_short,
       t.subscriptions_total, t.open_total, t.closed_total, t.closed_auto_total, t.unread_total
  FROM totals t
  LEFT JOIN picked p ON true
 ORDER BY p.last_at DESC NULLS LAST, p.issue_id`

	rows, err := s.pool.Query(ctx, sql, userID, WatchingFeedLimit, string(state), s.watchingWindow)
	if err != nil {
		return WatchingFeedResult{}, fmt.Errorf("listing watching feed: %w", err)
	}
	defer rows.Close()

	out := WatchingFeedResult{Rows: make([]WatchingBase, 0, 16)}
	for rows.Next() {
		var (
			issueID    *string
			subscribed *bool
			reasons    []string
			since      *time.Time
			unread     *int
			lastAt     *time.Time
			spaceShort *string
		)
		if err := rows.Scan(&issueID, &subscribed, &reasons, &since, &unread, &lastAt, &spaceShort,
			&out.SubscriptionsTotal, &out.OpenTotal, &out.ClosedTotal, &out.ClosedAutoTotal, &out.UnreadTotal); err != nil {
			return WatchingFeedResult{}, fmt.Errorf("scanning watching row: %w", err)
		}
		if issueID == nil {
			// The totals-only row: the filter selected nothing.
			continue
		}
		row := WatchingBase{IssueID: *issueID, Reasons: []string{}, LastActivity: lastAt}
		if spaceShort != nil {
			row.SpaceID = *spaceShort
		}
		if subscribed != nil && *subscribed {
			row.Reasons = reasons
			row.Since = since
		}
		if unread != nil {
			row.UnreadEventsCount = *unread
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return WatchingFeedResult{}, err
	}
	switch state {
	case WatchingStateOpen:
		out.StateTotal = out.OpenTotal
	case WatchingStateClosed:
		out.StateTotal = out.ClosedTotal
	default:
		out.StateTotal = out.OpenTotal + out.ClosedTotal
	}
	out.Truncated = out.StateTotal > len(out.Rows)
	return out, nil
}

// MaxBulkUnwatchIDs caps an explicit selection in BulkUnwatch. The
// Watching list shows at most WatchingFeedLimit cards, so a selection made
// on it is far below this; the cap only bounds a hand-written request.
const MaxBulkUnwatchIDs = 500

// ErrBulkUnwatchTooMany is returned when an explicit selection exceeds
// MaxBulkUnwatchIDs.
var ErrBulkUnwatchTooMany = errors.New("too many issue ids in one bulk unwatch")

// BulkUnwatch stops the caller watching many issues at once (sk-551.10,
// item 3; reworked for sk-622.21, US-1/US-6). Exactly one of two selectors is
// used:
//
//   - IssueIDs (an explicit selection — "Stop watching the selected"),
//     handled per card KIND. A card the caller is subscribed to gets the
//     explicit unwatch, exactly the single one (RemoveIssueWatch): the
//     subscription is deleted whatever its reasons (picking a card IS the
//     explicit choice, so a manual subscription goes too), the opt-out is
//     recorded and the card is cleared on all of the caller's
//     notifications. A HISTORICAL card (no subscription, notifications
//     still carded to it) is removed by dismissWatchingCardsTx: cards
//     cleared, no opt-out (R3). An id that is neither is ignored.
//   - State (a filter — "Stop watching the closed"): the caller's live
//     subscriptions on tasks in that state (see WatchingState) are deleted
//     WITHOUT an opt-out — a sweep over a filter is housekeeping, not a
//     refusal, so reopening such a task subscribes the person again. A
//     subscription that carries `manual` is NOT touched unless
//     IncludeManual is set: the person once asked for it by hand. Such rows
//     are counted in KeptManual. The Watching card is cleared on the
//     caller's notifications of every card the operation touched: the
//     deleted subscriptions' cards and every historical card of a task in
//     that state.
//
// Only the CALLER's rows are ever read or written: the user id is a
// predicate of every statement, never a value from the request body.
//
// One SERIALIZABLE transaction with a replay (K2): either the whole
// selection is applied or none of it, a concurrent fan-out writing one of
// the selected cards cannot leave it behind (INV-6), and a 40001 is resolved
// here rather than returned.
func (s *Store) BulkUnwatch(ctx context.Context, userID uuid.UUID, in BulkUnwatchInput) (BulkUnwatchResult, error) {
	if len(in.IssueIDs) > MaxBulkUnwatchIDs {
		return BulkUnwatchResult{}, ErrBulkUnwatchTooMany
	}
	if in.IssueIDs == nil {
		if _, ok := ParseWatchingState(string(in.State)); !ok {
			return BulkUnwatchResult{}, fmt.Errorf("bulk unwatch: unknown state %q", in.State)
		}
	}
	var out BulkUnwatchResult
	err := withSerializableRetry(ctx, s.pool, "bulkUnwatch", bulkUnwatchMaxAttempts, func(tx pgx.Tx) error {
		var err error
		if in.IssueIDs != nil {
			out, err = bulkUnwatchSelectionTx(ctx, tx, userID, in.IssueIDs)
		} else {
			out, err = bulkUnwatchStateTx(ctx, tx, userID, in.State, in.IncludeManual)
		}
		if err != nil {
			return err
		}
		// sk-622.21 T016 (US-11): "bulk unsubscribe / remove" — the caller's
		// Watching changed when a subscription or a historical card went.
		if out.Unwatched > 0 || out.HistoricalRemoved > 0 {
			signals.Add(tx, userID, signals.AreaWatching)
		}
		return nil
	})
	if err != nil {
		return BulkUnwatchResult{}, fmt.Errorf("bulk unwatch: %w", err)
	}
	return out, nil
}

// bulkUnwatchSelectionTx is BulkUnwatch over an explicit selection. Two
// statements in the caller's transaction: the explicit unwatch of the
// selected subscriptions (UnwatchExplicitTx over a set), then the R3 removal
// of the remaining, historical cards.
func bulkUnwatchSelectionTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, issueIDs []string) (BulkUnwatchResult, error) {
	const unwatchSQL = `
WITH del AS (
    DELETE FROM issue_watchers iw
     WHERE iw.user_id = $1
       AND iw.issue_id = ANY($2::text[])
    RETURNING iw.issue_id
),
opt AS (
    INSERT INTO issue_watch_optouts (issue_id, user_id)
    SELECT issue_id, $1 FROM del
    ON CONFLICT (issue_id, user_id) DO NOTHING
),
clr AS (
    UPDATE notifications n
       SET card_issue_id = NULL
      FROM del d
     WHERE n.user_id = $1
       AND n.card_issue_id = d.issue_id
)
SELECT COALESCE(array_agg(issue_id), '{}'::text[]),
       count(*)::int
  FROM del`
	var (
		deleted []string
		out     BulkUnwatchResult
	)
	if err := tx.QueryRow(ctx, unwatchSQL, userID, issueIDs).Scan(&deleted, &out.Unwatched); err != nil {
		return BulkUnwatchResult{}, fmt.Errorf("unwatching selection: %w", err)
	}
	done := make(map[string]struct{}, len(deleted))
	for _, id := range deleted {
		done[id] = struct{}{}
	}
	rest := make([]string, 0, len(issueIDs))
	for _, id := range issueIDs {
		if _, ok := done[id]; !ok {
			rest = append(rest, id)
		}
	}
	if len(rest) > 0 {
		n, err := dismissWatchingCardsTx(ctx, tx, userID, rest)
		if err != nil {
			return BulkUnwatchResult{}, err
		}
		out.HistoricalRemoved = n
	}
	return out, nil
}

// bulkUnwatchStateTx is BulkUnwatch over a state filter, one statement.
// The data-modifying CTEs share one snapshot, so `hist` — cards of tasks in
// the state with no live subscription of the caller — is computed from the
// subscriptions as they were before `del`, and never includes a card whose
// subscription this statement deletes (those are cleared through `picked`
// and reported as Unwatched). A kept manual subscription is not touched,
// and neither is its card. `hist` finds the cards the column shows (rows
// not archived); a card the operation takes is cleared on EVERY notification
// of the caller carded to it, archived ones included (INV-6, K6).
func bulkUnwatchStateTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, state WatchingState, includeManual bool) (BulkUnwatchResult, error) {
	sql := `
WITH target AS (
    SELECT iw.issue_id, ('manual' = ANY(iw.reasons)) AS has_manual
      FROM issue_watchers iw
      JOIN issues i ON i.id = iw.issue_id
     WHERE iw.user_id = $1
       AND ($2::text = 'all' OR (($2::text = 'closed') = (` + status.IsClosed("i") + `)))
),
picked AS (
    SELECT issue_id FROM target
     WHERE $3::boolean OR NOT has_manual
),
del AS (
    DELETE FROM issue_watchers iw
     USING picked p
     WHERE iw.user_id = $1
       AND iw.issue_id = p.issue_id
    RETURNING iw.issue_id
),
hist AS (
    SELECT DISTINCT n.card_issue_id AS issue_id
      FROM notifications n
      JOIN issues i ON i.id = n.card_issue_id
     WHERE n.user_id = $1
       AND n.card_issue_id IS NOT NULL
       AND n.archived_at IS NULL
       AND ($2::text = 'all' OR (($2::text = 'closed') = (` + status.IsClosed("i") + `)))
       AND NOT EXISTS (
           SELECT 1 FROM issue_watchers iw
            WHERE iw.user_id = $1
              AND iw.issue_id = n.card_issue_id)
),
clr AS (
    UPDATE notifications n
       SET card_issue_id = NULL
     WHERE n.user_id = $1
       AND n.card_issue_id IN (SELECT issue_id FROM picked
                               UNION ALL
                               SELECT issue_id FROM hist)
)
SELECT (SELECT count(*) FROM del)::int,
       ((SELECT count(*) FROM target) - (SELECT count(*) FROM picked))::int,
       (SELECT count(*) FROM hist)::int`
	var out BulkUnwatchResult
	if err := tx.QueryRow(ctx, sql, userID, string(state), includeManual).
		Scan(&out.Unwatched, &out.KeptManual, &out.HistoricalRemoved); err != nil {
		return BulkUnwatchResult{}, fmt.Errorf("unwatching by state %q: %w", state, err)
	}
	return out, nil
}

// dismissWatchingCardsTx is "remove" — remove HISTORICAL Watching cards
// from the caller's column (sk-622.21, US-6, decision R3), inside the
// caller's transaction; its one product caller is the selection branch of
// BulkUnwatch (bulkUnwatchSelectionTx), which owns SERIALIZABLE and the
// replay (K2). For every given issue the caller has NO live subscription to,
// the Watching card is cleared on the caller's notifications carded to it;
// the subscription table is not touched and no opt-out is written. An id the
// caller is subscribed to is skipped — stopping a subscription is the
// explicit unwatch's job, and it records an opt-out this operation must not.
// A historical card is one the column shows
// (found on rows not archived); once taken, it is cleared on EVERY
// notification of the caller carded to it, archived ones included (INV-6,
// K6). Returns how many cards were removed, counted in distinct card issues.
func dismissWatchingCardsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, issueIDs []string) (int, error) {
	const sql = `
WITH hist AS (
    SELECT DISTINCT n.card_issue_id AS issue_id
      FROM notifications n
     WHERE n.user_id = $1
       AND n.card_issue_id = ANY($2::text[])
       AND n.archived_at IS NULL
       AND NOT EXISTS (
           SELECT 1 FROM issue_watchers iw
            WHERE iw.user_id = $1
              AND iw.issue_id = n.card_issue_id)
),
clr AS (
    UPDATE notifications n
       SET card_issue_id = NULL
      FROM hist h
     WHERE n.user_id = $1
       AND n.card_issue_id = h.issue_id
)
SELECT count(*)::int FROM hist`
	var n int
	if err := tx.QueryRow(ctx, sql, userID, issueIDs).Scan(&n); err != nil {
		return 0, fmt.Errorf("dismissing historical cards: %w", err)
	}
	return n, nil
}

// HydrateWatchingRecentEvents loads the in-card activity timeline of the
// given Watching cards, keyed by card issue id (sk-622.21, US-5, R10): per
// card, the caller's notifications carrying that card, read or unread, not
// archived, written within WatchingRecentWindow (7 days), newest first, at
// most WatchingRecentTopN (5). The timeline shows exactly what the caller
// was notified about on the card, so card, counter and badge are views of
// one set; the activity of a subtree's descendants is on the card the
// fan-out gave it (the ancestor's).
//
// The 7-day / 5-row bound is the timeline's own (R10) and is not the
// Watching window: the window decides which cards exist and what they
// count, the timeline is a short history of each. ROW_NUMBER inside SQL
// keeps the payload bounded however noisy a card is; the rows are found
// through idx_notif_watching_card (user_id, card_issue_id, created_at DESC).
//
// The wire rows are hydrated through hydrateNotificationDTO exactly like
// the inbox, so a card row and an inbox row of the same notification are
// the same shape. `id` is the NOTIFICATION id.
func (s *Store) HydrateWatchingRecentEvents(
	ctx context.Context,
	userID uuid.UUID,
	cardIssueIDs []string,
) (map[string][]NotificationDTO, error) {
	if len(cardIssueIDs) == 0 {
		return map[string][]NotificationDTO{}, nil
	}
	sql := `
WITH windowed AS (
    SELECT n.id, n.type, n.created_at, n.read_at, n.event_id, n.card_issue_id,
           ROW_NUMBER() OVER (PARTITION BY n.card_issue_id
                              ORDER BY n.created_at DESC, n.id DESC) AS rn
      FROM notifications n
     WHERE n.user_id = $1
       AND n.card_issue_id = ANY($2::text[])
       AND n.archived_at IS NULL
       AND n.created_at >= now() - ($4::interval)
)
SELECT w.card_issue_id, w.id, w.type, w.created_at, w.read_at,
       e.actor, e.actor_user_id, e.actor_type,
       e.entity_type, e.entity_id, e.project_id,
       i.title, pr.id_prefix,
       e.field, e.old_data, e.new_data, e.payload,
       ci.id    AS comment_issue_id,
       ci.title AS comment_issue_title,
       cpr.id_prefix AS comment_project_prefix` + notifSpaceCol + `
  FROM windowed w
  JOIN events e ON e.id = w.event_id
  LEFT JOIN issues i  ON e.entity_type = 'issue' AND i.id = e.entity_id
  LEFT JOIN projects pr ON pr.id = e.project_id
  LEFT JOIN comments c ON e.entity_type = 'comment'
                       AND c.id = (CASE WHEN e.entity_type = 'comment'
                                        THEN e.entity_id::uuid
                                        ELSE NULL END)
  LEFT JOIN issues ci ON ci.id = c.issue_id
  LEFT JOIN projects cpr ON cpr.id = ci.project_id
 WHERE w.rn <= $3
 ORDER BY w.card_issue_id, w.created_at DESC, w.id DESC`

	rows, err := s.pool.Query(ctx, sql, userID, cardIssueIDs, WatchingRecentTopN, WatchingRecentWindow)
	if err != nil {
		return nil, fmt.Errorf("loading watching recent events: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]NotificationDTO, len(cardIssueIDs))
	for rows.Next() {
		var (
			cardIssueID                       string
			n                                 NotificationDTO
			actorName                         string
			actorUserID                       *uuid.UUID
			actorType                         string
			entityType, entityID              string
			projectID                         *uuid.UUID
			issueTitle, projectIDPrefix       *string
			fieldName                         *string
			oldData, newData, payload         []byte
			commentIssueID, commentIssueTitle *string
			commentProjectPrefix              *string
			spaceShort                        *string
		)
		if err := rows.Scan(
			&cardIssueID,
			&n.ID, &n.Type, &n.CreatedAt, &n.ReadAt,
			&actorName, &actorUserID, &actorType,
			&entityType, &entityID, &projectID,
			&issueTitle, &projectIDPrefix,
			&fieldName, &oldData, &newData, &payload,
			&commentIssueID, &commentIssueTitle, &commentProjectPrefix,
			&spaceShort,
		); err != nil {
			return nil, fmt.Errorf("scanning recent event: %w", err)
		}

		// Same actor / entity / field / summary hydration as the inbox
		// paths — routed through the shared helper so the issue-vs-comment
		// re-pointing cannot drift between this projection and ListInbox.
		hydrateNotificationDTO(&n,
			actorName, actorUserID, actorType,
			entityType, entityID, projectID,
			issueTitle, projectIDPrefix,
			fieldName, oldData, newData, payload,
			commentIssueID, commentIssueTitle, commentProjectPrefix,
		)
		if spaceShort != nil {
			n.SpaceID = *spaceShort
		}

		out[cardIssueID] = append(out[cardIssueID], n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// mentionMeta is the per-row metadata captured during ListInbox so the
// snippet-hydration second pass knows where to look for the underlying
// body. Internal to the package — only ListInbox + hydrateMentionContext
// reference it.
//
// For comment.mention: eventEntType='comment', eventEntID is the comment
// uuid. For issue.mention: eventEntType='issue', eventEntID is the
// hierarchical issue id and payload.fields[0] dictates which issue
// column to read (description / notes / acceptance_criteria / design).
type mentionMeta struct {
	idx          int    // position in the items slice the snippet should be written to
	evtType      string // "comment.mention" | "issue.mention"
	eventEntType string // events.entity_type — 'comment' | 'issue'
	eventEntID   string // events.entity_id — comment uuid or hierarchical issue id
	payload      []byte // raw events.payload for fields[] extraction (issue.mention)
}

// lookupUsername resolves a user's username for the snippet formatter.
// Used only for the mentions scope; non-fatal — caller falls back to
// skipping snippet hydration when lookup fails.
func (s *Store) lookupUsername(ctx context.Context, userID uuid.UUID) (string, error) {
	var username string
	err := s.pool.QueryRow(ctx,
		`SELECT username FROM users WHERE id = $1`, userID,
	).Scan(&username)
	if err != nil {
		return "", err
	}
	return username, nil
}

// hydrateMentionContext fills NotificationDTO.MentionContext on every
// mention-typed row by loading the relevant body (comment.body or one
// of issues.{description,notes,acceptance_criteria,design}) and
// extracting a ~120-char snippet around the first `@<callerUsername>`
// occurrence. Errors during fetch are swallowed — the worst case is an
// empty MentionContext on a row, which the UI handles gracefully.
//
// Implementation: two batched SELECTs — one over comment ids, one per
// distinct issue-field combo. Typical inbox page has ≤100 rows so the
// fan-out is bounded; per-field grouping keeps the SQL clean.
func (s *Store) hydrateMentionContext(
	ctx context.Context,
	items []NotificationDTO,
	meta []mentionMeta,
	callerUsername string,
) {
	// Partition into comment.mention vs issue.mention buckets.
	commentIDs := make([]uuid.UUID, 0)
	commentIdxByID := make(map[uuid.UUID][]int) // comment uuid → indices in items
	// issue.mention: group by field name so we issue one SELECT per
	// distinct column. `issueIdxByField[field]` maps issue id → items idx.
	issueByField := make(map[string]map[string][]int) // field → issueID → indices

	for _, m := range meta {
		switch m.evtType {
		case "comment.mention":
			if m.eventEntType != "comment" || m.eventEntID == "" {
				continue
			}
			cid, err := uuid.Parse(m.eventEntID)
			if err != nil {
				continue
			}
			if _, seen := commentIdxByID[cid]; !seen {
				commentIDs = append(commentIDs, cid)
			}
			commentIdxByID[cid] = append(commentIdxByID[cid], m.idx)
		case "issue.mention":
			if m.eventEntType != "issue" || m.eventEntID == "" {
				continue
			}
			fields := mentionFieldsFromPayload(m.payload)
			if len(fields) == 0 {
				// Defensive: payload without fields[] — fall back to
				// 'description' so the snippet still gets a shot at the
				// most common mention site.
				fields = []string{"description"}
			}
			field := fields[0]
			// Whitelist field names to prevent SQL injection via column
			// name. The mentions writer in skein-api/internal/issues only
			// emits these four. Anything else → skip snippet (defensive).
			switch field {
			case "description", "notes", "acceptance_criteria", "design":
				// ok
			default:
				continue
			}
			byIssue, ok := issueByField[field]
			if !ok {
				byIssue = make(map[string][]int)
				issueByField[field] = byIssue
			}
			byIssue[m.eventEntID] = append(byIssue[m.eventEntID], m.idx)
		}
	}

	// Pass 1 — comment bodies.
	if len(commentIDs) > 0 {
		rows, err := s.pool.Query(ctx,
			`SELECT id, body FROM comments WHERE id = ANY($1)`, commentIDs)
		if err == nil {
			for rows.Next() {
				var (
					cid  uuid.UUID
					body string
				)
				if err := rows.Scan(&cid, &body); err != nil {
					continue
				}
				snippet := extractMentionContext(body, callerUsername)
				for _, idx := range commentIdxByID[cid] {
					items[idx].MentionContext = snippet
				}
			}
			rows.Close()
		}
	}

	// Pass 2 — issue field bodies. One query per distinct field name so
	// we can interpolate the column safely (whitelisted above).
	for field, byIssue := range issueByField {
		if len(byIssue) == 0 {
			continue
		}
		ids := make([]string, 0, len(byIssue))
		for id := range byIssue {
			ids = append(ids, id)
		}
		// #nosec G201 — `field` is whitelisted above; ids passed as
		// parameter array so user input is fully separated from SQL.
		sql := fmt.Sprintf(`SELECT id, %s FROM issues WHERE id = ANY($1)`, field)
		rows, err := s.pool.Query(ctx, sql, ids)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id, body string
			if err := rows.Scan(&id, &body); err != nil {
				continue
			}
			snippet := extractMentionContext(body, callerUsername)
			for _, idx := range byIssue[id] {
				items[idx].MentionContext = snippet
			}
		}
		rows.Close()
	}
}

// extractMentionContext returns a ~120-char snippet of body centred on
// the first case-insensitive `@<callerUsername>` occurrence:
//
//   - 60 chars before + 60 chars after; trimmed at word boundaries so
//     partial words at either edge are dropped.
//   - `...` prepended/appended when content was trimmed on either side.
//   - Newlines collapsed to spaces so the snippet is single-line.
//   - When `@<callerUsername>` isn't found, returns the leading 120
//     chars (with trailing ellipsis if longer) — defensive fallback so
//     malformed/edge-case rows still surface something useful.
//   - Empty body → empty string.
//
// The helper is package-private on purpose — nothing outside this
// package needs it, and the in-package tests call it directly.
func extractMentionContext(body, callerUsername string) string {
	// Collapse newlines and tabs to single spaces; collapse runs of
	// whitespace too so the snippet reads cleanly on one line.
	flat := collapseWhitespace(body)
	if flat == "" {
		return ""
	}
	const before = 60
	const after = 60

	needle := "@" + callerUsername
	idx := indexFoldASCII(flat, needle)
	if idx < 0 {
		// Mention marker not found — defensive fallback. Take first 120
		// chars; append `...` if the body was longer.
		const max = before + after
		if len(flat) <= max {
			return flat
		}
		return trimToWordBoundary(flat[:max], false) + "..."
	}

	// Window: [idx-before, idx+len(needle)+after]
	start := idx - before
	end := idx + len(needle) + after
	leadingCut := start > 0
	trailingCut := end < len(flat)
	if start < 0 {
		start = 0
	}
	if end > len(flat) {
		end = len(flat)
	}
	window := flat[start:end]

	// Trim partial words at edges only when we actually cut. Order
	// matters: trim leading first so the trailing trim doesn't operate
	// on indices invalidated by the leading mutation.
	if leadingCut {
		window = trimToWordBoundary(window, true)
	}
	if trailingCut {
		window = trimToWordBoundary(window, false)
	}

	if leadingCut {
		window = "..." + window
	}
	if trailingCut {
		window = window + "..."
	}
	return window
}

// collapseWhitespace replaces every run of ASCII whitespace
// (space/tab/newline/CR) with a single space, then trims leading/
// trailing whitespace. Keeps non-ASCII characters intact.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // suppress leading whitespace
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteByte(c)
		prevSpace = false
	}
	out := b.String()
	if len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return out
}

// indexFoldASCII reports the first index of needle in haystack using
// case-insensitive ASCII comparison. Returns -1 if not found. Both
// strings are expected to be flattened single-line strings.
func indexFoldASCII(haystack, needle string) int {
	if needle == "" {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	hLow := strings.ToLower(haystack)
	nLow := strings.ToLower(needle)
	return strings.Index(hLow, nLow)
}

// trimToWordBoundary drops the leading or trailing partial word from s.
// When trimLeading=true, removes everything up to and including the
// first space (drops a possibly-half word at the start). When
// trimLeading=false, removes everything from the last space onward
// (drops a possibly-half word at the end). If s has no internal space,
// returns s unchanged — the whole snippet is one token, no boundary to
// trim against.
func trimToWordBoundary(s string, trimLeading bool) string {
	if trimLeading {
		if _, after, found := strings.Cut(s, " "); found {
			return after
		}
		return s
	}
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return s
	}
	return s[:i]
}

// ----------------- helpers -----------------

func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return "TRUE"
	case 1:
		return parts[0]
	default:
		var b strings.Builder
		b.WriteString(parts[0])
		for _, p := range parts[1:] {
			b.WriteString(" AND ")
			b.WriteString(p)
		}
		return b.String()
	}
}

// encodeIDCursor / decodeIDCursor — opaque base64 wrap so changes to the
// internal cursor schema (e.g. switching to (created_at, id)) don't break
// clients that store cursors verbatim.
func encodeIDCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

// DecodeCursor parses an opaque cursor string back into the bigint id.
// Exported so the handler layer can decode incoming ?cursor= without
// knowing the encoding.
func DecodeCursor(s string) (int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	id, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor payload: %w", err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid cursor payload: must be positive")
	}
	return id, nil
}

// ----------------- sk-303.13 / C1 — "reason × channel" settings -----------------

// GetReasonStates returns the caller's per-reason channel states, keyed by
// reason. A reason with no row falls back to the registry default rather than
// to "everything on": the absence of a row means "the new default is enough", not
// "the user switched everything on". That fallback is what keeps a freshly created
// account (whose rows the migration could not have written) from being louder
// than a migrated one.
func (s *Store) GetReasonStates(ctx context.Context, userID uuid.UUID) (map[ReasonKey]ReasonChannelState, error) {
	states := make(map[ReasonKey]ReasonChannelState, len(reasonRegistry))
	for _, d := range DefaultChannelStates() {
		states[d.Key] = ReasonChannelState{Feed: d.Feed, Push: d.Push}
	}

	rows, err := s.pool.Query(ctx, `
SELECT reason, feed_enabled, push_enabled
  FROM user_notification_reason_prefs
 WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("loading reason prefs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var feed, push bool
		if err := rows.Scan(&reason, &feed, &push); err != nil {
			return nil, fmt.Errorf("scanning reason prefs: %w", err)
		}
		key := ReasonKey(reason)
		// A row for a reason the registry no longer knows is ignored on read
		// and left untouched on write: the registry is the source of truth for
		// what the form shows, and silently deleting a stored state would lose
		// a preference the next version might understand again.
		if _, ok := reasonByKey[key]; !ok {
			continue
		}
		states[key] = ReasonChannelState{Feed: feed, Push: push}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading reason prefs: %w", err)
	}
	// An inapplicable channel cannot be on, whatever the row contains.
	for key, st := range states {
		r := reasonByKey[key]
		if !r.Feed.Applicable {
			st.Feed = false
		}
		if !r.Push.Applicable {
			st.Push = false
		}
		states[key] = st
	}
	return states, nil
}

// GetReasonPrefs builds the settings-form payload: the ACTIVE registry rows
// with this caller's channel states, plus the agent-action suppression flag
// that lives on the page beside the table.
func (s *Store) GetReasonPrefs(ctx context.Context, userID uuid.UUID) (ReasonPrefsDTO, error) {
	states, err := s.GetReasonStates(ctx, userID)
	if err != nil {
		return ReasonPrefsDTO{}, err
	}
	prefs, err := s.GetPrefs(ctx, userID)
	if err != nil {
		return ReasonPrefsDTO{}, err
	}

	// sk-551.10 — read on its own rather than through GetPrefs, so the
	// older /me/notification-prefs contract (PrefsDTO) does not grow a
	// field it never documented. No row → the default, ON (sk-622.21,
	// US-2: the same value a row created by another setting's upsert gets).
	var unwatchOnClose bool
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT unwatch_on_close FROM user_notification_prefs WHERE user_id = $1), true)`,
		userID).Scan(&unwatchOnClose); err != nil {
		return ReasonPrefsDTO{}, fmt.Errorf("reading unwatch_on_close: %w", err)
	}

	active := ActiveReasons()
	out := ReasonPrefsDTO{
		Reasons:              make([]ReasonRowDTO, 0, len(active)),
		SuppressAgentActions: prefs.SuppressAgentActions,
		UnwatchOnClose:       unwatchOnClose,
	}
	for _, r := range active {
		st := states[r.Key]
		out.Reasons = append(out.Reasons, ReasonRowDTO{
			Key:   string(r.Key),
			Label: r.Label,
			Hint:  r.Hint,
			Feed: ReasonChannelDTO{
				Applicable: r.Feed.Applicable,
				Audience:   string(r.Feed.Audience),
				Enabled:    st.Feed,
			},
			Push: ReasonChannelDTO{
				Applicable: r.Push.Applicable,
				Audience:   string(r.Push.Audience),
				Enabled:    st.Push,
			},
		})
	}
	return out, nil
}

// ErrUnknownReason is returned when an update names a reason outside the
// closed registry. The caller answers 422: the request was well-formed and the
// value is a business-level mistake.
var ErrUnknownReason = errors.New("unknown reason")

// ErrReasonNotSettable is returned when an update names a RESERVED reason —
// one the registry pins for a later wave, whose mechanics do not exist yet.
// The form never shows such a row precisely because a toggle without an effect
// is the defect this contract removes; the API is the second surface of the
// same contract, so it refuses the same thing instead of storing a preference
// nothing reads. Business error (422), not a syntax one: the key is spelled
// correctly, it is simply not settable in this wave.
var ErrReasonNotSettable = errors.New("this reason is reserved for a future release and cannot be set yet")

// ErrChannelNotApplicable is returned when an update tries to switch ON a
// channel the reason does not have. The form draws a dash there and cannot
// send it, so this is a contract violation rather than a user typo — but it is
// still a business error (422), not a syntax one.
var ErrChannelNotApplicable = errors.New("channel not applicable for this reason")

// upsertReasonPref merges ONE channel pair without reading first.
//
// $3/$4 are *bool: nil means «this channel was not named, keep it». The merge
// therefore happens inside the statement — `COALESCE($3, <table>.feed_enabled)`
// on conflict — instead of in a snapshot taken before the transaction. That is
// what makes the partial update actually partial: two concurrent requests
// touching different channels of one reason no longer overwrite each other,
// because each statement reads the row it locks rather than a snapshot read
// minutes (or microseconds) earlier.
//
// The explicit ::boolean casts are load-bearing: a bare parameter inside
// COALESCE has no inferable type, and the server would resolve the call as
// text and refuse the boolean column.
//
// $5/$6 carry the registry default of the same reason and are used ONLY on
// insert, when no row exists yet: the untouched channel must start from the
// narrow default, not from `false`. Writing `false` there would silently mute
// a channel the caller never named.
const upsertReasonPref = `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled, updated_at)
VALUES ($1, $2, COALESCE($3::boolean, $5::boolean), COALESCE($4::boolean, $6::boolean), now())
ON CONFLICT (user_id, reason) DO UPDATE
   SET feed_enabled = COALESCE($3::boolean, user_notification_reason_prefs.feed_enabled),
       push_enabled = COALESCE($4::boolean, user_notification_reason_prefs.push_enabled),
       updated_at   = now()`

// upsertSuppressAgentActions writes the single flag that lives on the same
// settings page beside the reason table. It touches ONLY that column, so it can
// run inside the same transaction as the reason rows without dragging the rest
// of user_notification_prefs (the event-type whitelist, the inbox scope) through
// a read-modify-write that would lose a concurrent edit of those.
const upsertSuppressAgentActions = `
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE
   SET suppress_agent_actions = EXCLUDED.suppress_agent_actions,
       updated_at                 = now()`

// upsertUnwatchOnClose writes the «unwatch on close» flag (sk-551.10) and
// nothing else, for the same reason upsertSuppressAgentActions touches only
// its own column. On insert, suppress_agent_actions takes the value the
// fan-out treats as default for a person with no row (true, sk-343) — a
// row created for this flag must not silently switch that one off.
//
// It returns the value in effect BEFORE the write — the stored one, or the
// no-row default ON that GetReasonPrefs answers with — read from the
// statement's snapshot, so the caller signals only an actual change
// (found in review).
const upsertUnwatchOnClose = `
WITH prev AS (
    SELECT unwatch_on_close FROM user_notification_prefs WHERE user_id = $1
)
INSERT INTO user_notification_prefs (user_id, suppress_agent_actions, unwatch_on_close, updated_at)
VALUES ($1, true, $2, now())
ON CONFLICT (user_id) DO UPDATE
   SET unwatch_on_close = EXCLUDED.unwatch_on_close,
       updated_at       = now()
RETURNING COALESCE((SELECT unwatch_on_close FROM prev), true)`

// foldReasonUpdates collapses repeated mentions of one reason in a single body
// into one update per reason, preserving first-mention order.
//
// Without the fold, `[{"reason":"mention","feed":false},{"reason":"mention","push":false}]`
// would be two statements and the merge semantics would have to hold ACROSS
// them; folding makes the request carry one final state per reason, which is
// also what the caller means. Within one reason the LAST named value of a
// channel wins — a body that contradicts itself resolves to its own tail
// rather than to an error nobody can act on.
func foldReasonUpdates(updates []ReasonChannelUpdate) []ReasonChannelUpdate {
	order := make([]string, 0, len(updates))
	byReason := make(map[string]ReasonChannelUpdate, len(updates))
	for _, u := range updates {
		prev, seen := byReason[u.Reason]
		if !seen {
			order = append(order, u.Reason)
			byReason[u.Reason] = u
			continue
		}
		if u.Feed != nil {
			prev.Feed = u.Feed
		}
		if u.Push != nil {
			prev.Push = u.Push
		}
		byReason[u.Reason] = prev
	}
	out := make([]ReasonChannelUpdate, 0, len(order))
	for _, key := range order {
		out = append(out, byReason[key])
	}
	return out
}

// UpdateReasonPrefs applies a PARTIAL update: only the named reasons (and only
// the named channels of those reasons) change; everything else keeps its
// stored state.
//
// That is not politeness, it is the compatibility rule of the contract: a form
// built against an older registry does not know every reason, and a full
// replace from such a client would silently reset the preferences it could not
// render. Unknown reasons are refused rather than stored, so the closed
// registry stays closed in the data too.
//
// The whole update — the reason rows AND the agent-action suppression flag — is
// one transaction. A PUT that applied the channels and then failed on the flag
// would leave exactly the half-applied settings state the contract promises
// never to leave, and the client could not tell a full refusal from a partial
// success on retry.
func (s *Store) UpdateReasonPrefs(ctx context.Context, userID uuid.UUID, input UpdateReasonPrefsInput) (ReasonPrefsDTO, error) {
	// Validate everything BEFORE writing anything: a partially applied update
	// would leave the user with a state they never asked for.
	for _, u := range input.Reasons {
		r, ok := reasonByKey[ReasonKey(u.Reason)]
		if !ok {
			return ReasonPrefsDTO{}, fmt.Errorf("%w: %s", ErrUnknownReason, u.Reason)
		}
		if r.State != ReasonStateActive {
			return ReasonPrefsDTO{}, fmt.Errorf("%w: %s", ErrReasonNotSettable, u.Reason)
		}
		if u.Feed != nil && *u.Feed && !r.Feed.Applicable {
			return ReasonPrefsDTO{}, fmt.Errorf("%w: %s/feed", ErrChannelNotApplicable, u.Reason)
		}
		if u.Push != nil && *u.Push && !r.Push.Applicable {
			return ReasonPrefsDTO{}, fmt.Errorf("%w: %s/push", ErrChannelNotApplicable, u.Reason)
		}
	}

	reasons := foldReasonUpdates(input.Reasons)
	if len(reasons) > 0 || input.SuppressAgentActions != nil || input.UnwatchOnClose != nil {
		// Turning «unwatch on close» on also rewrites the person's
		// subscriptions on closed issues (decision R23), which the close,
		// reopen and fan-out paths read and write in SERIALIZABLE
		// transactions — so that request runs SERIALIZABLE too (K2) and is
		// replayed on a class-40 abort. Every other update writes only the
		// person's own preference rows and keeps READ COMMITTED.
		iso := pgx.ReadCommitted
		if input.UnwatchOnClose != nil && *input.UnwatchOnClose {
			iso = pgx.Serializable
		}
		err := withTxRetry(ctx, s.pool, iso, "update reason prefs", updateReasonPrefsMaxAttempts, func(tx pgx.Tx) error {
			for _, u := range reasons {
				def := reasonByKey[ReasonKey(u.Reason)]
				if _, err := tx.Exec(ctx, upsertReasonPref,
					userID, u.Reason, u.Feed, u.Push, def.Feed.DefaultOn, def.Push.DefaultOn); err != nil {
					return fmt.Errorf("upserting reason pref %s: %w", u.Reason, err)
				}
			}
			if input.SuppressAgentActions != nil {
				if _, err := tx.Exec(ctx, upsertSuppressAgentActions,
					userID, *input.SuppressAgentActions); err != nil {
					return fmt.Errorf("upserting agent-action suppression: %w", err)
				}
			}
			if input.UnwatchOnClose == nil {
				return nil
			}
			var before bool
			if err := tx.QueryRow(ctx, upsertUnwatchOnClose,
				userID, *input.UnwatchOnClose).Scan(&before); err != nil {
				return fmt.Errorf("upserting unwatch-on-close: %w", err)
			}
			if before == *input.UnwatchOnClose {
				// A request that repeats the value already in effect changes
				// nothing and signals nothing (found in
				// review).
				return nil
			}
			// sk-622.21 T016 (US-11, "Settings change"): the unwatch-on-close
			// setting is shown with the Watching column; the reason toggles
			// and the agent-action suppression only shape future
			// notifications and change no current counter or list, so they
			// give no signal. The signal is published only if this attempt
			// commits.
			signals.Add(tx, userID, signals.AreaWatching)
			if *input.UnwatchOnClose {
				// R23: switching on applies to issues already closed, in this
				// same transaction. Switching off gives nothing back.
				if _, err := StripUserAutoWatchesOnClosed(ctx, tx, userID); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return ReasonPrefsDTO{}, err
		}
	}

	return s.GetReasonPrefs(ctx, userID)
}

// PendingInvitations lists the pending space invitations to user — the
// invitation half of his Inbox, for the agent's Inbox (sk-674.18 T029,
// US-5: without the token or the link).
func (s *Store) PendingInvitations(ctx context.Context, user uuid.UUID) ([]spaces.PendingInvitation, error) {
	return spaces.PendingInvitationsTo(ctx, s.pool, user)
}
