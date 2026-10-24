// Package issues implements R1.3 — Issues CRUD with hierarchical IDs,
// optimistic concurrency, comments, components, and labels.
//
// The package is shaped after internal/projects: a Store wraps *pgxpool.Pool,
// types.go holds wire DTOs, validate.go input validation, id.go ID-generation
// strategies (counter / hash / hierarchical / custom), events.go event-name
// constants and writers, and store.go business operations.
package issues

import (
	"time"

	"github.com/google/uuid"

	"skein-api/internal/person"
)

// Closed-set strings used both as DB CHECK constraint values and wire-API
// status / type / source enums. Keeping them as exported untyped string
// constants avoids per-value enum boilerplate while still letting callers
// import them by name (issues.StatusOpen) instead of magic strings.
const (
	StatusOpen       = "open"
	StatusInProgress = "in_progress"
	// StatusBlocked is DEPRECATED (sk-31). The 'blocked' string is no
	// longer a writable status — blocked-ness is derived at query time
	// from the dependency graph (typed-deps edges with type='blocks' /
	// 'waits-for' whose target is still open). The constant is retained
	// only for the validator's rejection branch (see
	// validate.go::ValidateStatus) and for any leftover string-compare
	// sites that need a stable spelling; do not write it to issues.status.
	StatusBlocked  = "blocked"
	StatusDeferred = "deferred"
	StatusClosed   = "closed"
	// StatusStanding (sk-118) is a long-lived "watch / recurring" task
	// that lives OUTSIDE the open→in_progress→closed work lifecycle: it
	// is not counted as active work, never gates readiness, and cannot be
	// claimed. This slice (F2a) only makes 'standing' a valid stored
	// status value (DB CHECK + validator); all guarding behavior (claim
	// reject, count exclusion, transition matrix, nested-standing ban) is
	// added in later slices F2b–F2i.
	StatusStanding = "standing"

	TypeBug      = "bug"
	TypeFeature  = "feature"
	TypeTask     = "task"
	TypeEpic     = "epic"
	TypeChore    = "chore"
	TypeDecision = "decision"
	TypeIdea     = "idea"
	TypeNote     = "note"

	SourceHuman = "human"
	// SourceAgentDirected — the human user explicitly asked the agent to
	// create this issue. Default origin for any agent-driven create where
	// the user is in the loop (skein_create kind="directed").
	SourceAgentDirected = "agent_directed"
	// SourceAgentAutonomous — the agent decided to create this issue mid-
	// task as a discovered side-quest / subtask, without an explicit user
	// instruction. Lets the Inbox visually distinguish self-spawned work
	// (skein_create kind="autonomous").
	SourceAgentAutonomous = "agent_autonomous"

	// OriginHumanDriven groups origins where a human is in the loop —
	// either the human created the issue themselves (source='human') or
	// they explicitly directed an agent to do so (source='agent_directed').
	// Used by the list endpoint's `origin=` filter (Phase 2 ui-redesign):
	// the default Inbox/board mode hides agent_autonomous chatter.
	OriginHumanDriven = "human-driven"
	// OriginAgentAutonomous selects only agent self-spawned work
	// (source='agent_autonomous') — the "agent firehose" toggle.
	OriginAgentAutonomous = "agent-autonomous"
	// OriginAll is the explicit no-filter sentinel; equivalent to omitting
	// the `origin` query param. Accepted on the wire so callers can flip
	// between modes via a plain UI selector without conditional URL
	// construction.
	OriginAll = "all"
)

// OriginGroupsToSources translates a set of validated origin-group tokens
// (OriginHumanDriven / OriginAgentAutonomous / OriginAll) into the
// deduplicated, order-stable list of issues.source values the caller should
// filter on. Multi-select analogue of the single-group switch every list
// path used to inline separately (store.go, list_all.go, list_by_user.go,
// internal/search) — extracted here so the four call sites share one
// translation table instead of drifting.
//
// noFilter is true when OriginAll is present anywhere in groups (a "select
// all" pill always means "no filter", even mixed with other groups) OR
// groups is empty. Selecting BOTH human-driven AND agent-autonomous
// explicitly is semantically equivalent to "all" — the union already covers
// every source — but is intentionally NOT collapsed to noFilter=true unless
// OriginAll is literally present: a caller that always sends an explicit
// non-empty set (never omits the param) needs the ANY() clause to still be
// correct and inert, not silently skipped.
func OriginGroupsToSources(groups []string) (sources []string, noFilter bool) {
	if len(groups) == 0 {
		return nil, true
	}
	for _, g := range groups {
		if g == OriginAll {
			return nil, true
		}
	}
	seen := make(map[string]struct{}, 3)
	add := func(s string) {
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		sources = append(sources, s)
	}
	for _, g := range groups {
		switch g {
		case OriginHumanDriven:
			add(SourceHuman)
			add(SourceAgentDirected)
		case OriginAgentAutonomous:
			add(SourceAgentAutonomous)
		}
	}
	return sources, false
}

