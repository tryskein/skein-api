package publicapi

// issues_read.go — the reads of issues of the public API (spec sk-878.3.1,
// "API surface"): listIssues, listReadyIssues, getIssue, listIssueEvents,
// listDependencies, listComments, getIssueMovePlan and search. Each reads
// through the store methods the MCP tool of the same name reads through,
// with the same visibility: an issue whose project is out of the key's
// scope answers exactly as a missing one (INV-2), and an edge to such an
// issue is redacted, as for a project the owner does not see (US-3).

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/errcode"
	"skein-api/internal/issues"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/projects"
	"skein-api/internal/search"
)

// The operationIds of the issue reads.
const (
	OperationListIssues       = "listIssues"
	OperationListReadyIssues  = "listReadyIssues"
	OperationGetIssue         = "getIssue"
	OperationListIssueEvents  = "listIssueEvents"
	OperationListDependencies = "listDependencies"
	OperationListComments     = "listComments"
	OperationGetIssueMovePlan = "getIssueMovePlan"
	OperationSearch           = "search"

	// OperationCreateIssue and OperationCloseIssue are the operations a move
	// plan prepares.
	OperationCreateIssue = "createIssue"
	OperationCloseIssue  = "closeIssue"
)

// The ranges of limit= of the issue lists.
var (
	issueListLimit = Limit{Min: 1, Max: issues.ListIssuesMaxLimit, Default: tools.ListLimitDefault}
	readyLimit     = Limit{Min: 1, Max: 100, Default: 25}
	eventListLimit = Limit{Min: 1, Max: 100, Default: 20}
	commentLimit   = Limit{Min: 1, Max: 100, Default: 50}
	searchLimit    = Limit{Min: 1, Max: search.MaxPageLimit, Default: tools.SearchLimitDefault}
)

// issuesAPI serves the issue resources.
type issuesAPI struct {
	pool     *pgxpool.Pool
	issues   *issues.Store
	deps     *dependencies.Store
	projects *projects.Store
	search   *search.Service
	embedder issues.QueryEmbedder
}

// writeIssueNotFound answers an issue that does not exist or is out of
// reach, named by the path or referenced by the request.
func writeIssueNotFound(w http.ResponseWriter, origin Origin, id string) {
	writeCode(w, errcode.IssueNotFound, origin, "issue does not exist or is not visible", map[string]any{"issueId": id})
}

// pathIssueID is the {issue} of the path: a hierarchical id. A value that is
// not one names no issue — 404.
func pathIssueID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(chi.URLParam(r, "issue"))
	if id == "" || !strings.Contains(id, "-") {
		writeIssueNotFound(w, OriginPath, id)
		return "", false
	}
	return id, true
}

// visibleIssue reads the issue of the path and checks that its project is
// visible to the caller within the key's scope — the gate of skein_show. A
// missing issue and one out of reach are the same 404.
func (a *issuesAPI) visibleIssue(w http.ResponseWriter, r *http.Request) (*issues.IssueDetail, bool) {
	id, ok := pathIssueID(w, r)
	if !ok {
		return nil, false
	}
	issue, err := a.issues.GetIssue(r.Context(), id)
	if errors.Is(err, issues.ErrNotFound) {
		writeIssueNotFound(w, OriginPath, id)
		return nil, false
	}
	if err != nil {
		WriteInternalError(w, r, "reading an issue", err)
		return nil, false
	}
	_, err = projects.EnsureProjectVisible(r.Context(), a.pool, issue.ProjectID, callerOf(r))
	if errors.Is(err, projects.ErrProjectNotVisible) {
		writeIssueNotFound(w, OriginPath, id)
		return nil, false
	}
	if err != nil {
		WriteInternalError(w, r, "checking an issue's visibility", err)
		return nil, false
	}
	return issue, true
}

// referencedProject resolves a project named by a query parameter, as the
// MCP tools resolve a project argument: UUID, prefix, alias, name or a
// confident fuzzy match among the projects within reach. The refusals are
// the MCP tools' codes from the origin of a reference (422).
func (a *issuesAPI) referencedProject(w http.ResponseWriter, r *http.Request, handle string) (uuid.UUID, bool) {
	id, err := tools.ResolveProjectHandle(r.Context(), a.pool, a.projects, handle, callerOf(r))
	if err == nil {
		return id, true
	}
	var val *tools.ValidationProjectError
	var ambig *tools.AmbiguousProjectError
	var missing *tools.ProjectNotFoundError
	switch {
	case errors.As(err, &val):
		writeCode(w, errcode.ValidationError, OriginParse, val.Message, map[string]any{"field": val.Field})
	case errors.As(err, &ambig):
		writeCode(w, errcode.AmbiguousProject, OriginDomain, "project handle matched multiple projects — pick one and re-call",
			map[string]any{"q": ambig.Query, "candidates": ambig.Candidates})
	case errors.As(err, &missing):
		writeCode(w, errcode.ProjectNotFound, OriginBody, "project does not exist or is not visible", map[string]any{"q": missing.Query})
	default:
		WriteInternalError(w, r, "resolving a project handle", err)
	}
	return uuid.Nil, false
}

