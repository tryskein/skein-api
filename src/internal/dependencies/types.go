// Package dependencies implements R1.4 — the issue-dependency graph.
//
// Six typed edges (db-schema §2.13, vision §3.Dependency / §6) wired into
// a single dependencies table with composite PK (from_id, to_id, type).
// Direction is canonical per type — see the table below — and never
// renegotiated by the store; callers either supply the correct direction
// or the cycle/visibility checks reject the row.
//
// Hierarchical children (epic→subtask) live on the `parent-child` typed
// edge: from = child, to = parent. sk-35 restored it as the canonical
// representation, beads-style — the `issues.parent_id` column is frozen
// (no new writes from the main code path) and will be dropped in a
// follow-up migration.
//
//	┌─────────────────┬──────────────────────────┬─────────────────────────────┐
//	│ type            │ from              │ to                                  │
//	├─────────────────┼──────────────────────────┼─────────────────────────────┤
//	│ blocks          │ blocked           │ blocker                             │
//	│ waits-for       │ waiter            │ awaited                             │
//	│ supersedes      │ newer             │ older                               │
//	│ duplicates      │ duplicate         │ original                            │
//	│ related         │ side A (symmetric)│ side B (symmetric)                  │
//	│ discovered-from │ discoverer        │ source-of-discovery                 │
//	│ parent-child    │ child             │ parent (canonical hierarchy)        │
//	└─────────────────┴──────────────────────────┴─────────────────────────────┘
//
// What this package is NOT: a status mutator. R1.4 NEVER writes
// issues.status, issues.version, or anything else on the target row when
// a dependency is created — even for `supersedes` / `duplicates`. The user
// closes the superseded/duplicate issue explicitly via POST /transitions
// (R1.3). The "auto-close" semantics described in vision §3 are explicitly
// deferred to a later release; F37a/F37b are storage-only here.
package dependencies

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ----------------- Re-parent / classification sentinel errors -----------------
//
// These typed errors are produced by the store layer (Create / Reparent)
// and consumed by the wiring pass (MCP tools + HTTP handlers), which maps
// each to a stable machine-readable code in the {"error":"<code>"} body.
// The string identities below are FROZEN — the wiring pass keys on them:
//
//	ErrParentAlreadySet         → "parent_already_set"
//	ErrSerializationRetry       → "serialization_retry"
//	ErrDependencyTargetNotReady → "dependency_target_not_visible_yet"
//	ErrReparentSelf             → "reparent_self"
//	ErrReparentCrossProject     → "reparent_cross_project"
//
// (ErrSerializationRetry / ErrDependencyTargetNotReady classify what used
// to surface as a generic internal_error on a transient INSERT failure —
// both are safe to retry.)

// ErrParentAlreadySet is returned by Create when a `parent-child` edge is
// requested for a from-issue that already has a parent-child edge pointing
// at a DIFFERENT parent. Re-parenting must go through Reparent (the
// skein_reparent tool), which atomically replaces the existing edge — a
// second Create would otherwise leave the child with two parent edges and
// the earliest (original) would keep winning. Code: "parent_already_set".
var ErrParentAlreadySet = errors.New("issue already has a parent — use skein_reparent to move it")

// ErrSerializationRetry is returned by Create when the INSERT loses a
// SERIALIZABLE race (SQLSTATE 40001). The operation made no durable
// change and is safe to retry verbatim. Code: "serialization_retry".
var ErrSerializationRetry = errors.New("dependency write lost a serialization race — safe to retry")

// ErrDependencyTargetNotReady is returned by Create when the INSERT trips
// the to_id foreign-key (SQLSTATE 23503): the target issue row was not
// visible inside this transaction's snapshot — typically because it was
// created microseconds earlier in a still-uncommitted concurrent tx. The
// caller should retry once the target has committed. Code:
// "dependency_target_not_visible_yet".
var ErrDependencyTargetNotReady = errors.New("dependency target not visible yet — retry shortly")