// IssueDetail is the full wire shape returned by GET /issues/{id} and by
// every mutation that needs to echo the post-state. Heavy fields
// (description, design, notes, acceptance_criteria) live here; the list
// endpoint uses IssueSummary instead to keep paginated responses small.
//
// Slice/map fields are always returned non-nil — `[]string{}` rather than
// `null` — so JS clients don't have to special-case absence vs empty.
type IssueDetail struct {
	ID        string    `json:"id"`
	ProjectID uuid.UUID `json:"projectId"`
	// ParentID is the current parent id derived from the dependencies
	// graph (parent-child edge, first by created_at). Wire field is
	// `parentIdCurrent` since sk-35 — the `issues.parent_id` column is
	// frozen for backward-compat reads but no longer authoritative.
	// Absent when the parent's project is not visible to the reader
	// (sk-674.18 T009).
	ParentID      *string    `json:"parentIdCurrent,omitempty"`
	Version       int        `json:"version"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	Type          string     `json:"type"`
	Priority      int        `json:"priority"`
	Status        string     `json:"status"`
	Pinned        bool       `json:"pinned"`
	Source        string     `json:"source"`
	SourceContext *string    `json:"sourceContext,omitempty"`
	Assignee      *string    `json:"assignee,omitempty"`
	LockedBy      *string    `json:"lockedBy,omitempty"`
	LockedAt      *time.Time `json:"lockedAt,omitempty"`
	// LockedByUsername is the username of the user who PERFORMED the claim
	// that holds the lock now (sk-421.1, US-15) — derived on read from that
	// claim's issue.claimed event, never stored. Present only while the
	// issue is locked AND that event is found; absent (not empty) otherwise,
	// so it disappears with the lock on unclaim, close, auto-release or a
	// forced release. For an admin who claimed on behalf of another user it
	// is the admin, not the assignee. A request that does not change the
	// lock cannot change it, whichever session it presents. LockedBy keeps
	// the MCP session id; this field never repeats it.
	LockedByUsername   *string    `json:"lockedByUsername,omitempty"`
	Design             string     `json:"design"`
	AcceptanceCriteria string     `json:"acceptanceCriteria"`
	Notes              string     `json:"notes"`
	EstimatedMinutes   *int       `json:"estimatedMinutes,omitempty"`
	DueAt              *time.Time `json:"dueAt,omitempty"`
	DeferUntil         *time.Time `json:"deferUntil,omitempty"`
	StartedAt          *time.Time `json:"startedAt,omitempty"`
	ClosedAt           *time.Time `json:"closedAt,omitempty"`
	ClosedReason       *string    `json:"closedReason,omitempty"`
	// CloseOutcome — the close outcome (sk-489, US-1): one of four values
	// from close_outcome.go, answering "how did it end", unlike the free
	// text of ClosedReason. It sits next to ClosedAt / ClosedReason because
	// it is a property of the same event — the close — while the wire name is taken from
	// the action (`closeOutcome`, like the "close" argument), not from the state.
	//
	// Present if and only if the issue is closed (INV-1): an unclosed issue
	// has no key at all, a closed one always has it. Hence *string and
	// omitempty: the invariant is held by storage (the CHECK and the trigger of migration
	// 000050), and the wire only repeats it — "no key" reads as "not
	// closed", and no value of the field can contradict that.
	//
	// A client that receives a value outside the four prints it as is and does not
	// count it as done or as dropped out (INV-2) — it takes the numbers from
	// the server and does not derive them from the row.
	CloseOutcome      *string    `json:"closeOutcome,omitempty"`
	ExternalRef       *string    `json:"externalRef,omitempty"`
	CreatedBy         uuid.UUID  `json:"createdBy"`
	CreatedByUsername *string    `json:"createdByUsername,omitempty"`
	AssigneeUserID    *uuid.UUID `json:"assigneeUserId,omitempty"`
	// AssigneeStatus / CreatedByStatus — the person marks of the assignee
	// and the author in the issue's space (sk-674.18 T027,
	// spaces/contract.go): departed, or deleted with the pseudonym. Absent
	// when there is nothing to mark, or when the assignee is free text.
	AssigneeStatus  *person.Status `json:"assigneeStatus,omitempty"`
	CreatedByStatus *person.Status `json:"createdByStatus,omitempty"`
	// SpaceID is the short id of the space of the issue's project
	// (sk-674.18 T032), not of its id prefix.
	SpaceID string `json:"spaceId,omitempty"`
	// AcceptedAt records when the current assignee explicitly accepted
	// the task. nil = "not yet accepted" (Inbox surfaces it as "needs
	// acceptance"). Reset to nil whenever the assignee changes; auto-
	// populated when the creator self-assigns. Phase 2 introduces
	// skein_accept + issue.accepted event for the explicit positive
	// transition.
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	// AcceptedBy is the user UUID that produced the latest acceptance
	// (FK → users(id) ON DELETE SET NULL — the timestamp survives a
	// hard-delete of the user record). nil iff AcceptedAt is nil.
	AcceptedBy *uuid.UUID `json:"acceptedBy,omitempty"`
	// AcceptedByUsername is the username for AcceptedBy, joined via
	// LEFT JOIN users so a deleted accepter surfaces as nil. Optional
	// in the wire shape.
	AcceptedByUsername *string   `json:"acceptedByUsername,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
	// RowSeq is the row freshness order of the issue (sk-622.21, US-15,
	// decision K16) — the same value and the same rule as
	// [IssueSummary.RowSeq]: an open card takes a phase-2 event row
	// ([NeutralRow]) only if the row's RowSeq is greater than this one.
	//
	// Read in ONE statement with the issue's own columns AND the card's
	// labels and components (issueRowSelect, rowComponentsLabelsCols —
	// decision R26 item 2): the card is one snapshot, exactly the state its
	// RowSeq names. A separate later read of the labels could show labels
	// newer than RowSeq, and an intermediate event row with a greater RowSeq
	// would then roll them back under the strict-greater rule. Zero (key absent) — the issue row has not been written
	// since the phase-2 rollout (migration 000064), order unknown; see
	// [IssueSummary.RowSeq].
	RowSeq   int64          `json:"rowSeq,omitempty"`
	Metadata map[string]any `json:"metadata"`

	// CanForceUnlock — sk-674.18 T034 (US-16, K-28): the reader may release
	// another holder's claim on this issue (force_unlock: the project
	// owner, the space owner, the instance admin). Set by the card read
	// (GET …/issues/{id}) and the unlock action, which answer per reader;
	// absent on every other surface that returns a card.
	CanForceUnlock *bool `json:"canForceUnlock,omitempty"`

	// Aggregates — joined / counted in GetIssue.
	ComponentIDs  []uuid.UUID `json:"componentIds"`
	Labels        []string    `json:"labels"`
	CommentsCount int         `json:"commentsCount"`
	CommitRefs    []CommitRef `json:"commitRefs"` // populated from issue_commits (R3.4 commit-on-close).

	// InheritedComponentIDs is the transient "echo what happened on
	// this call" channel populated exclusively by CreateIssue when the
	// caller omitted componentIds AND a parent was supplied AND the
	// parent (same project) had components attached. The store copies
	// the parent's set into ComponentIDs at INSERT time and mirrors
	// the inherited slice here so the caller can surface a warning
	// ("inherited foo, bar from parent — override via skein_update if
	// wrong"). GetIssue / Patch / List paths never populate this field;
	// omitempty keeps it absent on every non-create read.
	InheritedComponentIDs []uuid.UUID `json:"inheritedComponentIds,omitempty"`

	// IsBlocked is the derived "this issue is blocked" flag (sk-31), FOR THE
	// READER (sk-423): true when at least one outgoing blocks / waits-for
	// edge points at a target issue whose status is neither 'closed' nor
	// 'standing' AND whose project is inside the reader's horizon. A blocker
	// outside the horizon never makes this true — it is reported as a link in
	// UnavailableBlockingLinks instead. Agents never write it and the UI never
	// caches it across edge mutations.
	//
	// INV-11 (sk-421.1, restated by sk-423): on the detail it is DERIVED FROM
	// Blockers of the same response — true if and only if Blockers is
	// non-empty — so the flag and the list always describe one state of the
	// graph.
	IsBlocked bool `json:"isBlocked"`

	// Blockers is the populated list backing IsBlocked: one entry per gating
	// blocker (neither closed nor standing, on the TO-side of an outgoing
	// blocks / waits-for edge) whose project is inside the reader's horizon,
	// ordered by id. Every entry is {id, title, status}. Always non-nil
	// (empty array when not blocked) so JS clients don't have to
	// special-case absence.
	//
	// sk-423: the redacted form {id, redacted:true} that sk-421.1 put here
	// for a blocker outside the reader's horizon is gone — its mere presence
	// said that the hidden issue was not closed. Such edges are counted in
	// UnavailableBlockingLinks.
	Blockers []BlockerRef `json:"blockers"`

	// UnavailableBlockingLinks is the number of outgoing blocks / waits-for
	// edges of this issue whose target lives in a project outside the
	// reader's horizon — counted WHATEVER the target's status, closed and
	// standing included (sk-423). It is the fact of the link, never a claim
	// that the link blocks: it does not depend on the hidden issue's status,
	// so it cannot reveal it (the same rule as depsCounts' `unavailable`
	// bucket and epic.unavailableChildren). Always present; 0 when there is
	// none, and always 0 for a reader who sees every project.
	//
	// Readiness (skein_ready) keeps counting every blocker, so an issue with
	// isBlocked=false and UnavailableBlockingLinks > 0 may be absent from the
	// ready list: whether such a link still blocks is not knowable from here.
	UnavailableBlockingLinks int `json:"unavailableBlockingLinks"`

	// R4.3 — resolved @username mentions across description / notes /
	// acceptance_criteria / design. Frontend renderer substitutes the
	// <UserMention> component for `@username` matches whose username is
	// in this list; unresolved `@foo` stays plain text. Always non-nil
	// (`[]` rather than `null`) so JS clients don't have to special-
	// case absence.
	Mentions []MentionRef `json:"mentions"`

	// sk-32: Epic rollup — populated only when issue.type == 'epic'.
	// nil for every other type (and JSON-omitted via omitempty). The
	// child set is sourced from the dependencies graph (parent-child
	// edges). sk-35 restored this as canonical.
	//
	// EligibleForClose is true when TotalChildren > 0 AND
	// ClosedChildren == TotalChildren. Empty epics never become
	// eligible — they would be a false signal ("0 of 0 closed" is not
	// a meaningful "done" state).
	Epic *EpicRollup `json:"epic,omitempty"`

	// sk-34 — delegation chain. PreviousAssignees lists every user UUID
	// that was ever this issue's assignee (with no duplicates), append-
	// ordered by reassignment timestamp. PatchIssue's assignee-change
	// branch appends the old assignee on each reassign and prunes the
	// new assignee when they "come back" (the array tracks PAST
	// assignees only — the current assignee is never present).
	//
	// DismissedBy lists users who explicitly opted out of this issue's
	// Delegated lens via the "Drop from Delegated" affordance
	// (skein_drop_delegated MCP tool / POST /issues/{id}/dismiss). Sticky
	// — entries clear only when the user becomes the current assignee
	// again. The Delegated lens predicate is:
	//
	//   created_by = me
	//   OR (me ∈ previous_assignees
	//       AND me ∉ dismissed_by
	//       AND assignee != me)
	//
	// Both arrays are wire-omitempty — older clients that don't know the
	// fields simply see nothing extra. JSON shape is always []uuid (empty
	// arrays normalise to absence via omitempty + nil slice).
	PreviousAssignees []uuid.UUID `json:"previousAssignees,omitempty"`
	DismissedBy       []uuid.UUID `json:"dismissedBy,omitempty"`

	// PreviousAssigneeUsernames is the username projection of
	// PreviousAssignees in matching order. Surfaced so the UI can
	// render a human-readable delegation chain without an extra
	// per-uuid resolve. Missing users (deleted accounts) project
	// as empty strings — the UI should treat empty slots as
	// "unknown" and skip them when rendering.
	PreviousAssigneeUsernames []string `json:"previousAssigneeUsernames,omitempty"`
	// DismissedByUsernames is the username projection of
	// DismissedBy in matching order. Same missing-user semantics
	// as PreviousAssigneeUsernames.
	DismissedByUsernames []string `json:"dismissedByUsernames,omitempty"`

	// --- sk-303.14, contract C1 — the author's private
	// bookkeeping (INV-2, SC-028). ---
	//
	// Three review-mechanic fields. They land on the card ONLY when the
	// caller is the issue author (issues.CreatedBy), and they are filled
	// in EXACTLY one place — applyReviewVisibility (review_visibility.go).
	// Other readers have no keys in the object at all: not `null`, not
	// `false`, not zero — absence, because "no field" and "no mark" must
	// be indistinguishable to the assignee.
	//
	// Hence pointers and `omitempty` on all three, including the flag: on
	// a flat bool `omitempty` strips `false` and keeps `true`, which would
	// disclose exactly half the state; a nil pointer removes the key at
	// any value, and a non-nil pointer to false gives
	// `"reviewRequired":false` to the author.
	//
	// There is NO username projection of the mark author here on purpose:
	// contract C1 is three fields, and only the issue author may set the
	// mark (INV-4), so reviewedBy is always equal to createdBy, whose
	// name is already on the card as createdByUsername. A fourth key
	// would add no information and would widen the contract the frontend
	// consumes.
	ReviewRequired *bool      `json:"reviewRequired,omitempty"`
	ReviewedAt     *time.Time `json:"reviewedAt,omitempty"`
	ReviewedBy     *uuid.UUID `json:"reviewedBy,omitempty"`

	// raw fields are what actually sits in the row. Unexported, so
	// encoding/json never sees them under any circumstance, and no future
	// card serialiser can emit them by accident. Filled by row scanning;
	// only applyReviewVisibility reads them.
	rawReviewRequired bool
	rawReviewedAt     *time.Time
	rawReviewedBy     *uuid.UUID
}

