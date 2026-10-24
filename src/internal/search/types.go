// Package search implements the global hybrid search stack for Skein.
//
// Coverage:
//   - Full-text search (Postgres tsvector, dual russian + english) on the
//     four searchable entities: issues, comments, projects, components.
//   - Vector embeddings (qwen3-embedding:0.6b via Ollama, 1024-dim halfvec)
//     produced by a background backfill worker that catches up on rows whose
//     `embedding IS NULL`.
//   - Hybrid ranking via Reciprocal Rank Fusion (RRF) — vector and FTS
//     candidate lists are independently top-K'd then merged by reciprocal
//     rank.  RRF is robust to score-scale skew between the two retrievers.
//   - Cross-entity merge: the top-K of every kind is merged on a single
//     score axis and trimmed to the page size.
//   - ID-prefix fast-path: when the query looks like a typed-out issue/
//     project ID (e.g. `demo-42` or `demo-42.1.3`) we hit the row directly
//     and surface it with `exactMatch: true` regardless of RRF score.
//
// Permission filtering happens in SQL, not in the Go-side merge: every
// query joins to an `allowed_projects` CTE that resolves to either "all
// projects" (caller has role=admin) or `project_members.user_id = $caller`.
// This keeps the cap on candidate rows tight (LIMIT 50 each) while still
// guaranteeing no project a non-member can't see ever leaks into results.
//
// Graceful degrade: if Ollama is unreachable, the query embedding step
// returns an error and we fall back to FTS-only candidate lists in each
// CTE.  The response includes `degraded: true` so clients can surface a
// "vector ranking unavailable" hint.  We never return 5xx for an Ollama
// outage — a partial result is better than nothing.
package search

import (
	"time"

	"github.com/google/uuid"
)

// Kind identifies the entity type of a search result. Stable JSON values
// because they appear in the wire response and in the `kind` query
// parameter; renaming would be a breaking change.
type Kind string

const (
	KindIssue     Kind = "issue"
	KindComment   Kind = "comment"
	KindProject   Kind = "project"
	KindComponent Kind = "component"
)

// AllKinds is the canonical ordering used when no `kind` filter is set —
// it determines the round-robin priority for the backfill worker as well.
// Issues are first because they're the most-searched entity in practice.
var AllKinds = []Kind{KindIssue, KindComment, KindProject, KindComponent}

// IsValidKind reports whether s is one of the four canonical kinds.
func IsValidKind(s string) bool {
	switch Kind(s) {
	case KindIssue, KindComment, KindProject, KindComponent:
		return true
	}
	return false
}

// Match tiers for result ordering. Higher value ranks first. The id
// fast-path produces tierCode (the caller typed a full entity id that
// matched a row exactly) or tierNumber (the caller typed a bare number
// that matched the trailing `-N` / `.N` segment of an id); everything
// arriving via the RRF hybrid path stays at tierFuzzy (the zero value,
// so a freshly-scanned RRF Item needs no explicit assignment). Code
// beats number beats fuzzy — a number that also appears inside a task
// title still surfaces via RRF, landing below the exact id rows.
const (
	tierFuzzy  = 0
	tierNumber = 1
	tierCode   = 2
)

