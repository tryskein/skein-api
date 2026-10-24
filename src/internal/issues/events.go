package issues

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/mentions"
	"skein-api/internal/notifications"
)

// Event-type constants (db-schema.md §2.17 "type = <entity>.<action>").
// Issue mutation events are deliberately granular — clients filtering for
// status changes don't want to wake up on every metadata edit.
//
// R3.2b refactor: PatchIssue now writes a hybrid event family — one
// issue.updated parent (full diff in payload.changes) plus N
// issue.field_changed children (one per changed scalar). The legacy
// per-axis "issue.priority_changed" and "issue.assigned" types are gone;
// subscribers filter on the field_changed subject infix
// (skein.events.project.<pid>.issue.field.priority.<eid>) instead.
const (
	EventIssueCreated       = "issue.created"
	EventIssueUpdated       = "issue.updated"
	EventIssueFieldChanged  = "issue.field_changed"
	EventIssueStatusChanged = "issue.status_changed"
	EventIssueClaimed       = "issue.claimed"
	EventIssueUnclaimed     = "issue.unclaimed"
	EventIssueClosed        = "issue.closed"
	EventIssuePinned        = "issue.pinned"
	EventIssueUnpinned      = "issue.unpinned"
	EventIssueArchived      = "issue.archived"
	EventComponentsChanged  = "issue.components_changed"

	// EventIssueDelegationDropped — sk-34. Emitted by Store.DismissDelegated
	// when a user who is in the issue's previous_assignees chain explicitly
	// opts out of the Delegated lens for this issue. Side effects:
	//   - issues.dismissed_by += caller (idempotent)
	//   - notifications.UnwatchExplicitTx — the caller's subscription is
	//     deleted with an opt-out and the issue's Watching card is cleared,
	//     identical contract to the manual "Unwatch" (sk-622.21, US-1).
	// payload schema: {} (empty — the actor is the caller, entity is the
	// issue, no other context needed).
	EventIssueDelegationDropped = "issue.delegation_dropped"

	// EventIssueDelegationResumed — sk-34. Symmetric inverse of
	// EventIssueDelegationDropped. Emitted by Store.ResumeDelegated when
	// the caller removes their uuid from issues.dismissed_by. Does NOT
	// re-add the watcher row (the unwatch is the bell-silence; the user
	// can call skein_watchers(action="watch") separately if they want
	// notifications back).
	// payload schema: {}.
	EventIssueDelegationResumed = "issue.delegation_resumed"

	// EventIssueAccepted — Phase 2 of the ui-redesign acceptance flow.
	// Emitted by issues.Store.Accept when the current assignee (or an
	// admin acting on their behalf) explicitly acknowledges the task.
	// Side effects:
	//   - issues.accepted_at = now(), issues.accepted_by = caller's
	//     users.id, version + 1, updated_at = now()
	//   - notifications fan-out to creator + watchers (the type is in
	//     notifications.notifiableLifecycle; mention/recipient pools
	//     resolved via the standard FanOut UNION).
	// payload schema (events.payload):
	//   { acceptedBy: <uuid>, acceptedAt: <ISO 8601 timestamp> }
	EventIssueAccepted = "issue.accepted"

	// EventIssueCommitsAttached — sk-35. Emitted by Store.AttachCommits
	// when an agent attaches commit refs to an existing issue *without*
	// closing it (or after a close, when the original skein_close call
	// forgot to pass commits[]). Distinct from issue.closed even when
	// the issue happens to be closed already: this event records ONLY
	// the commit-link addition, not a re-close. closed_reason /
	// closed_at / version stay untouched.
	//
	// payload schema (events.new_data):
	//   { hashes: ["abc1234", ...] }  — distinct commit hashes attached
	//                                    in this call (dedup-aware:
	//                                    ON CONFLICT DO NOTHING means
	//                                    a re-attach of the same hash
	//                                    yields zero new rows; in that
	//                                    case the event is still
	//                                    written so the audit trail
	//                                    records the agent's intent).
	EventIssueCommitsAttached = "issue.commits_attached"

	// EventIssueCommitsDetached — sk-394. Emitted by Store.DetachCommits
	// when commit links are REMOVED from an issue. The counterpart to
	// issue.commits_attached, and the reason it exists at all: a hash is
	// accepted on its form alone (lowercase hex, 7..64), so a fabricated
	// one — an abbreviation padded out by hand — passes validation and
	// used to stay linked forever, because nothing could unlink it. A
	// wrong ref in commitRefs is only detectable by walking to git, so
	// the audit trail has to record its removal as loudly as its arrival.
	//
	// payload schema (events.old_data):
	//   { hashes: ["abc1234", ...],   — the hashes whose rows were
	//                                    ACTUALLY deleted by this call,
	//                                    one entry per hash;
	//     refs: [{repoId, hash}, ...] } — sk-540.9: every deleted row,
	//                                    one entry per (repo, hash) —
	//                                    detach matches by hash alone, so
	//                                    on a multi-repo issue only this
	//                                    key says which links went.
	//                                    Ordered by the caller's hash
	//                                    order, then repo id.
	//
	// Unlike issue.commits_attached, this event is NOT written when the
	// call removed nothing. Attach records intent because a re-attach of
	// an already-linked hash leaves the link in place either way — the
	// end state matches what the caller asked for. A detach that deleted
	// zero rows did not: an event claiming a removal that never happened
	// would be read by the next auditor as "somebody unlinked this
	// commit", which is precisely the question this event exists to
	// answer truthfully. The caller learns the difference from the
	// response instead (`detached` vs `notFound`).
	EventIssueCommitsDetached = "issue.commits_detached"

	// Comment lifecycle events. All three share one payload schema
	// (sk-201): { issueId: <hierarchical issue id> }. entity_id is the
	// comment uuid, so payload.issueId is the only route back to the
	// containing issue for a live subscriber holding just the envelope —
	// the frontend's live-invalidation router keys the comment-thread /
	// issue-detail / activity invalidations off it. Written by the single
	// WriteCommentEvent helper, which takes issueID as a required arg.
	EventCommentCreated = "comment.created"
	// EventCommentEdited — sk-57. Emitted by Store.UpdateComment when a
	// comment's body is edited by its author. old_data={"body":<old>},
	// new_data={"body":<new>,"id":...,"issueId":...}. Authorship columns
	// are never rewritten; the editor is recorded only via the event
	// actor. Newly-introduced @mentions in the edited body fire the same
	// comment.mention + autowatch path AddComment uses (mentions removed
	// in the edit are NOT un-notified).
	EventCommentEdited  = "comment.edited"
	EventCommentDeleted = "comment.deleted"

	EventLabelAdded   = "issue.label_added"
	EventLabelRemoved = "issue.label_removed"

	// EventReviewMarked / EventReviewFlagChanged — sk-303.14,
	// the author's private bookkeeping. The review mark and the
	// "review required" flag toggle; review_ops.go writes them.
	//
	// The values are NOT rewritten as literals: they are taken from
	// internal/notifications, where both
	// types are already registered — in the reason registry and in the journal set
	// (auditOnlyEventTypes). A second literal would be a second source
	// of truth: diverging by one character would produce an event the
	// classifier treats as UNKNOWN, and private silence would look like
	// a configuration error.
	//
	// Both types are journal-only: there is no fan-out to any recipient,
	// including the author (INV-2, the notification feed stays silent).
	//
	// payload: field = "review_required" / "reviewed_at";
	//   review.flag_changed  old={reviewRequired:<bool>} new={reviewRequired:<bool>}
	//   review.marked        old={reviewedAt:null,reviewedBy:null}
	//                        new={reviewedAt:<ts>,reviewedBy:<uuid>}
	EventReviewMarked      = notifications.EventTypeReviewMarked
	EventReviewFlagChanged = notifications.EventTypeReviewFlagChanged

	// EventIssueOutcomeChanged — sk-489, US-3: the outcome of a CLOSED issue
	// changed. Both close writers write it on a repeated close with a different
	// outcome, and so does the outcome edit (T003).
	//
	// The value is taken from internal/notifications — in the shape of the
	// EventReviewMarked alias above and for the same reason: a second literal would be
	// a second source of truth and, diverging by one character, would yield an event
	// the classifier considers UNKNOWN.
	//
	// Here the similarity to the review.* pair ends. Those two types are journal-only and
	// declared private (auditOnlyEventTypes); this one is an ORDINARY PUBLIC
	// type: it has a notification reason (notifications.
	// ReasonKeyOutcomeChanged) and a fan-out to the issue author, it is published to
	// the bus on a par with the other issue events, and it is not added to any private
	// set.
	//
	// payload: field = "closeOutcome";
	//   issue.outcome_changed  old=<outcome before>  new=<outcome after>
	//                          payload={closeOutcome, previousCloseOutcome}
	EventIssueOutcomeChanged = notifications.EventTypeOutcomeChanged

	// R4.3 — directed-recipient mention events. Distinct from the
	// audit-log lifecycle events (issue.created / comment.created): a
	// mention event is emitted ONCE PER MENTIONED USER as a point-to-
	// point ping. The matching notifications row is inserted by
	// notifications.WriteMentionNotification (NOT FanOut — see the
	// comment in fanout.go).
	//
	// payload schema:
	//   comment.mention {
	//     mentionedUsername: <string>,
	//     mentionedUserId:   <uuid>,
	//     actorUsername:     <string>,
	//   }
	//   issue.mention {
	//     mentionedUsername: <string>,
	//     mentionedUserId:   <uuid>,
	//     actorUsername:     <string>,
	//     fields:            ["description","notes",...] (one of:
	//                        description, notes, acceptance_criteria,
	//                        design)
	//   }
	EventCommentMention = "comment.mention"
	EventIssueMention   = "issue.mention"
)