// EpicRollup carries the cheap child-count aggregates surfaced on every
// epic-typed IssueDetail (sk-32, slice 1). The shape is wire-stable;
// any future widening (story points, burndown, swarm validation) lives
// behind a sk-24 follow-up.
//
// Status semantics:
//
//   - OpenChildren       — children with status = 'open'
//
//   - InProgressChildren — status = 'in_progress'
//
//   - ClosedChildren     — status = 'closed'
//
//   - DeferredChildren   — status = 'deferred' (sk-421.2, R-1). Own
//     bucket so the epic card's identity closes:
//     closed + (open + in_progress) + deferred +
//     unavailable = total.
//
//   - BlockedChildren    — status IN ('open','in_progress') AND EXISTS an
//     outgoing blocks/waits-for edge pointing at a
//     still-gating target (sk-31's derived flag).
//     sk-421.2 (R-11, owner answer N-1a) NARROWED
//     this: a DEFERRED child with an active blocker
//     is no longer counted. BlockedChildren is a
//     SUBSET of OpenChildren + InProgressChildren,
//     so the card reads it as a breakdown of "not
//     done" (B <= Y), not a free-floating flag.
//     CONTRACT CHANGE: REST card and MCP surface
//     both return the narrowed value.
//
//   - StandingChildren    — status = 'standing'. F2h (sk-118.9, §11.5):
//     standing children are INVISIBLE to the
//     rollup — they are EXCLUDED from TotalChildren,
//     OpenChildren, InProgressChildren and
//     BlockedChildren, and surfaced ONLY in this
//     dedicated bucket. An epic whose only
//     non-closed children are standing is therefore
//     EligibleForClose (Closed == Total).
//
//   - UnavailableChildren — children in a project the caller cannot see
//     (sk-291). Deliberately NOT broken down by
//     status, and deliberately still counted in
//     TotalChildren: the status of an invisible
//     task must not be recoverable from any bucket,
//     including by subtraction, so an invisible
//     child's standing-ness is not consulted
//     either. Same policy, same bucket name as the
//     sk-282 dependency counters — one product must
//     not answer "what is behind the wall" two
//     different ways.
//
// TotalChildren is COUNT(*) FILTER (status <> 'standing') over the VISIBLE
// children plus every invisible one; it includes deferred children (which
// since sk-421.2 also have their own bucket) but excludes standing (which
// never count toward close).
//
// EligibleForClose is therefore false whenever UnavailableChildren > 0:
// "every child I can see is closed" is not "this epic is done", and the
// counters answer "cannot say" rather than guess.
type EpicRollup struct {
	TotalChildren       int  `json:"totalChildren"`
	OpenChildren        int  `json:"openChildren"`
	InProgressChildren  int  `json:"inProgressChildren"`
	ClosedChildren      int  `json:"closedChildren"`
	DeferredChildren    int  `json:"deferredChildren"`
	StandingChildren    int  `json:"standingChildren"`
	UnavailableChildren int  `json:"unavailableChildren"`
	BlockedChildren     int  `json:"blockedChildren"`
	EligibleForClose    bool `json:"eligibleForClose"`

	// --- sk-489: progress numbers by OUTCOME, not by status. ---
	//
	// ClosedChildren above stays a breakdown BY STATUS and does not change meaning
	// (INV-3): it still includes all closed children whatever the outcome.
	// Progress (ring, branch, chips) is counted by one rule, INV-5:
	// share = DoneChildren / CountedChildren.
	//
	// All three are numbers WITHOUT omitempty, and this is a substantive requirement, not
	// a consequence of the type: zero must read as "counted, and there are none", not
	// as "the field did not arrive". A client that does not see the key must assume
	// an OLD api is answering and show the previous picture from the previous fields
	// (US-10) — and with omitempty these two states would merge on the very first
	// epic without dropped-out children.

	// DoneChildren — how many children VISIBLE to the reader are closed with the outcome
	// "done". The numerator of progress. Unavailable children are not included
	// under any of their outcomes (INV-4).
	DoneChildren int `json:"doneChildren"`
	// NoWorkChildren — how many VISIBLE children are closed without work (the outcome
	// differs from "done"). The number is counted by the SQL leg
	// `store.go#epicRollupFor` — the same as in the rows
	// (`store.go#epicSummaryNoWorkExpr`); the Go predicate
	// `close_outcome.go#IsNoWorkOutcome` writes the same rule as a whitelist,
	// but does not take part in the count. This is the "no work" tail next to
	// the ring; they are not part of the denominator.
	NoWorkChildren int `json:"noWorkChildren"`
	// CountedChildren — the progress denominator: TotalChildren minus the visible
	// dropped-out ones. Unavailable ones stay in the denominator, as today (INV-4):
	// it is not known whether they are done, and throwing them out would mean
	// asserting that they dropped out.
	//
	// Its own name, not "total for progress", because TotalChildren next to it
	// keeps its previous meaning, and two different numbers must be named
	// differently.
	CountedChildren int `json:"countedChildren"`
}