// Item is one row in the search response. The shape is intentionally flat
// (no per-kind sub-objects) so the BFF / frontend can render a single list
// without a switch on Kind for every common field.
//
// Per-kind extras live in optional pointer fields to keep the JSON small.
// `omitempty` + pointer means absent fields drop out of the JSON for kinds
// that don't carry them (e.g. `archived` is only emitted on KindProject).
type Item struct {
	Kind        Kind    `json:"kind"`
	ID          string  `json:"id"`
	ProjectID   *string `json:"projectId,omitempty"`
	ProjectName *string `json:"projectName,omitempty"`
	// SpaceID is the short id of the space of the hit's project (sk-674.18
	// T031) — with ProjectName the client renders «Space · Project».
	SpaceID    string    `json:"spaceId,omitempty"`
	Title      string    `json:"title"`
	Snippet    string    `json:"snippet"`
	Score      float64   `json:"score"`
	ExactMatch bool      `json:"exactMatch"`
	UpdatedAt  time.Time `json:"updatedAt"`

	// matchTier drives the three-tier result ordering in the service
	// merge (see tierCode / tierNumber / tierFuzzy). It is an internal
	// ranking signal, NOT part of the wire contract — the frontend keys
	// off ExactMatch (true for both id-code and id-number matches). A
	// row that comes back from the RRF path leaves this at the zero value
	// (tierFuzzy); the id fast-path stamps tierCode or tierNumber.
	matchTier int `json:"-"`

	// Issue-specific. Status, Priority, Type are always set on issue rows;
	// Pinned defaults to false in the column; Assignee is nullable so it
	// stays *string and is omitted when the issue has no assignee.
	Status   *string `json:"status,omitempty"`
	Priority *int    `json:"priority,omitempty"`
	Type     *string `json:"type,omitempty"`
	Pinned   *bool   `json:"pinned,omitempty"`
	Assignee *string `json:"assignee,omitempty"`

	// Comment-specific. ParentIssueID links the comment back to its issue
	// (replaces the legacy `issueId` field — frontend SearchItem.parentIssueId
	// is the contract).  AuthorUsername is the comment author's username,
	// resolved via JOIN users on comments.author_user_id; nullable because
	// system/agent comments may not have a user row.
	ParentIssueID  *string `json:"parentIssueId,omitempty"`
	AuthorUsername *string `json:"authorUsername,omitempty"`

	// Project-specific. True when the project's archived_at is non-NULL.
	Archived *bool `json:"archived,omitempty"`

	// Component-specific. True when the component's archived_at is non-NULL.
	// Named to disambiguate from the project-level `archived` in mixed
	// result lists on the client.
	ComponentArchived *bool `json:"componentArchived,omitempty"`
}

// Result is the wire response of GET /api/v1/search.
type Result struct {
	Items      []Item  `json:"items"`
	NextCursor *string `json:"nextCursor"`
	HasMore    bool    `json:"hasMore"`
	Degraded   bool    `json:"degraded"`
	TookMs     int64   `json:"tookMs"`
}

// Filters are the per-kind narrowing options. Statuses / Priorities /
// Types / Labels / Source are issue-only — they apply only to the issues
// query. CommentAuthorKinds is comment-only — it restricts the comment
// results by comments.author_kind and never touches issue results.
//
// Source narrows by issues.source (one of {human, agent_directed,
// agent_autonomous}). nil = no filter; validation lives at the handler
// boundary so the store can inline the value as a parameter.
//
// CommentAuthorKinds narrows comment results by comments.author_kind
// (any-of: {human, agent_directed, agent_autonomous}). Empty/nil = no
// restriction (all author kinds). Distinct from Source: Source filters
// issues, CommentAuthorKinds filters comments — the two are independent
// axes and the latter is applied inside both comment candidate CTEs
// before the per-source LIMIT so RRF ranks over the correctly-filtered
// pool. Validation lives at both input doors (REST + MCP).
type Filters struct {
	Statuses           []string
	Priorities         []int
	Types              []string
	Labels             []string
	Source             *string
	CommentAuthorKinds []string
}

// Params bundles the parsed query string into one struct so the service
// layer doesn't drown in arguments. CallerID is populated by the handler
// from the request context; visibility is the access relation's for it.
type Params struct {
	Query    string     // already trimmed, validated 1..500 chars
	Project  *uuid.UUID // optional scope; nil = all visible projects
	Space    string     // optional space short id (sk-674.18 T031); "" = every space
	Kinds    []Kind     // resolved set (default = AllKinds)
	Limit    int        // 1..100
	Cursor   string     // base64-encoded {cacheKey, offset}
	Filters  Filters    // issue-only filters
	CallerID uuid.UUID

	// visibility is the fingerprint of the caller's visible set at the
	// time of the request (Store.visibilityFingerprint), set by
	// Service.Search before the result cache is read. It partitions the
	// cache by effective visibility (INV-26): a pool filled while the
	// caller saw more never answers after its access narrowed.
	visibility string
}