// Actor mirrors projects.Actor. Duplicated to avoid an import cycle if we
// later need issues→projects. R1.3 always writes 'human' actors; agent
// actors land in R2.3.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	Role      string // 'human' | 'agent' | 'system'
	SessionID string // optional, agent only
}

// HumanActor is the constructor for the common path.
func HumanActor(userID uuid.UUID, username string) Actor {
	return Actor{UserID: userID, Username: username, Role: "human"}
}

// selfMentionSuppressed reports whether the actor's own `@username` must be
// dropped from the set the mention notifications are addressed to.
//
// The exclusion exists for ONE case: a person writing their own handle knows
// what they wrote and does not need telling. It is a statement about the
// author being the addressee, not about the API key.
//
// An agent is a different party from the person whose key it holds. Every MCP
// write carries the OWNER's users.id in Actor.UserID (IssuesAgentActor), so a
// naive comparison of ids makes "@alice, take a look" written BY Alice's agent
// look like Alice talking to herself and drops it — which is precisely the
// call the mention reason exists to deliver (INV-8: "`@owner` from an agent
// is the call the reason exists for").
//
// So the distinction is drawn on the actor's ROLE, not on a heuristic: only a
// human (the default when the role is unset, matching writeMentionEvent)
// suppresses their own mention.
func selfMentionSuppressed(actor Actor) bool {
	return actor.Role != "agent"
}

