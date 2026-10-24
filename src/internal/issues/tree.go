package issues

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/assignee"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/idorder"
	"skein-api/internal/redact"
)

// ────────── sk-36 — layered tree view ──────────
//
// IssueTreeArgs / IssueTreeNode / Store.IssueTree power the new
// /projects/{pid}/issues/tree and /issues/tree endpoints. The walk
// recurses across the seven typed-deps edges (parent-child + the six
// non-hierarchical types) and emits a flat, pre-order-traversal list so
// the frontend can render the tree without re-deriving structure
// client-side. parent-child is the canonical hierarchy edge (sk-35);
// the other six are user-toggled lenses on top.
//
// Wire shape (per the sk-36 plan): flat list of rows, each row carries
// (item, depth, parentRef, isReference, firstSeenAt, edgeType). The
// frontend layers the connector glyphs / colours per edge type and
// detects cycles via the IsReference + FirstSeenAt pair (first
// occurrence rendered full; subsequent occurrences emitted as a
// reference row with no children).
//
// Cross-project hidden ancestors (decision Q1 in the plan): when the
// recursive walk hits an issue in a project the caller cannot see, the
// row is emitted with IsReference=true + a stub IssueSummary
// (Title="<hidden>") so the chain doesn't break. Visibility is the access
// relation's (authz) for every reader, an admin included.

// IssueTreeMaxDepth caps the recursive walk's depth — defensive guard
// against pathological dep graphs. 32 levels is far beyond any real
// tree we expect to see. It bounds the depth only, not the row count —
// that is MaxNodes, enforced inside the walk (see IssueTree, "Bounded
// walk").
const IssueTreeMaxDepth = 32

// IssueTreeDefaultMaxNodes is the soft cap on the total rows the walk
// emits before the store returns ErrTreeTooLarge. Per the sk-36 plan
// this matches the UI's "tree too large" inline-banner threshold so
// the wire response stays bounded and the renderer doesn't have to
// virtualise. Callers can override via IssueTreeArgs.MaxNodes.
const IssueTreeDefaultMaxNodes = 1000

// IssueTreeArgs is the parsed and validated input for Store.IssueTree.
// Visibility predicate mirrors ListAllIssues: the seed holds only issues of
// projects the access relation grants Scope.
//
// ProjectID, when non-nil, narrows the forest to a single project (the
// /projects/{pid}/issues/tree surface). nil = cross-project (the
// /issues/tree surface). RootID, when non-nil, anchors the walk at one
// specific issue id; nil = forest of every issue with no parent-child
// edge.
type IssueTreeArgs struct {
	Caller uuid.UUID
	// Scope is the caller's horizon (authz.FromContext). The zero Scope
	// sees nothing.
	Scope     authz.Scope
	ProjectID *uuid.UUID
	RootID    *string
	// Types is the validated subset of dependencies.AllowedTypes the
	// walk recurses across. Empty defaults to ['parent-child'] at the
	// handler boundary (the store assumes a sane, non-empty value).
	Types []string
	// MaxNodes caps the row count. Zero/negative falls back to
	// IssueTreeDefaultMaxNodes.
	MaxNodes int
	// MaxDepth optionally caps how deep the walk descends, counted from
	// the seed row (depth 0). Zero/negative means "unset" and falls back
	// to IssueTreeMaxDepth, so the wire shape without the parameter is
	// unchanged (sk-421.2 INV-3).
	//
	// sk-421.2 F-14: the cap is a CONDITION OF THE RECURSIVE STEP, not a
	// filter applied to the finished walk — it is substituted into the
	// same placeholder the defensive ceiling uses, inside the recursive
	// member's WHERE. A post-filter would still pay for the full descent,
	// which is exactly what the "List" mode must not do on a large epic.
	//
	// Values above IssueTreeMaxDepth are refused at the handler boundary
	// (400 invalid_max_depth) rather than clamped here: silently returning
	// a shallower tree than asked for would be indistinguishable from a
	// tree that genuinely ends there.
	MaxDepth int
	// IncludeBlockers asks for IssueTreeNode.Blockers and
	// IssueTreeNode.UnavailableBlockingLinks (sk-421.2.8 US-4; sk-423).
	// When set, the store loads the blockers of every visible node in ONE
	// batched read and derives each visible row's Item.IsBlocked from that
	// same read (INV-2, R-3). Edges are classified by the READER's horizon
	// taken from the context (R-4), so the store also requires Caller and
	// Scope to name the context's principal when there is one (R-6).
	// Unset: the path, the reads and the wire shape are exactly the
	// previous ones.
	IncludeBlockers bool
}

// IssueTreeNode is one row in the flat wire response. ParentRef is nil
// for root rows (Depth == 0); for every other row it identifies the
// row's parent in the walk plus the edge type that connected the two
// (so the renderer can colour the connector). IsReference rows carry
// FirstSeenAt = the parent id at the time of the FIRST occurrence so
// the UI can render a "↻ see <firstSeenAt>" jump button.
type IssueTreeNode struct {
	Item        IssueSummary   `json:"item"`
	Depth       int            `json:"depth"`
	ParentRef   *TreeParentRef `json:"parentRef,omitempty"`
	IsReference bool           `json:"isReference"`
	FirstSeenAt *string        `json:"firstSeenAt,omitempty"`
	// Blockers lists the blockers of this row's issue — one entry per
	// gating blocks / waits-for edge whose target is inside the READER's
	// horizon, exactly like IssueDetail.Blockers for the same reader
	// (sk-421.2.8 US-4, INV-5; sk-423). Present only when the request asked
	// for blockers and the row's issue has at least one; absent otherwise, so
	// a response without that request keeps its previous key set. Never set
	// on a placeholder row for an inaccessible node, nor on a reference row
	// (INV-3). Entries: see TreeBlockerRef.
	Blockers []TreeBlockerRef `json:"blockers,omitempty"`
	// UnavailableBlockingLinks is IssueDetail.UnavailableBlockingLinks for
	// this row's issue (sk-423): the number of outgoing blocks / waits-for
	// edges to issues outside the reader's horizon, whatever their status.
	// Loaded by the same read as Blockers and placed by the same rule — only
	// when the request asked for blockers, only on a canonical visible row —
	// and, unlike the detail's field, absent when it is 0, so that a
	// response keeps its previous key set wherever there is no such link.
	// Its presence depends only on the edge existing, never on the hidden
	// issue's status.
	UnavailableBlockingLinks int `json:"unavailableBlockingLinks,omitempty"`
}

