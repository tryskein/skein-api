package projects

import (
	"time"

	"github.com/google/uuid"
	"skein-api/internal/person"
)

// Repo is one entry in projects.git_repos[]. All fields use camelCase JSON
// tags because that's what we serialize to clients; the JSONB stored in
// the DB also uses camelCase keys (we control both writers and readers, so
// staying consistent across wire and storage avoids mapping noise).
type Repo struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	URL           string    `json:"url"`
	DefaultBranch string    `json:"defaultBranch,omitempty"`
}

// ComponentPath is one entry in components.paths[]. repo_id refers to a
// Repo.ID inside the same project's git_repos.
type ComponentPath struct {
	RepoID uuid.UUID `json:"repoId"`
	Glob   string    `json:"glob"`
}

// ProjectStats is the per-status / caller-scoped breakdown attached to
// ProjectDTO for the project tooltip / detail view. All counts ignore
// the "issue archived" notion (issues have no archived_at column —
// `status = 'closed'` is the terminal state) and run against the
// project_id alone.
//
// Caller-scoped fields (MyAssigned / MyCreated) require the request
// actor's user id; stores fill them only when the caller is known.
//
// LastActivity is the max(updated_at) across all issues in the project
// (closed or otherwise) so the tooltip can render "last activity N
// hours ago". nil when the project has no issues at all.
type ProjectStats struct {
	// Open / InProgress / Blocked are disjoint; «blocked» counts only
	// blockers in projects the caller can see (sk-423, the same rule as
	// ProjectListItem.BlockedCount).
	Open       int `json:"open"`
	InProgress int `json:"inProgress"`
	Blocked    int `json:"blocked"`
	Deferred   int `json:"deferred"`
	Closed     int `json:"closed"`
	// Standing is the count of perpetual-obligation issues (status='standing')
	// in the project. It is surfaced as its OWN aggregate (sk-118.8,
	// decision #4): standing issues are EXCLUDED from Open / InProgress /
	// Blocked / MyAssigned (which all route through status.ActiveWork) and
	// reported here instead, so the board can render a distinct "Standing"
	// pill rather than folding standing into open work.
	Standing int `json:"standingCount"`
	// Done / NoWork split Closed by the CLOSE OUTCOME (sk-494, on the rule
	// sk-489 normalised): "closed" does not equal "done". Done counts closed
	// issues whose outcome is `done`; NoWork counts the other three
	// outcomes — "won't do", "duplicate", "superseded" — the ones the
	// spec calls "closed without work".
	//
	// ⚠ Closed KEEPS ITS MEANING — it is still every closed issue, at any
	// outcome (sk-489 INV-3, and the spec's "all breakdowns BY STATUS
	// stay unchanged": this block is a status breakdown). The split
	// is ADDED beside it, so `Done + NoWork = Closed` is an identity that
	// holds on the wire at any data — that identity is what a reader uses
	// to tell «this api knows outcomes» from «it does not», and it is what
	// `close_outcome_stats_test.go#TestGetProjectWithStats_CloseOutcomeSplit`
	// asserts.
	//
	// Why NoWork is the leg SQL counts and Done is derived in Go: exactly
	// as in `issues/store.go#epicSummaryNoWorkExpr`, one correlated leg
	// cannot drift from Closed the way two independent spellings of
	// «visible» and «closed» could. Same predicate, same spelling:
	// `status = 'closed' AND close_outcome <> 'done'`.
	//
	// Both are project-wide, like every other count here except MyAssigned
	// / MyCreated — the issue's own outcome is not horizon-dependent, and
	// a project the caller can read they can read whole.
	Done   int `json:"done"`
	NoWork int `json:"noWork"`

	Total        int        `json:"total"`
	MyAssigned   int        `json:"myAssigned"`
	MyCreated    int        `json:"myCreated"`
	LastActivity *time.Time `json:"lastActivity,omitempty"`
}

