package issues

import (
	"time"

	"github.com/google/uuid"
)

// CreateIssueInput is the parsed and validated request body for POST
// /projects/{projectID}/issues. The handler builds it from createIssueRequest
// and hands it to Store.CreateIssue.
//
//   - ID — only honoured when the project is in custom mode. counter/hash
//     modes ignore this field (we still accept it on the wire so a client
//     that sends it gets a clear error rather than a silent drop).
//   - ParentID — when set, ID generation switches to hierarchical (parent.N)
//     regardless of the project's default_id_mode.
//   - ComponentIDs — replaces the issue_components junction. Validated to be
//     in the same project before INSERT.
//   - Labels — free-form. Inserted via ON CONFLICT DO NOTHING into
//     issue_labels.
type CreateIssueInput struct {
	ID                 string
	ParentID           *string
	Title              string
	Description        string
	Type               string
	Priority           int
	Source             string
	SourceContext      *string
	Assignee           *string
	Design             string
	AcceptanceCriteria string
	Notes              string
	EstimatedMinutes   *int
	DueAt              *time.Time
	DeferUntil         *time.Time
	ExternalRef        *string

	ComponentIDs []uuid.UUID
	// ComponentIDsExplicit distinguishes "caller omitted componentIds"
	// (false — zero value; fall through to parent-inheritance when a
	// parentId is set) from "caller passed an explicit list, possibly
	// empty" (true — honour the value as-is, no inheritance). Without
	// this flag both `nil` and `[]uuid.UUID{}` look identical to the
	// store, and the natural reading of "explicit empty array means
	// no component" loses to the silent inheritance path. The handler /
	// MCP tool layer sets this from the wire-shape signal (REST: JSON
	// key present; MCP: `componentIds` argument present, even if empty).
	ComponentIDsExplicit bool
	Labels               []string

	// Standing (sk-118 F2i) creates the issue directly in the perpetual
	// `standing` state instead of the default `open`. A standing task is a
	// long-lived obligation acted on via spawned children (parent.N), never
	// claimed/worked in place — so on create it gets status='standing', NO
	// started_at, NO auto-accept, and NO lock/owner, regardless of whether an
	// assignee was supplied. Nesting standing under standing is forbidden
	// (§11.4): when Standing is true AND the resolved parent is itself
	// standing, CreateIssue rejects with ErrNestedStandingForbidden before any
	// insert. Default (false) is byte-identical to the legacy open create.
	Standing bool

	// Dependencies are typed edges to create atomically alongside the
	// new issue (Phase 1 inline-deps for skein_create / POST /issues).
	// Each entry maps to one dependencies row with from_id = the
	// newly-allocated issue id, to_id / type / gate from the entry.
	// The caller (handler / MCP tool layer) is responsible for the
	// 50-edge cap and for hierarchical-id shape validation on each
	// ToID — the store passes them straight to dependencies.CreateInTx
	// inside the same SERIALIZABLE tx as the issue insert.
	//
	// DependenciesCaller carries the user/role used for cross-project
	// visibility checks on each edge's TO-side. Required when
	// len(Dependencies) > 0; ignored otherwise.
	Dependencies       []DependencyEdgeInput
	DependenciesCaller *DependenciesCaller

	// ReviewRequired — sk-303.14, T009. The "review required"
	// flag for a CREATED issue.
	//
	// nil — the flag was not passed: the author's personal default
	// (user_notification_prefs.review_required_default) applies, and when
	// there is no prefs row — factory review.DefaultReviewRequired. A
	// pointer, not a bool, exists exactly for this distinction: "did not
	// pass" and "passed false" are different intents, and the zero value
	// of bool would glue them together, silently cancelling the personal
	// default on every path that does not fill the field.
	//
	// Explicit passing is supported only by the agent create path
	// (Q3/A3); other paths leave nil and get the author's default. This
	// is NOT a violation of "every create path yields one result": the
	// result is one WHEN SILENT, and explicit passing exists where the
	// path supports it.
	ReviewRequired *bool

	// AssignCreatorByDefault — sk-565 (P1, D1). When true and the issue
	// gets no explicit non-empty Assignee, the creator's personal setting
	// user_notification_prefs.assign_agent_issues_to_me decides whether the
	// creator becomes the assignee (standing issues and issues under
	// someone else's parent excepted, US-6).
	//
	// The zero value means «the default is off»: only the MCP create paths
	// (skein_create, skein_epic_create) set it, when `unassigned` is not
	// true. REST create never sets it, so it neither reads the setting nor
	// assigns anyone (INV-4) — by construction, not by inspecting Source.
	AssignCreatorByDefault bool
}