// ErrCloseOutcomeLinkRequired is returned by Delete when the edge being
// removed is the last one that justifies its closed issue's outcome
// (sk-540.8): the last outgoing `duplicates` edge of an issue closed as
// "duplicate", or the last incoming `supersedes` edge of an issue closed as
// "superseded". Nothing is written. The way out is to change the outcome or
// reopen the issue first. Code: "close_outcome_link_required".
//
// When the closed issue lies outside the caller's horizon, IssueID and
// CloseOutcome are empty and the message names neither (sk-423): the
// refusal still holds, it just says nothing about the hidden issue.
type ErrCloseOutcomeLinkRequired struct {
	// IssueID is the closed issue whose outcome needs the edge; empty when
	// the caller cannot see that issue.
	IssueID string
	// CloseOutcome is that issue's outcome: "duplicate" or "superseded";
	// empty when the caller cannot see that issue.
	CloseOutcome string
	// Type is the dependency type of the refused edge.
	Type string
}

// Hidden reports whether the closed issue is outside the caller's horizon,
// i.e. the refusal must not name it.
func (e *ErrCloseOutcomeLinkRequired) Hidden() bool {
	return e.IssueID == ""
}

func (e *ErrCloseOutcomeLinkRequired) Error() string {
	if e.Hidden() {
		return "this edge backs the close outcome of the linked issue and cannot be removed"
	}
	return fmt.Sprintf("%s is closed as %s and this is its last %s edge — change the close outcome or reopen the issue first",
		e.IssueID, e.CloseOutcome, e.Type)
}

// Details returns the machine-readable fields of the refusal for the REST
// and MCP error bodies: always the edge type, and the closed issue and its
// outcome only when the caller can see that issue.
func (e *ErrCloseOutcomeLinkRequired) Details() map[string]any {
	if e.Hidden() {
		return map[string]any{"type": e.Type}
	}
	return map[string]any{
		"issueId":      e.IssueID,
		"closeOutcome": e.CloseOutcome,
		"type":         e.Type,
	}
}

// ErrReparentSelf is returned by Reparent when newParentID == id. An issue
// cannot be its own parent. Code: "reparent_self".
var ErrReparentSelf = errors.New("an issue cannot be its own parent")

// ErrReparentCrossProject is returned by Reparent when newParentID lives
// in a different project than id. parent-child is an intra-project edge.
// Code: "reparent_cross_project".
var ErrReparentCrossProject = errors.New("new parent must be in the same project as the issue")

// statusStanding is the literal issues.status value for a standing task.
// dependencies cannot import internal/issues (issues imports dependencies —
// the dependency edge is one-way), so the const is duplicated here. It mirrors
// issues.StatusStanding 1:1.
const statusStanding = "standing"

// ErrNestedStandingForbidden is returned by CreateInTx (when inserting a
// `parent-child` edge) and Reparent (sk-118 F2e) when the requested move would
// place a standing task directly under another standing task — i.e. BOTH the
// would-be child AND the would-be parent are status='standing'. Nesting
// standing tasks reintroduces the §11.4 mutual-retire deadlock (the parent
// can't retire until the child closes; the child-standing can't close until
// ITS children close), so it is rejected at the edge-create / reparent
// boundary. It is a *ValidationError carrying the stable
// `nested_standing_forbidden` code (mirroring the issues-package F2c sentinel
// of the same code) so it flows through IsValidationError on both the MCP and
// REST boundaries; the wiring pass promotes the code to the top-level envelope
// `error`.
var ErrNestedStandingForbidden = newValidationError("nested_standing_forbidden",
	"cannot make a standing task the child of another standing task — "+
		"standing tasks must not be nested")

