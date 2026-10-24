package issues

import (
	"time"

	"github.com/google/uuid"

	"skein-api/internal/person"
)

// CommentDTO is the wire shape for one row of comments.
//
// AuthorType (transport) and AuthorKind (intent) are orthogonal axes —
// AuthorType is one of {human, agent, system} and reports HOW the
// comment was posted (cookie UI / MCP / server-emitted), while
// AuthorKind (sk-16) is one of {human, agent_directed,
// agent_autonomous} and reports WHO is semantically behind it (the
// user, or an agent acting on the user's instruction, or an agent
// acting on its own initiative). The Humans / Agents UI filter chips
// on the comment thread use AuthorKind; legacy callers that only know
// AuthorType continue to receive it unchanged.
type CommentDTO struct {
	ID           uuid.UUID  `json:"id"`
	IssueID      string     `json:"issueId"`
	Author       string     `json:"author"`
	AuthorUserID *uuid.UUID `json:"authorUserId,omitempty"`
	// AuthorStatus — the author's person marks in the issue's space
	// (sk-674.18 T027): departed, or deleted with the pseudonym; absent
	// when nothing to mark or the author is not a user (an agent label).
	AuthorStatus *person.Status `json:"authorStatus,omitempty"`
	AuthorType   string         `json:"authorType"`
	AuthorKind   string         `json:"authorKind"`
	Body         string         `json:"body"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`

	// sk-57 — edit/delete metadata.
	//
	// Version is the optimistic-concurrency counter (starts at 1, bumped
	// on every edit). Edits pass expectedVersion; a mismatch surfaces as
	// ErrVersionConflict carrying the live value.
	//
	// EditedAt is nil until the first edit; non-nil ⇒ the UI renders an
	// "edited" badge (tooltip = the edit timestamp). CreatedAt is never
	// rewritten.
	//
	// DeletedAt is nil for live comments. When non-nil the row is a
	// tombstone: ListComments blanks Body and Mentions on read so deleted
	// text never reaches clients, but the slot remains in the thread.
	Version   int        `json:"version"`
	EditedAt  *time.Time `json:"editedAt,omitempty"`
	DeletedAt *time.Time `json:"deletedAt,omitempty"`

	// R4.3 — resolved @username mentions in `body`. See
	// IssueDetail.Mentions for the contract — same shape, scope here
	// is just the comment body. Always non-nil.
	Mentions []MentionRef `json:"mentions"`
}

// ListCommentsOpts controls ListComments filtering and last-N selection
// (sk-242.1 US-5/6). Zero value = full chronological thread (REST default).
//
// Limit:
//   - 0 (or negative, treated as 0) — return the full filtered population
//   - N > 0 — return the last N of the filtered set, ordered ASC with
//     (created_at, id) tiebreak
//
// Since: when non-nil, population is rows with created_at strictly greater
// than Since (exclusive). Total is counted AFTER this filter and BEFORE Limit.
type ListCommentsOpts struct {
	Limit int
	Since *time.Time
}

// LabelStat is the wire shape for the per-project labels summary
// (GET /projects/{id}/labels). Count is "issues using this label" so the
// UI can rank suggestions by frequency.
type LabelStat struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// UserEventDTO is the wire shape returned by RecentEventsForUser — the
// per-user activity feed used by skein_prime to remind the agent what
// just happened in their world.
//
// Stays narrow on purpose: skein_prime aggregates several blocks and
// every byte counts against MAX_MCP_OUTPUT_TOKENS. Heavy fields
// (old_data, new_data, payload) are not surfaced; if the caller needs
// them, skein_show on the issue is the right path.
type UserEventDTO struct {
	OccurredAt time.Time `json:"at"`
	Type       string    `json:"type"`
	IssueID    string    `json:"issueId"`
	Title      string    `json:"title"`
	Actor      string    `json:"actor"`
}

// ActivityActor is the resolved actor on an ActivityEntry. UserID is
// nil for system events (entity_type='system'); Username is always
// populated (events.actor is NOT NULL).
type ActivityActor struct {
	UserID   *uuid.UUID `json:"userId,omitempty"`
	Username string     `json:"username"`
	Type     string     `json:"type"` // 'human' | 'agent' | 'system'
	// The person marks of the actor in the event's project space
	// (sk-674.18 T027, spaces/contract.go).
	person.Status
}