// buildForceReason composes an event reason string for force-overridden
// writes. When force is false, returns the input as-is. When force is
// true, appends " (force-overridden)" if input is non-empty, or returns
// "force-overridden" if input is empty. Used by UpdateStatus / Close to
// produce consistent audit-trail event reasons.
func buildForceReason(reason string, force bool) string {
	if !force {
		return reason
	}
	if reason == "" {
		return "force-overridden"
	}
	return reason + " (force-overridden)"
}

// writeEventSQL is shared by all issue/comment event writers.
//
// payload is the live-consumer channel: unlike old_data / new_data (which
// are the audit trail — "what the row looked like before/after"), payload
// carries the routing context a subscriber needs to act on the event
// without a DB round-trip. The outbox dispatcher passes events.payload
// through to the NATS envelope verbatim (outbox/envelope.go), and the
// frontend's live-invalidation router reads envelope.payload.<key>.
// Writers that have no such context pass nil and store '{}', which
// rawIfPresent suppresses from the wire.
//
// RETURNING id is required because R1.6 fan-out (notifications.FanOut)
// needs the event's bigserial id to populate the materialized
// notifications rows under the same transaction.
const writeEventSQL = `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, $13::jsonb
)
RETURNING id
`

// WriteIssueEvent inserts an event row scoped to a single issue. projectID
// is denormalised on the event row for project-filter without a join (per
// db-schema §2.17 "denorm for NATS subject filter").
//
// field is optional: status_changed events pass "status" so listeners
// can filter on the single change axis; lifecycle events that cover the
// whole row (issue.created, issue.closed, issue.archived, ...) leave it
// empty. R3.2b moved priority/assignee into issue.field_changed rows
// emitted by eventbus.WriteHybridUpdateEvents — this writer no longer
// produces them. old/new are interface{} so callers can pass any
// json-able payload; nil is allowed and stored as SQL NULL.
//
// Returns the new event's bigserial id so callers can chain notifications
// fan-out under the same transaction (R1.6).
//
// payload stays empty for issue.* rows written here: the entity IS the
// issue, so entity_id already gives subscribers everything they need to
// route. (The hybrid-update writers in internal/eventbus own their own
// INSERT and do populate payload.changes / payload.parentEventId.)
func WriteIssueEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	issueID string,
	projectID uuid.UUID,
	actor Actor,
	field string,
	oldData, newData any,
	reason string,
) (int64, error) {
	return writeEvent(ctx, tx, eventType, "issue", issueID, &projectID,
		actor, field, oldData, newData, reason, nil)
}