// query is the parsed query of a request: one value per parameter.
type query struct {
	w   http.ResponseWriter
	r   *http.Request
	bad bool
}

// one returns the single value of name; false when absent. A repeated
// parameter is refused (400).
func (q *query) one(name string) (string, bool) {
	vals, ok := q.r.URL.Query()[name]
	if !ok || q.bad {
		return "", false
	}
	if len(vals) != 1 {
		q.fail(name + " must be given once")
		return "", false
	}
	return strings.TrimSpace(vals[0]), true
}

// list returns the comma-separated values of name.
func (q *query) list(name string) []string {
	v, ok := q.one(name)
	if !ok || v == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// fail answers 400 validation_error once.
func (q *query) fail(detail string) {
	if !q.bad {
		q.bad = true
		WriteProblem(q.w, Problem{Code: errcode.ValidationError, Origin: OriginParse, Detail: detail})
	}
}

// choice checks v against the allowed values.
func (q *query) choice(name, v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	q.fail(name + " must be one of " + strings.Join(allowed, ", "))
	return false
}

// listIssuesArgs are the filters of listIssues shared by its two modes.
type listIssuesFilters struct {
	assignee          *string
	assigneeMe        bool
	createdBy         *uuid.UUID
	createdByExclude  *uuid.UUID
	acceptedNull      *bool
	acceptedNotNull   *bool
	origin            string
	isBlocked         *bool
	sortBy            string
	limit             int
	cursor            string
	cursorTime        time.Time
	cursorID          string
	statuses          []string
	projectOnlyFilter []string
}

// listIssues lists issues of one project or of every project within reach.
//
// @Summary List issues
// @Description Returns a page of issues — the same list as the MCP tool skein_list, in its two modes:
// @Description - with project (a UUID, prefix, alias or name of a project within reach): the issues of that project, newest change first, with the filters statuses, priority, assignee, createdBy, acceptedAt, origin, parentId, pinned, source, text, labels, isBlocked;
// @Description - without project: the issues of every project the key's owner sees within the key's scope, with q (full-text search over the title, the description, the acceptance criteria, the design and the notes, ordered by relevance), space, one status, type, assignee, createdBy, acceptedAt, origin, isBlocked.
// @Description A filter of the other mode is refused, never ignored. Each item is the issue's summary: its fields, labels, components, assignee, its lock holder and whether it is blocked — counted over the blockers the caller sees and the hidden ones of his spaces, as everywhere (US-3). Keyset paging: pass nextCursor back as cursor while hasMore is true. Read-only.
// @Tags Issues
// @Produce json
// @Security SkeinApiKey[read]
// @Param project query string false "Project handle: UUID, prefix, alias or name. Omitted: every project within reach"
// @Param q query string false "Full-text query (without project only)"
// @Param space query string false "Short id of a space (without project only)"
// @Param type query string false "Issue type (without project only)"
// @Param statuses query string false "Comma-separated statuses: open, in_progress, deferred, closed, standing; at most one without project"
// @Param priority query int false "Exact priority 0..4 (with project only)"
// @Param assignee query string false "Assignee username, or me"
// @Param createdBy query string false "me or not-me"
// @Param acceptedAt query string false "null or not-null"
// @Param origin query string false "human-driven, agent-autonomous or all"
// @Param parentId query string false "Exact parent id (with project only)"
// @Param pinned query bool false "The pinned flag (with project only)"
// @Param source query string false "Exact source: human, agent_directed, agent_autonomous or api (with project only)"
// @Param text query string false "Title substring (with project only)"
// @Param labels query string false "Comma-separated labels, any of them (with project only)"
// @Param isBlocked query string false "true or false"
// @Param sortBy query string false "updated (default) or created"
// @Param limit query int false "Page size, 1..100 (default 25)"
// @Param cursor query string false "The nextCursor of the previous page"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[issues.IssueSummary] "A page of issues"
// @Failure 400 {object} ProblemDocument "Unknown or repeated parameter, a value outside its set, a filter of the other mode, limit out of range, a foreign cursor (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "The project of the filter is not within reach (code project_not_found) or ambiguous (code ambiguous_project)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues [get]
// @ID listIssues
func (a *issuesAPI) listIssues(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "project", "q", "space", "type", "statuses", "priority", "assignee", "createdBy",
		"acceptedAt", "origin", "parentId", "pinned", "source", "text", "labels", "isBlocked", "sortBy", "limit", "cursor") {
		return
	}
	q := &query{w: w, r: r}
	f, ok := a.listFilters(q)
	if !ok {
		return
	}
	ctx := r.Context()
	callerID := callerOf(r)
	handle, inProject := q.one("project")
	if q.bad {
		return
	}
	var res *issues.ListResult
	var err error
	if inProject {
		for _, name := range []string{"q", "space", "type"} {
			if _, sent := r.URL.Query()[name]; sent {
				q.fail(name + " applies only without project")
				return
			}
		}
		args, ok := a.projectListArgs(q, f)
		if !ok {
			return
		}
		projectID, ok := a.referencedProject(w, r, handle)
		if !ok {
			return
		}
		args.ProjectID = projectID
		res, err = a.issues.ListIssues(ctx, args)
	} else {
		for _, name := range f.projectOnlyFilter {
			q.fail(name + " applies only with project")
			return
		}
		args := issues.ListAllIssuesArgs{Caller: callerID, Scope: authz.FromContext(ctx), Embedder: a.embedder}
		args.Space, _ = q.one("space")
		args.Q, _ = q.one("q")
		if t, sent := q.one("type"); sent && t != "" {
			canon, verr := issues.ValidateType(t)
			if verr != nil {
				q.fail(verr.Error())
				return
			}
			args.Type = canon
		}
		if len(f.statuses) > 1 {
			q.fail("without project, statuses takes a single status")
			return
		} else if len(f.statuses) == 1 {
			args.Status = f.statuses[0]
		}
		if f.origin != "" {
			o := f.origin
			args.OriginGroup = &o
		}
		if f.assigneeMe {
			id := callerID
			args.AssigneeUserID = &id
		} else {
			args.Assignee = f.assignee
		}
		args.CreatedBy, args.CreatedByExclude = f.createdBy, f.createdByExclude
		args.AcceptedAtNull, args.AcceptedAtNotNull = f.acceptedNull, f.acceptedNotNull
		args.IsBlocked, args.SortBy, args.Limit = f.isBlocked, f.sortBy, f.limit
		if f.cursor != "" {
			c := f.cursor
			args.Cursor = &c
		}
		if q.bad {
			return
		}
		res, err = a.issues.ListAllIssues(ctx, args)
	}
	if err != nil {
		WriteInternalError(w, r, "listing issues", err)
		return
	}
	items := res.Items
	if items == nil {
		items = []issues.IssueSummary{}
	}
	WriteJSON(w, r, http.StatusOK, List[issues.IssueSummary]{Items: items, NextCursor: res.NextCursor, HasMore: res.HasMore})
}