// DependencyEdgeInput is the per-edge shape accepted by CreateIssueInput.
// Mirrors dependencies.CreateDepInput one-for-one (we duplicate the type
// here so the issues package doesn't leak the dependencies package
// surface to handler-layer code — keeps the dependency direction one-way:
// callers build CreateIssueInput, the store converts to
// dependencies.CreateDepInput internally).
type DependencyEdgeInput struct {
	ToID string
	Type string
	Gate *string
}

// DependenciesCaller is the slim access info needed for cross-project
// visibility checks on each dep edge. Mirrors
// dependencies.CallerAccess; duplicated to avoid leaking that package
// across the issues<->handler/tool boundary.
type DependenciesCaller struct {
	UserID uuid.UUID
	Role   string
}

// PatchIssueInput captures the partial-update shape for PATCH
// /issues/{id}. Pointer fields distinguish "leave alone" (nil) from
// "set to value" (including "" and 0). ExpectedVersion is required and
// enforces optimistic concurrency at the SQL level.
//
// R2.3 added Force / SessionID for lock-aware updates:
//   - SessionID is the caller's Mcp-Session-Id (empty for REST). The store
//     uses it to enforce the "self holds lock" predicate inside the same
//     transaction.
//   - Force=true (admin only at the tool layer; the store assumes the
//     caller has already authorised it) skips the lock + version
//     predicate and writes reason="force-overridden" on the audit event.
//
// Existing R2.2 callers that pass zero values (Force=false, SessionID="")
// continue to work — the store falls back to the legacy version-only
// predicate when SessionID is empty.
type PatchIssueInput struct {
	ExpectedVersion int

	Title              *string
	Description        *string
	Type               *string
	Priority           *int
	Assignee           *string
	Design             *string
	AcceptanceCriteria *string
	Notes              *string
	EstimatedMinutes   *int
	DueAt              *time.Time
	DeferUntil         *time.Time
	ExternalRef        *string
	SourceContext      *string

	Force     bool
	SessionID string

	// CloseOutcome — an edit of the outcome of an ALREADY CLOSED issue (sk-489, US-3,
	// US-12) without reopening. nil — the outcome is not edited. On an unclosed issue
	// the outcome edit is rejected with the code close_outcome_requires_closed:
	// it has no outcome and cannot have one (INV-1).
	//
	// An outcome change is NOT part of the hybrid issue.updated diff: it has its own
	// event issue.outcome_changed and its own notification reason (R-7), and
	// were it in the general list of edits, the author would get two notifications
	// for one action.
	CloseOutcome *string
	// CloseTarget / DependenciesCaller — the same as on the close paths:
	// a target is allowed only together with an EXPLICIT CloseOutcome duplicate /
	// superseded, and the link is created in the same transaction (US-2, R-5).
	// Passing the same outcome again with a target is not an outcome change, but the
	// link is created by the rules of US-2 (US-3).
	CloseTarget        string
	DependenciesCaller *DependenciesCaller
}