// TreeBlockerRef is one entry of IssueTreeNode.Blockers (sk-421.2.8 US-4,
// R-1). It is a type of its own on purpose: the detail's BlockerRef is not
// reused with an extra key, so the detail, MCP and skein_close contracts
// stay untouched.
//
// One wire form: {"id", "title", "status", "projectId"}. Since sk-423 every
// entry is a blocker the reader can see; the two redacted forms sk-421.2.8
// defined ({id, redacted:true} and {redacted:true}) are gone, because an
// entry that exists only while the hidden target gates says that the hidden
// issue is not closed. Edges to issues outside the horizon are counted in
// IssueTreeNode.UnavailableBlockingLinks instead.
//
// Order: the detail's order (sortBlockerEntries — the id-order rule, sk-426).
type TreeBlockerRef struct {
	// ID is the blocker's hierarchical id.
	ID string `json:"id,omitempty"`
	// Title is the blocker's title.
	Title string `json:"title,omitempty"`
	// Status is the blocker's status: open, in_progress or deferred
	// (closed and standing targets never block).
	Status string `json:"status,omitempty"`
	// ProjectID is the project the blocker lives in, so the client can
	// open the blocker's card in its own project. A pointer because a zero
	// uuid.UUID would still be serialised under omitempty.
	ProjectID *uuid.UUID `json:"projectId,omitempty"`
}

// TreeParentRef identifies a node's parent in the flat walk + the
// typed-deps edge that connected the two. EdgeType matches the
// dependencies type constants ('parent-child' | 'blocks' | …).
type TreeParentRef struct {
	ID       string `json:"id"`
	EdgeType string `json:"edgeType"`
}

// IssueTreeResult is the wire wrapper for /issues/tree. Items is the
// pre-order flat list of rows. Always non-nil ([] not null) so JS
// clients don't need to special-case absence.
type IssueTreeResult struct {
	Items []IssueTreeNode `json:"items"`
}

// ErrTreeTooLarge signals the walk would produce more than MaxNodes
// rows. The store stops at MaxNodes+1 and returns this error so the
// handler can surface 422 tree_too_large with the supplied limit.
type ErrTreeTooLarge struct {
	Limit int
}

func (e *ErrTreeTooLarge) Error() string {
	return fmt.Sprintf("tree too large: exceeded %d nodes", e.Limit)
}