// listFilters reads the filters of both modes of listIssues, as skein_list
// reads them.
func (a *issuesAPI) listFilters(q *query) (listIssuesFilters, bool) {
	var f listIssuesFilters
	callerID := callerOf(q.r)
	for _, s := range q.list("statuses") {
		v, err := issues.ValidateStatus(s)
		if err != nil {
			q.fail(err.Error())
			return f, false
		}
		f.statuses = append(f.statuses, v)
	}
	if v, ok := q.one("assignee"); ok && v != "" {
		if v == "me" {
			f.assigneeMe = true
		} else {
			f.assignee = &v
		}
	}
	if v, ok := q.one("createdBy"); ok && q.choice("createdBy", v, "me", "not-me") {
		id := callerID
		if v == "me" {
			f.createdBy = &id
		} else {
			f.createdByExclude = &id
		}
	}
	if v, ok := q.one("acceptedAt"); ok && q.choice("acceptedAt", v, "null", "not-null") {
		t := true
		if v == "null" {
			f.acceptedNull = &t
		} else {
			f.acceptedNotNull = &t
		}
	}
	if v, ok := q.one("origin"); ok && q.choice("origin", v, issues.OriginHumanDriven, issues.OriginAgentAutonomous, issues.OriginAll) {
		f.origin = v
	}
	if v, ok := q.one("isBlocked"); ok && q.choice("isBlocked", v, "true", "false") {
		b := v == "true"
		f.isBlocked = &b
	}
	if v, ok := q.one("sortBy"); ok && q.choice("sortBy", v, "updated", "created") {
		f.sortBy = v
	}
	for _, name := range []string{"priority", "parentId", "pinned", "source", "text", "labels"} {
		if _, sent := q.r.URL.Query()[name]; sent {
			f.projectOnlyFilter = append(f.projectOnlyFilter, name)
		}
	}
	limit, err := ParseLimit(q.r, issueListLimit)
	if err != nil {
		q.fail(err.Error())
		return f, false
	}
	f.limit = limit
	if c, ok := q.one("cursor"); ok && c != "" {
		t, id, err := issues.DecodeCursor(c)
		if err != nil {
			q.fail(ErrBadCursor.Error())
			return f, false
		}
		f.cursor, f.cursorTime, f.cursorID = c, t, id
	}
	return f, !q.bad
}

// projectListArgs reads the filters of the project mode of listIssues.
func (a *issuesAPI) projectListArgs(q *query, f listIssuesFilters) (issues.ListIssuesArgs, bool) {
	callerID := callerOf(q.r)
	args := issues.ListIssuesArgs{Caller: &callerID, Statuses: f.statuses}
	if v, ok := q.one("priority"); ok {
		p, err := strconv.Atoi(v)
		if err != nil || issues.ValidatePriority(p) != nil {
			q.fail("priority must be an integer in [0, 4]")
			return args, false
		}
		args.Priorities = []int{p}
	}
	if f.assigneeMe {
		id := callerID
		args.AssigneeUserID = &id
	} else {
		args.Assignee = f.assignee
	}
	args.CreatedBy, args.CreatedByExclude = f.createdBy, f.createdByExclude
	args.AcceptedAtNull, args.AcceptedAtNotNull = f.acceptedNull, f.acceptedNotNull
	if f.origin != "" {
		args.OriginGroups = []string{f.origin}
	}
	if v, ok := q.one("parentId"); ok && v != "" {
		if !strings.Contains(v, "-") {
			q.fail(`parentId must be a full hierarchical id like "demo-42"`)
			return args, false
		}
		args.ParentID = &v
	}
	if v, ok := q.one("pinned"); ok && q.choice("pinned", v, "true", "false") {
		b := v == "true"
		args.Pinned = &b
	}
	if v, ok := q.one("source"); ok && v != "" {
		canon, err := issues.ValidateSource(v)
		if err != nil {
			q.fail(err.Error())
			return args, false
		}
		args.Source = &canon
	}
	if v, ok := q.one("text"); ok && v != "" {
		args.Text = &v
	}
	for _, l := range q.list("labels") {
		canon, err := issues.ValidateLabel(l)
		if err != nil {
			q.fail("labels: " + err.Error())
			return args, false
		}
		args.Labels = append(args.Labels, canon)
	}
	args.Limit, args.SortBy, args.IsBlocked = f.limit, f.sortBy, f.isBlocked
	if f.cursor != "" {
		t, id := f.cursorTime, f.cursorID
		args.CursorTime, args.CursorID = &t, &id
	}
	return args, !q.bad
}