// EpicProgress is the compact rollup surfaced on IssueSummary rows for
// epic-typed issues (sk-32, slice 2). One CASE-WHEN-derived projection
// per query so non-epic rows pay zero cost. nil/omitted on every other
// type. The UI reads it for inline "(closed/total closed)" progress
// chips in the task list.
type EpicProgress struct {
	Closed int `json:"closed"`
	Total  int `json:"total"`
	// Unavailable counts children in projects the caller cannot see
	// (sk-291). They stay in Total and are absent from Closed, so a chip
	// with a non-zero Unavailable can never read as complete — which is
	// the honest answer, since whether those children are done is not
	// knowable from here. Not omitempty: a zero has to read as "counted,
	// and there are none" rather than as "not counted".
	Unavailable int `json:"unavailable"`

	// --- sk-489: the same triple as in EpicRollup, with short names. ---
	//
	// One rule for all surfaces (INV-5): share = Done / Counted.
	// Closed / Total / Unavailable above do not change meaning (INV-3) — Closed
	// is still "closed by status".
	//
	// Not omitempty for the same reason as Unavailable next to it, and here it is
	// stricter: zero must read as "counted, and there are none". A client
	// that did not receive the key shows the previous picture from the previous fields
	// (US-10), so dropping the key at zero would tell the new
	// frontend "you are looking at an old api" on every epic without dropped-out children.

	// Done — how many visible children are closed with the outcome "done". The numerator.
	Done int `json:"done"`
	// NoWork — how many visible children are closed without work. They are not part of
	// the denominator; the chip prints them as a separate tail.
	NoWork int `json:"noWork"`
	// Counted — the progress denominator: Total minus the visible dropped-out ones.
	// Unavailable ones stay in it, as today (INV-4).
	Counted int `json:"counted"`
}

