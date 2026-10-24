package issues

// personal_signal.go — sk-622.21 T015 (US-11, K13): the addressees of the
// personal signal on the issue write paths. Each path names them inside its
// transaction attempt through signals.Add; the signal is published after the
// attempt commits (T014).
//
// Roles (K13, spec "Personal signal matrix"), read from the issue row
// before and/or after the write, inside the same transaction:
//
//   - assignee — issues.assignee_user_id (the resolved assignee; NULL when
//     the handle does not resolve);
//   - author — issues.created_by;
//   - previous assignees — previous_assignees minus dismissed_by, the same
//     set the "delegated" lens predicate reads (store.go, the lens filter:
//     `me = ANY(previous_assignees) AND NOT me = ANY(dismissed_by)`);
//   - subscribers — notifications.SubscriberUserIDs, which inherits none of
//     the notification suppression (own action, agent actor, settings).
//
// Every helper costs a fixed number of statements, independent of how many
// addressees it names.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz/authzsql"
	"skein-api/internal/eventbus"
	"skein-api/internal/notifications"
	"skein-api/internal/signals"
)

// signalRoles are the K13 roles of one issue row at one moment.
type signalRoles struct {
	assignee uuid.UUID
	author   uuid.UUID
	previous []uuid.UUID
	// inInbox — the issue is in its assignee's Inbox: open, not accepted,
	// created by somebody else (handler/me.go#countInbox).
	inInbox bool
}

// readSignalRoles reads the roles of issue id in one statement.
//
// The assignee is the person the issue is DELIVERED to (INV-30): assignee_user_id — the identity of the assignee text — only while
// that person may work in the project now (authzsql.WorkerEligibleExpr, the
// rule of the Inbox and of assignee.LeftJoin). An account whose handle merely
// equals the text of an issue it cannot see is no addressee.
func readSignalRoles(ctx context.Context, tx pgx.Tx, id string) (signalRoles, error) {
	var (
		r        signalRoles
		assignee *uuid.UUID
	)
	err := tx.QueryRow(ctx, `
SELECT d.assignee,
       i.created_by,
       ARRAY(SELECT p FROM unnest(i.previous_assignees) AS p WHERE NOT p = ANY(i.dismissed_by)),
       (d.assignee IS NOT NULL
        AND i.accepted_at IS NULL
        AND i.status = 'open'
        AND i.created_by IS DISTINCT FROM d.assignee)
  FROM issues i
 CROSS JOIN LATERAL (SELECT CASE WHEN i.assignee_user_id IS NOT NULL
                                  AND `+authzsql.WorkerEligibleExpr("i.assignee_user_id", "i.project_id")+`
                                 THEN i.assignee_user_id END AS assignee) d
 WHERE i.id = $1`, id).Scan(&assignee, &r.author, &r.previous, &r.inInbox)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return signalRoles{}, ErrNotFound
		}
		return signalRoles{}, fmt.Errorf("reading signal roles of %s: %w", id, err)
	}
	if assignee != nil {
		r.assignee = *assignee
	}
	return r, nil
}

// people returns assignee, author and the previous assignees of every given
// role set (nil ids are skipped by signals.Add).
func people(sets ...signalRoles) []uuid.UUID {
	out := make([]uuid.UUID, 0, 4)
	for _, r := range sets {
		out = append(out, r.assignee, r.author)
		out = append(out, r.previous...)
	}
	return out
}

// signalCreated — "Issue created": the assignee gets inbox (unless they are
// the author), lens and tasks; the author lens and tasks.
func signalCreated(tx pgx.Tx, author, assignee uuid.UUID) {
	signals.Add(tx, author, signals.AreaLens, signals.AreaTasks)
	if assignee == uuid.Nil {
		return
	}
	if assignee != author {
		signals.Add(tx, assignee, signals.AreaInbox)
	}
	signals.Add(tx, assignee, signals.AreaLens, signals.AreaTasks)
}

// signalRowFields — "Row field change" and "Due date and
// estimate change": subscribers get watching; assignee, author and previous assignees
// get tasks; the assignee also gets inbox when the issue is in their Inbox.
func signalRowFields(ctx context.Context, tx pgx.Tx, id string) error {
	r, err := readSignalRoles(ctx, tx, id)
	if err != nil {
		return err
	}
	for _, uid := range people(r) {
		signals.Add(tx, uid, signals.AreaTasks)
	}
	if r.inInbox {
		signals.Add(tx, r.assignee, signals.AreaInbox)
	}
	return signalSubscribers(ctx, tx, id)
}

// signalSubscribers gives every subscriber of id the watching area.
func signalSubscribers(ctx context.Context, tx pgx.Tx, id string) error {
	subs, err := notifications.SubscriberUserIDs(ctx, tx, id)
	if err != nil {
		return err
	}
	for _, uid := range subs {
		signals.Add(tx, uid, signals.AreaWatching)
	}
	return nil
}