// UpdateStatusInput is the body for POST /issues/{id}/transitions. Status
// is required; Reason is optional and lands in events.reason and (for
// closed transitions) issues.closed_reason.
//
// R2.3 added SessionID for lock-aware status transitions; Force already
// existed for "close pinned issues" and now ALSO bypasses the lock check.
// The store routes status='closed' to Close() — UpdateStatus refuses
// status='closed' so the close path is never side-stepped.
type UpdateStatusInput struct {
	ExpectedVersion int
	Status          string
	Reason          *string
	Force           bool // bypasses pinned + lock + version predicate (admin only at the tool layer)

	SessionID string

	// CloseOutcome / CloseTarget / DependenciesCaller — the same as on
	// CloseInput, with the same meaning: "all close paths share one
	// semantics", and two writers would mean two carriers of one input.
	// Passing an outcome with a status other than "closed" is a rejection
	// close_outcome_not_a_close: the very combination of request fields
	// is impossible, so the error class is shape (400), not state. The code
	// for an outcome edit on an unclosed issue (close_outcome_requires_closed,
	// PatchIssueInput above) is different and answers 422.
	CloseOutcome       *string
	CloseTarget        string
	DependenciesCaller *DependenciesCaller

	// Archive marks a close by ARCHIVING (sk-540.4; see
	// CloseOutcomeRequest.Archive for the rule). An unclosed non-standing
	// issue then needs an explicit CloseOutcome — without one this row is
	// refused with close_outcome_required inside the transaction, which is
	// what catches a row reopened after the REST batch preflight. An already
	// closed or standing issue ignores CloseOutcome and CloseTarget: the
	// closed one keeps its outcome, the standing one gets done, no edge is
	// created.
	//
	// The field exists only here and only because archiving goes through
	// this writer (handler/bulk.go#runOne, the close/archive branch). On
	// CloseInput there is NO such field — this is a recorded "no link", not a
	// forgotten symmetry: agent archive paths do not exist.
	Archive bool
}

// ClaimInput is the request shape for Store.Claim. Caller's session id is
// required (Claim's whole point is to bind a session to the issue's
// locked_by). OptionalAssignee, when non-nil, overrides the issue's
// assignee — used by the "admin claims on behalf of <user>" path.
type ClaimInput struct {
	ID               string
	SessionID        string
	OptionalAssignee *string
	Actor            Actor
}

// ClaimManyInput is the atomic batch variant of ClaimInput. IDs are returned
// in caller order; the store acquires rows in deterministic ID order.
type ClaimManyInput struct {
	IDs              []string
	SessionID        string
	OptionalAssignee *string
	Actor            Actor
}

// UnclaimInput is the request shape for Store.Unclaim. Force=true bypasses
// the lock-holder predicate (admin only at the tool layer); the store
// emits reason="force-overridden" on the audit event in that case.
//
// sk-19: NextStatus + Reason are required for any real unclaim. NextStatus
// is one of "open" | "in_progress" | "deferred" — the caller declares
// intent: handoff (stays in_progress), give-up (→open), or punt (→deferred).
// "closed" is rejected — closing has its own tool (skein_close) that also
// carries a reason and commits[]. Reason is non-empty after trim, lands on
// both the issue.unclaimed and (when status actually changed) the
// issue.status_changed event.
type UnclaimInput struct {
	ID         string
	SessionID  string
	Force      bool
	NextStatus string
	Reason     string
	Actor      Actor
}

// AcceptInput is the request shape for Store.Accept. Issued by a current
// assignee (or admin) to explicitly acknowledge a task — flips
// accepted_at / accepted_by, bumps version, writes one issue.accepted
// event, and fans out a notification to the creator + watchers.
//
// ExpectedVersion is optional: nil = skip the optimistic-concurrency
// check (caller doesn't care which exact version they accept; rare —
// agents typically supply one). Non-nil = enforce, mismatch returns
// ErrVersionConflict with the live version.
//
// Idempotency: when the issue is already accepted (accepted_at IS NOT
// NULL), Accept returns the current IssueDetail without writing — no
// new event row, no version bump. The caller cannot distinguish first-
// accept from re-accept by comparing the returned DTO; the only way to
// tell is by inspecting the events table.
type AcceptInput struct {
	ID              string
	ExpectedVersion *int // optional; nil = skip optimistic check
	Actor           Actor
}

