package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
)

// IssuesAllHandler hosts the cross-project GET /api/v1/issues endpoint —
// the "All projects" surface in the new shell's Tasks column (Sc3). Read-
// only, caller-scoped, no path params. The handler resolves the caller's
// visible project set (admin: nil + isAdmin=true) and delegates to
// issues.Store.ListAllIssues which inlines the visibility predicate as
// `i.project_id = ANY(...)`.
//
// FTS + optional semantic rerank piggyback on the same Embedder
// instance the projects search path uses — passed in once at construction
// time. nil Embedder collapses to FTS-only without round-trips.
type IssuesAllHandler struct {
	pool        *pgxpool.Pool
	issuesStore *issues.Store
	embedder    issues.QueryEmbedder
}

// NewIssuesAllHandler constructs the handler. The embedder is optional —
// pass nil to disable the semantic rerank layer entirely.
func NewIssuesAllHandler(pool *pgxpool.Pool, issuesStore *issues.Store, embedder issues.QueryEmbedder) *IssuesAllHandler {
	return &IssuesAllHandler{pool: pool, issuesStore: issuesStore, embedder: embedder}
}

// List handles GET /api/v1/issues — cross-project list, visibility-
// filtered to the caller's project_members horizon. Admins see all.
//
// Query params (all optional unless noted):
//
//	q              — FTS over title + description + acceptance + design +
//	                 notes. Looks-like-issue-id matches get an exact-id
//	                 boost so typing "demo-2.5" jumps that row to the top.
//	status         — single status filter
//	type           — single type filter
//	origin         — human-driven | agent-autonomous | all
//	assignee       — "me" (caller) or a free-form username
//	assigneeUserId — direct uuid (mutually exclusive with assignee)
//	createdBy      — "me" (caller's creations) | "not-me" (exclude caller's)
//	acceptedAt     — only the literal "null" — Inbox-style "unaccepted"
//	isBlocked      — true | false: blocked only / unblocked only, where
//	                 blocked means a blocks / waits-for blocker in a
//	                 project the caller can see (sk-423) — the same rule
//	                 as item.isBlocked
//	cursor         — opaque keyset cursor
//	limit          — 1..500, default 500
//	sortBy         — "updated" (default) | "created"
//
// Response shape: issues.ListResult with the populated Total field
// (exact COUNT(*) over the same WHERE clause). Every row carries `rowSeq`,
// the row freshness order (sk-622.21, US-15) — see issues.IssueSummary.RowSeq.
//
// @Summary List issues across every visible project
// @Description Returns a page of issues from every project the caller can see (admins: all projects), with the exact total. Business meaning: the cross-project task list — the "All projects" Tasks column of the web shell and the cross-project lenses (On me, Delegated, Inbox-style unaccepted); the selection is done on the server, the client never filters a wider answer down.
// @Description
// @Description Algorithm: authenticate -> load the caller's visible-project horizon (a non-admin with no visible project gets an empty page without querying issues) -> validate every query parameter (an unrecognised value is REFUSED with 400, never ignored) -> without `q`: one keyset-paginated query ordered by `sortBy` DESC, id DESC; with `q`: full-text search over title, description, acceptance criteria, design and notes, an exact-id boost for an id-shaped query, ordered by score, and on the first page an optional semantic rerank when FTS is thin and an embedder is configured (an embedder failure degrades to FTS only) -> exact COUNT(*) over the same filter without the cursor -> return the page, the total and the next cursor.
// @Description
// @Description Rows: each row is the list shape issues.IssueSummary — reader-dependent fields `iAssigned` (the caller created it and delegated it to somebody else) and `isBlocked` (a blocks / waits-for blocker in a project the caller can see, sk-423), `epicProgress` on epic-typed rows, and `closeOutcome` on closed issues only (the key is absent otherwise).
// @Description
// @Description Row freshness order (sk-622.21, US-15): every row carries `rowSeq` — the issue's freshness order, taken from a global sequence on every committed write of the issue row, including writes that do not move `version` (label add/remove, component replace, a parent-child edge added or removed). For ONE issue a greater rowSeq is a later committed state; values of different issues are not comparable. The row's labels and component ids are read in the same statement as its rowSeq, so each row is exactly the state its rowSeq names. A phase-2 client substitutes an event's row into a loaded row only if the event row's rowSeq is greater than the one shown. An issue not written since the phase-2 rollout (migration 000064) has no rowSeq key (absent ≡ 0, order unknown); any event row with a positive rowSeq is fresher than it.
// @Description
// @Description Side effects: none — the call is a pure read and writes no events.
// @Description
// @Description Edge cases: a cursor from another sort order or a malformed cursor is not validated up front and currently surfaces as 500 internal_error; `limit` outside 1..500 is refused, omitted means 500.
// @Description
// @Description Visibility: rows of the projects the access relation grants the caller (sk-674.18). parentIdCurrent of a row is present only when the caller sees the parent's project — a row never names an inaccessible issue.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param space query string false "Optional space short id; only issues of that space's projects (the space of the issue's project, not of its id prefix). An unknown or unseen space yields nothing"
// @Param q query string false "Full-text query over title, description, acceptance criteria, design and notes; an id-shaped value (e.g. demo-2.5) is boosted to the top. Switches ordering to relevance"
// @Param status query string false "Exactly one known status; blocked is refused (derived — use isBlocked=true)"
// @Param type query string false "Exactly one known issue type"
// @Param origin query string false "Origin group of the issue's source" Enums(human-driven, agent-autonomous, all)
// @Param assignee query string false "me resolves to the caller; any other value is a username equality filter. Mutually exclusive with assigneeUserId"
// @Param assigneeUserId query string false "Assignee user UUID. Mutually exclusive with assignee"
// @Param createdBy query string false "me — issues the caller created; not-me — issues created by somebody else" Enums(me, not-me)
// @Param acceptedAt query string false "null (not yet accepted) or not-null (accepted)" Enums(null, not-null)
// @Param isBlocked query bool false "Exactly true or false: blocked only / unblocked only, by the same rule as item.isBlocked"
// @Param sortBy query string false "Order of the plain (no q) path; default updated" Enums(updated, created)
// @Param cursor query string false "Opaque keyset cursor from the previous page's nextCursor"
// @Param limit query int false "Page size, 1..500; default 500"
// @Success 200 {object} issues.ListResult "One page, the exact total and the next cursor"
// @Failure 400 {object} map[string]any "validation_error — origin, createdBy, acceptedAt or sortBy outside its set, assignee together with assigneeUserId, assigneeUserId not a UUID; invalid_status / invalid_type — unknown status or type; invalid_isBlocked; invalid_limit"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 422 {object} map[string]any "blocked_is_derived — status=blocked is a derived state and cannot be selected; use isBlocked=true"
// @Failure 500 {object} map[string]any "internal_error — includes a malformed cursor"
// @Router /api/v1/issues [get]
func (h *IssuesAllHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	q := r.URL.Query()

	args := issues.ListAllIssuesArgs{
		Caller:         c.UserID,
		CallerUsername: strings.ToLower(strings.TrimSpace(c.Username)),
		Scope:          authz.FromContext(ctx),
		Embedder:       h.embedder,
		// sk-674.18 T032: the page filter — one space's projects.
		Space: strings.TrimSpace(q.Get("space")),
	}

	if raw := strings.TrimSpace(q.Get("q")); raw != "" {
		args.Q = raw
	}
	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		canon, vErr := issues.ValidateStatus(raw)
		if vErr != nil {
			mapIssueValidation(w, vErr)
			return
		}
		args.Status = canon
	}
	if raw := strings.TrimSpace(q.Get("type")); raw != "" {
		canon, vErr := issues.ValidateType(raw)
		if vErr != nil {
			mapIssueValidation(w, vErr)
			return
		}
		args.Type = canon
	}
	if raw := strings.TrimSpace(q.Get("origin")); raw != "" {
		switch raw {
		case issues.OriginHumanDriven, issues.OriginAgentAutonomous, issues.OriginAll:
			v := raw
			args.OriginGroup = &v
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"origin must be one of human-driven, agent-autonomous, all", nil)
			return
		}
	}

	// assignee resolution: "me" → caller.UserID via the LEFT JOIN to
	// users (ua.id). Any other non-empty value is a free-form username
	// equality. assignee + assigneeUserId are mutually exclusive — we
	// reject the combination rather than silently pick one.
	rawAssignee := strings.TrimSpace(q.Get("assignee"))
	rawAssigneeUUID := strings.TrimSpace(q.Get("assigneeUserId"))
	if rawAssignee != "" && rawAssigneeUUID != "" {
		writeProblem(w, http.StatusBadRequest, "validation_error",
			"assignee and assigneeUserId are mutually exclusive", nil)
		return
	}
	if rawAssignee != "" {
		if rawAssignee == "me" {
			id := c.UserID
			args.AssigneeUserID = &id
		} else {
			v := rawAssignee
			args.Assignee = &v
		}
	}
	if rawAssigneeUUID != "" {
		id, parseErr := uuid.Parse(rawAssigneeUUID)
		if parseErr != nil {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"assigneeUserId is not a UUID", nil)
			return
		}
		args.AssigneeUserID = &id
	}

	// createdBy=me / createdBy=not-me — see /projects/{id}/issues for the
	// rationale on rejecting raw UUIDs (we don't want a knob that bypasses
	// our visibility model when used across visible projects). "not-me"
	// powers the "On me" cross-project lens.
	if raw := strings.TrimSpace(q.Get("createdBy")); raw != "" {
		id := c.UserID
		switch raw {
		case "me":
			args.CreatedBy = &id
		case "not-me":
			args.CreatedByExclude = &id
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"createdBy must be \"me\" or \"not-me\"", nil)
			return
		}
	}

	if raw := strings.TrimSpace(q.Get("acceptedAt")); raw != "" {
		t := true
		switch raw {
		case "null":
			args.AcceptedAtNull = &t
		case "not-null":
			args.AcceptedAtNotNull = &t
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"acceptedAt accepts only the literal values \"null\" or \"not-null\"", nil)
			return
		}
	}

	// sk-31: derived blocked-ness filter; same shape as the project-
	// scoped path. Strict whitelist.
	if raw := strings.TrimSpace(q.Get("isBlocked")); raw != "" {
		switch raw {
		case "true":
			t := true
			args.IsBlocked = &t
		case "false":
			f := false
			args.IsBlocked = &f
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_isBlocked",
				"isBlocked must be true or false", nil)
			return
		}
	}

	if raw := strings.TrimSpace(q.Get("sortBy")); raw != "" {
		switch raw {
		case "updated", "created":
			args.SortBy = raw
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"sortBy must be \"updated\" or \"created\"", nil)
			return
		}
	}

	if cursor := strings.TrimSpace(q.Get("cursor")); cursor != "" {
		args.Cursor = &cursor
	}

	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 500 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit",
				"limit must be in [1, 500]", nil)
			return
		}
		args.Limit = n
	}

	res, err := h.issuesStore.ListAllIssues(ctx, args)
	if err != nil {
		slog.ErrorContext(ctx, "list all issues failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Tree handles GET /api/v1/issues/tree — sk-36 cross-project tree
// view. Visibility-filtered to the caller's project_members horizon
// (admins see all). Query param shape matches the project-scoped
// /projects/{pid}/issues/tree:
//
//	types[]   — repeated, validated, defaults to ['parent-child']
//	rootId    — optional anchor; when omitted the forest is every
//	            visible issue with no outgoing parent-child edge
//	includeBlockers — optional, exactly `true` or `false` (sk-421.2.8
//	            R-8, parseTreeIncludeBlockers). `true` adds `blockers` to
//	            every visible blocked row; absent, empty or `false` leaves
//	            the response as it was
//
// Cross-project hidden ancestors (any issue reachable from a visible
// root but residing in a project the caller can't see) surface as
// stub IssueSummary rows with Title="<hidden>" and IsReference=true
// so the chain doesn't break.
//
// @Summary Layered tree across every visible project
// @Description Returns a FLAT, pre-order (DFS) row list — {item, depth, parentRef, isReference, firstSeenAt} — spanning every project the caller can see (admins see all). Business meaning: the cross-project twin of GET /api/v1/projects/{projectID}/issues/tree, used where a tree must cross project boundaries.
// @Description
// @Description Algorithm: authenticate -> load the caller's visible-project horizon -> parse `types[]` (validated; empty defaults to ['parent-child']) -> take `rootId` as an optional anchor -> walk the dependency graph with a recursive CTE -> redact rows outside the horizon -> order siblings by the id rule -> emit.
// @Description
// @Description Rows the caller cannot see: a cross-project ancestor outside the horizon surfaces as a STUB — title "<hidden>", isReference=true, EMPTY status and type (sk-421.2 INV-2), keys retained. For every caller (internal and external alike) stubs are emitted after every visible sibling, so their position among visible siblings leaks nothing about their real id; among themselves they follow the id the caller receives: the real id by the id-order rule for an internal caller, the synthetic id (hidden-1 before hidden-2) for an external one. External callers also receive a prefix-free synthetic id and an empty project id, so neither a real id nor a project uuid crosses the wire.
// @Description
// @Description Blockers (sk-421.2.8, sk-423): with `includeBlockers=true` every VISIBLE row whose item.isBlocked is true carries `blockers` — one entry {id, title, status, projectId} per gating blocks / waits-for edge whose target is inside the CALLER's horizon (the same one that decides which rows are stubs), the same entries in the same order as the issue detail. An edge to an issue outside the horizon is never an entry and never makes the row blocked; the canonical visible row instead carries `unavailableBlockingLinks` — the number of such edges, whatever their status, closed and standing included — and the key is absent when that number is 0. ⚠ Contract change in sk-423: the redacted entries {id, redacted:true} / {redacted:true} of sk-421.2.8 are gone. Stubs and reference rows carry neither key; unblocked rows carry no `blockers`. Without the flag item.isBlocked follows the same rule (blockers the caller can see); with it, isBlocked of every visible row is derived from that same list. Without the parameter, empty or `false`, the response has neither key.
// @Description
// @Description Branch progress (sk-461): `item.epicProgress` is present on every VISIBLE row that has parent-child children, whatever its type — not only on epic-typed rows, as the list endpoints still do. The six numbers count the row's DIRECT children: `total` excludes visible standing children and KEEPS children outside the caller's horizon, `closed` counts only visible closed children, `unavailable` counts the out-of-horizon ones, and — since sk-489 — `done` counts the visible children closed WITH the outcome `done`, `noWork` the visible children closed with any other outcome (wont_do, duplicate, superseded), and `counted` is the denominator `total - noWork`. `closed` keeps its old meaning, «closed by status», so an existing client reading closed/total sees exactly what it saw before; a client that wants progress reads done/counted, which is the one rule every Skein surface shares. Children outside the horizon stay in `counted`: «cannot say» is not «dropped out» — the same reading of "unavailable" as `epic.unavailableChildren` on the issue card. Children are parent-child children regardless of `types[]`. A row with no children keeps the previous behaviour: an epic gets {closed:0,total:0,unavailable:0,done:0,noWork:0,counted:0}, any other type gets no key. Stubs for inaccessible rows never carry it.
// @Description
// @Description Close outcome (sk-489): a row whose issue is CLOSED carries `closeOutcome` — one of done, wont_do, duplicate, superseded — the machine-readable answer to «how did it end» that the free-text close reason never gave. On an issue that is not closed the KEY IS ABSENT rather than empty: an outcome without a close is a state the model does not have. Stubs for rows the caller may not see never carry it — it is card content about a task outside the horizon.
// @Description
// @Description Note on `rootId`: it is taken as an opaque string and is NOT parsed as a UUID, so this endpoint has no invalid_id refusal — an unknown or out-of-horizon anchor simply yields an empty forest instead of disclosing that the id exists.
// @Description
// @Description Row freshness order (sk-622.21, US-15): every row carries `rowSeq` — the issue's freshness order, taken from a global sequence on every committed write of the issue row, including writes that do not move `version` (label add/remove, component replace). For ONE issue a greater rowSeq is a later committed state; values of different issues are not comparable. A phase-2 client substitutes an event's row into a loaded row only if the event row's rowSeq is greater than the one shown. An issue not written since the phase-2 rollout (migration 000064) has no rowSeq key (absent ≡ 0, order unknown); any event row with a positive rowSeq is fresher than it. Redacted `<hidden>` rows carry no rowSeq.
// @Description
// @Description Side effects: none, the endpoint is read-only.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param types query []string false "Dependency types to recurse across; repeated. Defaults to ['parent-child']"
// @Param rootId query string false "Anchor the walk at this issue id instead of the visible forest"
// @Param maxDepth query int false "Stop the walk at this depth, counted from the seed row (depth 0); 1 returns the roots plus their direct children. Omitted means the server ceiling"
// @Param includeBlockers query bool false "Exactly true or false. true adds `blockers` (blockers the caller can see) and `unavailableBlockingLinks` (links to issues outside the caller's horizon, when > 0) to visible rows; omitted, empty or false adds neither"
// @Success 200 {object} issues.IssueTreeResult "Flat pre-order row list"
// @Failure 400 {object} map[string]any "invalid_dep_type — types[] carried an unknown value; invalid_max_depth — maxDepth is not a whole number, is below 1, or exceeds the server ceiling; invalid_include_blockers — includeBlockers is neither true nor false"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 422 {object} map[string]any "tree_too_large — the walk exceeded the node limit (extra: limit)"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/issues/tree [get]
func (h *IssuesAllHandler) Tree(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	args := issues.IssueTreeArgs{
		Caller: c.UserID,
		Scope:  authz.FromContext(ctx),
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("rootId")); raw != "" {
		args.RootID = &raw
	}

	types, ok := parseTreeTypes(w, r)
	if !ok {
		return
	}
	args.Types = types

	maxDepth, ok := parseTreeMaxDepth(w, r)
	if !ok {
		return
	}
	args.MaxDepth = maxDepth

	includeBlockers, ok := parseTreeIncludeBlockers(w, r)
	if !ok {
		return
	}
	args.IncludeBlockers = includeBlockers

	res, err := h.issuesStore.IssueTree(ctx, args)
	if err != nil {
		var tooLarge *issues.ErrTreeTooLarge
		if errors.As(err, &tooLarge) {
			writeProblem(w, http.StatusUnprocessableEntity, "tree_too_large",
				"tree exceeds node limit; narrow the filters",
				map[string]any{"limit": tooLarge.Limit})
			return
		}
		slog.ErrorContext(ctx, "issues tree (all) failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