// IssueTree runs the recursive walk and returns the flat row list. The
// caller is expected to have already validated args.Types (subset of
// dependencies.AllowedTypes); the store doesn't re-validate but inlines
// the slice directly into a single placeholder. parent-child is the
// only mandatory edge type per the sk-36 spec; callers may pass it
// implicitly or as part of args.Types.
//
// Implementation:
//
//  1. Recursive CTE walks outgoing edges from each seed root, carrying
//     a `path` array so the SQL itself short-circuits cycles and the
//     dedup below can tell occurrences apart, plus the edge type column
//     so the post-processor can reconstruct the ParentRef. One row per
//     path, so the walk must stay bounded — see "Bounded walk".
//  2. Seed roots: when RootID is nil, the forest is every issue in
//     VisibleProjects (or every issue when admin) with no outgoing
//     parent-child edge. When RootID is non-nil, the seed is that
//     single issue. Either way the seed rows emit at Depth=0 with
//     ParentRef=nil.
//  3. The walk is cut at MaxNodes+1 rows BEFORE it is sorted (sk-721):
//     LIMIT sits in a subquery directly over the CTE, and ORDER BY
//     applies to those rows only. See "Bounded walk" below.
//  4. Post-process in Go:
//     a) batch-load IssueSummary rows for every unique id touched by
//     the walk (one round-trip; components / labels are aggregated in
//     the same statement, rowComponentsLabelsCols)
//     b) apply the visibility gate: non-admin callers see issues in
//     their VisibleProjects only; everything else surfaces as a
//     stub `<hidden>` IssueSummary with IsReference=true
//     c) dedup: the SQL walk emits one row per (issue, parent, edge)
//     triple, but the wire shape wants first-occurrence canonical +
//     subsequent-occurrence reference rows. We pre-order-traverse
//     the SQL output by depth (already sorted) and mark each id's
//     second-and-later occurrences as IsReference rows with
//     FirstSeenAt = the parent_id from the first row.
//
// The indexes powering the join are already in place:
//   - dependencies (from_id, type) — idx_deps_from_type from migration
//     000005, used to enumerate outgoing edges by type
//   - issues PK on id, hit during the FROM-side JOIN
//
// No new migration is needed for sk-36.
//
// Bounded walk (sk-721). A walk row is one PATH to a node, not one node:
// on a dense DAG of blocks / waits-for edges (a user can switch those types
// on in the tree view) the number of paths is exponential — 22 issues with
// every i → j>i edge give 2^21 rows from the first one. The query used to
// end in `ORDER BY … LIMIT MaxNodes+1` over the CTE itself, and a sort needs
// its whole input: Postgres generated every path, spilled them to pgsql_tmp
// (EXPLAIN ANALYZE on that 22-issue graph: 147 315 temp blocks written,
// ≈1.1 GB, 8 s) and only then kept the first MaxNodes+1 — the same disk
// exhaustion sk-647 hit in the cycle check.
//
// Now the LIMIT is applied to the CTE scan directly and the ORDER BY to the
// limited subquery. Postgres evaluates a WITH query only as far as the
// parent fetches it (docs, "WITH Queries", 7.8.2), and the recursive union
// streams each row of the recursive term as it is produced, so the walk stops
// after MaxNodes+1 rows — mid-iteration, not at an iteration boundary: on the
// graph above EXPLAIN shows Recursive Union rows=1001 with the third level
// cut short, in memory, no temp files. Every blocking node of the recursive
// step only consumes its inputs — the previous level (itself at most
// MaxNodes+1 rows, all already fetched) and the dependencies table — never
// the step's output. Keep it so: an ORDER BY, DISTINCT, aggregate or join
// between `walk` and the LIMIT brings the full enumeration back.
//
// Why the output contract is unchanged. The limited subquery is either the
// whole walk (it has at most MaxNodes rows) or MaxNodes+1 rows of it. In the
// first case the outer ORDER BY sorts exactly the rows the old query sorted,
// with the same key, fed by the same CTE scan in the same order — the output
// is the same. In the second the store answers ErrTreeTooLarge, as before,
// whichever MaxNodes+1 rows it got: a truncated walk never reaches the dedup
// below. Sorting stays in SQL rather than Go on purpose: the key compares ids
// under the database collation, and a byte-order sort in Go could pick a
// different first occurrence (the canonical row).
//
// sk-421.2.8 T004: with args.IncludeBlockers the args are first checked
// against the context's principal (checkTreeHorizon, R-6), and after the
// rows are built one batched read (treeBlockersFor) fills Blockers on the
// canonical visible rows and IsBlocked on every visible row.
func (s *Store) IssueTree(ctx context.Context, args IssueTreeArgs) (*IssueTreeResult, error) {
	maxNodes := args.MaxNodes
	if maxNodes <= 0 {
		maxNodes = IssueTreeDefaultMaxNodes
	}

	if args.IncludeBlockers {
		if err := checkTreeHorizon(ctx, args); err != nil {
			return nil, err
		}
	}

	if len(args.Types) == 0 {
		// Caller forgot to default; emit parent-child only so the
		// forest still has shape (sk-36 spec — parent-child is
		// mandatory-on).
		args.Types = []string{dependencies.DepParentChild}
	}

	// Build the seed (non-recursive term) WHERE clause.
	//
	//   - RootID set       → seed is exactly i.id = $rootID
	//   - RootID nil       → seed is every issue with no outgoing
	//                        parent-child edge AND (project filter)
	//
	// We don't filter by status here: the tree view shows closed
	// issues too (the renderer dims them via row CSS). Filtering would
	// silently truncate trees mid-branch and confuse users.
	var (
		seedClauses []string
		params      []any
	)
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}

	if args.RootID != nil {
		seedClauses = append(seedClauses, fmt.Sprintf("i.id = %s", addParam(*args.RootID)))
	} else {
		// "Root issues" = issues with no outgoing parent-child edge.
		// We always anchor the forest on parent-child so the spine is
		// stable across user toggles of the other six edge types.
		seedClauses = append(seedClauses, fmt.Sprintf(`NOT EXISTS (
    SELECT 1 FROM dependencies d
     WHERE d.from_id = i.id AND d.type = %s
)`, addParam(dependencies.DepParentChild)))
	}

	// Project / visibility scoping applies to BOTH seed shapes (rootId set
	// or not), NOT just the rootId-nil forest. Without it on the rootId
	// path, a caller could pass rootId=<issue in a project they cannot
	// see> and get back a redacted "<hidden>" stub — leaking the issue's
	// existence, project, type and status (sk-115 security review). Issue
	// ids are enumerable (prefix-N), so that is a real metadata probe.
	// Constraining the SEED means an out-of-scope rootId yields an empty
	// forest. Cross-project hidden ANCESTORS discovered during the
	// recursive walk are unaffected — they still surface as <hidden> stubs
	// (that is the recursive step below, which intentionally crosses
	// project boundaries to keep the chain unbroken).
	if args.ProjectID != nil {
		seedClauses = append(seedClauses,
			fmt.Sprintf("i.project_id = %s", addParam(*args.ProjectID)))
	} else {
		unrestricted, caller, hErr := args.Scope.Bind(ctx, s.pool)
		if hErr != nil {
			return nil, hErr
		}
		seedClauses = append(seedClauses,
			authz.ProjectVisibleExpr("i", addParam(unrestricted), addParam(caller)))
	}

	// Recursive step: the semantic direction of `parent-child` edges is
	// INVERTED relative to every other type. Canonical convention (sk-35):
	//   parent-child: fromId = CHILD, toId = PARENT
	//   all others:   fromId = FROM,  toId = TO (outgoing semantics)
	//
	// In a tree view the user expands a parent and expects to see its
	// CHILDREN below — which means for parent-child we walk INCOMING
	// (parent → child). For all other types we walk OUTGOING. PostgreSQL
	// rejects a 3-arm UNION inside WITH RECURSIVE (seed + 2 recursive),
	// so we materialise a `normalised_edges` CTE that pre-flips
	// parent-child direction. The recursive arm then walks one
	// "normalised" hop with a clean prev_id → next_id contract.
	//
	// parent-child is always present in args.Types per the handler
	// default + F1 setter invariant.
	typesPh := addParam(args.Types)
	parentChildPh := addParam(dependencies.DepParentChild)
	// sk-421.2: the caller's depth limit rides the SAME placeholder as the
	// defensive ceiling, so it stays a condition of the recursive step.
	// Never exceeds the ceiling: the handler refuses larger values, and the
	// min() here keeps the store safe for any other caller (MCP, tests).
	effectiveMaxDepth := IssueTreeMaxDepth
	if args.MaxDepth > 0 && args.MaxDepth < effectiveMaxDepth {
		effectiveMaxDepth = args.MaxDepth
	}
	maxDepthPh := addParam(effectiveMaxDepth)
	limitPh := addParam(maxNodes + 1)

	sql := fmt.Sprintf(`
WITH RECURSIVE
  normalised_edges AS (
    -- parent-child: flip direction (walk parent → child)
    SELECT to_id::text   AS prev_id, from_id::text AS next_id, type
      FROM dependencies WHERE type = %[5]s
    UNION ALL
    -- all other types: keep outgoing direction
    SELECT from_id::text AS prev_id, to_id::text   AS next_id, type
      FROM dependencies WHERE type <> %[5]s
  ),
  walk(id, depth, parent_id, parent_depth, edge_type, path) AS (
    SELECT i.id::text, 0, NULL::text, NULL::int, NULL::text, ARRAY[i.id::text]
      FROM issues i
     WHERE %[1]s
    UNION ALL
    SELECT e.next_id, w.depth + 1, w.id, w.depth, e.type, w.path || e.next_id
      FROM normalised_edges e
      JOIN walk w ON e.prev_id = w.id
     WHERE e.type = ANY(%[2]s)
       AND w.depth < %[3]s
       AND NOT (e.next_id = ANY(w.path))
  )
SELECT id, depth, parent_id, parent_depth, edge_type, path
  FROM (SELECT id, depth, parent_id, parent_depth, edge_type, path
          FROM walk
         LIMIT %[4]s) bounded
 ORDER BY depth, parent_id NULLS FIRST, id`,
		strings.Join(seedClauses, " AND "), typesPh, maxDepthPh, limitPh, parentChildPh)

	rows, err := s.pool.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("walking issue tree: %w", err)
	}
	defer rows.Close()

	type walkRow struct {
		ID          string
		Depth       int
		ParentID    *string
		ParentDepth *int
		EdgeType    *string
		Path        []string
	}
	walked := make([]walkRow, 0, 64)
	for rows.Next() {
		var r walkRow
		if err := rows.Scan(&r.ID, &r.Depth, &r.ParentID, &r.ParentDepth, &r.EdgeType, &r.Path); err != nil {
			return nil, fmt.Errorf("scanning walk row: %w", err)
		}
		walked = append(walked, r)
		if len(walked) > maxNodes {
			return nil, &ErrTreeTooLarge{Limit: maxNodes}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating walk rows: %w", err)
	}

	if len(walked) == 0 {
		return &IssueTreeResult{Items: []IssueTreeNode{}}, nil
	}

	// Collect unique ids for the IssueSummary batch fetch.
	idSet := make(map[string]struct{}, len(walked))
	for _, r := range walked {
		idSet[r.ID] = struct{}{}
	}
	uniqueIDs := make([]string, 0, len(idSet))
	for id := range idSet {
		uniqueIDs = append(uniqueIDs, id)
	}

	summaries, err := s.summariesForIDs(ctx, uniqueIDs, args.Caller)
	if err != nil {
		return nil, err
	}

	// Visibility gate: a node in a project the reader's scope does not see
	// becomes a redacted stub. The access relation decides, for every reader
	// — an admin's reach is its 'admin' level there, not a bypass here.
	projectIDs := make([]uuid.UUID, 0, len(summaries))
	seenProject := make(map[uuid.UUID]struct{}, len(summaries))
	for _, sum := range summaries {
		if _, dup := seenProject[sum.ProjectID]; !dup {
			seenProject[sum.ProjectID] = struct{}{}
			projectIDs = append(projectIDs, sum.ProjectID)
		}
	}
	visibleSet, err := authz.VisibleAmong(ctx, s.pool, args.Scope, projectIDs)
	if err != nil {
		return nil, err
	}
	// The project variant (/projects/{pid}/issues/tree) shows a node of any
	// other project as a stub, visible or not — the rule of that route: its
	// handler authorised the caller for this one project only.
	if args.ProjectID != nil {
		_, own := visibleSet[*args.ProjectID]
		visibleSet = map[uuid.UUID]struct{}{}
		if own {
			visibleSet[*args.ProjectID] = struct{}{}
		}
	}

	// Redaction of hidden-project stubs is Hard for every reader
	// (sk-163.19 for externals; sk-674.18 T009 for everyone): the stub
	// drops id, prefix and projectId — a hidden hierarchical id embeds the
	// project prefix and names the issue (INV-28 (4) allows only the fact
	// of the node). The stub keeps a prefix-free synthetic id so the tree
	// still renders.
	level := redact.Hard

	// hidden tracks the real ids replaced by a "<hidden>" stub, so the Hard
	// path below can remap them to prefix-free synthetic ids everywhere they
	// appear on the wire (stub id, ParentRef.ID, FirstSeenAt).
	hidden := make(map[string]struct{})
	for id, sum := range summaries {
		if _, ok := visibleSet[sum.ProjectID]; !ok {
			hidden[id] = struct{}{}
			// sk-421.2 US-5 / INV-2: the status and type of an unavailable issue are
			// the content of its card and must not be on the wire.
			// The keys are KEPT (frontend 0.54.0 reads them) but carry empty
			// values. Every other row field is empty too: the blocked flag
			// and the nested epic's progress are not set here
			// at all, and the INV-2 allowlist guards exactly that.
			summaries[id] = IssueSummary{
				ID:           sum.ID,
				ProjectID:    sum.ProjectID,
				Title:        "<hidden>",
				Type:         "",
				Status:       "",
				ComponentIDs: []uuid.UUID{},
				Labels:       []string{},
			}
		}
	}

	// Hard redaction (external): assign every hidden real id a stable,
	// prefix-free synthetic token ("hidden-1", "hidden-2", …) in sorted order
	// so the wire JSON never carries a real hidden id/prefix while the tree
	// still renders (frontend structure is positional — depth + DFS order —
	// so the synthetic token only has to be non-empty and internally
	// consistent). Soft/None leave synthetic nil, so wireID is the identity
	// and internal/admin output is byte-for-byte unchanged.
	var synthetic map[string]string
	if level == redact.Hard && len(hidden) > 0 {
		hiddenIDs := make([]string, 0, len(hidden))
		for id := range hidden {
			hiddenIDs = append(hiddenIDs, id)
		}
		sort.Strings(hiddenIDs)
		synthetic = make(map[string]string, len(hiddenIDs))
		for i, id := range hiddenIDs {
			synthetic[id] = fmt.Sprintf("hidden-%d", i+1)
		}
	}
	wireID := func(id string) string {
		if syn, ok := synthetic[id]; ok {
			return syn
		}
		return id
	}

	// Parents that are not rows of this tree: keep the visible ones only.
	var outside []string
	for _, sum := range summaries {
		if sum.ParentID == nil {
			continue
		}
		if _, inTree := summaries[*sum.ParentID]; !inTree {
			outside = append(outside, *sum.ParentID)
		}
	}
	outsideParents, err := s.visibleIssueIDs(ctx, outside, args.Scope)
	if err != nil {
		return nil, err
	}

	// Build the flat wire list using PATH-based dedup. The SQL emits a
	// full ancestor path per row (the array carried through the
	// recursive CTE). Semantics:
	//
	//   - For each id, the FIRST BFS-ordered occurrence is the
	//     "canonical" one. We pin its path prefix as the canonical
	//     route to that id.
	//   - A subsequent occurrence of the same id at a different path
	//     is a REFERENCE — rendered gray with `↻ see`; its subtree
	//     is NOT expanded.
	//   - A row whose ancestor chain contains ANY non-canonical edge
	//     (i.e., reached via a reference link) is SKIPPED entirely.
	//     We don't render duplicate subtrees beneath reference rows.
	//
	// Path-based disambiguation is necessary because two occurrences
	// of the same id at the same depth (e.g. multi-parent) are
	// indistinguishable by (id, depth) alone — only the full path
	// tells us which one is canonical.
	pathPrefix := func(path []string) []string {
		if len(path) == 0 {
			return nil
		}
		return path[:len(path)-1]
	}
	pathEq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Pass 1 — establish the canonical path-prefix per id. SQL ORDER
	// BY (depth, parent_id, id) ensures BFS order so each row's
	// ancestors are already-processed by the time we get to it. A row
	// only pins its id's canonical prefix when its ENTIRE ancestor
	// chain is itself canonical — otherwise we'd canonicalise via a
	// reference link and propagate the duplication downward. The
	// canonical chain is the BFS-first chain whose every step satisfies
	// "this ancestor was reached via its own canonical prefix".
	canonicalPrefix := make(map[string][]string, len(walked))
	for _, r := range walked {
		ancestorOk := true
		for i := 0; i < len(r.Path)-1; i++ {
			ancestorID := r.Path[i]
			expected := r.Path[:i]
			actual, has := canonicalPrefix[ancestorID]
			if !has || !pathEq(expected, actual) {
				ancestorOk = false
				break
			}
		}
		if !ancestorOk {
			continue
		}
		if _, seen := canonicalPrefix[r.ID]; !seen {
			prefix := pathPrefix(r.Path)
			cp := make([]string, len(prefix))
			copy(cp, prefix)
			canonicalPrefix[r.ID] = cp
		}
	}

	// Pass 2 — emit. A row is renderable iff every ancestor in its
	// path was reached via that ancestor's canonical route. The row
	// itself is canonical iff its prefix matches the canonical
	// prefix for its id.
	//
	// We attach each emitted row's path so we can re-sort to DFS
	// (pre-order) at the end — the recursive CTE walks BFS naturally
	// (needed for canonical-prefix correctness in pass 1) but the
	// frontend TaskTreeNode consumes a flat DFS list so parents
	// render directly above their children.
	type emittedRow struct {
		row  IssueTreeNode
		path []string
		// realID / hidden: the row's real issue id and whether it is a stub
		// for an inaccessible node. Kept beside the row because Hard
		// redaction swaps the wire id, and the blockers batch is keyed by
		// the real one.
		realID string
		hidden bool
	}
	emitted := make([]emittedRow, 0, len(walked))
	for _, r := range walked {
		ancestorOk := true
		for i := 0; i < len(r.Path)-1; i++ {
			ancestorID := r.Path[i]
			expected := r.Path[:i]
			actual := canonicalPrefix[ancestorID]
			if !pathEq(expected, actual) {
				ancestorOk = false
				break
			}
		}
		if !ancestorOk {
			// Reached via a reference somewhere up the chain — the
			// canonical subtree elsewhere already surfaces this
			// content. Skip silently.
			continue
		}

		sum, ok := summaries[r.ID]
		if !ok {
			// Defensive: SQL returned an id we couldn't hydrate.
			continue
		}

		// Hard redaction (external): swap the hidden stub's real
		// hierarchical id for its prefix-free synthetic token and drop the
		// invisible project's uuid. Title is already "<hidden>" and the row
		// is forced IsReference below, so the frontend renders it as a
		// non-clickable stub.
		if _, isHidden := hidden[r.ID]; isHidden && level == redact.Hard {
			sum.ID = wireID(r.ID)
			sum.ProjectID = uuid.UUID{}
		}

		// Hard redaction covers parentIdCurrent on EVERY row: a parent pointing
		// into a project the reader cannot see must not leak its real id (which
		// embeds the prefix). wireID alone is insufficient — it only maps hidden
		// ancestors that appear AS ROWS in this tree; a parent-child parent in an
		// invisible project need not be walked into the tree at all:
		//   - in-tree hidden ancestor → its synthetic token;
		//   - NOT a tree row → kept only when the reader sees it
		//     (outsideParents), dropped otherwise;
		//   - in-tree visible row → keep as-is.
		// (sk-163.19 — found in review; sk-674.18 T009 for every reader.)
		if sum.ParentID != nil {
			if syn, ok := synthetic[*sum.ParentID]; ok {
				sum.ParentID = &syn
			} else if _, inTree := summaries[*sum.ParentID]; !inTree {
				if _, visible := outsideParents[*sum.ParentID]; !visible {
					sum.ParentID = nil
				}
			}
		}

		row := IssueTreeNode{
			Item:  sum,
			Depth: r.Depth,
		}
		if r.ParentID != nil && r.EdgeType != nil {
			row.ParentRef = &TreeParentRef{ID: wireID(*r.ParentID), EdgeType: *r.EdgeType}
		}

		isCanonical := pathEq(pathPrefix(r.Path), canonicalPrefix[r.ID])
		if !isCanonical {
			row.IsReference = true
			// FirstSeenAt = the parent id from the canonical first
			// occurrence. For canonical roots (empty prefix) we encode
			// the id itself so the frontend's "jump to first" handler
			// always has a target. wireID keeps a hidden ancestor's id
			// out of this field under Hard redaction.
			cp := canonicalPrefix[r.ID]
			if len(cp) == 0 {
				idCopy := wireID(r.ID)
				row.FirstSeenAt = &idCopy
			} else {
				canonicalParent := wireID(cp[len(cp)-1])
				row.FirstSeenAt = &canonicalParent
			}
		}
		// Hidden-project stub: surface as a reference row so the
		// frontend stops recursing under it.
		//
		// sk-421.2 (R-4, F-6): membership in `hidden`, NOT a title
		// comparison. The old test asked whether the title happened to
		// read "<hidden>", so a VISIBLE task a user had titled that way
		// was marked as an inaccessible stub. `hidden` is keyed by the
		// real id — the same key the Hard-redaction branch above uses —
		// so it stays correct after the id is swapped for a synthetic
		// token on the wire.
		_, isHidden := hidden[r.ID]
		if isHidden {
			row.IsReference = true
		}
		pathCopy := make([]string, len(r.Path))
		copy(pathCopy, r.Path)
		emitted = append(emitted, emittedRow{row: row, path: pathCopy, realID: r.ID, hidden: isHidden})
	}

	if args.IncludeBlockers {
		// GUARD — INV-2, by the pattern of the INV-10 guard in
		// hydrateIssueDetail: on every visible row of this response the flag
		// IS the non-emptiness of the list loaded here, for canonical and
		// reference rows alike (R-3), and the list is placed on the canonical
		// row only. Flag and list come from one read of the graph. Do not
		// keep the flag summariesForIDs computed, and do not filter the list
		// after this read — dropping an entry would change the flag with it.
		// Stubs for inaccessible nodes get neither (INV-3).
		visibleIDs := make([]string, 0, len(emitted))
		seen := make(map[string]struct{}, len(emitted))
		for _, e := range emitted {
			if e.hidden {
				continue
			}
			if _, dup := seen[e.realID]; dup {
				continue
			}
			seen[e.realID] = struct{}{}
			visibleIDs = append(visibleIDs, e.realID)
		}
		blockers, err := s.treeBlockersFor(ctx, visibleIDs)
		if err != nil {
			return nil, err
		}
		for i := range emitted {
			e := &emitted[i]
			if e.hidden {
				continue
			}
			b := blockers[e.realID]
			e.row.Item.IsBlocked = len(b.list) > 0
			if e.row.IsReference {
				continue
			}
			if len(b.list) > 0 {
				e.row.Blockers = b.list
			}
			e.row.UnavailableBlockingLinks = b.hidden
		}
	}

	// sk-461 — branch progress for every row that HAS CHILDREN, not merely
	// for every row that IS an epic.
	//
	// summariesForIDs filled epicProgress from the shared projection, whose
	// gate is `type = 'epic'`. In a tree that reached exactly the rows the
	// epic section never draws: rooted at an epic, the root carries the field
	// and every descendant task does not, so the branch indicator the section
	// renders had no data to render on ANY row it produces. The batch below
	// answers for parents of any type; see branchProgressFor for why it is a
	// separate read and what it costs.
	//
	// Unconditional, unlike the blockers batch: `blockers` is an opt-in key
	// behind includeBlockers, whereas epicProgress is an existing key whose
	// POPULATION is what changes here. Gating it behind a new parameter would
	// leave the default response — the one the epic section actually sends —
	// exactly as broken as it is now. It stays ONE statement whatever the tree
	// looks like, so the read counter's "one batch per response, independent
	// of tree shape" property holds for this read as well.
	//
	// Applied to reference rows too, and to them only as a field: today an
	// epic's reference row already carries the triple (it comes from the
	// shared summary, which knows nothing about references), and a branch's
	// progress is a fact about the issue rather than about this occurrence of
	// it. Stubs for inaccessible nodes get NOTHING — INV-2 names the nested
	// epic's progress among the things that must not appear on a stub, and the
	// stub summary is rebuilt without it above; skipping them here is what
	// keeps it that way.
	//
	// Rows absent from the batch keep whatever the projection gave them: an
	// epic with no children keeps its {0,0,0} (present, and meaning "nothing
	// to do yet"), a childless task keeps nil. So the only rows that change
	// are parents that had no triple before.
	progressIDs := make([]string, 0, len(emitted))
	seenProgress := make(map[string]struct{}, len(emitted))
	for _, e := range emitted {
		if e.hidden {
			continue
		}
		if _, dup := seenProgress[e.realID]; dup {
			continue
		}
		seenProgress[e.realID] = struct{}{}
		progressIDs = append(progressIDs, e.realID)
	}
	branch, err := s.branchProgressFor(ctx, progressIDs)
	if err != nil {
		return nil, err
	}
	for i := range emitted {
		e := &emitted[i]
		if e.hidden {
			continue
		}
		if p, ok := branch[e.realID]; ok {
			e.row.Item.EpicProgress = &p
		}
	}

	// DFS (pre-order) sort by path: compare element-by-element so children
	// render right beneath their parents. Stable sort isn't strictly
	// required — two rows with identical paths can't exist (each row is
	// uniquely identified by its full path).
	//
	// sk-421.2 US-6: segments are compared by idorder.Compare, NOT as raw
	// strings. The old `a[k] < b[k]` put `x.10` before `x.2`, because "1"
	// sorts before "2" bytewise. Paths carry REAL ids (the CTE emits them
	// before Hard redaction swaps in synthetic tokens), so the order is the
	// one the rule describes.
	//
	// F-24: among siblings, a stub standing for an invisible project goes
	// after EVERY visible sibling, whatever its real id — the caller must
	// not be able to infer a hidden id's position. Stubs among themselves
	// follow their wire id (the synthetic token for an external caller).
	// Comparing at the first differing segment keeps the "node + its
	// descendants" block contiguous.
	//
	// The rule applies to EVERY reader, not only to the external one: the
	// `hidden` set is filled at Soft redaction too, and this comparator
	// deliberately does not look at the redaction level. Maintainers' decision
	// of 2026-09-18 (sk-488): an internal reader already sees the stub's
	// real id, so the order discloses nothing to them, and one rule for all
	// readers is simpler than two. The sk-421.2 spec (US-6)
	// first scoped it to the external reader; it carries a dated amendment.
	// Pinned by TestTreeAll_StubSortsAfterVisibleSiblings, which runs as an
	// internal reader.
	sort.Slice(emitted, func(i, j int) bool {
		a, b := emitted[i].path, emitted[j].path
		minLen := len(a)
		if len(b) < minLen {
			minLen = len(b)
		}
		for k := 0; k < minLen; k++ {
			if a[k] != b[k] {
				_, aHidden := hidden[a[k]]
				_, bHidden := hidden[b[k]]
				if aHidden != bHidden {
					return bHidden
				}
				// sk-428 (F-24): two stubs are ordered by their WIRE ids.
				// Under Hard redaction that is the synthetic token, so the
				// caller sees hidden-1 before hidden-2 whatever the rule does
				// with the real ids behind them (the tokens are numbered
				// bytewise by real id, sk-163.19; comparing real ids here put
				// hidden-2 above hidden-1 on a `-2` / `-10` pair). Soft/None
				// keep synthetic nil, wireID is the identity, and the real id
				// the caller sees is ordered by the rule.
				return idorder.Compare(wireID(a[k]), wireID(b[k])) < 0
			}
		}
		return len(a) < len(b)
	})

	items := make([]IssueTreeNode, len(emitted))
	for i := range emitted {
		items[i] = emitted[i].row
	}
	return &IssueTreeResult{Items: items}, nil
}