// BlockerRef is one entry of IssueDetail.Blockers — a slim summary of
// the issue blocking the loaded one. Title + status let the UI render
// a tooltip / chip without a second fetch per blocker.
//
// One wire form: {"id", "title", "status"}. Since sk-423 every entry is a
// blocker the reader can see; a blocker outside the reader's horizon is not
// an entry at all (IssueDetail.UnavailableBlockingLinks counts such edges).
//
// The type is shared with Store.EpicOpenChildrenFor (the skein_close epic
// warning). That list OMITS children the caller cannot see, so its entries
// have the same form.
type BlockerRef struct {
	// ID is the blocker's hierarchical id.
	ID string `json:"id"`
	// Title is the blocker's title.
	Title string `json:"title,omitempty"`
	// Status is the blocker's status: open, in_progress or deferred
	// (closed and standing targets never block).
	Status string `json:"status,omitempty"`
}

// MentionRef is one resolved mention entry on the wire. Username is
// the canonical lowercase form (matches users.username); UserID is
// the users.id the mention resolves to. Used by IssueDetail.Mentions
// and CommentDTO.Mentions.
type MentionRef struct {
	Username string    `json:"username"`
	UserID   uuid.UUID `json:"userId"`
	// Status — the mentioned person's marks in the issue's space
	// (sk-674.18 T027): deleted with the pseudonym; absent when nothing
	// to mark. A person outside the space does not resolve at all (INV-9).
	Status *person.Status `json:"status,omitempty"`
}