// ProjectDTO is the wire shape returned by GET /projects/{id} and friends.
//
// Fields:
//   - createdBy: only the UUID, not the full user. Callers that need name
//     can resolve via /me/users (R5+).
//   - counts: cheap aggregates the UI needs to render headers without a
//     second round-trip.
//   - stats: per-status breakdown + caller-scoped my-load + last-activity
//     timestamp. Powers the project tooltip in the new shell.
type ProjectDTO struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	IDPrefix      string     `json:"idPrefix"`
	DefaultIDMode string     `json:"defaultIdMode"`
	Description   string     `json:"description"`
	GitRepos      []Repo     `json:"gitRepos"`
	GitlabGroup   *string    `json:"gitlabGroup,omitempty"`
	ArchivedAt    *time.Time `json:"archivedAt,omitempty"`
	CreatedBy     uuid.UUID  `json:"createdBy"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	// SpaceID is the short id of the project's space (sk-674.18, never
	// null since T024).
	SpaceID string `json:"spaceId"`
	// Visibility is "space" (every participant of the space sees it) or
	// "members" (only its members and the space owner).
	Visibility string `json:"visibility"`

	MembersCount    int `json:"membersCount"`
	ComponentsCount int `json:"componentsCount"`
	ReposCount      int `json:"reposCount"`

	Stats ProjectStats `json:"stats"`
}

// ProjectListItem is a leaner shape used by the list endpoint — drops
// gitlabGroup and the repo / member counts to keep paginated responses
// small. Issue counts (open / in-progress / blocked) and the active
// component count ride along because the new shell renders them on every
// row of the Projects column without triggering N round-trips.
type ProjectListItem struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	IDPrefix      string     `json:"idPrefix"`
	DefaultIDMode string     `json:"defaultIdMode"`
	Description   string     `json:"description"`
	ArchivedAt    *time.Time `json:"archivedAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	// SpaceID is the short id of the project's space (sk-674.18 T024).
	SpaceID string `json:"spaceId"`
	// Visibility is "space" or "members" (sk-674.18 T024).
	Visibility string `json:"visibility"`

	// OpenCount / InProgressCount / BlockedCount are aggregated in the
	// same SELECT as the row itself via LEFT JOIN issues + COUNT(*)
	// FILTER. Issues have no archived_at — `status = 'closed'` is the
	// terminal state, so the filters key off status alone.
	//
	// sk-423: «blocked» here — and in every *BlockedCount below, with the
	// open / in_progress counts as its disjoint complement — means blocked
	// by a blocks / waits-for target that gates readiness AND lives in a
	// project the CALLER can see (authz.ReaderBlockedExpr). An issue
	// blocked only from outside the caller's horizon counts as open /
	// in_progress: counting it as blocked would reveal whether that hidden
	// blocker is closed. Readiness (skein_ready) still counts every blocker.
	OpenCount       int `json:"openCount"`
	InProgressCount int `json:"inProgressCount"`
	BlockedCount    int `json:"blockedCount"`

	// StandingCount is the count of standing (perpetual-obligation) issues
	// on the project row. Surfaced as its OWN pill (sk-118.8, decision #4):
	// standing is EXCLUDED from OpenCount / InProgressCount / BlockedCount
	// (which route through status.ActiveWork) and reported here so the board
	// renders a distinct "Standing" pill rather than folding it into open.
	StandingCount int `json:"standingCount"`

	// MyOutbox{Open,InProgress,Blocked}Count are the same per-status
	// aggregates scoped to the caller's Delegated (outbox) lens (sk-34):
	//
	//	(created_by = caller
	//	  AND assignee IS NOT NULL AND LOWER(assignee) <> LOWER(callerUsername))
	//	OR (caller = ANY(previous_assignees)
	//	  AND NOT caller = ANY(dismissed_by)
	//	  AND assignee IS NOT NULL AND LOWER(assignee) <> LOWER(callerUsername))
	//
	// Powers the shell's `My-assigned mode` (outboxMode) header toggle so
	// every row count flips to the caller's delegated scope when ON.
	// Filled only when the caller identity is known (CallerID != uuid.Nil);
	// otherwise stays at zero.
	MyOutboxOpenCount       int `json:"myOutboxOpenCount"`
	MyOutboxInProgressCount int `json:"myOutboxInProgressCount"`
	MyOutboxBlockedCount    int `json:"myOutboxBlockedCount"`

	// MyOutboxStandingCount is the outbox-scoped standing aggregate — the
	// "Standing" pill under the `My-assigned mode` (outboxMode) toggle.
	// Same Delegated predicate as MyOutboxOpenCount, narrowed to
	// status='standing'. Honors the outboxMode invariant (CLAUDE.md).
	MyOutboxStandingCount int `json:"myOutboxStandingCount"`

	// OnMe{Open,InProgress,Blocked}Count are the "On me" lens
	// counterparts (sk-30): per-status aggregates narrowed to "issues
	// assigned to the caller by SOMEONE ELSE that the caller has already
	// accepted" — LOWER(assignee) = LOWER(callerUsername) AND
	// created_by <> caller AND accepted_at IS NOT NULL. Powers the
	// header "On me" toggle so every row count flips to the caller's
	// active-work scope when ON. Filled only when the caller identity
	// is known; collapses to zero otherwise.
	OnMeOpenCount       int `json:"onMeOpenCount"`
	OnMeInProgressCount int `json:"onMeInProgressCount"`
	OnMeBlockedCount    int `json:"onMeBlockedCount"`

	// EpicsEligibleCount is the number of epic-typed, non-closed issues
	// in this project whose child set is non-empty AND every child is
	// closed — i.e. epics the UI should surface as "ready to close"
	// (sk-32). Empty epics never qualify. Children sourced from the
	// dependencies graph (parent-child edges). sk-35 restored this as
	// canonical. Computed in the same SELECT as the row-level status
	// counters so we don't pay a second round-trip.
	EpicsEligibleCount int `json:"epicsEligibleCount"`

	// ComponentsCount is the number of NON-archived components of the
	// project (sk-551.53) — the same figure GET /projects/{id} reports as
	// ProjectDTO.ComponentsCount; archived components are not counted. A
	// project with no active component reports 0. It is a property of the
	// project, not of the caller's issues, so it is identical under every
	// lens and toggle (no myOutbox / onMe variant). Lets the shell decide
	// whether a project row can expand into components without a per-row
	// request.
	ComponentsCount int `json:"componentsCount"`
}