// TreeBlockersBatchLabel is a SQL comment that opens the one statement
// loading the blockers of a tree response (sk-421.2.8 T004). A read counter
// recognises the batch by this label, not by the edge predicate, which
// summariesForIDs carries too (T006).
const TreeBlockersBatchLabel = "/* skein:tree_blockers_batch */"

// errTreeHorizonMismatch: the tree args describe a different reader than the
// context's principal (R-6). Surfaces as the endpoint's 500.
var errTreeHorizonMismatch = errors.New("issue tree: args do not match the context principal")

// checkTreeHorizon enforces R-6 (INV-7): nodes are classified by the args
// (Scope), blocker entries by the context's principal. When a principal is
// present the two must describe the same reader — otherwise a call with one
// reader's args under another reader's context would hand full entries to
// someone shown stubs. Contexts without a principal are outside INV-7 and
// are not checked.
func checkTreeHorizon(ctx context.Context, args IssueTreeArgs) error {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil
	}
	switch {
	case args.Caller != p.UserID:
		return fmt.Errorf("%w: caller", errTreeHorizonMismatch)
	case args.Scope.IsUnrestricted() || args.Scope.CallerID() != p.UserID:
		return fmt.Errorf("%w: scope", errTreeHorizonMismatch)
	}
	return nil
}