// ErrBlockerTargetStanding is returned by CreateInTx (sk-118 F2f) when a
// `blocks` or `waits-for` edge is requested whose TO-side (target) issue is
// status='standing'. A standing task is a perpetual obligation that is never
// "done", so a blocks/waits-for edge pointing AT it would gate the blocked
// issue's readiness FOREVER (the GatesReadiness predicate now excludes standing
// precisely so an already-existing such edge stops gating — but creating a fresh
// one is a user error we reject at the source). Only the two readiness-gating
// edge types are banned; `related`, `discovered-from`, `supersedes`,
// `duplicates`, `parent-child` may still target a standing issue. It is a
// *ValidationError carrying the stable `blocker_target_standing` code (mirroring
// the nested-standing sentinel) so it flows through IsValidationError on both
// the MCP and REST boundaries; the wiring pass promotes the code to the
// top-level envelope `error`.
var ErrBlockerTargetStanding = newValidationError("blocker_target_standing",
	"cannot point a blocks/waits-for edge at a standing task — "+
		"a standing task is never done, so it would gate the blocked issue forever")

// ErrVersionConflict surfaces optimistic-concurrency loss on Reparent: a
// concurrent writer bumped the issue's version after the caller read but
// before they wrote. CurrentVersion echoes the live value so the wiring
// pass can hand it back without an extra GET. Mirrors the issues package's
// type of the same name (we can't import that package — issues imports
// dependencies, so the dependency edge is one-way). Code: "version_conflict".
type ErrVersionConflict struct{ CurrentVersion int }

func (e *ErrVersionConflict) Error() string {
	return fmt.Sprintf("version conflict — current version is %d", e.CurrentVersion)
}

// Dep type constants — closed set mirroring the DB CHECK in 000005
// (tightened in 000020 to drop parent-child, re-opened in 000022 to
// restore it as the canonical hierarchy). Stored as the literal
// lowercase strings on the wire and in DB.
const (
	DepBlocks  = "blocks"
	DepRelated = "related"
	// DepParentChild is the canonical hierarchy edge (sk-35). from =
	// child, to = parent. Each issue has at most one parent in the tree
	// view but the table itself has no UNIQUE constraint on parent-child
	// — a child MAY have multiple parent edges; consumers (epic rollup,
	// breadcrumb, tree builder) pick the first by `created_at`.
	DepParentChild    = "parent-child"
	DepDiscoveredFrom = "discovered-from"
	DepSupersedes     = "supersedes"
	DepDuplicates     = "duplicates"
	DepWaitsFor       = "waits-for"
)

// Gate values for waits-for.metadata.gate. Other types must not carry a
// gate — validate.go enforces that. Optional on waits-for, and stored only:
// ListReady does not evaluate it (sk-483, see the note there).
const (
	GateAllChildren = "all-children"
	GateAnyChildren = "any-children"
)

// CreateDepInput is the parsed body of POST /issues/{from}/dependencies.
// FromID is taken from the URL by the handler and supplied to Store.Create
// alongside this struct.
type CreateDepInput struct {
	ToID string `json:"toId"`
	Type string `json:"type"`
	// Gate is allowed only on waits-for. Stored under metadata.gate.
	Gate *string `json:"gate,omitempty"`
}

// DependencyDTO is the wire shape returned by POST. List responses use
// DependencyEdge instead, which carries an embedded issue summary.
type DependencyDTO struct {
	FromID    string         `json:"fromId"`
	ToID      string         `json:"toId"`
	Type      string         `json:"type"`
	Metadata  map[string]any `json:"metadata"`
	Version   int            `json:"version"`
	CreatedBy uuid.UUID      `json:"createdBy"`
	CreatedAt time.Time      `json:"createdAt"`
}