// WriteIssueEventWithPayload is WriteIssueEvent plus events.payload.
//
// It exists because sk-489 gave the two close events a piece of routing
// context that old/new cannot carry: the close outcome. new_data on
// issue.status_changed is the JSON string of the new status, and
// outbox/push.go#delegatedStatusHead reads exactly that — including on a
// pod still running the previous release during a rollout. Widening it
// into an object would break those readers; payload is additive and the
// outbox passes it to the NATS envelope verbatim, which is what a live
// subscriber needs to redraw a card without a round-trip.
//
// Same contract as WriteIssueEvent in every other respect (entity_type
// "issue", entity_id = issueID, projectID denormalised on the row).
// A nil or empty payload is stored as the '{}' the NOT NULL column
// requires, so callers with nothing to add may simply use
// WriteIssueEvent.
func WriteIssueEventWithPayload(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	issueID string,
	projectID uuid.UUID,
	actor Actor,
	field string,
	oldData, newData any,
	reason string,
	payload map[string]any,
) (int64, error) {
	return writeEvent(ctx, tx, eventType, "issue", issueID, &projectID,
		actor, field, oldData, newData, reason, payload)
}

// WriteCommentEvent records a comment.* event — the single writer for all
// three of comment.created / comment.edited / comment.deleted. entity_id is
// the comment's uuid (string form for the events table's text column).
// projectID is the containing issue's project — caller passes it explicitly
// to keep this helper free of an extra DB round-trip.
//
// issueID is mandatory and lands in payload as {"issueId": <id>} (sk-201).
// A comment event's entity_id is the comment uuid, so a live subscriber that
// only sees the envelope has no way back to the issue whose comment thread
// must be invalidated — the frontend router reads exactly
// envelope.payload.issueId (skein-frontend/src/lib/liveEventInvalidation.ts)
// and silently no-ops without it. Taking issueID as a required parameter (as
// opposed to an optional payload map) makes it structurally impossible for a
// future comment-event call site to forget it; the same contract already
// holds for attachment.* (internal/attachments/store.go).
//
// An empty issueID is rejected rather than written: {"issueId": ""} is a
// non-empty object, so it would survive rawIfPresent and reach the wire
// looking like a valid payload, while the frontend router (which requires a
// non-empty string) silently no-ops — reproducing exactly the invisible
// failure this event's payload exists to prevent. A blank id can only come
// from a broken call site, never from user input, so this is a programmer
// error: fail the transaction loudly instead of shipping a dead event.
//
// new_data / old_data are untouched by this: they remain the audit trail.
//
// Returns the new event's bigserial id (R1.6 fan-out).
func WriteCommentEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	commentID uuid.UUID,
	issueID string,
	projectID uuid.UUID,
	actor Actor,
	oldData, newData any,
	reason string,
) (int64, error) {
	if issueID == "" {
		return 0, fmt.Errorf("writing %s for comment %s: issueID is required for payload.issueId",
			eventType, commentID)
	}
	return writeEvent(ctx, tx, eventType, "comment", commentID.String(), &projectID,
		actor, "", oldData, newData, reason,
		map[string]any{"issueId": issueID})
}