// ListResult is the cursor-paginated wrapper returned by GET /projects.
//
// Total carries the exact caller-visible count for the same WHERE clause
// used to fetch Items (permission + archived + optional search). The new
// shell renders it next to the column header as `Projects (N)` so the
// user sees the universe their toggle / search is filtering over. Cost on
// a 13k-row projects table is ~2 ms (Seq Scan + hash semijoin against
// project_members) — verified with EXPLAIN ANALYZE before shipping. When
// the row count grows past ~100k we'll revisit and fall back to a
// `pg_class.reltuples` estimate with a caveat in the response, but for
// the realistic shape of a Skein deployment (teams of 10–100 with
// hundreds of projects each) exact COUNT(*) is honest and free.
type ListResult struct {
	Items      []ProjectListItem `json:"items"`
	Total      int               `json:"total"`
	NextCursor *string           `json:"nextCursor,omitempty"`
	HasMore    bool              `json:"hasMore"`
}

// AliasDTO is one row from project_aliases, with embedded "added by" info.
type AliasDTO struct {
	Alias       string    `json:"alias"`
	ProjectID   uuid.UUID `json:"projectId"`
	AddedBy     uuid.UUID `json:"addedBy"`
	AddedByKind string    `json:"addedByKind"`
	CreatedAt   time.Time `json:"createdAt"`
}

