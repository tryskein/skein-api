package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/authz"
	"skein-api/internal/hierarchy"
	"skein-api/internal/signals"
)

// mentionEventTypes is the closed set of event types that bypass the fan-out's
// pool-broadcast pattern. Their rows are written by WriteMentionNotification —
// one row per directly addressed recipient, resolved at write time against
// the recipient filter (authz.RecipientEligibleExpr), not against the
// watcher pools.
//
// A mention is a personal call, not an audit-log entry the whole pool should
// see, and INV-8 closes its filter matrix separately: the reason and the
// project membership apply, the subscription machinery (subscription, sticky
// unwatch, type whitelist) and the agent-action suppression do NOT. INV-8
// names the owner mentioned by their OWN agent as the very case the mention
// channel exists for, so suppressing it would empty the channel of its
// purpose.
var mentionEventTypes = map[string]struct{}{
	"comment.mention": {},
	"issue.mention":   {},
}

// notifiableUpdateFields is the set of issue.updated field names that carry a
// delivery path. It is used ONLY to render the summary suffix ("… updated
// demo-42 (priority, assignee)"); the delivery decision itself belongs to
// Classify, which reads the same two field names from EventFacts.
var notifiableUpdateFields = map[string]struct{}{
	fieldPriority: {},
	fieldAssignee: {},
}

// PushDeliveryDeadline bounds how long a push-eligible notification stays
// retryable. After it the delivery runtime (T010) stops retrying and records a
// final outcome, so a transport outage cannot keep a row pending forever.
//
// One hour is chosen for what a push IS: an interruption that is worth making
// while the action it reports is still current. A push delivered a day late is
// noise the recipient cannot act on, and keeping the row retryable that long
// only grows the backlog an outage leaves behind. The value is written here,
// at the moment the row is created, so a row's deadline never depends on which
// version of the runner happens to pick it up.
const PushDeliveryDeadline = time.Hour

// FanOutInput is everything the fan-out needs about one event. It is a struct
// rather than a parameter list because the write-side filter needs BOTH halves
// of the recipient story — who watches the issue, and whom the event concerns
// personally — and a positional signature that grew a "creator" and a "new
// assignee" argument would be read wrong at half its call sites.
type FanOutInput struct {
	// EventID is the events row this fan-out materialises notifications for.
	EventID int64
	// EventType is the events.type value; it is also stored on the
	// notification row as `type`.
	EventType string
	// EntityType is the events.entity_type discriminator. {issue, comment,
	// attachment} all resolve to the SAME issue watcher pool — the comment and
	// attachment events carry their own uuid as entity_id, so the parent issue
	// is passed separately in IssueID.
	EntityType string
	// IssueID is the parent issue of the event; empty when the event is not
	// tied to an issue.
	IssueID string
	// ProjectID is the project the event happened in. uuid.Nil makes the
	// fan-out a no-op (defensive — every issue/comment event carries one).
	ProjectID uuid.UUID
	// ActorUserID is who performed the action. uuid.Nil means "no actor"
	// (system events) and disables self-action exclusion entirely.
	ActorUserID uuid.UUID
	// ActorType is one of {human, agent, system}. Empty defaults to 'human' so
	// a forgotten call site fails closed and never activates the agent
	// suppression branch by accident.
	ActorType string
	// Payload is events.payload — consulted for issue.updated's changed field
	// list. nil is fine for every other type.
	Payload []byte

	// ---- role-path context ------------------------------------------------
	//
	// The subscription path finds people who asked to be told; the role path
	// finds the people the event is ABOUT. They are unioned, and the gates
	// differ: the subscription filters (sticky unwatch, the type whitelist)
	// bind the subscription path only (R-14).

	// CreatorUserID is the issue's author (creator), resolved to a real
	// user. It feeds the «creator» and «delegating_creator» push audiences.
	CreatorUserID uuid.UUID
	// AssigneeUserID is the issue's assignee. It is read only as the NEGATIVE
	// half of the delegating-creator predicate: a creator who is also the
	// assignee has delegated nothing.
	AssigneeUserID uuid.UUID
	// NewAssigneeUserID is the assignee THIS event installed — set on
	// issue.created with an assignee and on an issue.updated that changed the
	// assignee. It feeds the «new_assignee» push audience.
	NewAssigneeUserID uuid.UUID
	// ExplicitAccept marks an issue.accepted written by the explicit
	// acceptance tool. Acceptance stamped automatically (claim, self-assign)
	// is journal-only.
	ExplicitAccept bool
	// StaleLockRelease marks an issue.unclaimed emitted by the stale-lock
	// reaper. A human unclaim is journal-only: its sibling
	// issue.status_changed already carries the signal (INV-6).
	StaleLockRelease bool
}

// facts projects the input onto the classifier's view of the event.
func (in FanOutInput) facts() EventFacts {
	return EventFacts{
		Type:             in.EventType,
		ChangedFields:    changedFieldsFromPayload(in.Payload),
		ExplicitAccept:   in.ExplicitAccept,
		StaleLockRelease: in.StaleLockRelease,
		OutcomeChanged:   outcomeChangedFromPayload(in.Payload),
	}
}

// relationOf builds the recipient's relation to the issue from the input plus
// the one fact only the query knows: whether they came in as a subscriber.
func (in FanOutInput) relationOf(userID uuid.UUID, isSubscriber bool) RecipientRelation {
	return RecipientRelation{
		IsSubscriber:  isSubscriber,
		IsNewAssignee: in.NewAssigneeUserID != uuid.Nil && userID == in.NewAssigneeUserID,
		IsAssignee:    in.AssigneeUserID != uuid.Nil && userID == in.AssigneeUserID,
		IsCreator:     in.CreatorUserID != uuid.Nil && userID == in.CreatorUserID,
		IsActor:       in.ActorUserID != uuid.Nil && userID == in.ActorUserID,
	}
}