// writeEvent is the shared INSERT path. payload nil (or empty) is stored as
// the '{}' the NOT NULL column requires; a non-empty map is marshalled and
// flows to the NATS envelope verbatim.
func writeEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType, entityType, entityID string,
	projectID *uuid.UUID,
	actor Actor,
	field string,
	old, newVal any,
	reason string,
	payload map[string]any,
) (int64, error) {
	var oldJSON, newJSON *string
	if old != nil {
		b, err := json.Marshal(old)
		if err != nil {
			return 0, fmt.Errorf("marshalling old_data for %s: %w", eventType, err)
		}
		s := string(b)
		oldJSON = &s
	}
	if newVal != nil {
		b, err := json.Marshal(newVal)
		if err != nil {
			return 0, fmt.Errorf("marshalling new_data for %s: %w", eventType, err)
		}
		s := string(b)
		newJSON = &s
	}

	// events.payload is NOT NULL — an absent payload is the empty object,
	// which outbox.rawIfPresent then drops from the wire envelope.
	payloadJSON := "{}"
	if len(payload) > 0 {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("marshalling payload for %s: %w", eventType, err)
		}
		payloadJSON = string(b)
	}

	var sessionID *string
	if actor.SessionID != "" {
		s := actor.SessionID
		sessionID = &s
	}

	var actorUserID any
	if actor.UserID != uuid.Nil {
		actorUserID = actor.UserID
	}

	actorType := actor.Role
	if actorType == "" {
		actorType = "human"
	}

	var fieldArg any
	if field != "" {
		fieldArg = field
	}
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}

	var eventID int64
	err := tx.QueryRow(ctx, writeEventSQL,
		eventType,
		actor.Username,
		actorUserID,
		sessionID,
		actorType,
		entityType,
		entityID,
		projectID,
		fieldArg,
		oldJSON,
		newJSON,
		reasonArg,
		payloadJSON,
	).Scan(&eventID)
	if err != nil {
		return 0, fmt.Errorf("inserting event %s: %w", eventType, err)
	}
	return eventID, nil
}

// writeMentionEventSQL is identical to writeEventSQL except for the
// payload column — mention events carry the recipient + actor metadata
// in payload (NOT in old/new) so the inbox can render the row without
// joining back to users. The ordering matches the writeEvent helper so
// the actor / entity / projectID columns are populated identically.
const writeMentionEventSQL = `
INSERT INTO events (
    type, actor, actor_user_id, actor_session_id, actor_type,
    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, NULL, NULL, NULL, NULL, $9::jsonb
)
RETURNING id
`

// writeMentionEvent writes one comment.mention or issue.mention row and
// returns its bigserial id. Helper used by HandleCommentMentions and
// HandleIssueMentions; lives here because it shares the events table
// invariants with the other writers.
//
// For comment.mention entityType is "comment" and entityID is the
// comment uuid string; for issue.mention they are "issue" and the
// issue id.
//
// payload schema is documented on the EventCommentMention /
// EventIssueMention constants.
func writeMentionEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType, entityType, entityID string,
	projectID uuid.UUID,
	actor Actor,
	payload map[string]any,
) (int64, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshalling mention payload: %w", err)
	}

	var sessionID *string
	if actor.SessionID != "" {
		s := actor.SessionID
		sessionID = &s
	}
	var actorUserID any
	if actor.UserID != uuid.Nil {
		actorUserID = actor.UserID
	}
	actorType := actor.Role
	if actorType == "" {
		actorType = "human"
	}

	var eventID int64
	if err := tx.QueryRow(ctx, writeMentionEventSQL,
		eventType,
		actor.Username,
		actorUserID,
		sessionID,
		actorType,
		entityType,
		entityID,
		projectID,
		string(payloadBytes),
	).Scan(&eventID); err != nil {
		return 0, fmt.Errorf("inserting mention event %s: %w", eventType, err)
	}
	return eventID, nil
}