// CloseInput is the request shape for Store.Close. Reason is required at
// the tool layer (skein_close enforces non-empty); the store accepts the
// empty string for the back-compat "skein_update(status=closed)" path
// where the legacy callers may not supply a reason. ExpectedVersion is
// required and enforces optimistic concurrency. Force=true (admin only at
// the tool layer) bypasses lock + version + pinned predicate.
//
// Commits, when non-empty, attaches commit-refs to the issue inside the
// same SERIALIZABLE transaction as the status flip. Idempotent — a
// re-close with the same (issue_id, repo_id, hash) tuple silently keeps
// the existing row (ON CONFLICT DO NOTHING).
type CloseInput struct {
	ID              string
	ExpectedVersion int
	Reason          string
	Force           bool
	SessionID       string
	Actor           Actor

	Commits []CloseCommitInput

	// CloseOutcome — the EXPLICITLY passed close outcome (sk-489, US-1). nil
	// means "the caller said nothing about the outcome": a transition gets "done",
	// a repeated close keeps the previous outcome (INV-10). It is a pointer exactly
	// for this distinction — see CloseOutcomeRequest.Requested.
	CloseOutcome *string
	// RequireOutcomeOnTransition marks the calling surface as one where an
	// explicit outcome is mandatory on a TRANSITION into closed (sk-540.1).
	// Only the MCP close paths set it (skein_close, skein_close_many,
	// skein_update / skein_update_many with status=closed); REST closes
	// through UpdateStatus and keeps the `done` default. Whether the call
	// is a transition is decided from the live status inside the close
	// transaction, so a re-close of an already closed issue without an
	// outcome still passes and keeps the previous outcome.
	RequireOutcomeOnTransition bool
	// CloseTarget — the target of a "duplicate" / "superseded" link (US-2). Allowed
	// only together with an EXPLICIT CloseOutcome and only for the values duplicate /
	// superseded; the link is created in the same transaction as the close (R-5).
	CloseTarget string
	// DependenciesCaller — the user's SYSTEM role (users.role), against
	// which the target project's visibility is checked. Required when
	// CloseTarget is non-empty. Precisely the system role, not Actor.Role (human / agent):
	// the visibility key is the same as for a manual link, otherwise an admin and a non-admin
	// with the same Actor.Role would get the same answer for an invisible
	// target. Modelled on CreateIssueInput.DependenciesCaller.
	DependenciesCaller *DependenciesCaller
}

// AttachCommitsInput is the request shape for Store.AttachCommits.
// Pure attach — no status flip, no version bump, no closed_reason
// touch. Issue may be in any status (the operation is valid on closed
// issues too — that's the headline use case, attaching commits the
// caller forgot to pass to skein_close). ID must be the full
// hierarchical id; Commits must be non-empty and within
// MaxCommitsPerClose. Force=true (admin only at the tool layer)
// bypasses the lock predicate; non-admin callers must hold the lock
// OR the issue must be unlocked.
//
// Idempotency: the underlying issue_commits unique constraint
// (issue_id, repo_id, hash) plus ON CONFLICT DO NOTHING means
// re-attaching the same hash is a silent no-op at the row level.
// One issue.commits_attached event is still written so the audit
// trail captures the agent's intent (and the response carries the
// full CommitRefs[] snapshot regardless).
type AttachCommitsInput struct {
	ID        string
	Commits   []CloseCommitInput
	SessionID string
	Actor     Actor
	Force     bool
}

