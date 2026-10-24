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
//	q         (required, 1..500 chars) — search query string
//	project   UUID — narrow to a single project (caller still must
//	                 have access)
//	kind      CSV  — restrict to {issue,comment,project,component}.
//	                 Default: all four.
//	limit     int  — page size, 1..100 (default 50)
//	cursor    str  — opaque, returned by previous response
//	status    CSV  — issue status filter (only honoured when kind = issue)
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
//
// Responses:
//
//	200 — search.Result JSON.  `degraded: true` means Ollama was
//	      unavailable and results came from the FTS-only fallback.
//	400 — invalid query / cursor / filter
//	401 — caller not authenticated (RequireUser)
//	422 — business validation (q too long, q empty)
//
// We never return 5xx for an Ollama outage — `degraded` carries the
// signal.
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
