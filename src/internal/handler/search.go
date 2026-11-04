package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"skein-api/internal/issues"
	"skein-api/internal/search"
)

// SearchHandler is the thin HTTP front for the search.Service. It owns
// no state beyond the service reference; the service holds the LRU cache
// and the embedder, both of which are process-singletons wired in main().
type SearchHandler struct {
	svc *search.Service
}

// NewSearchHandler returns a handler wrapping the given service.
func NewSearchHandler(svc *search.Service) *SearchHandler {
	return &SearchHandler{svc: svc}
}

// Search handles GET /api/v1/search.
//
// Query parameters:
//
//	q         (required, 1..500 UTF-8 bytes) — search query string
//	project   UUID — narrow to a single project (caller still must
//	                 have access)
//	kind      CSV  — restrict to {issue,comment,project,component}.
//	                 Default: all four.
//	limit     int  — page size, 1..100 (default 50)
//	cursor    str  — opaque continuation for the same query, caller,
//	                 visibility and ranking mode; returned by the previous response
//	status    CSV  — issue status filter (issue results only)
//	priority  CSV  — issue priority filter (0..4)
//	type      CSV  — issue type filter
//	label     CSV  — issue label filter (any-of)
//	source    str  — issue origin filter, one of {human, agent_directed,
//	                 agent_autonomous} (Phase 1 ui-redesign — the
//	                 legacy "agent" enum was split into directed
//	                 (user-asked) and autonomous (side-quest) variants)
//	commentAuthorKind CSV — comment author-kind filter (only restricts
//	                 COMMENT results; any-of {human, agent_directed,
//	                 agent_autonomous}). Independent of `source` — it
//	                 never affects issue results.
//	assignee  str  — literal me resolves the caller; other handles match
//	                 case-insensitively (issue results only)
//	createdBy str  — me selects the existing Delegated lens; not-me excludes
//	                 issues created by the caller
//	acceptedAt str — null or not-null (issue results only)
//	isBlocked bool — reader-visible blockedness (issue results only)
//	componentId UUID — issue has this component (issue results only)
//	origin CSV — human-driven, agent-autonomous, all; independent of source
//
// Responses:
//
//	200 — search.Result JSON. `degraded: true` identifies an FTS-only
//	      ranking, including continuation after the embedder recovers.
//	400 — invalid query / cursor / filter; invalid_cursor also covers
//	      a legacy cursor whose cached pool expired, or a hybrid cursor
//	      whose missing pool cannot be rebuilt during an embedder outage.
//	      Start again without cursor in those cases.
//	401 — caller not authenticated (RequireUser)
//	422 — business validation (q too long, q empty)
//
// Mode-aware cursors rebuild a missing pool at their saved offset. FTS
// continuations keep FTS ranking; hybrid continuations never silently switch
// ranking. Legacy {key,offset} cursors remain usable while their pool is
// warm. These are continuations over current data, not database snapshots:
// edits between pages may still change membership or rank. No 5xx is returned
// for an Ollama outage.
// Queries including issues with the new ownership/assignee/acceptance/blocked/
// component/origin constraints instead refresh cursorless membership and issue
// an opaque snapshot cursor. An old warm snapshot remains unchanged by a new
// first page; on its expiry or eviction the caller gets invalid_cursor and
// restarts at the first page with the same filters. Other kinds alone and
// legacy queries without those selectors retain their existing cache policy.
//
// @Summary Search visible issues, comments, projects and components
// @Description Runs ID/numeric lookup and hybrid full-text/vector retrieval over the caller's visible projects. Scope and issue filters are applied before each candidate limit; ranking is never replaced by client-side filtering.
// @Description Status, priority, type, label, source, origin, assignee, createdBy, acceptedAt, isBlocked and componentId restrict only issue results; other kinds retain their own selection. human-driven includes human and agent_directed. source and origin intersect when both are present. Blockedness counts only open, in-progress or deferred blockers the caller can see. An unknown component UUID yields no matching issues.
// @Description createdBy=me preserves the ordinary list's Delegated selection: the assignee is set and is not delivered to the caller, and the caller either created the issue or is a previous assignee who did not dismiss it. Dismissal narrows only the previous-assignee branch; authors keep their existing branch. As on the ordinary list, a principal without a username uses the strict creator-only fallback. createdBy=not-me strictly excludes the caller as creator; combine it with assignee=me and acceptedAt=not-null for On me.
// @Description Cursors continue the same caller, visibility, query, filters and ranking. A changed filter refuses the cursor. On an embedding outage a new search degrades to FTS; a hybrid continuation whose pool cannot be rebuilt returns invalid_cursor. This is a pure read with no events or other side effects.
// @Description When issue results use ownership, assignee, acceptance, blocked, component or origin selectors, a cursorless request reads fresh membership and paged results carry an opaque snapshot generation. New first pages never replace already-issued warm pools. A scoped snapshot that expires, is evicted or is missing on another replica returns 400 invalid_cursor: restart from the first page with the same query and filters. Legacy queries without those issue selectors retain mode-aware cache-miss rebuilding.
// @Tags Search
// @Produce json
// @Security ApiKeyAuth
// @Param q query string true "Search text, 1..500 UTF-8 bytes"
// @Param project query string false "Project UUID; caller visibility still applies"
// @Param space query string false "Space short id; unknown or invisible matches nothing"
// @Param kind query string false "CSV of issue, comment, project, component; omitted means all"
// @Param status query string false "Issue statuses, CSV"
// @Param priority query string false "Issue priorities 0..4, CSV"
// @Param type query string false "Issue types, CSV"
// @Param label query string false "Issue labels, CSV, any-of"
// @Param source query string false "Exact issue source" Enums(human, agent_directed, agent_autonomous)
// @Param origin query string false "Issue origin groups, CSV: human-driven, agent-autonomous, all"
// @Param assignee query string false "me resolves the caller; any other handle is a case-insensitive text match"
// @Param createdBy query string false "me: existing Delegated selection; not-me: caller is not the creator" Enums(me, not-me)
// @Param acceptedAt query string false "Issue acceptance: null or not-null" Enums(null, not-null)
// @Param isBlocked query bool false "Issue blockedness within the reader's visibility horizon"
// @Param componentId query string false "Issue component UUID; unknown or nonmatching returns no issues"
// @Param commentAuthorKind query string false "Comment author kinds, CSV: human, agent_directed, agent_autonomous; does not filter issues"
// @Param limit query int false "Page size 1..100, default 50"
// @Param cursor query string false "Opaque continuation from the previous response"
// @Success 200 {object} search.Result "Ranked page with cursor and degraded flag"
// @Failure 400 {object} map[string]any "invalid_project, invalid_limit, invalid_priority, invalid_source, invalid_origin, invalid_created_by, invalid_accepted_at, invalid_isBlocked, invalid_component_id, invalid_comment_author_kind or invalid_cursor"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 408 {object} map[string]any "request_cancelled"
// @Failure 422 {object} map[string]any "invalid_query or invalid_kind"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/search [get]
func (h *SearchHandler) Search(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	q := r.URL.Query()

	// Required: q.
	queryStr := strings.TrimSpace(q.Get("q"))
	if queryStr == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_query",
			"q is required", nil)
		return
	}
	if len(queryStr) > 500 {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_query",
			"q must be at most 500 characters", nil)
		return
	}

	// Optional: project UUID.
	var projectID *uuid.UUID
	if raw := strings.TrimSpace(q.Get("project")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_project",
				"project must be a UUID", nil)
			return
		}
		projectID = &id
	}

	// Optional: kind CSV.
	kinds, kerr := parseKinds(q.Get("kind"))
	if kerr != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_kind",
			kerr.Error(), nil)
		return
	}

	// Optional: limit.
	limit := 50
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit",
				"limit must be in [1, 100]", nil)
			return
		}
		limit = n
	}

	// Optional: cursor (opaque). We don't validate here — the service
	// will surface a clean error from DecodeCursor.
	cursor := strings.TrimSpace(q.Get("cursor"))

	// Issue-only filters.
	filters := search.Filters{}
	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		filters.Statuses = splitCSV(raw)
	}
	if raw := strings.TrimSpace(q.Get("priority")); raw != "" {
		ps, err := parsePriorityCSV(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_priority",
				err.Error(), nil)
			return
		}
		filters.Priorities = ps
	}
	if raw := strings.TrimSpace(q.Get("type")); raw != "" {
		filters.Types = splitCSV(raw)
	}
	if raw := strings.TrimSpace(q.Get("label")); raw != "" {
		filters.Labels = splitCSV(raw)
	}
	// source: closed set {human, agent_directed, agent_autonomous}.
	// Whitelisted here so the store can inline the value without
	// revalidating. Invalid input → 400 so monitoring doesn't see
	// "search degraded" alarms for typos.
	if raw := strings.TrimSpace(q.Get("source")); raw != "" {
		switch raw {
		case issues.SourceHuman, issues.SourceAgentDirected, issues.SourceAgentAutonomous:
			v := raw
			filters.Source = &v
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_source",
				"source must be one of: human, agent_directed, agent_autonomous", nil)
			return
		}
	}
	if raw := strings.TrimSpace(q.Get("assignee")); raw != "" {
		if raw == "me" {
			id := c.UserID
			filters.AssigneeUserID = &id
		} else {
			filters.Assignee = &raw
		}
	}
	if raw := strings.TrimSpace(q.Get("createdBy")); raw != "" {
		id := c.UserID
		switch raw {
		case "me":
			filters.CreatedBy = &id
			filters.DelegatedLens = c.Username != ""
		case "not-me":
			filters.CreatedByExclude = &id
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_created_by", "createdBy must be me or not-me", nil)
			return
		}
	}
	if raw := strings.TrimSpace(q.Get("acceptedAt")); raw != "" {
		yes := true
		switch raw {
		case "null":
			filters.AcceptedAtNull = &yes
		case "not-null":
			filters.AcceptedAtNotNull = &yes
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_accepted_at", "acceptedAt must be null or not-null", nil)
			return
		}
	}
	if raw := strings.TrimSpace(q.Get("isBlocked")); raw != "" {
		var blocked bool
		switch raw {
		case "true":
			blocked = true
		case "false":
			blocked = false
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_isBlocked", "isBlocked must be true or false", nil)
			return
		}
		filters.IsBlocked = &blocked
	}
	if raw := strings.TrimSpace(q.Get("componentId")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_component_id", "componentId must be a UUID", nil)
			return
		}
		filters.ComponentID = &id
	}
	if raw := strings.TrimSpace(q.Get("origin")); raw != "" {
		var groups []string
		for _, part := range strings.Split(raw, ",") {
			group := strings.TrimSpace(part)
			switch group {
			case issues.OriginHumanDriven, issues.OriginAgentAutonomous, issues.OriginAll:
				groups = append(groups, group)
			default:
				writeProblem(w, http.StatusBadRequest, "invalid_origin", "origin must be a CSV of human-driven, agent-autonomous, all", nil)
				return
			}
		}
		if sources, noFilter := issues.OriginGroupsToSources(groups); !noFilter {
			filters.OriginSources = sources
		}
	}
	// commentAuthorKind: CSV closed set {human, agent_directed,
	// agent_autonomous}. Comment-only filter — restricts COMMENT results by
	// comments.author_kind; it does NOT affect issue results (that is the
	// separate `source` filter). CSV style mirrors status/type/label;
	// validated here so the store can inline the values without
	// revalidating. Unknown value → 400 so monitoring doesn't alarm on a
	// typo as a "search degraded" event.
	if raw := strings.TrimSpace(q.Get("commentAuthorKind")); raw != "" {
		kinds := splitCSV(raw)
		for _, k := range kinds {
			switch k {
			case issues.CommentAuthorKindHuman,
				issues.CommentAuthorKindAgentDirected,
				issues.CommentAuthorKindAgentAutonomous:
			default:
				writeProblem(w, http.StatusBadRequest, "invalid_comment_author_kind",
					"commentAuthorKind must be a CSV of: human, agent_directed, agent_autonomous", nil)
				return
			}
		}
		filters.CommentAuthorKinds = kinds
	}

	params := search.Params{
		Query:   queryStr,
		Project: projectID,
		// sk-674.18 T031: the page filter — one space's projects only.
		Space:    strings.TrimSpace(q.Get("space")),
		Kinds:    kinds,
		Limit:    limit,
		Cursor:   cursor,
		Filters:  filters,
		CallerID: c.UserID,
	}

	result, err := h.svc.Search(ctx, params)
	if err != nil {
		// Cursor decode errors come back from DecodeCursor wrapped with
		// search.ErrInvalidCursor. Surface as 400 invalid_cursor.
		if errors.Is(err, search.ErrInvalidCursor) {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor",
				err.Error(), nil)
			return
		}
		// Client-cancelled / deadline-exceeded — not a server bug. Drop
		// without noisy error logs; the client already knows.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeProblem(w, http.StatusRequestTimeout, "request_cancelled", "", nil)
			return
		}
		// Bubble validation failures from the service.
		// TODO(R6): wire concrete validation error types that implement
		// ValidationCode() string. Today no error implements it; this
		// branch is scaffolding for future business errors.
		var verr interface{ ValidationCode() string }
		if errors.As(err, &verr) {
			writeProblem(w, http.StatusBadRequest, verr.ValidationCode(),
				err.Error(), nil)
			return
		}
		slog.ErrorContext(ctx, "search failed",
			"userId", c.UserID.String(),
			"queryLen", len(queryStr),
			"error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// LookupID handles GET /api/v1/search/lookup-id.
//
// The lightweight ID-prefix lookup endpoint powering the typeahead
// direct-jump UX in the new shell: typing `sk-14` in the tasks-column
// search box should let the user press Enter and navigate to the issue
// without round-tripping through the full /search pipeline.
//
// Query parameters:
//
//	q       (required, 1..500 chars) — query string; the SQL self-gates
//	                                   on the id-prefix regex, so free
//	                                   text yields an empty items array
//	                                   with HTTP 200, not an error.
//	project UUID — narrow to a single project (caller still must have
//	               access to it; admins bypass the membership check).
//
// Responses:
//
//	200 — {"items": [<search.Item>]} — empty array when the query does
//	      not look like an entity id or matches nothing. The Item shape
//	      mirrors GET /api/v1/search exactly, so the frontend can share
//	      the SearchItem TypeScript type between the two endpoints.
//	400 — invalid project (not a UUID)
//	401 — caller not authenticated (RequireUser)
//	422 — q missing or too long
func (h *SearchHandler) LookupID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	q := r.URL.Query()

	queryStr := strings.TrimSpace(q.Get("q"))
	if queryStr == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_query",
			"q is required", nil)
		return
	}
	if len(queryStr) > 500 {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_query",
			"q must be at most 500 characters", nil)
		return
	}

	var projectID *uuid.UUID
	if raw := strings.TrimSpace(q.Get("project")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_project",
				"project must be a UUID", nil)
			return
		}
		projectID = &id
	}

	params := search.LookupIDParams{
		Query:    queryStr,
		Project:  projectID,
		CallerID: c.UserID,
	}

	items, err := h.svc.LookupID(ctx, params)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeProblem(w, http.StatusRequestTimeout, "request_cancelled", "", nil)
			return
		}
		slog.ErrorContext(ctx, "search lookup-id failed",
			"userId", c.UserID.String(),
			"queryLen", len(queryStr),
			"error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	writeJSON(w, http.StatusOK, lookupIDResponse{Items: items})
}