// emitCommentMentions takes a pre-resolved set of mentioned users and
// emits, per user, the comment.mention event and the addressed
// notification — and NO subscription: being named is not a request to
// follow the issue (US-8 / R-7, INV-8). A HUMAN's self-mention is excluded here (plan §F-5); an
// agent's mention of its own owner is NOT — see selfMentionSuppressed. The
// set the caller keeps (for dto.Mentions hydration) is untouched either
// way, since the frontend still wants to render @self as a clickable
// mention.
//
// Inline in the source mutation's tx — atomicity is load-bearing for
// the watcher-graph invariant (plan §F-13).
func emitCommentMentions(
	ctx context.Context,
	tx pgx.Tx,
	commentID uuid.UUID,
	issueID string,
	projectID uuid.UUID,
	actor Actor,
	resolved []mentions.ResolvedUser,
) error {
	notifyTargets := resolved
	if selfMentionSuppressed(actor) {
		notifyTargets = mentions.FilterOutSelf(resolved, actor.UserID)
	}
	for _, r := range notifyTargets {
		// NO auto-watch for the mentioned user (US-8 / R-7, INV-8). Being
		// named once is not a request to follow everything that happens next:
		// "a mention is delivered to the addressee independently of
		// subscriptions and does not create a subscription". The delivery below is addressed — it does not go
		// through the watcher pools and does not need a subscription to
		// arrive.
		payload := map[string]any{
			"mentionedUsername": r.Username,
			"mentionedUserId":   r.UserID,
			"actorUsername":     actor.Username,
		}
		eventID, err := writeMentionEvent(
			ctx, tx, EventCommentMention,
			"comment", commentID.String(),
			projectID, actor, payload,
		)
		if err != nil {
			return err
		}
		if err := notifications.WriteMentionNotification(
			ctx, tx, eventID, r.UserID, EventCommentMention, projectID,
		); err != nil {
			return err
		}
	}
	return nil
}

// IssueMentionField is the closed set of issue text fields scanned for
// `@username` mentions. Mirrors plan §F-11 — title is intentionally
// excluded (titles are short and frequently edited; mention noise here
// would dominate the inbox).
type IssueMentionField string

const (
	IssueMentionFieldDescription        IssueMentionField = "description"
	IssueMentionFieldNotes              IssueMentionField = "notes"
	IssueMentionFieldAcceptanceCriteria IssueMentionField = "acceptance_criteria"
	IssueMentionFieldDesign             IssueMentionField = "design"
)

// IssueMentionTexts groups the four scanned fields by name. Used by
// HandleIssueMentions and the PatchIssue diff to enumerate fields in a
// stable order.
type IssueMentionTexts struct {
	Description        string
	Notes              string
	AcceptanceCriteria string
	Design             string
}

// scanFields invokes Parse() per field and returns a per-field map
// of unique usernames. Caller composes the union (one event per user
// with payload.fields = list of fields where the user was mentioned).
//
// Order of the returned map is not guaranteed; the caller should
// iterate IssueMentionField in canonical order (description / notes /
// acceptance_criteria / design) to keep payload.fields stable across
// runs (eases debugging + testing).
func (t IssueMentionTexts) scanFields() map[IssueMentionField][]string {
	out := make(map[IssueMentionField][]string, 4)
	out[IssueMentionFieldDescription] = mentions.Parse(t.Description)
	out[IssueMentionFieldNotes] = mentions.Parse(t.Notes)
	out[IssueMentionFieldAcceptanceCriteria] = mentions.Parse(t.AcceptanceCriteria)
	out[IssueMentionFieldDesign] = mentions.Parse(t.Design)
	return out
}