// readyCursor is the keyset position of a page of listReadyIssues: the last
// row's priority, updated_at and id.
type readyCursor struct {
	Priority  int       `json:"p"`
	UpdatedAt time.Time `json:"u"`
	ID        string    `json:"i"`
}

// listReadyIssues lists the unblocked open issues of a project.
//
// @Summary List ready issues
// @Description Returns the open issues of a project that nothing blocks — no open blocks or waits-for edge, counting the blockers the caller cannot see (US-3) — by priority, then the most recently updated, then id: the same list as the MCP tool skein_ready. Keyset paging: pass nextCursor back as cursor while hasMore is true (skein_ready has no cursor). Read-only.
// @Tags Issues
// @Produce json
// @Security SkeinApiKey[read]
// @Param project query string true "Project handle: UUID, prefix, alias or name"
// @Param assignee query string false "Exact assignee username"
// @Param priorityMax query int false "Issues with priority <= N (0..4)"
// @Param type query string false "Exact issue type"
// @Param limit query int false "How many, 1..100 (default 25)"
// @Param cursor query string false "The nextCursor of the previous page"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[dependencies.IssueSummary] "The ready issues"
// @Failure 400 {object} ProblemDocument "project missing, an unknown parameter, a value out of range, a cursor this list did not issue (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "The project is not within reach (code project_not_found) or ambiguous (code ambiguous_project)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/ready [get]
// @ID listReadyIssues
func (a *issuesAPI) listReadyIssues(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "project", "assignee", "priorityMax", "type", "limit", "cursor") {
		return
	}
	q := &query{w: w, r: r}
	handle, ok := q.one("project")
	if q.bad {
		return
	}
	if !ok || handle == "" {
		q.fail("project is required")
		return
	}
	var filters dependencies.ReadyFilters
	if v, ok := q.one("assignee"); ok && v != "" {
		filters.Assignee = &v
	}
	if v, ok := q.one("priorityMax"); ok {
		p, err := strconv.Atoi(v)
		if err != nil || p < 0 || p > 4 {
			q.fail("priorityMax must be in [0, 4]")
			return
		}
		filters.Priority = &p
	}
	if v, ok := q.one("type"); ok && v != "" {
		canon, err := issues.ValidateType(v)
		if err != nil {
			q.fail(err.Error())
			return
		}
		filters.Type = &canon
	}
	limit, err := ParseLimit(r, readyLimit)
	if err != nil {
		q.fail(err.Error())
		return
	}
	if c, ok := q.one("cursor"); ok && c != "" {
		var cur readyCursor
		if err := DecodeCursor(c, &cur); err != nil {
			WriteParseError(w, err)
			return
		}
		filters.After = &dependencies.ReadyPosition{Priority: cur.Priority, UpdatedAt: cur.UpdatedAt, ID: cur.ID}
	}
	if q.bad {
		return
	}
	projectID, ok := a.referencedProject(w, r, handle)
	if !ok {
		return
	}
	items, err := a.deps.ListReady(r.Context(), projectID, filters, limit+1)
	if err != nil {
		WriteInternalError(w, r, "listing ready issues", err)
		return
	}
	out := List[dependencies.IssueSummary]{Items: items}
	if len(items) > limit {
		last := items[limit-1]
		out.Items, out.HasMore = items[:limit], true
		c, err := EncodeCursor(readyCursor{Priority: last.Priority, UpdatedAt: last.UpdatedAt, ID: last.ID})
		if err != nil {
			WriteInternalError(w, r, "encoding a cursor", err)
			return
		}
		out.NextCursor = &c
	}
	if out.Items == nil {
		out.Items = []dependencies.IssueSummary{}
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// getIssue reads one issue.
//
// @Summary Get an issue
// @Description Returns the issue named by the path — its hierarchical id — with every field, its labels, components, commits, its lock holder (the MCP session or the REST executor api:<keyId>[:<workerId>] holding it) and its blockers as the caller sees them. The ETag header is the issue's version: send it as If-Match with a change. The ETag validates the version, not the representation — labels and the review flag change without a new version — so no 304 is ever answered. The same issue as the MCP tool skein_show; an issue out of reach answers as a missing one. Read-only.
// @Tags Issues
// @Produce json
// @Security SkeinApiKey[read]
// @Param issue path string true "The issue's hierarchical id, e.g. sk-42"
// @Param fields query string false "Comma-separated fields to return (plus id)"
// @Success 200 {object} issues.IssueDetail "The issue; ETag is its version"
// @Header 200 {string} ETag "The version of the issue, as a strong entity tag"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue} [get]
// @ID getIssue
func (a *issuesAPI) getIssue(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	issue, ok := a.visibleIssue(w, r)
	if !ok {
		return
	}
	SetETag(w, issue.Version)
	WriteJSON(w, r, http.StatusOK, issue)
}

// eventCursor is the position of a page of events: the id of its last
// event.
type eventCursor struct {
	Before int64 `json:"b"`
}

// listIssueEvents lists the events of an issue.
//
// @Summary List the events of an issue
// @Description Returns the events of the issue named by the path, newest first — creation, every field and status change, claims, closes, links, comments — each with its time, type, the actor, the actor's type (human or agent; a write by key is an agent's) and session, the field it changed and its reason. The same events as the events block of the MCP tool skein_show. Keyset paging: pass nextCursor back as cursor while hasMore is true. Read-only.
// @Tags Issues
// @Produce json
// @Security SkeinApiKey[read]
// @Param issue path string true "The issue's hierarchical id"
// @Param limit query int false "Page size, 1..100 (default 20)"
// @Param cursor query string false "The nextCursor of the previous page"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[issues.EventDTO] "A page of events"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field, limit out of range, a foreign cursor (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/events [get]
// @ID listIssueEvents
func (a *issuesAPI) listIssueEvents(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "limit", "cursor") {
		return
	}
	limit, err := ParseLimit(r, eventListLimit)
	if err != nil {
		WriteParseError(w, err)
		return
	}
	var before *int64
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur eventCursor
		if err := DecodeCursor(c, &cur); err != nil {
			WriteParseError(w, err)
			return
		}
		before = &cur.Before
	}
	issue, ok := a.visibleIssue(w, r)
	if !ok {
		return
	}
	events, err := a.issues.ListEventsPage(r.Context(), issue.ID, before, limit+1)
	if err != nil {
		WriteInternalError(w, r, "listing events", err)
		return
	}
	out := List[issues.EventDTO]{Items: events}
	if len(events) > limit {
		out.Items, out.HasMore = events[:limit], true
		c, err := EncodeCursor(eventCursor{Before: events[limit-1].ID})
		if err != nil {
			WriteInternalError(w, r, "encoding a cursor", err)
			return
		}
		out.NextCursor = &c
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// DependencyItem is one edge of an issue, seen from the issue: direction
// outgoing — the issue is its from-side; incoming — its to-side.
type DependencyItem struct {
	// Direction is outgoing or incoming.
	Direction string `json:"direction"`
	dependencies.DependencyEdge
}

// listDependencies lists the edges of an issue.
//
// @Summary List the dependencies of an issue
// @Description Returns the edges of the issue named by the path — blocks, waits-for, related, discovered-from, supersedes, duplicates, parent-child — outgoing (the issue is the from-side) then incoming, each with the other end's summary. An edge whose other end the caller cannot see within the key's scope is returned redacted: no issue, a synthetic hidden-N id — as for a project the owner does not see (US-3). type and direction narrow the list. The same edges as the deps block of the MCP tool skein_show. Not paged: every edge, nextCursor is always null. Read-only.
// @Tags Issues
// @Produce json
// @Security SkeinApiKey[read]
// @Param issue path string true "The issue's hierarchical id"
// @Param type query string false "Edge type: blocks, waits-for, related, discovered-from, supersedes, duplicates, parent-child"
// @Param direction query string false "outgoing, incoming or both (default)"
// @Param fields query string false "Comma-separated fields of each item to return"
// @Success 200 {object} List[DependencyItem] "The edges"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field, a type or direction outside its set (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/dependencies [get]
// @ID listDependencies
func (a *issuesAPI) listDependencies(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "type", "direction") {
		return
	}
	q := &query{w: w, r: r}
	typ, _ := q.one("type")
	if typ != "" {
		canon, err := dependencies.ValidateType(typ)
		if err != nil {
			q.fail(err.Error())
		}
		typ = canon
	}
	direction := "both"
	if v, ok := q.one("direction"); ok && q.choice("direction", v, "outgoing", "incoming", "both") {
		direction = v
	}
	if q.bad {
		return
	}
	issue, ok := a.visibleIssue(w, r)
	if !ok {
		return
	}
	list, err := a.deps.List(r.Context(), issue.ID, authz.FromContext(r.Context()))
	if err != nil {
		WriteInternalError(w, r, "listing dependencies", err)
		return
	}
	items := []DependencyItem{}
	add := func(dir string, edges []dependencies.DependencyEdge) {
		if direction != "both" && direction != dir {
			return
		}
		for _, e := range edges {
			if typ == "" || e.Type == typ {
				items = append(items, DependencyItem{Direction: dir, DependencyEdge: e})
			}
		}
	}
	add("outgoing", list.Outgoing)
	add("incoming", list.Incoming)
	WriteJSON(w, r, http.StatusOK, List[DependencyItem]{Items: items})
}