// MemberDTO is one row from project_members joined with users.
type MemberDTO struct {
	UserID   uuid.UUID `json:"userId"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	AddedAt  time.Time `json:"addedAt"`
	AddedBy  uuid.UUID `json:"addedBy"`
	// The person marks (sk-674.18 T027, spaces/contract.go): a deleted
	// account is marked deleted with its pseudonym; a member row is current,
	// so a member is never departed.
	person.Status
}

// ComponentDTO is the wire shape for components — mirror of the DB row
// plus a UTC-converted timestamp. Issue counts (open / in-progress /
// blocked) are aggregated in `ListComponents` so the new shell can
// render them on every component row without N+1 round-trips.
// Single-component reads (`GetComponent`) leave the counts at zero —
// only the list path computes them, on the assumption that detail
// callers don't render the same right-aligned counts.
type ComponentDTO struct {
	ID          uuid.UUID       `json:"id"`
	ProjectID   uuid.UUID       `json:"projectId"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Paths       []ComponentPath `json:"paths"`
	ArchivedAt  *time.Time      `json:"archivedAt,omitempty"`
	CreatedBy   uuid.UUID       `json:"createdBy"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`

	// OpenCount / InProgressCount / BlockedCount are populated by
	// ListComponents via a LEFT JOIN against issue_components + issues.
	// Get / Patch / Create paths leave them at zero (the caller doesn't
	// render right-aligned counts in those flows).
	//
	// sk-423: as on the project row, «blocked» here and in every
	// *BlockedCount below (open / in_progress being its disjoint
	// complement) counts only blockers in projects the CALLER can see.
	OpenCount       int `json:"openCount"`
	InProgressCount int `json:"inProgressCount"`
	BlockedCount    int `json:"blockedCount"`

	// StandingCount is the count of standing (perpetual-obligation) issues
	// attached to the component. Its OWN pill (sk-118.8, decision #4):
	// standing is EXCLUDED from OpenCount / InProgressCount / BlockedCount
	// (which route through status.ActiveWork) and reported here. Filled only
	// by ListComponents; Get / Patch / Create leave it zero.
	StandingCount int `json:"standingCount"`

	// MyOutbox{Open,InProgress,Blocked}Count narrow component issue
	// aggregates to "issues the caller created AND delegated to someone
	// else" — created_by = caller AND assignee IS NOT NULL AND
	// LOWER(assignee) <> LOWER(callerUsername).
	//
	// NOTE: the component path intentionally uses this narrower
	// created_by-only predicate (unlike ProjectListItem.MyOutbox*, which
	// includes the sk-34 previous_assignees / dismissed_by chain). Aligning
	// components to the full Delegated lens is a separate follow-up.
	// ListComponents fills these only when the caller is known; Get /
	// Patch / Create leave them at zero.
	MyOutboxOpenCount       int `json:"myOutboxOpenCount"`
	MyOutboxInProgressCount int `json:"myOutboxInProgressCount"`
	MyOutboxBlockedCount    int `json:"myOutboxBlockedCount"`

	// MyOutboxStandingCount is the outbox-scoped standing aggregate — the
	// "Standing" pill under outboxMode. Same created_by-only Delegated
	// predicate as MyOutboxOpenCount, narrowed to status='standing'.
	MyOutboxStandingCount int `json:"myOutboxStandingCount"`

	// OnMe{Open,InProgress,Blocked}Count are the "On me" lens
	// counterparts (sk-30): per-status aggregates narrowed to "issues
	// assigned to the caller by someone else that the caller has
	// accepted" — LOWER(assignee) = LOWER(callerUsername) AND
	// created_by <> caller AND accepted_at IS NOT NULL. Same fill rules
	// as MyOutbox*.
	OnMeOpenCount       int `json:"onMeOpenCount"`
	OnMeInProgressCount int `json:"onMeInProgressCount"`
	OnMeBlockedCount    int `json:"onMeBlockedCount"`
}

// ResolveMatch is a single resolved candidate (or, when wrapped in
// ResolveResult.Match, the auto-matched winner).
type ResolveMatch struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	IDPrefix   string    `json:"idPrefix"`
	Similarity float64   `json:"similarity"`
	MatchedVia string    `json:"matchedVia"` // 'id' | 'id_prefix' | 'alias' | 'name' | 'repo' | 'search'
	// SpaceID and SpaceName name the project's space (sk-674.18 T024): the
	// same-named projects of different spaces are told apart by them.
	SpaceID   string `json:"spaceId"`
	SpaceName string `json:"spaceName"`
}

// ResolveResult is the response shape for GET /projects/resolve. Match is
// non-nil only when the algorithm could pick a single winner with high
// confidence; otherwise candidates carries up to 10 alternatives.
type ResolveResult struct {
	Match      *ResolveMatch  `json:"match"`
	Candidates []ResolveMatch `json:"candidates"`
	// Ambiguous marks the final refusal of INV-29: the string is the exact
	// name of two or more accessible projects (and no id, prefix or alias).
	// Candidates are then exactly those projects; no later form (repo URL,
	// fuzzy) was tried. Callers answer spaces.CodeAmbiguousProject.
	Ambiguous bool `json:"-"`
}