// canonicalFieldOrder is the stable iteration order over
// IssueMentionField values — guarantees deterministic payload.fields
// across runs (handy for tests + log readability).
var canonicalFieldOrder = []IssueMentionField{
	IssueMentionFieldDescription,
	IssueMentionFieldNotes,
	IssueMentionFieldAcceptanceCriteria,
	IssueMentionFieldDesign,
}

// HandleIssueMentions parses the four issue text fields, resolves the
// union of @username candidates against project membership, computes
// the per-user list of fields they were mentioned in, and emits one
// issue.mention event per recipient (payload.fields lists the fields).
//
// When `oldTexts` is non-nil the caller is in the PATCH path and only
// newly-introduced usernames per field trigger notifications: a name that
// was already in the text is not a fresh call, so re-saving the field does
// not re-notify. Plan §F-9. Like the comment path, this creates no
// subscription for anybody.
//
// A human's self-mention is excluded per plan §F-5; an agent mentioning its
// own owner is delivered — see selfMentionSuppressed.
//
// Errors propagate out of the calling tx so a mention failure rolls
// back the source mutation.
func HandleIssueMentions(
	ctx context.Context,
	tx pgx.Tx,
	issueID string,
	projectID uuid.UUID,
	actor Actor,
	newTexts IssueMentionTexts,
	oldTexts *IssueMentionTexts,
) error {
	newPerField := newTexts.scanFields()

	// PATCH path: diff per field against the old text, keeping only the
	// newly-introduced usernames. CREATE path (oldTexts == nil) treats
	// every parsed username as new.
	deltaPerField := make(map[IssueMentionField][]string, 4)
	if oldTexts == nil {
		deltaPerField = newPerField
	} else {
		oldPerField := oldTexts.scanFields()
		for _, field := range canonicalFieldOrder {
			deltaPerField[field] = mentions.Diff(newPerField[field], oldPerField[field])
		}
	}

	// Build the union: collect every candidate from any field, then
	// resolve in one DB round-trip (Resolve is single-SQL). After
	// resolution build a per-user list of fields where they were
	// mentioned; iterate canonicalFieldOrder for deterministic ordering.
	var unionCandidates []string
	seenUnion := make(map[string]struct{}, 8)
	for _, field := range canonicalFieldOrder {
		for _, u := range deltaPerField[field] {
			if _, dup := seenUnion[u]; dup {
				continue
			}
			seenUnion[u] = struct{}{}
			unionCandidates = append(unionCandidates, u)
		}
	}
	if len(unionCandidates) == 0 {
		return nil
	}
	resolved, err := mentions.Resolve(ctx, tx, projectID, unionCandidates)
	if err != nil {
		return err
	}
	if selfMentionSuppressed(actor) {
		resolved = mentions.FilterOutSelf(resolved, actor.UserID)
	}
	if len(resolved) == 0 {
		return nil
	}
	resolvedByUsername := make(map[string]uuid.UUID, len(resolved))
	for _, r := range resolved {
		resolvedByUsername[r.Username] = r.UserID
	}

	// Build the per-user fields list. We iterate canonicalFieldOrder
	// (not the map) so payload.fields ordering is deterministic and
	// matches the input schema's column order.
	type userFields struct {
		userID uuid.UUID
		fields []string
	}
	perUser := make(map[string]*userFields, len(resolved))
	var orderedUsernames []string
	for _, field := range canonicalFieldOrder {
		for _, u := range deltaPerField[field] {
			uid, ok := resolvedByUsername[u]
			if !ok {
				// Unresolved (non-member or doesn't exist) — silently
				// dropped per plan §H. Plain @user remains in the text.
				continue
			}
			entry, exists := perUser[u]
			if !exists {
				entry = &userFields{userID: uid}
				perUser[u] = entry
				orderedUsernames = append(orderedUsernames, u)
			}
			entry.fields = append(entry.fields, string(field))
		}
	}

	for _, username := range orderedUsernames {
		entry := perUser[username]

		// NO auto-watch here either — the same rule on the issue-text path
		// (issue create and patch), so no route to a mention creates a
		// subscription.

		payload := map[string]any{
			"mentionedUsername": username,
			"mentionedUserId":   entry.userID,
			"actorUsername":     actor.Username,
			"fields":            entry.fields,
		}
		eventID, err := writeMentionEvent(
			ctx, tx, EventIssueMention,
			"issue", issueID,
			projectID, actor, payload,
		)
		if err != nil {
			return err
		}
		if err := notifications.WriteMentionNotification(
			ctx, tx, eventID, entry.userID, EventIssueMention, projectID,
		); err != nil {
			return err
		}
	}
	return nil
}