// lookupIDResponse wraps LookupID / LookupIDBatch results. Typed so the
// wire shape stays `{"items":[...]}` without map[string]any boxing.
type lookupIDResponse struct {
	Items []search.Item `json:"items"`
}

// lookupIDBatchRequest is the POST body for /api/v1/search/lookup-id-batch.
type lookupIDBatchRequest struct {
	IDs []string `json:"ids"`
}

// LookupIDBatch handles POST /api/v1/search/lookup-id-batch.
//
// Resolves a batch of EXACT issue ids in one round-trip, visibility-
// filtered, returning the found rows. Backs the "paste a blob of text →
// extract task codes → render the tasks" frontend surface.
//
// Request body:
//
//	{"ids": ["sk-44", "demo-42.1", ...]} — ids are deduped + lowercased
//	server-side and capped at 100. Matching is EXACT id (not prefix);
//	issues only (not projects/comments/components).
//
// Responses:
//
//	200 — {"items": [<search.Item>]} — the SAME Item shape as
//	      /api/v1/search and /api/v1/search/lookup-id. Contains ONLY the
//	      ids that were found AND visible to the caller. Order is not
//	      guaranteed; any requested id absent from items = "not found".
//	400 — body does not parse, or `ids` is missing / empty / not an
//	      array / contains only blanks, or more than 100 ids supplied.
//	401 — caller not authenticated (RequireUser)
func (h *SearchHandler) LookupIDBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	var req lookupIDBatchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", err.Error(), nil)
		return
	}
	if req.IDs == nil {
		writeProblem(w, http.StatusBadRequest, "invalid_ids",
			"ids is required and must be a non-empty array", nil)
		return
	}

	// The 100-cap is enforced on the RAW array length (pre-dedupe) — an
	// oversized payload is rejected regardless of how many unique ids
	// survive dedupe/blank-strip, since the cap exists to bound the
	// request body, not the effective query. >100 raw ids → explicit 400
	// (too_many_ids) so the client learns it overshot rather than getting
	// a silently-clamped result. Dedupe + lowercase + blank-strip happen
	// AFTER this check; an all-blank/empty surviving set is a separate 400
	// below.
	if len(req.IDs) > search.LookupIDsBatchMax {
		writeProblem(w, http.StatusBadRequest, "too_many_ids",
			"at most 100 ids may be supplied", nil)
		return
	}

	seen := make(map[string]struct{}, len(req.IDs))
	ids := make([]string, 0, len(req.IDs))
	for _, raw := range req.IDs {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_ids",
			"ids is required and must be a non-empty array", nil)
		return
	}

	items, err := h.svc.LookupIDsBatch(ctx, ids, c.UserID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeProblem(w, http.StatusRequestTimeout, "request_cancelled", "", nil)
			return
		}
		slog.ErrorContext(ctx, "search lookup-id-batch failed",
			"userId", c.UserID.String(),
			"idCount", len(ids),
			"error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	writeJSON(w, http.StatusOK, lookupIDResponse{Items: items})
}

// parseKinds resolves the comma-separated `kind` parameter.  Empty → all
// four canonical kinds. Unknown kind → error.
func parseKinds(raw string) ([]search.Kind, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return search.AllKinds, nil
	}
	seen := make(map[search.Kind]struct{})
	out := make([]search.Kind, 0, 4)
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !search.IsValidKind(p) {
			return nil, errors.New("kind must be one of: issue, comment, project, component")
		}
		k := search.Kind(p)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	if len(out) == 0 {
		return search.AllKinds, nil
	}
	return out, nil
}

// splitCSV is a trivial CSV splitter that drops empty entries and trims
// whitespace.  Used for status/type/label filters where the values are
// caller-supplied strings; downstream SQL passes them as ANY($1).
func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parsePriorityCSV converts a comma-separated list of priority numerics
// into ints with bounds 0..4.
func parsePriorityCSV(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 4 {
			return nil, errors.New("priority must be a CSV of integers in [0, 4]")
		}
		out = append(out, n)
	}
	return out, nil
}