// signalAssigneeChanged — "Assignee change": the old and the new assignee,
// the author and the previous assignees (before and after) get inbox, lens
// and tasks; the author also gets unreviewed.
func signalAssigneeChanged(tx pgx.Tx, before, after signalRoles) {
	for _, uid := range people(before, after) {
		signals.Add(tx, uid, signals.AreaInbox, signals.AreaLens, signals.AreaTasks)
	}
	signals.Add(tx, before.author, signals.AreaUnreviewed)
	signals.Add(tx, after.author, signals.AreaUnreviewed)
}

// signalStatusChanged — "Status change / close / reopen / outcome":
// the assignee, the author and the previous assignees (before and after) get
// inbox, lens, tasks and unreviewed; the subscribers get watching.
func signalStatusChanged(ctx context.Context, tx pgx.Tx, id string, before signalRoles) error {
	after, err := readSignalRoles(ctx, tx, id)
	if err != nil {
		return err
	}
	for _, uid := range people(before, after) {
		signals.Add(tx, uid, signals.AreaInbox, signals.AreaLens, signals.AreaTasks, signals.AreaUnreviewed)
	}
	return signalSubscribers(ctx, tx, id)
}

// signalAccepted — "Issue accepted": the assignee and the author get inbox,
// lens and tasks.
func signalAccepted(tx pgx.Tx, r signalRoles) {
	signals.Add(tx, r.assignee, signals.AreaInbox, signals.AreaLens, signals.AreaTasks)
	signals.Add(tx, r.author, signals.AreaInbox, signals.AreaLens, signals.AreaTasks)
}

// signalClaim — «Claim / unclaim»: the assignee before and after and the
// author get inbox, lens and tasks.
func signalClaim(ctx context.Context, tx pgx.Tx, id string, before signalRoles) error {
	after, err := readSignalRoles(ctx, tx, id)
	if err != nil {
		return err
	}
	for _, uid := range []uuid.UUID{before.assignee, after.assignee, before.author, after.author} {
		signals.Add(tx, uid, signals.AreaInbox, signals.AreaLens, signals.AreaTasks)
	}
	return nil
}

// signalDelegation — "Delegation refused / resumed": the acting user
// gets lens, tasks and unreviewed, plus watching when the operation cleared
// their Watching cards.
func signalDelegation(tx pgx.Tx, actor uuid.UUID, cardsCleared bool) {
	signals.Add(tx, actor, signals.AreaLens, signals.AreaTasks, signals.AreaUnreviewed)
	if cardsCleared {
		signals.Add(tx, actor, signals.AreaWatching)
	}
}

// signalReview — "Review flag / review mark": the author, and only the
// author, gets unreviewed. Review data is private to the author, so no other
// user is named.
func signalReview(tx pgx.Tx, author uuid.UUID) {
	signals.Add(tx, author, signals.AreaUnreviewed)
}

// rowSignalFields are the patch fields of the matrix rows "Row field
// change" (title, type, priority) and "Due date and estimate change" (due and
// defer dates, estimate). Text fields (description, design, acceptance
// criteria, notes) and external/source references feed no personal counter
// or personal list and give no signal.
var rowSignalFields = map[string]struct{}{
	"title":            {},
	"type":             {},
	"priority":         {},
	"estimatedMinutes": {},
	"dueAt":            {},
	"deferUntil":       {},
}

// signalPatch names the addressees of one committed PatchIssue attempt:
// "Assignee change" when the assignee changed (rolesBefore was read before
// the write), "Row field / due date and estimate change" when a row field
// changed, and "... outcome" when the close outcome of a closed issue changed.
func signalPatch(ctx context.Context, tx pgx.Tx, id string, changes []eventbus.FieldChange, rolesBefore signalRoles, outcomeChanged bool) error {
	var assigneeChanged, rowChanged bool
	for _, fc := range changes {
		if fc.Field == "assignee" {
			assigneeChanged = true
		}
		if _, ok := rowSignalFields[fc.Field]; ok {
			rowChanged = true
		}
	}
	if assigneeChanged {
		after, err := readSignalRoles(ctx, tx, id)
		if err != nil {
			return err
		}
		signalAssigneeChanged(tx, rolesBefore, after)
	}
	if rowChanged {
		if err := signalRowFields(ctx, tx, id); err != nil {
			return err
		}
	}
	if outcomeChanged {
		if err := signalStatusChanged(ctx, tx, id, signalRoles{}); err != nil {
			return err
		}
	}
	return nil
}

// SignalStaleLockReleased names the addressees of a lock the stale-lock
// reaper released (lockreap), in the reaper's event transaction: the
// release is a row of "Status change / close / reopen / outcome"
// (plan K13, spec sk-622.21 US-11) — assignee, author and previous assignees
// get inbox, lens, tasks and unreviewed, subscribers get watching. tx must be
// opened with signals.BeginTxFunc.
func SignalStaleLockReleased(ctx context.Context, tx pgx.Tx, issueID string) error {
	return signalStatusChanged(ctx, tx, issueID, signalRoles{})
}