// WritePrivateReviewEvent writes a journal row of the author's private
// bookkeeping — the review mark and the "review required" flag toggle.
//
// THE ROW SHAPE is the plan's carrying decision (sk-303.14, key-decisions
// table), not a placement detail:
//
//	entity_type = 'user'      the entity is the AUTHOR, not the issue;
//	entity_id   = author      their identifier;
//	project_id  = NULL        the row has no project;
//	payload     = {issueId}   the issue lives in the payload.
//
// One property of the row closes four journal readers at once, and none
// of them has to add a filter — so there is nowhere to forget it:
//
//  1. the issue activity feed and the agent event block (ListEvents,
//     ListIssueActivity) select on entity_type='issue' / entity_id=<issue>;
//  2. the user's priority sample (RecentEventsForUser) — on
//     entity_type='issue' plus a JOIN issues on entity_id;
//  3. the live-feed project subject is built only when project_id is
//     non-empty (outbox.BuildSubject); with entity_type='user' the address
//     goes into the skein.user.<author>.* space, which T008 silences
//     wholesale, not publishing the private types at all;
//  4. dashboard aggregates count either rows with project_id IS NOT NULL,
//     or rows of an explicitly named type and entity_type.
//
// The rejected alternative — an issue-scoped event plus an exception list
// in five reading sites: a list you must not forget at a sixth site is
// not a guarantee, it is a promise.
//
// ⚠ WHY THE EVENT TYPE IS NOT A STRING (repair A/8). The row shape
// closes journal readers, but NOT the live-feed user subject: its bridge
// fans out to the addressee and every admin, and it is closed by not
// publishing against the private-type REGISTRY. While this writer
// accepted a string, the private shape could be written with a type that
// is not in the registry — and the row went onto the bus. That is what
// happened with the nudge digest. Now it accepts
// notifications.PrivateEventType, whose value is issued only by
// registration in the registry.
//
// ⚠ NARROWED WORDING (repair A/8, second pass). This used to say that
// "writing the private shape past the registry stopped compiling". That
// is stronger than the fact: what does not compile is A CALL OF THIS
// WRITER with an unregistered type — an ordinary INSERT into events with
// entity_type = 'user' inside any package is still possible and still
// compiles. The type-pass closes the path by which the private shape is
// written in the ordinary case; the shape as such is held by other
// assertions (nudges.TestDigestAudit_PrivateFormAndRegisteredType,
// handler/issues_review_channels_test.go), and it is they, not this
// signature.
//
// issueID is required and must not be empty: without it the journal row
// points at NOTHING — INV-10 requires a record of a state change of a
// concrete issue, and an empty payload would turn it into a mark that
// "something happened". An empty id arrives only from a broken call
// site, so this is a programmer error — abort the transaction loudly.
func WritePrivateReviewEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType notifications.PrivateEventType,
	authorUserID uuid.UUID,
	issueID string,
	actor Actor,
	field string,
	oldData, newData any,
) (int64, error) {
	if eventType.IsZero() {
		return 0, fmt.Errorf("writing a private review event for issue %s: "+
			"the event type is not registered as private", issueID)
	}
	if issueID == "" {
		return 0, fmt.Errorf("writing %s: issueID is required for payload.issueId", eventType)
	}
	if authorUserID == uuid.Nil {
		return 0, fmt.Errorf("writing %s for issue %s: author user id is required as entity_id",
			eventType, issueID)
	}
	return writeEvent(ctx, tx, eventType.String(), "user", authorUserID.String(), nil,
		actor, field, oldData, newData, "",
		map[string]any{"issueId": issueID})
}