// IssueSummary is the leaner shape used by the list endpoint. Drops the long
// text fields (description, design, notes, acceptanceCriteria) and metadata
// to keep paginated responses small enough to serialize at full speed.
//
// CreatedByUsername / AssigneeUserID are populated via the same JOIN to
// users that GetIssue uses (R1.6 polish — F258).
type IssueSummary struct {
	ID        string    `json:"id"`
	ProjectID uuid.UUID `json:"projectId"`
	// sk-35: derived from the parent-child dep graph (first edge by
	// created_at). Wire field is `parentIdCurrent` — see IssueDetail
	// for the full rationale; absent when the reader cannot see the
	// parent (sk-674.18 T009).
	ParentID          *string    `json:"parentIdCurrent,omitempty"`
	Version           int        `json:"version"`
	Title             string     `json:"title"`
	Type              string     `json:"type"`
	Priority          int        `json:"priority"`
	Status            string     `json:"status"`
	Pinned            bool       `json:"pinned"`
	Assignee          *string    `json:"assignee,omitempty"`
	AssigneeUserID    *uuid.UUID `json:"assigneeUserId,omitempty"`
	CreatedBy         uuid.UUID  `json:"createdBy"`
	CreatedByUsername *string    `json:"createdByUsername,omitempty"`
	// AssigneeStatus / CreatedByStatus — the person marks of the assignee
	// and the author in the issue's space (sk-674.18 T027,
	// spaces/contract.go): departed, or deleted with the pseudonym. Absent
	// when there is nothing to mark, or when the assignee is free text.
	AssigneeStatus  *person.Status `json:"assigneeStatus,omitempty"`
	CreatedByStatus *person.Status `json:"createdByStatus,omitempty"`
	// SpaceID is the short id of the space of the issue's project
	// (sk-674.18 T032), not of its id prefix.
	SpaceID      string      `json:"spaceId,omitempty"`
	ComponentIDs []uuid.UUID `json:"componentIds"`
	Labels       []string    `json:"labels"`
	// AcceptedAt mirrors IssueDetail.AcceptedAt but at the summary
	// level so the Inbox / list views can render the "needs acceptance"
	// chip without round-tripping for the full detail. AcceptedBy /
	// AcceptedByUsername are NOT carried on the summary — only needed
	// when the user opens the issue.
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	// CloseOutcome mirrors IssueDetail.CloseOutcome on the row (sk-489,
	// US-1): the same key, the same Go type, the same rule "present exactly for
	// a closed one" (INV-1). One type on the card and on the row — so that
	// the consumer does not have to know which surface answered.
	//
	// In MCP rows the field is "selectable, not default": the default set
	// does not contain it, and it arrives only on an explicit `fields`
	// (`mcp/tools/issuerow.go`, the classification is in its guard test).
	// The task column and the board do not show the field — it is on the wire, and
	// it has two surfaces: the card and the rows of the epic section.
	CloseOutcome *string   `json:"closeOutcome,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
	CreatedAt    time.Time `json:"createdAt"`

	// RowSeq is the row freshness order (sk-622.21, US-15, decision K16):
	// the value of the issues row's freshness sequence after the last
	// committed change of ANY of its columns — including writes that do not
	// bump Version (label add/remove, component replace). For one issue a
	// greater RowSeq is always a later committed state; values of different
	// issues are not comparable in any useful way. It is the same order the
	// phase-2 event row ([NeutralRow.RowSeq]) carries, so a client compares
	// the two directly — see [NeutralRow] for the substitution rule.
	//
	// Populated on every REST list, tree and card read since migration
	// 000064 (T026): the column `issues.row_seq` is NOT NULL and a trigger
	// assigns the next value of the global sequence on every INSERT and
	// UPDATE of the issue row, after the row lock is taken — so the order of
	// RowSeq for one issue is the order of its commits. Writes that touch
	// only side tables (labels, components, the child's parent-child edge —
	// dependencies/store.go#touchChildRow) also UPDATE the issue row and
	// therefore move it. Every REST read takes a row's labels and component
	// ids in the SAME statement as its RowSeq (store.go#rowComponentsLabelsCols,
	// decision R26 item 2), so a row is one snapshot: its fields are exactly
	// the state its RowSeq names, never newer or older.
	//
	// Zero means "order unknown", and on the wire it is the ABSENT key
	// (`omitempty`, deliberately: absent ≡ 0, a client reads a missing
	// `rowSeq` as 0). Two sources of zero:
	//   - the issue row has not been written since the phase-2 rollout:
	//     migration 000064 adds the column without a backfill (decision R26,
	//     no table rewrite), so untouched rows read 0 until their next write;
	//   - a carrier that does not read the column (a redacted `<hidden>`
	//     tree stub, the Ready surface).
	// Any write after the rollout gives the row a positive value, and an
	// event row is read from the issue's current state, so an event row
	// with a positive RowSeq is always fresher than a shown zero; an event
	// older than a REST row showing zero cannot exist. An event row with
	// zero carries the same state as a shown zero row. The value fits a
	// JSON number exactly (a global sequence stays far below 2^53).
	RowSeq int64 `json:"rowSeq,omitempty"`

	// IAssigned is true when the caller created the issue and assigned it
	// to someone else. Used by the UI to highlight "tasks I delegated" in
	// default mode. Computed in ListIssues based on the caller's user id
	// (passed via ListIssuesArgs.Caller); absent (false) for unauthenticated
	// callers and for issues the caller is also the assignee of (no point
	// highlighting your own work). Phase 2 ui-redesign — B5.
	IAssigned bool `json:"iAssigned,omitempty"`

	// IsBlocked is the derived "this issue is blocked" flag (sk-31), for
	// the reader (sk-423: only blockers inside the reader's horizon count) —
	// EXISTS over the dep graph at SELECT time (isBlockedExprFor). List
	// endpoints surface it on every row so cards can render the [blocked]
	// chip without a per-row Get round-trip. The detail's
	// IssueDetail.IsBlocked is no longer this expression: since sk-421.1 it
	// is derived from the detail's own blockers list (non-empty ⇔ blocked),
	// over the same set of blocking statuses.
	IsBlocked bool `json:"isBlocked"`

	// EpicProgress carries the cheap (closed,total) child-count pair on
	// epic rows so the task list can render inline progress without a
	// per-row IssueDetail fetch. nil/omitted on every non-epic row.
	// Slice 2 of sk-32 — children sourced from the dependencies graph
	// (parent-child edges). sk-35 restored this as canonical.
	EpicProgress *EpicProgress `json:"epicProgress,omitempty"`
}

// NeutralRow is the reader-independent list row of an issue that a phase-2
// issue event carries in its envelope field `row` (sk-622.21, US-15,
// decisions K16 / K17, INV-14). One event goes to many readers with
// different visibility horizons, so the row holds only what is the same
// for every reader: it is [IssueSummary] WITHOUT the reader-dependent
// fields `iAssigned`, `isBlocked` and `epicProgress`, plus [NeutralRow.RowSeq].
// The field set is an explicit allow-list on purpose: a reader-dependent
// field added to IssueSummary later must not reach the wire through this
// type by accident.
//
// Every field has the same JSON name, Go type and meaning as on
// IssueSummary, with one exception: `parentIdCurrent` is present only when
// the parent is in the event's project; otherwise the key is absent and the
// client keeps the parent it already shows (INV-17).
//
// The row is the issue's full state after the commit that the event's
// dispatch tick observed — not the state at the moment the event row was
// written — so it is never older than the event.
//
// Freshness rule for the receiver: substitute the row into a shown list
// row or open card ONLY if its RowSeq is greater than the RowSeq already
// shown for that issue. A row that is not fresher changes nothing and
// causes no request on its own account; the event's other reactions still
// apply. Because the row is a full state, skipped intermediate events do
// not matter. Reader fields are computed by the client: `iAssigned` by the
// server rule `createdBy == me && assigneeUserId != null &&
// assigneeUserId != me`; `isBlocked` and `epicProgress` are kept as shown
// and re-read when an event can change them.
//
// Only the type and its documentation live here; the row is built by the
// outbox dispatcher (T027) from the issue's current state.
type NeutralRow struct {
	ID        string    `json:"id"`
	ProjectID uuid.UUID `json:"projectId"`
	// ParentID — see the type comment: absent when the parent is outside
	// the event's project.
	ParentID          *string    `json:"parentIdCurrent,omitempty"`
	Version           int        `json:"version"`
	Title             string     `json:"title"`
	Type              string     `json:"type"`
	Priority          int        `json:"priority"`
	Status            string     `json:"status"`
	Pinned            bool       `json:"pinned"`
	Assignee          *string    `json:"assignee,omitempty"`
	AssigneeUserID    *uuid.UUID `json:"assigneeUserId,omitempty"`
	CreatedBy         uuid.UUID  `json:"createdBy"`
	CreatedByUsername *string    `json:"createdByUsername,omitempty"`
	// AssigneeStatus / CreatedByStatus — the person marks of the assignee
	// and the author in the issue's space (sk-674.18 T027,
	// spaces/contract.go): departed, or deleted with the pseudonym. Absent
	// when there is nothing to mark, or when the assignee is free text.
	AssigneeStatus  *person.Status `json:"assigneeStatus,omitempty"`
	CreatedByStatus *person.Status `json:"createdByStatus,omitempty"`
	// SpaceID is the short id of the space of the issue's project
	// (sk-674.18 T032), not of its id prefix.
	SpaceID      string      `json:"spaceId,omitempty"`
	ComponentIDs []uuid.UUID `json:"componentIds"`
	Labels       []string    `json:"labels"`
	AcceptedAt   *time.Time  `json:"acceptedAt,omitempty"`
	CloseOutcome *string     `json:"closeOutcome,omitempty"`
	UpdatedAt    time.Time   `json:"updatedAt"`
	CreatedAt    time.Time   `json:"createdAt"`
	// RowSeq is the row freshness order; same meaning as
	// [IssueSummary.RowSeq]. The key is always present on an event row
	// (no `omitempty`); 0 means the issue row has not been written since
	// the phase-2 rollout (migration 000064), order unknown — the row then
	// carries the same state as any REST row showing 0, and the strict
	// "greater" rule leaves the shown row as is.
	RowSeq int64 `json:"rowSeq"`
}

// ListResult is the paginated wrapper for GET /projects/{id}/issues and
// the cross-project GET /issues endpoints. Keyset pagination by (sort
// column, id). Total carries the exact caller-visible count for the
// same WHERE clause as the row query (sans cursor) — populated by the
// cross-project list path (Sc3 "All projects" Tasks column) so the UI
// can render an absolute "X tasks visible" counter without a second
// round-trip; left as zero by the project-scoped + caller-scoped paths
// that historically don't surface a total.
type ListResult struct {
	Items      []IssueSummary `json:"items"`
	Total      int            `json:"total"`
	NextCursor *string        `json:"nextCursor,omitempty"`
	HasMore    bool           `json:"hasMore"`
}