// roleCandidates lists the user ids the role path has to consider. Mentions
// and reminders are NOT here: they are point-to-point paths with a single
// known recipient and their own write helpers.
func (in FanOutInput) roleCandidates() []uuid.UUID {
	out := make([]uuid.UUID, 0, 2)
	seen := make(map[uuid.UUID]struct{}, 2)
	for _, id := range []uuid.UUID{in.NewAssigneeUserID, in.CreatorUserID} {
		if id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// querier is the multi-row read surface the fan-out needs. pgx.Tx and the
// savepoint it hands out both satisfy it.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// fanOutCandidate is one recipient the selection query returned, together with
// the single fact SQL knows and Go does not: whether the subscription path
// found them.
type fanOutCandidate struct {
	userID       uuid.UUID
	isSubscriber bool
	// cardIssueID is the Watching card the subscription arms gave this
	// recipient (sk-622.21, US-4): the event's issue for a subscription on
	// it, else the NEAREST ancestor with a subtree subscription, else nil.
	cardIssueID *string
}

// FanOutRecipient is one notification row the fan-out actually wrote, with
// the Watching card it got (sk-622.21, US-4; consumed by the personal
// signal, T016). CardIssueID is nil when the row is not in Watching.
type FanOutRecipient struct {
	UserID      uuid.UUID
	CardIssueID *string
	Feed        bool
}

// FanOutResult is FanOut's full answer: the rows written (ON CONFLICT
// duplicates are not in it).
type FanOutResult struct {
	Recipients []FanOutRecipient
}

// Rows is the number of notification rows the fan-out wrote.
func (r FanOutResult) Rows() int64 { return int64(len(r.Recipients)) }

// FanOut materialises notification rows for an event, applying the reason
// filter AT WRITE TIME (ADR "filter at creation time"). It runs inside
// the caller's transaction, so the event and its notifications land together
// or not at all.
//
// The shape is three set-based statements, and their count does not depend on
// how many recipients the event has — "the reason filter is applied in the
// same query as recipient selection: there must not be a separate round-trip
// per recipient":
//
//  1. recipient selection — the subscription pools (issue, subtree, project,
//     custom) UNIONed with the role recipients, each already filtered by
//     membership, self-action and the subscription-only gates;
//  2. one read of the channel preferences of exactly those recipients for
//     exactly the reasons this event can produce;
//  3. one INSERT of the surviving pairs.
//
// Everything between them is pure: Compose decides, per candidate, which
// channels the single row belongs to.
//
// # Two failures that are not the same failure
//
// A failure to READ THE PREFERENCES is not an error of the parent operation.
// The spec is explicit: settings unavailable — the row is not created, the
// fact is observable, the parent operation does not fail. Writing the row "just in case"
// is not available as a fallback — with the read-side filter gone such a row
// would stay visible forever. So the whole fan-out runs inside a SAVEPOINT: a
// preferences read that fails rolls back to it, leaves zero rows, is logged,
// and returns (0, nil) — the comment, the edit or the issue the user actually
// asked for still commits.
//
// «A failure to read the preferences» is narrower than «any error out of that
// statement», and the difference is load-bearing: a concurrency failure
// (SQLSTATE class 40) or a lost connection is not an outage of the settings,
// it is "retry this transaction". Swallowed, it would let the parent COMMIT
// with the comment written and the notification gone, under a metric and a log
// line naming a cause that never happened. prefsReadUnavailable draws that
// line; everything it rejects goes back to the caller.
//
// Every OTHER database error stays an error and aborts the caller's
// transaction, because it says nothing about the user's intent having been
// honoured.
//
// Returns the number of rows inserted.
func FanOut(ctx context.Context, tx pgx.Tx, in FanOutInput) (int64, error) {
	res, err := FanOutWithRecipients(ctx, tx, in)
	if err != nil {
		return 0, err
	}
	return res.Rows(), nil
}

// FanOutWithRecipients is FanOut returning the written rows themselves —
// recipient, Watching card and feed membership — instead of their count.
// Same statements, same transaction rules; see FanOut.
func FanOutWithRecipients(ctx context.Context, tx pgx.Tx, in FanOutInput) (FanOutResult, error) {
	res, err := fanOut(ctx, tx, in)
	return res, err
}

func fanOut(ctx context.Context, tx pgx.Tx, in FanOutInput) (FanOutResult, error) {
	var none FanOutResult
	if in.ProjectID == uuid.Nil || in.EventID == 0 {
		return none, nil
	}
	// Mentions are written by WriteMentionNotification. Calling FanOut on one
	// is a no-op rather than an error so a call site that routes every event
	// through the same helper does not have to special-case them.
	if _, isMention := mentionEventTypes[in.EventType]; isMention {
		return none, nil
	}

	ev := in.facts()
	switch EventDeliveryOutcome(ev) {
	case OutcomeAuditOnly:
		// Journal-only by decision (claim, close, archival, dependencies,
		// edits of undeliverable fields, automatic acceptance, a human
		// unclaim). Not a failure — and deliberately NOT an unmapped signal.
		return none, nil
	case OutcomeUnmapped:
		// The type claims a delivery path the registry cannot name. Silently
		// dropping it is exactly the loss this wave forbids: make it visible.
		fanoutUnmappedEventsTotal.Inc()
		slog.ErrorContext(ctx, "notification fan-out: event type is not mapped to any reason",
			"eventId", in.EventID,
			"eventType", in.EventType,
			"projectId", in.ProjectID.String(),
		)
		return none, nil
	}

	// The savepoint is what lets a preferences outage leave the parent
	// transaction usable: without it the failed statement would abort the
	// whole transaction and the user's comment would be lost with the
	// notification.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return none, fmt.Errorf("fan-out savepoint for event %d (%s): %w", in.EventID, in.EventType, err)
	}
	defer func() { _ = sp.Rollback(ctx) }()

	candidates, err := selectFanOutCandidates(ctx, sp, in)
	if err != nil {
		return none, err
	}
	if len(candidates) == 0 {
		if err := sp.Commit(ctx); err != nil {
			return none, fmt.Errorf("fan-out savepoint release for event %d: %w", in.EventID, err)
		}
		return none, nil
	}

	// Which reasons can this event produce for THESE recipients? Asking the
	// classifier first keeps the preferences read narrow: at most a couple of
	// rows per recipient instead of the whole registry.
	userIDs := make([]uuid.UUID, 0, len(candidates))
	reasonSet := make(map[ReasonKey]struct{}, 4)
	for _, c := range candidates {
		userIDs = append(userIDs, c.userID)
		cl := Classify(ev, in.relationOf(c.userID, c.isSubscriber))
		if cl.Outcome == OutcomeReason {
			reasonSet[cl.Reason] = struct{}{}
		}
	}
	if len(reasonSet) == 0 {
		if err := sp.Commit(ctx); err != nil {
			return none, fmt.Errorf("fan-out savepoint release for event %d: %w", in.EventID, err)
		}
		return none, nil
	}
	reasons := make([]ReasonKey, 0, len(reasonSet))
	for key := range reasonSet {
		reasons = append(reasons, key)
	}

	states, err := loadReasonStates(ctx, sp, userIDs, reasons)
	if err != nil {
		if !prefsReadUnavailable(err) {
			// Not a preferences outage: a concurrency failure or a broken
			// connection. Hand it to the parent so its own retry runs —
			// swallowing it here commits the parent's work with the
			// notification silently missing, under a cause that never
			// happened.
			return none, fmt.Errorf("fan-out for event %d (%s): %w", in.EventID, in.EventType, err)
		}
		// Preferences unavailable — no rows, parent operation survives.
		_ = sp.Rollback(ctx)
		fanoutPrefsUnavailableTotal.Inc()
		slog.ErrorContext(ctx, "notification fan-out skipped: reason preferences unavailable",
			"eventId", in.EventID,
			"eventType", in.EventType,
			"projectId", in.ProjectID.String(),
			"candidates", len(candidates),
			"error", err.Error(),
		)
		return none, nil
	}

	rowUsers := make([]uuid.UUID, 0, len(candidates))
	rowReasons := make([]string, 0, len(candidates))
	rowFeed := make([]bool, 0, len(candidates))
	rowPush := make([]bool, 0, len(candidates))
	rowCards := make([]*string, 0, len(candidates))
	for _, c := range candidates {
		m := Compose(ev, in.relationOf(c.userID, c.isSubscriber), states.lookup(c.userID))
		if !m.Materialises() {
			continue
		}
		rowUsers = append(rowUsers, c.userID)
		rowReasons = append(rowReasons, string(m.Reason))
		rowFeed = append(rowFeed, m.Feed)
		rowPush = append(rowPush, m.Push)
		rowCards = append(rowCards, c.cardIssueID)
	}
	if len(rowUsers) == 0 {
		if err := sp.Commit(ctx); err != nil {
			return none, fmt.Errorf("fan-out savepoint release for event %d: %w", in.EventID, err)
		}
		return none, nil
	}

	rows, err := sp.Query(ctx, insertFanOutRowsSQL,
		in.EventID, in.EventType, in.ProjectID,
		rowUsers, rowReasons, rowFeed, rowPush,
		int64(PushDeliveryDeadline/time.Second),
		rowCards,
	)
	if err != nil {
		return none, fmt.Errorf("fan-out for event %d (%s): %w", in.EventID, in.EventType, err)
	}
	written := make([]FanOutRecipient, 0, len(rowUsers))
	for rows.Next() {
		var r FanOutRecipient
		if err := rows.Scan(&r.UserID, &r.CardIssueID, &r.Feed); err != nil {
			rows.Close()
			return none, fmt.Errorf("fan-out for event %d (%s): %w", in.EventID, in.EventType, err)
		}
		written = append(written, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return none, fmt.Errorf("fan-out for event %d (%s): %w", in.EventID, in.EventType, err)
	}
	// sk-622.21 T016 (US-11, "notification of the feed written"): a recipient whose
	// written row carries a Watching card re-reads Watching. Collected on the
	// savepoint, so a rolled-back fan-out names nobody.
	for _, r := range written {
		if r.CardIssueID != nil {
			signals.Add(sp, r.UserID, signals.AreaWatching)
		}
	}
	if err := sp.Commit(ctx); err != nil {
		return none, fmt.Errorf("fan-out savepoint release for event %d: %w", in.EventID, err)
	}
	return FanOutResult{Recipients: written}, nil
}

// insertFanOutRowsSQL writes the composed pairs in ONE statement. The four
// parallel arrays are the composition result, so the row carries its reason
// and its two channel facts atomically: there is no window in which a row
// exists without knowing what it is about or which channels it belongs to.
//
// push_state is the DB-backed lifecycle of the push, not a side effect of the
// NATS publication: 'pending' means "owed to a device", 'none' means the row
// has no push at all. The deadline is stamped here so it is anchored to the
// event, not to whenever a runner first looks at the row.
//
// sk-622.21 (US-4, K4, K6): the Watching card computed by the selection query
// rides in $9, and only a FEED row keeps it (INV-4: a push-only row is in no
// feed, so it is in no Watching card). card_resolved = true on every row this
// version writes — the reconciliation (T020) skips such rows. RETURNING hands
// the written rows back (ON CONFLICT duplicates are not returned).
const insertFanOutRowsSQL = `
INSERT INTO notifications (
    user_id, event_id, type, project_id,
    reason, feed_channel,
    push_state, push_next_attempt_at, push_deadline_at,
    card_issue_id, card_resolved)
SELECT c.user_id, $1::bigint, $2::text, $3::uuid,
       c.reason, c.feed,
       CASE WHEN c.push THEN 'pending' ELSE 'none' END,
       CASE WHEN c.push THEN now() END,
       CASE WHEN c.push THEN now() + ($8::bigint * interval '1 second') END,
       CASE WHEN c.feed THEN ($9::text[])[c.ord] END,
       true
  FROM unnest($4::uuid[], $5::text[], $6::boolean[], $7::boolean[]) WITH ORDINALITY
       AS c(user_id, reason, feed, push, ord)
ON CONFLICT (user_id, event_id) DO NOTHING
RETURNING user_id, card_issue_id, feed_channel`

// selectFanOutCandidateSQL unions the subscription pools with the role
// recipients and answers ONE row per candidate, carrying the single fact only
// SQL knows: whether the subscription path found them. Every row also carries
// the hierarchy-loop report of the event's issue (cycle_at, cycle_chain —
// US-3); when no candidate survived, the statement still answers one row,
// with NULL candidate columns, so the loop is reported without recipients.
//
// The four subscription arms carry the same gates — sticky unwatch,
// self-action exclusion, agent-action suppression, the membership/
// internal-admin recheck, the project type whitelist and the custom filter.
// The role arm is new and its matrix is closed and DIFFERENT (US-2, "on
// the role path the matrix is different"):
//
//	adressee == actor            → excluded (no signal for your own action)
//	project membership / admin   → checked, at write time
//	agent-action suppression     → NOT applied (sk-335): the setting silences
//	                               broadcast noise, and a role recipient is
//	                               addressed personally — suppressing them
//	                               would stop a person learning a task was
//	                               assigned to them. Since sk-343 the same
//	                               people are exempt from it on the
//	                               SUBSCRIPTION arms too, so their row can
//	                               belong to a subscriber-audience feed.
//	subscription, sticky unwatch → NOT applied (R-14): they are broadcast
//	and the type whitelist         machinery, not personal addressing
//
// # What the agent-action suppression means, and whom it may reach
//
// `suppress_agent_actions` (sk-335) drops the recipient from the SUBSCRIPTION
// pools whenever the event's actor is an agent — ANY agent, not only one of
// the recipient's own. Two of the three conjuncts are therefore the stored
// flag and `$5 = 'agent'`.
//
// Its predecessor, `suppress_own_agent_actions`, carried a conjunct
// `<watcher>.user_id = $4` — which the self-action exclusion on the line above
// had already made false for every surviving row. The branch removed nothing,
// on or off, and the toggle was visible in the settings form the whole time.
// The owner's own agent was, and still is, silenced by the self-action
// exclusion, because an agent acts under its owner's key.
//
// The COALESCE fallback is the DEFAULT for an account with no
// `user_notification_prefs` row at all, and since sk-343 it reads `true`. That
// is not a duplicate of the column's DEFAULT — it is the only place the column
// default could ever be observed from: rows here are inserted lazily, by an
// upsert that always names the column, so a fresh account has no row and the
// schema default never fires. Fallback and column default therefore have to be
// changed together, and they are: migration 000043 sets one, this line is the
// other, and Store.GetPrefs answers the settings form with the same value so
// the checkbox cannot disagree with the fan-out.
//
// The third conjunct — `NOT (<watcher>.user_id = ANY($6))` — is sk-343, and it
// is a CHANGE OF REQUIREMENT, not a repair: the two-conjunct predicate did
// exactly what it claimed. What was wrong is the claim. Read it as
// "suppression is not applied to the recipient this event addresses
// personally by role": `$6` is the role arm's own input (FanOutInput.roleCandidates — the
// new assignee and the creator), so the exemption set is precisely the set of
// people this event is ABOUT, and nothing new is computed for it.
//
// Why the exemption sits in the SUBSCRIPTION arms and not anywhere else. The
// role arm already delivered to those people — but only as
// `is_subscriber = false`, and the feed halves of "Issue assignment",
// "Issue accepted by assignee" and "Delegated issue status" have the
// SUBSCRIBERS audience. So the delegating
// creator got the personal push and no feed row at all, and with that reason's
// push switched off they got nothing whatsoever about a task they had
// delegated. Exempting them here makes them arrive as a subscriber
// (`is_subscriber = true`), and the composition rule hands them the feed
// channel on its own, if the reason's feed toggle is on.
//
// What the exemption deliberately does NOT do is release the REASON. The feed
// audience of those reasons is every subscriber, and a user who merely
// watches the same issue is not personally addressed — for them the agent's
// action is exactly the machine echo the setting exists to hide. Only the
// role-addressed recipient steps out of the predicate; everyone else stays in.
//
// A recipient still dropped from the subscription pools can arrive through the
// role arm — as `is_subscriber = false`, so a subscriber-audience channel
// cannot claim their row while a personal one still can. That is the same
// mechanism sticky unwatch uses, and sticky unwatch is deliberately NOT
// exempted: "do not show me this issue" is a decision about this issue and
// outranks "do not show machine echo".
//
// bool_or over the two paths is what makes the composition rule work at the
// row level: a person found by both paths is ONE candidate who IS a
// subscriber, so their single row can belong to both channels; a person found
// only by the role path is a candidate who is NOT a subscriber, so a
// subscriber-audience channel cannot claim their row — which is precisely how
// sticky unwatch mutes the feed without muting the personal push.
var selectFanOutCandidateSQL = `
WITH RECURSIVE ` + hierarchy.AncestorsCTE("event_anc", "SELECT $3::text AS issue_id WHERE $3::varchar <> ''") + `,
issue_facets AS (
    SELECT
        i.type     AS issue_type,
        i.priority AS issue_priority,
        COALESCE((SELECT array_agg(label ORDER BY label) FROM issue_labels WHERE issue_id = i.id), '{}'::text[]) AS labels,
        COALESCE((SELECT array_agg(component_id::text ORDER BY component_id::text) FROM issue_components WHERE issue_id = i.id), '{}'::text[]) AS components
      FROM issues i
     WHERE $3::varchar <> '' AND i.id = $3::varchar
),
subscribers AS (
    -- Arm 1: watchers of the issue itself. Matches on iw.issue_id regardless
    -- of scope, so it covers the exact-id case for BOTH 'issue' and 'subtree'
    -- watchers and arm 2 stays disjoint from it.
    SELECT DISTINCT iw.user_id, iw.issue_id::text AS card, 0 AS depth
      FROM issue_watchers iw
      LEFT JOIN user_notification_prefs p ON p.user_id = iw.user_id
     WHERE iw.issue_id = $3::varchar
       AND $3::varchar <> ''
       AND ($4::uuid IS NULL OR iw.user_id <> $4::uuid)
       AND NOT (
           COALESCE(p.suppress_agent_actions, true) = true
           AND $5::text = 'agent'
           AND NOT (iw.user_id = ANY($6::uuid[]))
       )
       -- The recipient filter (authz): active and sees the event's project.
       -- A stale watch row after losing access grants nothing; mirrored on
       -- every arm and on the role recipients.
       AND ` + authz.RecipientEligibleExpr("iw.user_id", "$2::uuid") + `
    UNION
    -- Arm 2: subtree delivery (sk-622.21, US-3, INV-3, K1). A watcher with
    -- scope='subtree' on an ANCESTOR of the event's issue — an issue on its
    -- chain of canonical parents (internal/hierarchy, event_anc above), at
    -- the moment of the fan-out — receives the event. The id-prefix rule it
    -- replaces was wrong after a reparent and for a prefix that is not a
    -- descendant. depth >= 1 keeps the arm disjoint from arm 1; is_cycle rows
    -- are the repetition of a loop in the data, not an ancestor. The card of
    -- this arm is the ancestor itself; the nearest one wins below.
    SELECT DISTINCT iw.user_id, iw.issue_id::text AS card, a.depth
      FROM event_anc a
      JOIN issue_watchers iw ON iw.issue_id = a.ancestor_id
      LEFT JOIN user_notification_prefs p ON p.user_id = iw.user_id
     WHERE iw.scope = 'subtree'
       AND a.depth >= 1
       AND NOT a.is_cycle
       AND ($4::uuid IS NULL OR iw.user_id <> $4::uuid)
       AND NOT (
           COALESCE(p.suppress_agent_actions, true) = true
           AND $5::text = 'agent'
           AND NOT (iw.user_id = ANY($6::uuid[]))
       )
       AND ` + authz.RecipientEligibleExpr("iw.user_id", "$2::uuid") + `
    UNION
    -- Arm 3: project watchers, with their optional per-row event_types
    -- whitelist. NULL keeps the historical "all notifiable types" meaning.
    SELECT DISTINCT pw.user_id, NULL::text AS card, NULL::int AS depth
      FROM project_watchers pw
      LEFT JOIN user_notification_prefs p ON p.user_id = pw.user_id
     WHERE pw.project_id = $2::uuid
       AND NOT 'unwatched' = ANY(pw.reasons)
       AND (pw.event_types IS NULL OR $1::text = ANY(pw.event_types))
       AND ($4::uuid IS NULL OR pw.user_id <> $4::uuid)
       AND NOT (
           COALESCE(p.suppress_agent_actions, true) = true
           AND $5::text = 'agent'
           AND NOT (pw.user_id = ANY($6::uuid[]))
       )
       -- The arm rechecks access like every other (sk-674.18 T010): a
       -- project_watchers row outliving its membership delivers nothing.
       AND ` + authz.RecipientEligibleExpr("pw.user_id", "$2::uuid") + `
    UNION
    -- Arm 4: custom watchers, with SQL-side jsonb filter evaluation.
    SELECT DISTINCT cw.user_id, NULL::text AS card, NULL::int AS depth
      FROM custom_watchers cw
      LEFT JOIN user_notification_prefs p ON p.user_id = cw.user_id
      LEFT JOIN issue_facets f ON true
     WHERE ($4::uuid IS NULL OR cw.user_id <> $4::uuid)
       AND NOT (
           COALESCE(p.suppress_agent_actions, true) = true
           AND $5::text = 'agent'
           AND NOT (cw.user_id = ANY($6::uuid[]))
       )
       AND ` + authz.RecipientEligibleExpr("cw.user_id", "$2::uuid") + `
       AND (
           cw.filter->'event_types' IS NULL
           OR jsonb_array_length(cw.filter->'event_types') = 0
           OR cw.filter->'event_types' @> to_jsonb($1::text)
       )
       AND (
           cw.filter->'projects' IS NULL
           OR jsonb_array_length(cw.filter->'projects') = 0
           OR cw.filter->'projects' @> to_jsonb($2::uuid::text)
       )
       AND (
           cw.filter->'types' IS NULL
           OR jsonb_array_length(cw.filter->'types') = 0
           OR (f.issue_type IS NOT NULL AND cw.filter->'types' @> to_jsonb(f.issue_type))
       )
       -- priority_max — an upper BOUND on the numeric priority (lower number
       -- = more important in skein semantics), i.e. "anything at or above this
       -- importance". Watchers omitting the field accept any priority.
       AND (
           cw.filter->'priority_max' IS NULL
           OR (f.issue_priority IS NOT NULL
               AND f.issue_priority <= (cw.filter->>'priority_max')::int)
       )
       AND (
           cw.filter->'labels' IS NULL
           OR jsonb_array_length(cw.filter->'labels') = 0
           OR (f.labels IS NOT NULL AND EXISTS (
               SELECT 1 FROM jsonb_array_elements_text(cw.filter->'labels') l
                WHERE l.value = ANY(f.labels)
           ))
       )
       AND (
           cw.filter->'components' IS NULL
           OR jsonb_array_length(cw.filter->'components') = 0
           OR (f.components IS NOT NULL AND EXISTS (
               SELECT 1 FROM jsonb_array_elements_text(cw.filter->'components') c
                WHERE c.value = ANY(f.components)
           ))
       )
),
role_recipients AS (
    SELECT u.id AS user_id
      FROM users u
     WHERE u.id = ANY($6::uuid[])
       AND ($4::uuid IS NULL OR u.id <> $4::uuid)
       AND ` + authz.RecipientEligibleExpr("u.id", "$2::uuid") + `
),
candidates AS (
    SELECT c.user_id, bool_or(c.subscriber) AS is_subscriber,
           (array_agg(c.card ORDER BY c.depth) FILTER (WHERE c.card IS NOT NULL))[1] AS card
      FROM (
            SELECT user_id, true  AS subscriber, card, depth FROM subscribers
            UNION ALL
            SELECT user_id, false AS subscriber, NULL::text, NULL::int FROM role_recipients
           ) c
     GROUP BY c.user_id
),
-- US-3 / edge 7: a loop in the canonical-parent chain of the event's issue is
-- reported whatever the candidate pool is. This CTE is exactly ONE row (the
-- aggregate over an empty event_anc still yields a row), and the final SELECT
-- is driven by it with a LEFT JOIN, so the loop reaches Go even when no
-- candidate survived. chain is the walk from the issue (depth 0) up to the
-- repetition, nearest first; it is built only when there is a loop.
cycle_report AS (
    SELECT (SELECT a.ancestor_id FROM event_anc a WHERE a.is_cycle LIMIT 1) AS cycle_at,
           CASE WHEN EXISTS (SELECT 1 FROM event_anc a WHERE a.is_cycle)
                THEN (SELECT array_agg(a.ancestor_id ORDER BY a.depth)
                        FROM event_anc a WHERE NOT a.is_cycle)
           END AS cycle_chain
)
SELECT k.user_id, k.is_subscriber, k.card, r.cycle_at, r.cycle_chain
  FROM cycle_report r
  LEFT JOIN candidates k ON true`

// selectFanOutCandidates runs the recipient-selection query. One statement,
// whatever the size of the pool.
func selectFanOutCandidates(ctx context.Context, q querier, in FanOutInput) ([]fanOutCandidate, error) {
	// {issue, comment, attachment} all wake the SAME issue watcher pool: the
	// comment and attachment events carry their own uuid as entity_id, so the
	// parent issue arrives separately. Any other entity type has no issue arm,
	// and passing '' short-circuits arms 1 and 2 without disabling the project
	// and custom arms.
	issueArg := in.IssueID
	if in.EntityType != "issue" && in.EntityType != "comment" && in.EntityType != "attachment" {
		issueArg = ""
	}

	// nil → SQL NULL → self-action exclusion is short-circuited entirely.
	// Passing uuid.Nil instead would be a valid uuid and would silently
	// exclude whoever happened to hold it.
	var actorArg any
	if in.ActorUserID != uuid.Nil {
		actorArg = in.ActorUserID
	}

	actorType := in.ActorType
	if actorType == "" {
		actorType = "human"
	}

	rows, err := q.Query(ctx, selectFanOutCandidateSQL,
		in.EventType, in.ProjectID, issueArg, actorArg, actorType, in.roleCandidates())
	if err != nil {
		return nil, fmt.Errorf("fan-out recipients for event %d (%s): %w", in.EventID, in.EventType, err)
	}
	defer rows.Close()

	// The statement always answers at least one row (cycle_report drives it):
	// with no candidate that row carries NULL candidate columns and only the
	// loop report.
	out := make([]fanOutCandidate, 0, 16)
	var (
		cycleAt    *string
		cycleChain []string
	)
	for rows.Next() {
		var (
			userID       *uuid.UUID
			isSubscriber *bool
			card         *string
		)
		if err := rows.Scan(&userID, &isSubscriber, &card, &cycleAt, &cycleChain); err != nil {
			return nil, fmt.Errorf("fan-out recipients for event %d: %w", in.EventID, err)
		}
		if userID == nil {
			continue
		}
		out = append(out, fanOutCandidate{
			userID:       *userID,
			isSubscriber: isSubscriber != nil && *isSubscriber,
			cardIssueID:  card,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fan-out recipients for event %d: %w", in.EventID, err)
	}
	if cycleAt != nil {
		// The walk stopped at the repetition, so the fan-out is still
		// correct; the loop itself is a data defect to report (US-3, INV-3)
		// — with or without recipients.
		hierarchy.LogCycle(ctx, issueArg, *cycleAt, cycleChain)
	}
	return out, nil
}

// reasonStates holds the stored channel states of the recipients of ONE event,
// indexed by user and reason. A missing entry is not "off": it means the user
// has no stored row and the registry default applies.
type reasonStates map[uuid.UUID]map[ReasonKey]ReasonChannelState

// lookup produces the per-recipient preference function Compose expects.
func (m reasonStates) lookup(userID uuid.UUID) func(ReasonKey) ReasonChannelState {
	return func(key ReasonKey) ReasonChannelState {
		if byReason, ok := m[userID]; ok {
			if st, ok := byReason[key]; ok {
				return st
			}
		}
		return defaultReasonState(key)
	}
}

// defaultReasonState is the registry default of one reason, with an
// inapplicable channel forced off. Storage cannot switch on a channel a reason
// does not have, and neither can a default.
func defaultReasonState(key ReasonKey) ReasonChannelState {
	r, ok := reasonByKey[key]
	if !ok {
		return ReasonChannelState{}
	}
	return ReasonChannelState{
		Feed: r.Feed.Applicable && r.Feed.DefaultOn,
		Push: r.Push.Applicable && r.Push.DefaultOn,
	}
}

// prefsReadUnavailable classifies a failure of loadReasonStates into the only
// two classes that have different handling, and it is a NARROW predicate on
// purpose: it says "true" only for a failure that is genuinely "settings
// unavailable".
//
// The distinction matters because the two are handled in opposite directions.
// "Settings unavailable" is swallowed: no rows, the failure is counted and
// logged, and the parent operation commits — one signal is knowingly lost so
// that an unreadable preference cannot override an explicit refusal.
//
// A CONCURRENCY failure is not that. Class 40 (40001 serialization_failure,
// 40P01 deadlock_detected) says "this transaction run must be retried", not
// "settings are unreadable" — and a savepoint rollback genuinely restores the
// parent transaction, so the parent's COMMIT then SUCCEEDS with the comment
// written and the notification missing, under a log line and a metric that
// name a cause which never happened. R-15 ("the parent operation does not fail")
// is not violated by returning it: repeating a serializable transaction is the
// normal course of that isolation level, not a failure of the user's
// operation. The same holds for anything that is not a server answer at all —
// a dropped connection, a cancelled context — where the transaction is not
// usable and only the caller can decide.
func prefsReadUnavailable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		// No SQLSTATE: the server did not answer. Connection loss, context
		// cancellation, a driver-level failure. Not a statement about the
		// preferences table.
		return false
	}
	switch {
	case strings.HasPrefix(pgErr.Code, "40"):
		// Class 40 — transaction rollback. Retryable by construction.
		return false
	case strings.HasPrefix(pgErr.Code, "08"):
		// Class 08 — connection exception.
		return false
	}
	// Everything else the server can say about this read IS the outage this
	// class exists for: the table is missing (42P01), unreadable (42501),
	// shaped differently than this build expects (42703), …
	return true
}

// loadReasonStates reads the channel preferences of the given recipients for
// the given reasons in ONE statement — never one per recipient.
//
// An error here is a CANDIDATE for the "preferences unavailable" class; which
// errors actually belong to it is prefsReadUnavailable's decision, and the
// callers ask it before swallowing anything. That is why this read is its own
// statement instead of a join inside the INSERT — a join would make the outage
// indistinguishable from any other database failure, and the two have opposite
// handling.
func loadReasonStates(ctx context.Context, q querier, userIDs []uuid.UUID, reasons []ReasonKey) (reasonStates, error) {
	keys := make([]string, len(reasons))
	for i, k := range reasons {
		keys[i] = string(k)
	}
	rows, err := q.Query(ctx, `
SELECT user_id, reason, feed_enabled, push_enabled
  FROM user_notification_reason_prefs
 WHERE user_id = ANY($1::uuid[])
   AND reason  = ANY($2::text[])`, userIDs, keys)
	if err != nil {
		return nil, fmt.Errorf("loading reason preferences: %w", err)
	}
	defer rows.Close()

	out := make(reasonStates, len(userIDs))
	for rows.Next() {
		var (
			userID     uuid.UUID
			reason     string
			feed, push bool
		)
		if err := rows.Scan(&userID, &reason, &feed, &push); err != nil {
			return nil, fmt.Errorf("scanning reason preferences: %w", err)
		}
		key := ReasonKey(reason)
		r, known := reasonByKey[key]
		if !known {
			// A stored state for a reason this build does not know is ignored
			// on read and left untouched in storage — same rule as the
			// settings form.
			continue
		}
		byReason, ok := out[userID]
		if !ok {
			byReason = make(map[ReasonKey]ReasonChannelState, len(reasons))
			out[userID] = byReason
		}
		byReason[key] = ReasonChannelState{
			Feed: feed && r.Feed.Applicable,
			Push: push && r.Push.Applicable,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading reason preferences: %w", err)
	}
	return out, nil
}

// changedFieldsFromPayload extracts payload.changes[*].field from an
// issue.updated payload — ALL of them, not just the deliverable ones. Which
// fields carry a delivery path is the classifier's decision, and duplicating
// that judgement here would give the registry a second, silent competitor.
//
// Returns nil for a nil or unparseable payload: a silent miss is preferred to
// a spurious notification.
func changedFieldsFromPayload(payload []byte) []string {
	if len(payload) == 0 {
		return nil
	}
	var p struct {
		Changes []struct {
			Field string `json:"field"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	out := make([]string, 0, len(p.Changes))
	for _, c := range p.Changes {
		out = append(out, c.Field)
	}
	return out
}

// outcomeChangedFromPayload reports whether a closing event's payload records
// a CHANGE of the close outcome (sk-489, US-11).
//
// The rule is the one T003 wrote into the payload and it has exactly one
// shape: `previousCloseOutcome` is present if and only if a repeat close
// replaced an outcome that was already there. Presence — not equality with
// `closeOutcome`, not the truthiness of the value: the writer never emits the
// key for a first close, and an outcome cannot change into itself (T003
// refuses to call that a change), so comparing the two values here would be a
// SECOND copy of the rule that could disagree with the first.
//
// A nil or unparseable payload answers false: the fact is additive, and "could
// not be read" must degrade into "there was no change", never into a
// suppressed notification for the author.
func outcomeChangedFromPayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	_, ok := p["previousCloseOutcome"]
	return ok
}

// closeOutcomeFromPayload returns the close outcome recorded in a closing
// event's payload, or "" when the payload carries none. Used by the summary
// and by the inbox projection; both need the value, neither needs to know how
// the writer shaped it.
func closeOutcomeFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		CloseOutcome         string `json:"closeOutcome"`
		PreviousCloseOutcome string `json:"previousCloseOutcome"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.CloseOutcome
}

// previousCloseOutcomeFromPayload returns the outcome a repeat close replaced.
func previousCloseOutcomeFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		PreviousCloseOutcome string `json:"previousCloseOutcome"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.PreviousCloseOutcome
}

// AddIssueWatcher inserts or updates an issue_watchers row, accumulating
// reasons. Idempotent for the same (issue, user, reason) tuple. `since` is
// set on first insert and never bumped.
//
// This is the scope-unaware entry point — it adds the watcher with
// scope='issue' (the default for every auto-watch and the legacy manual
// watch). The opt-out rules (sk-622.21, US-1) are AddIssueWatcherScoped's.
func AddIssueWatcher(
	ctx context.Context,
	tx pgx.Tx,
	issueID string,
	userID uuid.UUID,
	reason string,
) error {
	return AddIssueWatcherScoped(ctx, tx, issueID, userID, reason, ScopeIssue)
}

// AddIssueWatcherScoped is AddIssueWatcher with an explicit watch scope
// (sk-142.8 / spec §7). scope is one of ScopeIssue | ScopeSubtree;
// anything else is normalised to ScopeIssue.
//
// Opt-outs (sk-622.21, US-1, INV-2, D2). The reason decides which kind of
// subscription this is:
//
//   - reason = 'manual' (the person's own "watch", including the one a task
//     monitor places — decision R4) deletes the opt-out (U, X) and
//     subscribes.
//   - any other reason is an AUTOMATIC subscription (creator, assignee,
//     reopen). It is not created when the user has an opt-out on exactly
//     this issue — the key is the pair (user, issue), never the user or the
//     issue alone. A blocked add leaves an existing row, its reasons and its
//     scope untouched (an existing row means a live subscription: R2's CHECK
//     on issue_watchers forbids the old version's tombstones, INV-1).
//   - a repeat of a reason the row already has, with no scope widening,
//     changes nothing and is not counted as a written row (no signal).
//
// Only the automatic branch reads opt-outs; no other path in the system
// does (INV-2).
//
// Scope semantics (monotonic — subtree dominates):
//
//   - On first insert the row takes the requested scope.
//
//   - subtree DOMINATES issue: adding with scope='subtree' upgrades an
//     existing 'issue' row to 'subtree'.
//
//   - A scope='issue' add NEVER downgrades an existing 'subtree' row — a
//     stray re-watch must not silently narrow the subscription. An explicit
//     downgrade is a separate, deliberate step (not in v1).
//
// The caller's transaction must be SERIALIZABLE (K2): the opt-out check and
// a concurrent explicit unwatch of the same pair then serialise — either
// the unwatch wins and no subscription is created, or the subscription is
// created first and the unwatch deletes it and records the opt-out (INV-7).
func AddIssueWatcherScoped(
	ctx context.Context,
	tx pgx.Tx,
	issueID string,
	userID uuid.UUID,
	reason string,
	scope string,
) error {
	if reason == "" || userID == uuid.Nil || issueID == "" {
		return nil
	}
	if scope != ScopeSubtree {
		scope = ScopeIssue
	}

	// Single statement. The leading CTE deletes the opt-out on a manual
	// watch; the INSERT … SELECT gates an automatic add on the opt-out (the
	// NOT EXISTS reads the statement's snapshot, so a manual add is never
	// gated by the opt-out it is deleting — it bypasses the check on $3).
	// ON CONFLICT then applies the accumulation rules: the reasons CASE and
	// the monotonic scope CASE read the pre-update row (issue_watchers.*);
	// $4 is the requested scope.
	const sql = `
WITH clear_optout AS (
    DELETE FROM issue_watch_optouts
     WHERE $3 = 'manual' AND issue_id = $1 AND user_id = $2
)
INSERT INTO issue_watchers (issue_id, user_id, reasons, since, scope)
SELECT $1, $2, ARRAY[$3]::text[], now(), $4
 WHERE $3 = 'manual'
    OR NOT EXISTS (SELECT 1 FROM issue_watch_optouts o
                    WHERE o.issue_id = $1 AND o.user_id = $2)
ON CONFLICT (issue_id, user_id) DO UPDATE
SET reasons =
    CASE
        WHEN $3 = ANY(issue_watchers.reasons)
            THEN issue_watchers.reasons    -- already present
        ELSE
            array_append(issue_watchers.reasons, $3)
    END,
    scope =
    CASE
        WHEN $4 = 'subtree'
            THEN 'subtree'                 -- subtree dominates: upgrade/set
        ELSE
            issue_watchers.scope           -- monotonic: 'issue' never downgrades
    END
 -- Update only a row whose reasons or scope actually move; an unchanged row
 -- is then not counted in RowsAffected and gives no signal.
 WHERE NOT ($3 = ANY(issue_watchers.reasons))
    OR ($4 = 'subtree' AND issue_watchers.scope <> 'subtree')
`
	tag, err := tx.Exec(ctx, sql, issueID, userID, reason, scope)
	if err != nil {
		return fmt.Errorf("adding issue watcher (%s, scope=%s): %w", reason, scope, err)
	}
	// sk-622.21 (K13, US-11): the user whose subscription was written —
	// manual watch or an automatic one (creator, assignee, reopen) — re-reads
	// Watching. A row the statement did not touch is no change and no
	// signal: an automatic add blocked by an opt-out, and
	// a repeat of a reason and scope the row already has (the conflict
	// update's WHERE skips it, so RowsAffected stays 0).
	if tag.RowsAffected() > 0 {
		signals.Add(tx, userID, signals.AreaWatching)
	}
	return nil
}

// WriteMentionNotification inserts a single notifications row for one
// directly-targeted recipient. R4.3 mention path — bypasses FanOut's
// watcher-pool UNION because mentions are a point-to-point ping, not
// an audit-log entry that the issue/project watcher pool should see.
//
// The caller (issues.AddComment / issues.CreateIssue / issues.PatchIssue
// via the mentions package) is expected to:
//
//  1. Have already resolved the candidate username through
//     mentions.Resolve so the recipient filter (authz: active and sees
//     the project) was checked at write time.
//
//  2. Have written the matching event row (comment.mention or
//     issue.mention) inside the same SERIALIZABLE tx and captured its
//     eventID.
//
//  3. Have skipped self-mentions (actor == recipient) per plan §F-5
//     before calling — this helper does NOT re-check actor identity.
//
// projectID is denormalised onto the notifications row so the inbox
// query's project filter joins cheaply (mirrors the FanOut INSERT).
//
// # The write-side filter applies here too (T007)
//
// A mention is addressed, not broadcast, but it is NOT exempt from the
// reason: US-4 puts "Mention via @" in the registry with two channels
// precisely so that switching it off finally does something. So this helper
// composes the same way the fan-out does — reason "mention", feed channel iff
// the feed toggle is on, push channel iff the push toggle is on, and NO ROW AT
// ALL when both are off (INV-1, and the acceptance "there is no row in storage
// at all — verified by a query against the data").
//
// What does NOT apply, per INV-8: subscription state, sticky unwatch, the
// subscription type whitelist and — separately — the agent-action suppression
// (`suppress_agent_actions`). The last one is deliberate and survives sk-335's
// widening of that setting to every agent: "@owner, take a look" from the
// owner's own agent is exactly the personal call the reason exists for, and a
// mention from somebody else's agent is no less personal.
//
// A preferences read that FAILS is handled like the fan-out handles it, and
// with the same split: a real outage of the settings leaves no row, keeps the
// parent transaction intact and is logged — the comment or the issue edit that
// carried the mention still commits — while a concurrency failure goes back to
// the caller so its retry can write both.
//
// Idempotence: ON CONFLICT (user_id, event_id) DO NOTHING — a duplicate
// insert is a silent no-op that returns nil (not ErrNoOp). The UNIQUE
// constraint still guarantees at-most-once per (user, event) on retried
// transactions; callers treat any non-nil error as abort-the-tx.
func WriteMentionNotification(
	ctx context.Context,
	tx pgx.Tx,
	eventID int64,
	userID uuid.UUID,
	eventType string,
	projectID uuid.UUID,
) error {
	if eventID == 0 || userID == uuid.Nil || eventType == "" || projectID == uuid.Nil {
		return fmt.Errorf("WriteMentionNotification: required arg missing")
	}
	if _, ok := mentionEventTypes[eventType]; !ok {
		// Defensive: if a non-mention event type is ever passed here it
		// would silently bypass the watcher-pool fan-out. Reject loudly.
		return fmt.Errorf("WriteMentionNotification: %q is not a mention event type", eventType)
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mention notification savepoint (event %d): %w", eventID, err)
	}
	defer func() { _ = sp.Rollback(ctx) }()

	states, err := loadReasonStates(ctx, sp, []uuid.UUID{userID}, []ReasonKey{ReasonKeyMention})
	if err != nil {
		if !prefsReadUnavailable(err) {
			// Same split as the fan-out: only a real preferences outage is
			// swallowed. A concurrency failure goes back to the parent, whose
			// retry is what actually saves the mention.
			return fmt.Errorf("mention notification for event %d: %w", eventID, err)
		}
		_ = sp.Rollback(ctx)
		fanoutPrefsUnavailableTotal.Inc()
		slog.ErrorContext(ctx, "mention notification skipped: reason preferences unavailable",
			"eventId", eventID,
			"eventType", eventType,
			"projectId", projectID.String(),
			"error", err.Error(),
		)
		return nil
	}

	m := Compose(
		EventFacts{Type: eventType},
		RecipientRelation{IsMentioned: true},
		states.lookup(userID),
	)
	if !m.Materialises() {
		return sp.Commit(ctx)
	}

	const sql = `
INSERT INTO notifications (
    user_id, event_id, type, project_id,
    reason, feed_channel,
    push_state, push_next_attempt_at, push_deadline_at,
    card_resolved)
VALUES ($1, $2, $3, $4,
        $5, $6,
        CASE WHEN $7::boolean THEN 'pending' ELSE 'none' END,
        CASE WHEN $7::boolean THEN now() END,
        CASE WHEN $7::boolean THEN now() + ($8::bigint * interval '1 second') END,
        true) -- a mention never has a Watching card (US-4); resolved = decided (K6)
ON CONFLICT (user_id, event_id) DO NOTHING`
	tag, err := sp.Exec(ctx, sql,
		userID, eventID, eventType, projectID,
		string(m.Reason), m.Feed, m.Push,
		int64(PushDeliveryDeadline/time.Second),
	)
	if err != nil {
		return fmt.Errorf("inserting mention notification: %w", err)
	}
	// sk-622.21 T016 (US-11): a mention row in the feed changes the
	// recipient's Mentions; a push-only row is in no feed.
	if tag.RowsAffected() > 0 && m.Feed {
		signals.Add(sp, userID, signals.AreaMentions)
	}
	return sp.Commit(ctx)
}

// RowQuerier is the single-row read surface ResolveAssigneeUserID needs.
// Both pgx.Tx and *pgxpool.Pool satisfy it, so the same resolver serves the
// store's in-transaction predicates and the MCP tool layer's pre-flight
// permission gate — which is what keeps "is this handle me?" from having two
// answers on one call path (sk-294).
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ResolveAssigneeUserID maps an assignee HANDLE (issues.assignee — free-form
// text on the issue row) to a real users.id. Returns uuid.Nil + nil when the
// handle matches no user.
//
// The three forms it resolves are exactly the three the write surface
// documents and accepts: skein_create's `assignee` is described as
// "username, email, or user UUID — stored verbatim, resolved on read", and
// the column has no CHECK narrowing that. A resolver that understood only
// one of the three made "resolved on read" false for the other two, and
// every predicate built on it (claim's assignee gate, accept's permission
// gate, auto-accept-on-self-assign, auto-watch) silently treated the same
// person as a stranger — sk-294.
//
// Matching all three in ONE statement is safe because the namespaces cannot
// collide: users.username is CHECK-constrained to
// `^[a-z0-9][a-z0-9_-]{2,29}$`, so it is at most 30 characters (a UUID is
// 36) and can never contain '@'. No handle can therefore be a username for
// one user and an e-mail or an id for another, and the LIMIT 1 below is not
// hiding an ambiguity.
//
// What is still deliberately NOT matched is the display name (users.name).
// Assignee strings often carry free-form labels ("the docs team") and a
// fuzzy name match would explode them into spurious watchers. Only the
// three unique, caller-typed handles earn an identity.
//
// The id leg compares `id::text` rather than casting the INPUT to uuid: a
// non-UUID handle passed to `$1::uuid` raises 22P02 instead of returning
// "no match", which would turn every free-form label into a 500.
//
// sk-674.18 T028 (INV-30): the handle resolves only to a user who may work
// in projectID now (authz.WorkerEligibleExpr) — the delivery rule. Anybody
// else's handle is text and resolves to nobody: no watcher, no notification,
// no acceptance, no «on me» (E-33, E-37), exactly as for a handle that names
// no account; the answer does not tell the two apart.
func ResolveAssigneeUserID(
	ctx context.Context,
	q RowQuerier,
	assignee string,
	projectID uuid.UUID,
) (uuid.UUID, error) {
	if assignee == "" {
		return uuid.Nil, nil
	}
	var id uuid.UUID
	err := q.QueryRow(ctx, `
SELECT u.id FROM users u
 WHERE (LOWER(u.username) = LOWER($1)
        OR LOWER(u.email) = LOWER($1)
        OR u.id::text     = LOWER($1))
   AND `+authz.WorkerEligibleExpr("u.id", "$2::uuid")+`
 LIMIT 1`, assignee, projectID,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil
		}
		return uuid.Nil, fmt.Errorf("resolving assignee %q: %w", assignee, err)
	}
	return id, nil
}

// ResolveAssigneeIdentity is ResolveAssigneeUserID without the access
// condition: the user the handle names, whatever his access now (sk-674.18
// T028). It answers identity questions that are not delivery — is this
// handle the creator (the default-assignee rule), are two handles one person,
// which user to append to previous_assignees — so a person's history keeps
// his id after he loses the access. Nothing that notifies, watches or
// accepts uses it.
func ResolveAssigneeIdentity(ctx context.Context, q RowQuerier, assignee string) (uuid.UUID, error) {
	if assignee == "" {
		return uuid.Nil, nil
	}
	var id uuid.UUID
	err := q.QueryRow(ctx, `
SELECT id FROM users
 WHERE LOWER(username) = LOWER($1)
    OR LOWER(email)    = LOWER($1)
    OR id::text        = LOWER($1)
 LIMIT 1`, assignee).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil
		}
		return uuid.Nil, fmt.Errorf("resolving assignee identity %q: %w", assignee, err)
	}
	return id, nil
}

// FormatSummary builds the human-readable summary string shown in the
// inbox row. Kept simple: "<actor> <verb> <entityRef>". Verb is derived
// from the event type; for issue.updated the suffix lists the notifiable
// fields that actually changed (e.g. "alice updated demo-42 (priority,
// assignee)") so the inbox row carries actionable context without a
// drill-down. Detailed before/after rendering happens in the frontend
// using the events.payload.changes array.
//
// payload is the events.payload jsonb bytes; it is consulted only for
// issue.updated. nil / unparseable payload falls back to the generic
// "<actor> updated <entityRef>" summary so a malformed row still
// surfaces something useful.
func FormatSummary(eventType, actor, entityRef string, payload []byte) string {
	if entityRef == "" {
		entityRef = "this issue"
	}
	if actor == "" {
		actor = "someone"
	}
	switch eventType {
	case "issue.created":
		return fmt.Sprintf("%s created %s", actor, entityRef)
	case "issue.status_changed":
		return fmt.Sprintf("%s changed status of %s", actor, entityRef) +
			closeOutcomeSuffix(payload)
	case "issue.closed":
		// A GUARD, not a working path. The type is declared journal-only
		// (auditOnlyEventTypes), so no notification rows with it exist and
		// this branch is unreachable today. It is left on purpose: if the type
		// ever gets a reason, the summary will speak about the close and the outcome
		// by itself, and not fall into the generic "updated". The doc block must call it
		// a guard — otherwise the next reader will take it for a description of
		// observable behaviour.
		return fmt.Sprintf("%s closed %s", actor, entityRef) +
			closeOutcomeSuffix(payload)
	case EventTypeOutcomeChanged:
		prev := previousCloseOutcomeFromPayload(payload)
		next := closeOutcomeFromPayload(payload)
		if prev == "" || next == "" {
			// The change cannot be named: the "was -> became" pair is incomplete. Printing
			// half a pair is worse than not printing it at all.
			return fmt.Sprintf("%s changed the outcome of %s", actor, entityRef)
		}
		return fmt.Sprintf("%s changed the outcome of %s (%s → %s)",
			actor, entityRef, closeOutcomeLabel(prev), closeOutcomeLabel(next))
	case "issue.archived":
		return fmt.Sprintf("%s archived %s", actor, entityRef)
	case "issue.accepted":
		// Phase 2 ui-redesign — assignee (or admin) explicitly
		// acknowledged the task. Plain wording mirrors the verb-tense
		// pattern of the surrounding cases (created/archived/closed).
		return fmt.Sprintf("%s accepted %s", actor, entityRef)
	case "comment.created":
		return fmt.Sprintf("%s commented on %s", actor, entityRef)
	case "issue.updated":
		fields := notifiableFieldsFromPayload(payload)
		if len(fields) == 0 {
			return fmt.Sprintf("%s updated %s", actor, entityRef)
		}
		return fmt.Sprintf("%s updated %s (%s)", actor, entityRef, strings.Join(fields, ", "))
	case "comment.mention":
		// R4.3: comment mention summary deliberately omits the comment
		// body quote — recipients click through to the issue page to
		// see context. Plan §F-10.
		return fmt.Sprintf("%s mentioned you in %s", actor, entityRef)
	case "issue.mention":
		// payload.fields is the union of fields where the recipient was
		// mentioned in this write (description / notes /
		// acceptance_criteria / design). Single-field renders as
		// `(field)`; multi-field as `(field1, field2)`. Empty / missing
		// payload falls back to the generic "mentioned you" wording so
		// a malformed row still surfaces something actionable.
		fields := mentionFieldsFromPayload(payload)
		if len(fields) == 0 {
			return fmt.Sprintf("%s mentioned you in %s", actor, entityRef)
		}
		return fmt.Sprintf("%s mentioned you in %s (%s)", actor, entityRef, strings.Join(fields, ", "))
	default:
		return fmt.Sprintf("%s updated %s", actor, entityRef)
	}
}

// closeOutcomeLabels renders the four outcomes of sk-489 for the English
// summary line.
//
// The keys are string literals and not issues.CloseOutcome* constants because
// internal/issues imports THIS package — the dependency cannot be reversed.
// The copy is safe in the one direction that matters: an outcome this map has
// not been taught degrades to its own raw value (INV-2 "an unknown value
// is printed as is"), so a drift shows up as an untranslated but truthful
// line, never as a wrong one.
var closeOutcomeLabels = map[string]string{
	"done":       "done",
	"wont_do":    "won't do",
	"duplicate":  "duplicate",
	"superseded": "superseded",
}

// closeOutcomeLabel renders one outcome, falling back to the raw value.
func closeOutcomeLabel(outcome string) string {
	if lbl, ok := closeOutcomeLabels[outcome]; ok {
		return lbl
	}
	return outcome
}

// closeOutcomeSuffix is the « (outcome: …)» tail a closing row carries when
// the issue did NOT end up done.
//
// "Done" is the default outcome of every close, so naming it would append
// a tail to the overwhelming majority of closing rows and change the text of
// the case nothing was asking about. Silence for `done` is therefore the
// decision, not an omission — SC-011 asks the outcome to be NAMED exactly
// where it is not the expected one.
func closeOutcomeSuffix(payload []byte) string {
	outcome := closeOutcomeFromPayload(payload)
	if outcome == "" || outcome == "done" {
		return ""
	}
	return " (outcome: " + closeOutcomeLabel(outcome) + ")"
}

// mentionFieldsFromPayload extracts the fields[] list from an
// issue.mention event payload. Returns nil for nil / unparseable
// payload — the FormatSummary caller falls back to the generic
// "mentioned you" wording.
func mentionFieldsFromPayload(payload []byte) []string {
	if len(payload) == 0 {
		return nil
	}
	var p struct {
		Fields []string `json:"fields"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	return p.Fields
}

// notifiableFieldsFromPayload extracts the notifiable subset of changed
// field names from an issue.updated parent payload. Order is preserved
// from payload.changes so the rendered summary matches the registry
// ordering recorded by eventbus.WriteHybridUpdateEvents. Returns nil for
// nil / unparseable payloads — the FormatSummary caller falls back to a
// generic verb.
func notifiableFieldsFromPayload(payload []byte) []string {
	if len(payload) == 0 {
		return nil
	}
	var p struct {
		Changes []struct {
			Field string `json:"field"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	// Grow from nil rather than pre-sizing by len(p.Changes). The audit
	// (sk-194, rev-A14-018) read len(p.Changes) as the upper bound, but the
	// bound on the OUTPUT is len(notifiableUpdateFields) — two fields — while
	// p.Changes carries every field the PATCH touched. Pre-sizing by the input
	// therefore allocates a buffer that is nearly always oversized, and on the
	// dominant path (a PATCH touching only non-notifiable fields: title,
	// description, notes, status …) it allocates a slice that stays empty,
	// where growing from nil allocates nothing at all.
	//
	// Measured with BenchmarkNotifiableFieldsFromPayload (see
	// fanout_bench_test.go), pre-sizing vs growing from nil:
	//   1 non-notifiable change:  11 allocs/361 B  vs  10 allocs/345 B
	//   6 mixed changes:          19 allocs/704 B  vs  19 allocs/640 B
	// i.e. the "optimization" cost an allocation on the common path and 64 B
	// on the wide one. Do not re-apply it without a benchmark showing a win.
	var out []string
	for _, c := range p.Changes {
		if _, ok := notifiableUpdateFields[c.Field]; ok {
			out = append(out, c.Field)
		}
	}
	return out
}

// selectSubscriberUserIDsSQL answers the SUBSCRIBERS of one issue for the
// personal signal (sk-622.21, K13, US-11): arms 1 and 2 of the fan-out —
// watchers of the issue itself and subtree watchers of an issue on its chain
// of canonical parents — with the fan-out's visibility recheck (project
// membership, or internal admin), and NOTHING of its notification
// suppression. The actor's own action, an agent actor and the recipient's
// notification settings silence notifications, never the signal ("the signal
// is sent whether or not a notification was written"), so the self-action
// exclusion, `suppress_agent_actions`, the type whitelist and the reason
// preferences are deliberately absent.
//
// The fan-out has no recipient budget today, so neither has this selector:
// both answer every subscriber the arms find.
//
// Like the fan-out, the statement is driven by a one-row cycle report, so a
// loop in the canonical-parent chain is reported (US-3) even when there is no
// subscriber.
var selectSubscriberUserIDsSQL = `
WITH RECURSIVE ` + hierarchy.AncestorsCTE("sub_anc", "SELECT $1::text AS issue_id") + `,
target AS (
    SELECT project_id FROM issues WHERE id = $1::varchar
),
subscribers AS (
    SELECT iw.user_id
      FROM issue_watchers iw
     WHERE iw.issue_id = $1::varchar
    UNION
    SELECT iw.user_id
      FROM sub_anc a
      JOIN issue_watchers iw ON iw.issue_id = a.ancestor_id
     WHERE iw.scope = 'subtree'
       AND a.depth >= 1
       AND NOT a.is_cycle
),
visible AS (
    SELECT s.user_id
      FROM subscribers s, target t
     WHERE ` + authz.RecipientEligibleExpr("s.user_id", "t.project_id") + `
),
cycle_report AS (
    SELECT (SELECT a.ancestor_id FROM sub_anc a WHERE a.is_cycle LIMIT 1) AS cycle_at,
           CASE WHEN EXISTS (SELECT 1 FROM sub_anc a WHERE a.is_cycle)
                THEN (SELECT array_agg(a.ancestor_id ORDER BY a.depth)
                        FROM sub_anc a WHERE NOT a.is_cycle)
           END AS cycle_chain
)
SELECT v.user_id, r.cycle_at, r.cycle_chain
  FROM cycle_report r
  LEFT JOIN visible v ON true`

// SubscriberUserIDs returns the users subscribed to issueID — directly, or
// through a subtree subscription on an ancestor — who can see the issue, for
// the personal `watching` signal (sk-622.21, K13). One statement, whatever
// the number of subscribers. See selectSubscriberUserIDsSQL for what is and
// is not inherited from the fan-out.
func SubscriberUserIDs(ctx context.Context, tx pgx.Tx, issueID string) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, selectSubscriberUserIDsSQL, issueID)
	if err != nil {
		return nil, fmt.Errorf("subscribers of %s: %w", issueID, err)
	}
	defer rows.Close()
	var (
		out        []uuid.UUID
		cycleAt    *string
		cycleChain []string
	)
	for rows.Next() {
		var uid *uuid.UUID
		if err := rows.Scan(&uid, &cycleAt, &cycleChain); err != nil {
			return nil, fmt.Errorf("subscribers of %s: %w", issueID, err)
		}
		if uid != nil {
			out = append(out, *uid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("subscribers of %s: %w", issueID, err)
	}
	if cycleAt != nil {
		hierarchy.LogCycle(ctx, issueID, *cycleAt, cycleChain)
	}
	return out, nil
}