// visibleIssueIDs returns which of ids are issues of projects the scope
// sees.
func (s *Store) visibleIssueIDs(ctx context.Context, ids []string, scope authz.Scope) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	unrestricted, caller, hErr := scope.Bind(ctx, s.pool)
	if hErr != nil {
		return nil, hErr
	}
	rows, err := s.pool.Query(ctx, `
SELECT i.id FROM issues i
 WHERE i.id = ANY($1::text[]) AND `+authz.ProjectVisibleExpr("i", "$2", "$3"),
		ids, unrestricted, caller)
	if err != nil {
		return nil, fmt.Errorf("checking visibility of parents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning visible parent: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading visible parents: %w", err)
	}
	return out, nil
}

// treeBlockers is what treeBlockersFor knows about one issue: its blockers
// for the reader, and how many of its blocks / waits-for edges lead outside
// the reader's horizon (sk-423).
type treeBlockers struct {
	list   []TreeBlockerRef
	hidden int
}

// treeBlockersFor loads, in ONE statement, the blockers of every issue in
// ids (sk-421.2.8 T004, R-2, INV-4) and returns them keyed by the blocked
// issue's id; an issue without blockers and without hidden links has no key.
//
// Membership, order and the hidden-link count are the detail's (INV-5, R-4;
// sk-423): readerBlockerPredicate, hiddenBlockingLinkPredicate and
// sortBlockerEntries, the pieces blockersFor uses, with the reader's
// horizon from authz.FromContext. For an edge whose target is outside
// the horizon the query projects NULLs — its id, title, status and project
// never reach this process — and the edge is only counted, whatever the
// target's status. Visible entries always carry projectId.
func (s *Store) treeBlockersFor(ctx context.Context, ids []string) (map[string]treeBlockers, error) {
	out := make(map[string]treeBlockers, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	scope := authz.FromContext(ctx)
	unrestricted, callerID, hErr := scope.Bind(ctx, s.pool)
	if hErr != nil {
		return nil, hErr
	}
	ownSpaces, sErr := scope.BindSpaces(ctx, s.pool)
	if sErr != nil {
		return nil, sErr
	}
	vis := authz.ProjectVisibleExpr("b", "$2", "$3")
	rows, err := s.pool.Query(ctx, TreeBlockersBatchLabel+`
SELECT d.from_id,
       CASE WHEN `+vis+` THEN b.id         END AS id,
       CASE WHEN `+vis+` THEN b.title      END AS title,
       CASE WHEN `+vis+` THEN b.status     END AS status,
       CASE WHEN `+vis+` THEN b.project_id END AS project_id
  FROM dependencies d
  JOIN issues b ON b.id = d.to_id
 WHERE d.from_id = ANY($1)
   AND ((`+readerBlockerPredicate("d", "b", "$2", "$3")+`)
     OR (`+hiddenBlockingLinkPredicate("d", "b", "$2", "$3", "$4")+`))`,
		ids, unrestricted, callerID, ownSpaces)
	if err != nil {
		return nil, fmt.Errorf("loading tree blockers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			fromID        string
			id, title, st *string
			projectID     *uuid.UUID
		)
		if err := rows.Scan(&fromID, &id, &title, &st, &projectID); err != nil {
			return nil, fmt.Errorf("scanning tree blocker: %w", err)
		}
		g := out[fromID]
		// b.id and b.status are NOT NULL, so NULLs can only come from the
		// CASE: the edge's target is outside the reader's horizon.
		if id == nil || st == nil {
			g.hidden++
			out[fromID] = g
			continue
		}
		entry := TreeBlockerRef{ID: *id, Status: *st, ProjectID: projectID}
		if title != nil {
			entry.Title = *title
		}
		g.list = append(g.list, entry)
		out[fromID] = g
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tree blockers: %w", err)
	}
	for _, g := range out {
		sortBlockerEntries(g.list, func(b TreeBlockerRef) string { return b.ID })
	}
	return out, nil
}

// TreeBranchProgressBatchLabel is a SQL comment that opens the one statement
// loading the branch progress of a tree response (sk-461). Named for the same
// reason TreeBlockersBatchLabel is: a read counter must be able to tell this
// batch from summariesForIDs, which carries the parent-child predicate too.
const TreeBranchProgressBatchLabel = "/* skein:tree_branch_progress_batch */"

// branchProgressFor batch-loads the (closed, total, unavailable, noWork)
// child quadruple for every supplied row id — sk-461, extended by sk-489.
//
// # Why this exists next to epicProgressProjection instead of inside it
//
// The projection that list and tree rows share gates its three legs on
// `i.type = 'epic'`. That gate is the whole defect: a row is given the triple
// because of its TYPE, while the thing the tree draws — a branch's progress —
// is a property of HAVING CHILDREN. In a tree rooted at an epic, every row
// below depth 0 is typically a task, so the field reached exactly one row: the
// root, which the epic section does not even render as a child.
//
// Widening the shared gate was rejected on two counts, both measured:
//
//   - Cost. The gate is what keeps the three correlated subqueries off the
//     rows that do not need them. Widening it to "epic OR has children" makes
//     all three fire on nearly every row AND re-evaluates the EXISTS gate once
//     per leg: on a 1000-row slice of a 20k-issue set that is 45005 shared
//     buffer hits / 20.9 ms against 859 / 2.08 ms for the epic-gated form.
//     One batched GROUP BY over the same ids costs 15107 / 9.8 ms — the
//     cheapest of the shapes measured, because it reads each child row once
//     instead of once per leg. The residual rise over 859 is the work itself:
//     classifying N children by status and horizon cannot be cheaper than
//     reading them. Bounded by IssueTreeDefaultMaxNodes, so it is one batched
//     read of at most ~5 edges per row, never a per-row round-trip.
//
//   - Blast radius. epicProgressProjection has six call sites, and
//     summariesForIDs — the one the tree uses — is shared with /me/watching.
//     Widening there would have changed a surface nobody asked about. Loading
//     the triple beside the summaries leaves all six projections, and every
//     list row, byte for byte as they were.
//
// # The policy is the SAME policy, deliberately
//
// The four legs are spelled to match
// epicSummary{Total,Closed,Unavailable,NoWork}Expr leg for leg, over the same
// authz.ProjectVisibleExpr horizon: TOTAL keeps
// invisible children and drops only VISIBLE standing ones, CLOSED counts only
// visible closed children, NO_WORK is the subset of CLOSED whose outcome is
// not "done", and an invisible child lands in `unavailable`
// instead of any status leg (sk-291, sk-243/F4). That is not decoration — the
// branch bar and the header ring are on screen together, so a second reading of
// "does an unavailable child count" would show two different numbers for one
// epic. An epic's own row therefore gets the same quadruple from here that the
// projection computed for it; the two agree by construction, which is why
// overwriting it below is a no-op rather than a second opinion.
//
// sk-489 T005 — the fourth leg is why "match verbatim" is a checkable
// claim and not a wish: this function builds EpicProgress by a direct Scan,
// BYPASSING epicProgressFrom, so the compiler guard that protects the six
// projection call sites does not protect this one. Done and Counted are
// therefore derived here by the same arithmetic epicProgressFrom does
// (Done = Closed - NoWork, Counted = Total - NoWork), and a test pins the
// two spellings against each other — see
// tree_branch_progress_test.go#TestBranchProgress_LegsMatchTheProjection.
//
// Children are parent-child children regardless of which types[] the walk
// recursed across: "child" is that edge by definition (sk-35), and the six
// other lenses do not make a node a parent.
//
// Rows with no parent-child children are ABSENT from the result rather than
// present with a zero triple, so the caller can tell "no children" from "no
// children closed" and leave a leaf's field alone.
func (s *Store) branchProgressFor(ctx context.Context, ids []string) (map[string]EpicProgress, error) {
	if len(ids) == 0 {
		return map[string]EpicProgress{}, nil
	}

	// sk-291 — the triple is scoped to the caller's horizon, same as the
	// projection's. $1 is the id list; $2/$3 are the horizon args, $4 the
	// reader's own spaces: a hidden child counts only there (INV-28 (4)).
	h, hErr := s.horizon(ctx)
	if hErr != nil {
		return nil, hErr
	}
	unrestricted, caller := h.unrestricted, h.visible
	vis := authz.ProjectVisibleExpr("c", "$2", "$3")
	hid := authz.HiddenCountableExpr("c.project_id", "$2", "$3", "$4")

	rows, err := s.pool.Query(ctx, TreeBranchProgressBatchLabel+`
SELECT d.to_id,
       COUNT(*) FILTER (WHERE (`+vis+` AND c.status <> 'standing') OR `+hid+`) AS total,
       COUNT(*) FILTER (WHERE `+vis+` AND c.status = 'closed')       AS closed,
       COUNT(*) FILTER (WHERE `+hid+`)                               AS unavailable,
       COUNT(*) FILTER (WHERE `+vis+` AND c.status = 'closed'
                          AND c.close_outcome <> 'done')             AS no_work
  FROM dependencies d
  JOIN issues c ON c.id = d.from_id
 WHERE d.to_id = ANY($1) AND d.type = 'parent-child'
 GROUP BY d.to_id`, ids, unrestricted, caller, h.spaces)
	if err != nil {
		return nil, fmt.Errorf("loading tree branch progress: %w", err)
	}
	defer rows.Close()

	out := make(map[string]EpicProgress, len(ids))
	for rows.Next() {
		var (
			id string
			p  EpicProgress
		)
		if err := rows.Scan(&id, &p.Total, &p.Closed, &p.Unavailable, &p.NoWork); err != nil {
			return nil, fmt.Errorf("scanning tree branch progress: %w", err)
		}
		// The same arithmetic as epicProgressFrom (INV-5) — see the doc block above.
		p.Done = p.Closed - p.NoWork
		p.Counted = p.Total - p.NoWork
		out[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tree branch progress: %w", err)
	}
	return out, nil
}

// summariesForIDs batch-loads IssueSummary rows for the supplied id
// list. Mirrors the ListIssues SELECT projection (parent_id_current,
// is_blocked, epic_progress, etc.) so the wire-shape stays consistent
// across list / tree surfaces. Components and labels are aggregated in the
// same statement (rowComponentsLabelsCols) — one snapshot with each row's
// rowSeq (sk-622.21, R26 item 2).
//
// sk-461 — "consistent wire shape" means the same KEYS, and it still holds:
// the tree populates epic_progress more widely than the list does (every row
// with children, not only epic-typed rows) by overwriting it from
// branchProgressFor after this call. That widening deliberately does not
// happen HERE, because this helper also serves /me/watching, whose rows are
// list cards and are meant to keep the list's population.
//
// Caller is the resolved user id used to compute the IAssigned flag —
// nil callers get a constant FALSE so anonymous lookups (none exist
// today, but the helper stays uniform with ListIssues).
func (s *Store) summariesForIDs(ctx context.Context, ids []string, caller uuid.UUID) (map[string]IssueSummary, error) {
	if len(ids) == 0 {
		return map[string]IssueSummary{}, nil
	}

	// Issue id list parameter, plus the caller for the iAssigned
	// expression. Keep the LEFT JOINs identical to ListIssues so the
	// scan shape matches.
	idsPh := "$1"
	callerPh := "$2"
	// sk-291 — epic progress is scoped to the caller's horizon; sk-423 —
	// so is the blocked flag. Both bind the same pair ($3, $4).
	h, hErr := s.horizon(ctx)
	if hErr != nil {
		return nil, hErr
	}
	epicUnrestricted, epicCaller := h.unrestricted, h.visible

	sql := `
SELECT i.id, i.project_id, ` + parentIDCurrentExpr + `, i.version, i.title,
       i.type, i.priority, i.status, i.pinned, i.assignee,
       i.created_by, uc.username, ua.id, ` + issueSummaryPersonCols + `,
       i.created_at, i.updated_at, i.row_seq,
       ` + rowComponentsLabelsCols + `,
       i.accepted_at, i.close_outcome,
       (i.created_by = ` + callerPh + ` AND ua.id IS NOT NULL AND ua.id <> ` + callerPh + `) AS i_assigned,
       ` + isBlockedExprFor("$3", "$4") + ` AS is_blocked,
       ` + epicProgressProjection("$3", "$4", "$5") + `
  FROM issues i
  LEFT JOIN users uc ON uc.id = i.created_by
  ` + assignee.LeftJoin("ua", "i") + `
 WHERE i.id = ANY(` + idsPh + `)`

	rows, err := s.pool.Query(ctx, sql, ids, caller, epicUnrestricted, epicCaller, h.spaces)
	if err != nil {
		return nil, fmt.Errorf("loading tree summaries: %w", err)
	}
	defer rows.Close()

	out := make(map[string]IssueSummary, len(ids))
	for rows.Next() {
		var (
			item              IssueSummary
			parentID          *string
			assignee          *string
			createdByUsername *string
			assigneeUserID    *uuid.UUID
			acceptedAt        *time.Time
			closeOutcome      *string
			iAssigned         bool
			isBlocked         bool
			epicTotal         *int
			epicClosed        *int
			epicUnavailable   *int
			epicNoWork        *int
		)
		if err := rows.Scan(
			&item.ID, &item.ProjectID, &parentID, &item.Version, &item.Title,
			&item.Type, &item.Priority, &item.Status, &item.Pinned, &assignee,
			&item.CreatedBy, &createdByUsername, &assigneeUserID, &item.AssigneeStatus, &item.CreatedByStatus, &item.SpaceID,
			&item.CreatedAt, &item.UpdatedAt, &item.RowSeq,
			&item.ComponentIDs, &item.Labels,
			&acceptedAt, &closeOutcome,
			&iAssigned,
			&isBlocked,
			&epicTotal, &epicClosed, &epicUnavailable, &epicNoWork,
		); err != nil {
			return nil, fmt.Errorf("scanning tree summary: %w", err)
		}
		item.ParentID = parentID
		item.Assignee = assignee
		item.CreatedByUsername = createdByUsername
		item.AssigneeUserID = assigneeUserID
		item.AcceptedAt = acceptedAt
		item.CloseOutcome = closeOutcome
		item.IAssigned = iAssigned
		item.IsBlocked = isBlocked
		item.EpicProgress = epicProgressFrom(epicTotal, epicClosed, epicUnavailable, epicNoWork)
		normalizeComponentsLabels(&item.ComponentIDs, &item.Labels)
		out[item.ID] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Components and labels arrived with the rows (rowComponentsLabelsCols),
	// the same snapshot as each row's rowSeq (R26 item 2).

	return out, nil
}

// SummariesForIDs is the exported wrapper around summariesForIDs used
// by handlers that need to hydrate a list of issues into IssueSummary
// rows (e.g. /me/watching in sk-39). Same wire shape as ListIssues —
// reusing the helper keeps the projection consistent across surfaces.
func (s *Store) SummariesForIDs(ctx context.Context, ids []string, caller uuid.UUID) (map[string]IssueSummary, error) {
	return s.summariesForIDs(ctx, ids, caller)
}

// AllowedTreeTypes returns the closed set of dependency types accepted
// by the /issues/tree query param `types[]`. Mirrors the
// dependencies-package whitelist so the validator stays a single source
// of truth (any change to dependencies.validTypes flows through here
// without per-list copy/paste drift).
func AllowedTreeTypes() []string {
	return []string{
		dependencies.DepParentChild,
		dependencies.DepBlocks,
		dependencies.DepWaitsFor,
		dependencies.DepRelated,
		dependencies.DepDiscoveredFrom,
		dependencies.DepSupersedes,
		dependencies.DepDuplicates,
	}
}

// IsAllowedTreeType reports whether t is one of the seven dep types
// the tree walker recurses across. Used by the handler to validate
// the wire `types[]` query param at the request boundary.
func IsAllowedTreeType(t string) bool {
	switch t {
	case dependencies.DepParentChild,
		dependencies.DepBlocks,
		dependencies.DepWaitsFor,
		dependencies.DepRelated,
		dependencies.DepDiscoveredFrom,
		dependencies.DepSupersedes,
		dependencies.DepDuplicates:
		return true
	}
	return false
}

// Ensure ErrTreeTooLarge stays a discoverable error type via
// errors.As — handler maps it to 422 tree_too_large.
var _ error = (*ErrTreeTooLarge)(nil)

// pgxNoRows is a compile-time sanity check that pgx is imported (it's
// referenced indirectly via the *pgxpool.Pool methods used on s.pool;
// keeping the explicit symbol use prevents accidental import removal
// during refactors).
var _ = pgx.ErrNoRows

// errorsAsAlias keeps `errors` imported even if every typed-error
// branch is later inlined.
var _ = errors.Is