// commentCursor is the position of a page of comments.
type commentCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"i"`
}

// listComments lists the comments of an issue.
//
// @Summary List the comments of an issue
// @Description Returns the comments of the issue named by the path, oldest first, each with its author, the author's kind (human, agent_directed, agent_autonomous, api), its version, the mentions it makes and its edit and delete times; a deleted comment stays in the thread as a tombstone with an empty body. The same thread as the comments of the MCP tool skein_show. Keyset paging: pass nextCursor back as cursor while hasMore is true. Read-only.
// @Tags Issues
// @Produce json
// @Security SkeinApiKey[read]
// @Param issue path string true "The issue's hierarchical id"
// @Param limit query int false "Page size, 1..100 (default 50)"
// @Param cursor query string false "The nextCursor of the previous page"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[issues.CommentDTO] "A page of comments"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field, limit out of range, a foreign cursor (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/comments [get]
// @ID listComments
func (a *issuesAPI) listComments(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "limit", "cursor") {
		return
	}
	limit, err := ParseLimit(r, commentLimit)
	if err != nil {
		WriteParseError(w, err)
		return
	}
	opts := issues.ListCommentsOpts{First: limit + 1}
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur commentCursor
		if err := DecodeCursor(c, &cur); err != nil {
			WriteParseError(w, err)
			return
		}
		opts.After = &issues.CommentKey{CreatedAt: cur.CreatedAt, ID: cur.ID}
	}
	issue, ok := a.visibleIssue(w, r)
	if !ok {
		return
	}
	comments, _, err := a.issues.ListComments(r.Context(), issue.ID, issues.CommentOriginAll, opts)
	if err != nil {
		WriteInternalError(w, r, "listing comments", err)
		return
	}
	out := List[issues.CommentDTO]{Items: comments}
	if out.Items == nil {
		out.Items = []issues.CommentDTO{}
	}
	if len(comments) > limit {
		last := comments[limit-1]
		out.Items, out.HasMore = comments[:limit], true
		c, err := EncodeCursor(commentCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			WriteInternalError(w, r, "encoding a cursor", err)
			return
		}
		out.NextCursor = &c
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// MoveStep is one request of a move plan: the operation, its method and
// path, the headers and the body to send.
type MoveStep struct {
	// OperationID names the operation: createIssue, then closeIssue.
	OperationID string `json:"operationId"`
	// Method and Path of the request.
	Method string `json:"method"`
	Path   string `json:"path"`
	// Headers to send (If-Match of the close).
	Headers map[string]string `json:"headers,omitempty"`
	// Body of the request; placeholders in angle brackets are filled in by
	// the client.
	Body map[string]any `json:"body"`
	// Comment says what the client decides before sending.
	Comment string `json:"comment"`
}

// MoveTarget is the project of a move plan.
type MoveTarget struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IDPrefix string `json:"idPrefix"`
}

// MovePlan is the answer of getIssueMovePlan.
type MovePlan struct {
	// Supported is false: an issue never changes its project in place.
	Supported bool `json:"supported"`
	// Reason explains the copy-and-close workflow.
	Reason string `json:"reason"`
	// Source is the source issue's card minus the fields the copy carries.
	Source map[string]any `json:"source"`
	// TargetProject is the project the copy goes to.
	TargetProject MoveTarget `json:"targetProject"`
	// TargetComponents are the target's components to pick from.
	TargetComponents []tools.MoveComponent `json:"targetComponents"`
	// NextSteps are the two requests that make the move.
	NextSteps []MoveStep `json:"nextSteps"`
}

// moveNewIDPlaceholder stands for the id createIssue answers in step 1.
const moveNewIDPlaceholder = "<NEW-ID-FROM-createIssue-RESPONSE>"

// getIssueMovePlan plans moving an issue to another project.
//
// @Summary Plan moving an issue to another project
// @Description Returns the plan of moving the issue named by the path to the project of targetProject — the same plan as the MCP tool skein_move. Skein has no in-place move: the hierarchical id lives in commits, chats and other references, so the move is a copy in the target plus a supersedes edge plus a close of the source. The plan only prepares: nextSteps are createIssue on the target with the source's fields (a supersedes edge to the source, a moved-from footer) and closeIssue of the source as superseded, its If-Match the source's version now. Pick a component, run step 1, put the new id into step 2's placeholders, run step 2.
// @Description
// @Description Allowed when the key's owner may write in both projects, and the two are in one space (a target in another space is answered as a missing project). Writes nothing.
// @Tags Issues
// @Produce json
// @Security SkeinApiKey[read]
// @Param issue path string true "The issue's hierarchical id"
// @Param targetProject query string true "The target project: UUID, prefix, alias or name"
// @Param fields query string false "Comma-separated fields to return"
// @Success 200 {object} MovePlan "The plan"
// @Failure 400 {object} ProblemDocument "targetProject missing, an unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope); the owner may not write in the source or the target (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "The target is not within reach or in another space (code project_not_found), ambiguous (code ambiguous_project), or the issue's own project (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/move-plan [get]
// @ID getIssueMovePlan
func (a *issuesAPI) getIssueMovePlan(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "targetProject") {
		return
	}
	q := &query{w: w, r: r}
	target, ok := q.one("targetProject")
	if q.bad {
		return
	}
	if !ok || target == "" {
		q.fail("targetProject is required")
		return
	}
	ctx := r.Context()
	callerID := callerOf(r)
	id, ok := pathIssueID(w, r)
	if !ok {
		return
	}
	source, err := a.issues.GetIssue(ctx, id)
	if errors.Is(err, issues.ErrNotFound) {
		writeIssueNotFound(w, OriginPath, id)
		return
	}
	if err != nil {
		WriteInternalError(w, r, "reading an issue", err)
		return
	}
	if !a.actionGate(w, r, source.ProjectID, func() { writeIssueNotFound(w, OriginPath, id) }) {
		return
	}
	targetID, ok := a.referencedProject(w, r, target)
	if !ok {
		return
	}
	same, err := projects.SameSpace(ctx, a.pool, source.ProjectID, targetID)
	if err != nil {
		WriteInternalError(w, r, "comparing spaces", err)
		return
	}
	notTarget := func() {
		writeCode(w, errcode.ProjectNotFound, OriginBody, "project does not exist or is not visible", map[string]any{"q": target})
	}
	if !same {
		notTarget()
		return
	}
	if !a.actionGate(w, r, targetID, notTarget) {
		return
	}
	if targetID == source.ProjectID {
		writeCode(w, errcode.ValidationError, OriginDomain, "issue is already in the target project — nothing to move",
			map[string]any{"issueId": id, "currentProjectId": source.ProjectID.String(), "targetProjectId": targetID.String()})
		return
	}
	targetProj, err := a.projects.GetProject(ctx, targetID)
	if err != nil {
		WriteInternalError(w, r, "reading the target project", err)
		return
	}
	components, err := a.projects.ListComponents(ctx, targetID, "false", callerID, "")
	if err != nil {
		WriteInternalError(w, r, "listing the target's components", err)
		return
	}
	kit, err := tools.BuildMoveKit(source, targetProj, components)
	if err != nil {
		WriteInternalError(w, r, "building the move plan", err)
		return
	}
	create := map[string]any{}
	for k, v := range kit.CreateArgs {
		if k != "project" {
			create[k] = v
		}
	}
	WriteJSON(w, r, http.StatusOK, MovePlan{
		Supported: false,
		Reason: "Skein has no in-place project change for issues: the hierarchical id is embedded in commits, chats " +
			"and other references. The move is a copy in the target project with a supersedes edge to the source, " +
			"then a close of the source as superseded. Run the two nextSteps in order.",
		Source:           kit.Source,
		TargetProject:    MoveTarget{ID: targetProj.ID.String(), Name: targetProj.Name, IDPrefix: targetProj.IDPrefix},
		TargetComponents: kit.Components,
		NextSteps: []MoveStep{
			{
				OperationID: OperationCreateIssue, Method: http.MethodPost,
				Path: PathPrefix + "projects/" + targetProj.ID.String() + "/issues", Body: create,
				Comment: "Pick a componentId from targetComponents (or leave componentIds empty), then send. " +
					"Copy the id of the answer into step 2's placeholders.",
			},
			{
				OperationID: OperationCloseIssue, Method: http.MethodPost,
				Path:    PathPrefix + "issues/" + source.ID + "/close",
				Headers: map[string]string{"If-Match": fmt.Sprintf("%q", strconv.Itoa(source.Version))},
				Body: map[string]any{
					"reason":       fmt.Sprintf("moved to %s in project %s — <one-sentence why>", moveNewIDPlaceholder, targetProj.IDPrefix),
					"closeOutcome": issues.CloseOutcomeSuperseded,
					"closeTarget":  moveNewIDPlaceholder,
				},
				Comment: "Replace the placeholder in reason and closeTarget with the id of step 1 and add a one-sentence why. " +
					"If-Match is the source's version now: a change in between answers 412 — read the issue again.",
			},
		},
	})
}

// actionGate checks that the caller may write in project — the gate of the
// MCP tools (issueActionGate, projectActionGate). notVisible answers a
// project out of reach as the operation's missing object.
func (a *issuesAPI) actionGate(w http.ResponseWriter, r *http.Request, project uuid.UUID, notVisible func()) bool {
	_, err := projects.EnsureProjectAction(r.Context(), a.pool, project, callerOf(r), authz.ActionWrite)
	switch {
	case err == nil:
		return true
	case errors.Is(err, projects.ErrProjectNotVisible):
		notVisible()
	case errors.Is(err, projects.ErrProjectActionForbidden):
		writeCode(w, errcode.Forbidden, OriginDomain, "the action is not allowed in this project", nil)
	default:
		WriteInternalError(w, r, "checking the project action", err)
	}
	return false
}

// SearchPage is a page of search: the list envelope plus degraded, as the
// MCP tool skein_search answers it.
type SearchPage struct {
	// Items are the hits, by relevance.
	Items []search.Item `json:"items"`
	// NextCursor is the cursor of the next page, null on the last one.
	NextCursor *string `json:"nextCursor" extensions:"x-nullable"`
	// HasMore — another page follows.
	HasMore bool `json:"hasMore"`
	// Degraded — the meaning reranking was unavailable and the hits are
	// ordered by full-text relevance only.
	Degraded bool `json:"degraded"`
}

func (SearchPage) itemType() reflect.Type { return reflect.TypeFor[search.Item]() }

// search searches issues, comments, projects and components.
//
// @Summary Search
// @Description Searches the issues, comments, projects and components the key's owner sees within the key's scope — full-text, reranked by meaning when the instance has an embedder — the same search as the MCP tool skein_search. kinds narrows what is searched; the issue filters apply to issues only. Keyset paging: pass nextCursor back as cursor while hasMore is true. Read-only.
// @Tags Search
// @Produce json
// @Security SkeinApiKey[read]
// @Param query query string true "The query, 1..500 characters"
// @Param project query string false "Narrow to one project: UUID, prefix, alias or name"
// @Param space query string false "Narrow to one space by its short id"
// @Param kinds query string false "Comma-separated kinds: issue, comment, project, component (default all)"
// @Param statuses query string false "Comma-separated issue statuses"
// @Param priorities query string false "Comma-separated issue priorities 0..4"
// @Param types query string false "Comma-separated issue types"
// @Param labels query string false "Comma-separated issue labels"
// @Param source query string false "Issue source: human, agent_directed, agent_autonomous or api"
// @Param commentAuthorKinds query string false "Comma-separated comment author kinds: human, agent_directed, agent_autonomous, api"
// @Param limit query int false "Page size (default 20)"
// @Param cursor query string false "The nextCursor of the previous page"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} SearchPage "A page of hits; degraded marks a page ordered by full-text relevance only"
// @Failure 400 {object} ProblemDocument "query missing or too long, an unknown parameter or field, a value outside its set, a foreign cursor (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "The project is not within reach (code project_not_found) or ambiguous (code ambiguous_project)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/search [get]
// @ID search
func (a *issuesAPI) searchAll(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "query", "project", "space", "kinds", "statuses", "priorities", "types", "labels",
		"source", "commentAuthorKinds", "limit", "cursor") {
		return
	}
	q := &query{w: w, r: r}
	text, _ := q.one("query")
	if q.bad {
		return
	}
	if text == "" {
		q.fail("query is required")
		return
	}
	if len(text) > 500 {
		q.fail("query must be at most 500 characters")
		return
	}
	params := search.Params{Query: text, CallerID: callerOf(r)}
	params.Space, _ = q.one("space")
	params.Cursor, _ = q.one("cursor")
	for _, k := range q.list("kinds") {
		if !search.IsValidKind(k) {
			q.fail("kinds must be of issue, comment, project, component")
			return
		}
		params.Kinds = appendKind(params.Kinds, search.Kind(k))
	}
	if len(params.Kinds) == 0 {
		params.Kinds = search.AllKinds
	}
	for _, s := range q.list("statuses") {
		v, err := issues.ValidateStatus(s)
		if err != nil {
			q.fail(err.Error())
			return
		}
		params.Filters.Statuses = append(params.Filters.Statuses, v)
	}
	for _, s := range q.list("priorities") {
		p, err := strconv.Atoi(s)
		if err != nil || p < 0 || p > 4 {
			q.fail("priorities must be in [0, 4]")
			return
		}
		params.Filters.Priorities = append(params.Filters.Priorities, p)
	}
	params.Filters.Types = q.list("types")
	params.Filters.Labels = q.list("labels")
	if v, ok := q.one("source"); ok && v != "" && q.choice("source", v, issues.SourceHuman, issues.SourceAgentDirected, issues.SourceAgentAutonomous, issues.SourceAPI) {
		params.Filters.Source = &v
	}
	for _, k := range q.list("commentAuthorKinds") {
		if !q.choice("commentAuthorKinds", k, issues.CommentAuthorKindHuman, issues.CommentAuthorKindAgentDirected,
			issues.CommentAuthorKindAgentAutonomous, issues.CommentAuthorKindAPI) {
			return
		}
		params.Filters.CommentAuthorKinds = append(params.Filters.CommentAuthorKinds, k)
	}
	limit, err := ParseLimit(r, searchLimit)
	if err != nil {
		q.fail(err.Error())
		return
	}
	params.Limit = limit
	if q.bad {
		return
	}
	if handle, ok := q.one("project"); ok && handle != "" {
		pid, ok := a.referencedProject(w, r, handle)
		if !ok {
			return
		}
		params.Project = &pid
	}
	res, err := a.search.Search(r.Context(), params)
	if errors.Is(err, search.ErrInvalidCursor) {
		WriteParseError(w, ErrBadCursor)
		return
	}
	if err != nil {
		WriteInternalError(w, r, "searching", err)
		return
	}
	items := res.Items
	if items == nil {
		items = []search.Item{}
	}
	WriteJSON(w, r, http.StatusOK, SearchPage{Items: items, NextCursor: res.NextCursor, HasMore: res.HasMore, Degraded: res.Degraded})
}

func appendKind(kinds []search.Kind, k search.Kind) []search.Kind {
	for _, have := range kinds {
		if have == k {
			return kinds
		}
	}
	return append(kinds, k)
}