// IssueRef is the slim summary embedded inside DependencyEdge so the UI
// can render outgoing/incoming lists without a per-row fetch. Mirrors the
// public-facing subset of issues.IssueSummary — kept as a separate struct
// to avoid the dependencies package importing issues (would create an
// import cycle).
type IssueRef struct {
	ID        string    `json:"id"`
	ProjectID uuid.UUID `json:"projectId"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Priority  int       `json:"priority"`
	Type      string    `json:"type"`
}

// DependencyEdge is one row in the outgoing/incoming list. Issue is
// nil-able: when the OTHER side of the edge belongs to a project the
// caller cannot see, we keep the edge visible (the user knows there IS a
// link) but blank out the issue payload via Redacted=true. This matches
// vision §10 "non-members must not learn project contents".
type DependencyEdge struct {
	FromID    string         `json:"fromId"`
	ToID      string         `json:"toId"`
	Type      string         `json:"type"`
	Metadata  map[string]any `json:"metadata"`
	Version   int            `json:"version"`
	CreatedAt time.Time      `json:"createdAt"`

	// Issue is the OTHER end of the edge from the request perspective —
	// for outgoing lists Issue == to-side; for incoming, Issue == from-side.
	// Always non-nil unless Redacted is true.
	Issue *IssueRef `json:"issue,omitempty"`
	// Redacted marks an edge whose other end the caller cannot see: no
	// Issue, and that end's id (ToID outgoing, FromID incoming) is a
	// synthetic "hidden-N" token, never the real id (sk-674.18 T009).
	Redacted bool `json:"redacted,omitempty"`
}

// DependencyList is the response body for GET /issues/{id}/dependencies.
// Fields are always non-nil so the UI doesn't special-case absence vs
// empty.
type DependencyList struct {
	Outgoing []DependencyEdge `json:"outgoing"`
	Incoming []DependencyEdge `json:"incoming"`
}

// ReadyFilters narrows the GET /projects/{id}/ready result set. Empty
// fields mean "no filter on this dimension".
type ReadyFilters struct {
	Assignee *string
	Priority *int    // upper bound — issues with priority <= filter
	Type     *string // exact match (bug | feature | task | …)
	// ComponentID restricts the result to issues that have this
	// component pinned via the issue_components junction. Nil = no
	// filter. Pushed into SQL as an EXISTS clause so callers don't need
	// to over-fetch and post-filter.
	ComponentID *uuid.UUID
}

// DuplicateCandidate is one row of POST /issues/check-duplicates.
//
// Similarity is the headline score the UI ranks/filters on:
//   - trigram-only path: pg_trgm `similarity(title, query)` (0..1)
//   - hybrid path (R4.1): max(trigram_sim, vector_sim) so the wire
//     contract stays the same — the strongest evidence wins.
//
// TrigramSimilarity and SemanticSimilarity carry the per-layer component
// scores, and MatchedBy names the layer(s) that produced them. All three
// keys are ALWAYS present on the wire (sk-228): a component score is
// `null` when that layer did not score this row, never absent. Before
// sk-228 `semanticSimilarity` was `omitempty`, so a caller could only
// guess which layer had spoken from which key happened to exist — and a
// trigram-only verdict was indistinguishable on the wire from a hybrid
// one whose two scores agreed. Provenance is now stated, not inferred.
//
// Reading the row:
//   - MatchedBy == "trigram"  → only pg_trgm scored it; SemanticSimilarity is nil.
//   - MatchedBy == "semantic" → only the vector layer scored it; TrigramSimilarity is nil.
//   - MatchedBy == "both"     → both scored it; Similarity is the max.
//
// A nil SemanticSimilarity on a "trigram" row means this row was not in
// the vector layer's top-K (or the vector layer did not run at all — the
// enclosing DuplicateReport.Degraded says which).
type DuplicateCandidate struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Status             string   `json:"status"`
	Type               string   `json:"type"`
	Similarity         float64  `json:"similarity"`
	TrigramSimilarity  *float64 `json:"trigramSimilarity"`
	SemanticSimilarity *float64 `json:"semanticSimilarity"`
	MatchedBy          string   `json:"matchedBy"`
}