// ActivityEntity is the entity an ActivityEntry refers to. For comment
// events the entity points to the parent issue (more useful UX than
// linking to a transient comment id) while preserving the comment's
// project context.
// A Hard-redacted (external war-room) row drops ID / ProjectID /
// ProjectIDPrefix entirely — hence the omitempty on the string ids and
// the *uuid.UUID on ProjectID. Zeroing a uuid.UUID value would still
// serialize "00000000-…" (a distinguishable value, not absence), so the
// id MUST be a pointer that goes nil, not a zeroed array.
type ActivityEntity struct {
	Type            string     `json:"type"` // 'issue' | 'project' | 'comment' | 'dep' | 'component'
	ID              string     `json:"id,omitempty"`
	Title           string     `json:"title,omitempty"`
	ProjectID       *uuid.UUID `json:"projectId,omitempty"`
	ProjectIDPrefix string     `json:"projectIdPrefix,omitempty"`
}

// ActivityChange is one field-level change inside an issue.updated
// event. It mirrors eventbus.FieldChange (the shape persisted in
// events.payload.changes[]) so the frontend can render an explicit
// "<field>: <old> → <new>" line per changed field instead of guessing
// from the issue's current state. OldValue / NewValue are free-form
// (string / number / bool / null) keyed by the field — the frontend
// formats them per field name.
type ActivityChange struct {
	Field    string `json:"field"`
	OldValue any    `json:"oldValue"`
	NewValue any    `json:"newValue"`
}

// ActivityEntry is one row in the project / me activity feed
// (R3.4 — third-person audit timeline).
//
// Summary is built via notifications.FormatSummary so the wording
// matches the Inbox row for the same event. sk-73 adds the structured
// change detail (OldValue / NewValue / Changes) so the Activity UI can
// render "open → in_progress" and per-field diffs from the event's own
// audit data rather than a flat summary string. The three fields are
// additive and omitempty: lifecycle events with no diff (created,
// commented, claimed) carry none of them and the wire shape is
// unchanged for those rows.
type ActivityEntry struct {
	ID         int64          `json:"id"`
	OccurredAt time.Time      `json:"occurredAt"`
	Type       string         `json:"type"`
	Actor      ActivityActor  `json:"actor"`
	Entity     ActivityEntity `json:"entity"`
	// SpaceID is the short id of the space of the event's project
	// (sk-674.18 T030: the HOT ZONE feed rows); dropped by the hard
	// redaction.
	SpaceID string  `json:"spaceId,omitempty"`
	Field   *string `json:"field,omitempty"`
	// Summary is omitempty so a redacted row (Summary stripped to "")
	// drops it from the wire rather than emitting "summary":"".
	Summary string `json:"summary,omitempty"`
	// OldValue / NewValue carry the single-field old/new for events
	// that store them in events.old_data / new_data (issue.status_changed
	// → status strings; issue.reparented → {parentIds}/{parentId};
	// dependency.created/deleted → the edge descriptor). nil (omitted)
	// for events with no scalar diff.
	OldValue any `json:"oldValue,omitempty"`
	NewValue any `json:"newValue,omitempty"`
	// Changes is the per-field diff for issue.updated, lifted verbatim
	// from events.payload.changes[]. nil/omitted for every other event
	// type.
	Changes []ActivityChange `json:"changes,omitempty"`
	// ProjectID is a pointer + omitempty so a Hard-redacted row drops it
	// from the wire entirely (a zeroed uuid.UUID would still serialize
	// "00000000-…"). Non-nil for every non-Hard-redacted row (the query
	// requires project_id IS NOT NULL).
	ProjectID *uuid.UUID `json:"projectId,omitempty"`
	// Redacted is true when this row belongs to a project the caller
	// cannot see (war-room cross-project view). Under Soft redaction the
	// content (Title/Summary/diff) is stripped but identity survives;
	// under Hard redaction the id, project prefix and both projectId
	// fields are dropped too. Actor, time and type always remain so the
	// activity is visible but its content is hidden.
	Redacted bool `json:"redacted,omitempty"`
}

// ActivityListResult is the paginated wire shape for the activity
// endpoints. NextCursor is opaque (base64url over int64) and absent
// when there is no further page.
type ActivityListResult struct {
	Items      []ActivityEntry `json:"items"`
	NextCursor *string         `json:"nextCursor,omitempty"`
}