// DetachCommitsInput is the request shape for Store.DetachCommits —
// the removal counterpart of AttachCommitsInput, and the only route
// back from a commit ref that should never have been written.
//
// Hashes, not (repo, hash) pairs, and that asymmetry with attach is
// deliberate. Attach MUST resolve a repo because it inserts a row, and
// resolution can fail with repo_ambiguous when the project lists
// several repos and the caller named none. That failure is one of the
// ways a wrong row gets written in the first place (the caller retries
// with a guessed repoUrl), so a recovery path that could itself fail
// with repo_ambiguous would not be a recovery path. Detach therefore
// removes every row of THIS issue carrying the named hash, whichever
// repo it was filed under, and the response names each removed row's
// repo so nothing disappears silently.
//
// Force=true (admin only at the tool layer) bypasses the lock
// predicate; non-admin callers must hold the lock OR the issue must be
// unlocked — the same guard AttachCommits uses.
type DetachCommitsInput struct {
	ID        string
	Hashes    []string
	SessionID string
	Actor     Actor
	Force     bool
}

// DetachedCommitRef is one row DetachCommits actually deleted. RepoID
// is carried because the caller never named it: detach matches on hash
// alone, so the repo is information the caller gains from the answer
// rather than supplies with the request.
type DetachedCommitRef struct {
	RepoID uuid.UUID
	Hash   string
}

// DetachCommitsResult is what DetachCommits answers with: the issue's
// post-state plus a per-hash verdict.
//
// The split is the point of the whole operation. «The call succeeded»
// says nothing about whether the bogus ref is gone — a typo in the hash
// would produce exactly the same success while the bad row stayed
// linked. Detached lists the rows that really left; NotFound lists the
// hashes that had no row on this issue, in the caller's input order.
type DetachCommitsResult struct {
	Issue    *IssueDetail
	Detached []DetachedCommitRef
	NotFound []string
}

// CloseCommitInput is one entry in CloseInput.Commits. Hash is required
// and validated as lowercase hex of length 7..64 at the store boundary;
// the rest are advisory — repo resolution falls back to RepoURL when
// RepoID is nil, then to "exactly one repo in the project" when both
// are absent (see projects.ResolveRepoForCommit).
type CloseCommitInput struct {
	Hash        string     `json:"hash"`
	RepoURL     string     `json:"repoUrl,omitempty"`
	RepoID      *uuid.UUID `json:"repoId,omitempty"`
	URL         *string    `json:"url,omitempty"`
	Message     *string    `json:"message,omitempty"`
	Author      *string    `json:"author,omitempty"`
	CommittedAt *time.Time `json:"committedAt,omitempty"`
}

// CommitRef is one row from issue_commits enriched with the project's
// repo URL (joined via projects.git_repos[]). Returned in
// IssueDetail.CommitRefs and emitted on every close that attaches
// commits.
//
// LinkedBy carries the actor's username (resolved via LEFT JOIN users
// on linked_by). nil for system-initiated links.
type CommitRef struct {
	Hash         string     `json:"hash"`
	RepoID       uuid.UUID  `json:"repoId"`
	RepoURL      string     `json:"repoUrl,omitempty"`
	URL          *string    `json:"url,omitempty"`
	Message      *string    `json:"message,omitempty"`
	Author       *string    `json:"author,omitempty"`
	CommittedAt  *time.Time `json:"committedAt,omitempty"`
	LinkedAt     time.Time  `json:"linkedAt"`
	LinkedBy     *string    `json:"linkedBy,omitempty"`
	LinkedByKind string     `json:"linkedByKind"`
}

// LockOpts is the optional last argument on ReplaceComponents / AddLabel /
// RemoveLabel for lock-aware updates. Zero value (Force=false,
// SessionID="") preserves the legacy R2.2 behaviour — REST callers and
// any code that doesn't care about locks pass `LockOpts{}` (or a zero-
// initialised struct). The MCP write tools build a populated LockOpts
// from the caller's session id and admin force flag.
type LockOpts struct {
	Force     bool
	SessionID string
}
