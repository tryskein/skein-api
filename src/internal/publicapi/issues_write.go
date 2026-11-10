package publicapi

// issues_write.go — the writes of issues of the public API (spec
// sk-878.3.1, "API surface"). Each one is a thin adapter over the
// transport-neutral operation of internal/issueops that the MCP tool of the
// same name also calls (INV-4): the request body carries the arguments under
// the tool's names, If-Match carries the expected version, ?force= the
// privileged mode of a guarded operation; a refusal of the operation is the
// RFC 9457 problem of its code, the status from the code's class and from
// where the failed object came from — the path (404) or a reference in the
// body (422).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/errcode"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
)

// The operationIds of the issue writes (createIssue and closeIssue are
// declared with the reads: the move plan names them).
const (
	OperationCreateEpic       = "createEpic"
	OperationUpdateIssue      = "updateIssue"
	OperationClaimIssue       = "claimIssue"
	OperationClaimIssues      = "claimIssues"
	OperationUnclaimIssue     = "unclaimIssue"
	OperationReleaseIssueLock = "releaseIssueLock"
	OperationAcceptIssue      = "acceptIssue"
	OperationReparentIssue    = "reparentIssue"
	OperationDropDelegated    = "dropDelegated"
	OperationResumeDelegated  = "resumeDelegated"
	OperationAttachCommits    = "attachCommits"
	OperationDetachCommit     = "detachCommit"
)

// issueWrites serves the issue writes.
type issueWrites struct {
	*issuesAPI
	ops      issueops.Deps
	outcomes Outcomes
	// attachments are the stores of the attachment operations.
	attachments issueops.AttachmentDeps
}

// decodeArgs reads the JSON body strictly into typed (an unknown field, a
// value of the wrong type, malformed JSON — 400) and returns it as the
// arguments of the operation, with null kept apart from absent. An empty
// body is {}.
func decodeArgs(w http.ResponseWriter, r *http.Request, typed any) (issueops.Args, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		if IsBodyTooLarge(err) {
			WriteProblem(w, Problem{Code: errcode.PayloadTooLarge, Origin: OriginParse})
			return nil, false
		}
		WriteParseError(w, errors.New("the request body could not be read"))
		return nil, false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte("{}")
	}
	if err := decodeStrict(raw, typed); err != nil {
		WriteParseError(w, err)
		return nil, false
	}
	args := issueops.Args{}
	if err := json.Unmarshal(raw, &args); err != nil {
		WriteParseError(w, err)
		return nil, false
	}
	if args == nil {
		// A body of JSON null leaves the map nil; it carries no option.
		args = issueops.Args{}
	}
	return args, true
}

// kindOf validates the optional kind of a write body (400 when outside
// directed | autonomous).
func kindOf(w http.ResponseWriter, kind *string) bool {
	if _, err := ParseKind(kind); err != nil {
		WriteParseError(w, err)
		return false
	}
	return true
}

// writeFailure answers a refusal of an operation. A "not found" code is 404
// when the object is the one of the path (pathIDs: the issue of the path,
// the project id it was resolved to), 422 when it is a reference of the
// body; every other code takes its class's status. The members of the
// refusal become extensions; the validator's own code travels as
// "validation".
func writeFailure(w http.ResponseWriter, f *issueops.Failure, pathIDs ...string) {
	code, ok := errcode.Lookup(f.Code)
	ext := map[string]any{}
	for k, v := range f.Fields {
		if k == "code" {
			ext["validation"] = v
			continue
		}
		ext[k] = v
	}
	if !ok {
		// A validator code outside the closed set (blocked_is_derived):
		// the refusal of a value.
		ext["validation"] = f.Code
		code = errcode.ValidationError
	}
	origin := OriginDomain
	if code.Class() == errcode.ClassNotFound {
		origin = OriginBody
		for _, key := range []string{"id", "issueId", "q", "projectId", "fromId", "commentId", "attachmentId", "reminderId", "userId", "space"} {
			if v, ok := f.Fields[key].(string); ok {
				for _, p := range pathIDs {
					if p != "" && v == p {
						origin = OriginPath
					}
				}
			}
		}
	}
	if len(ext) == 0 {
		ext = nil
	}
	WriteProblem(w, Problem{Code: code, Origin: origin, Detail: f.Message, Extensions: ext})
}

// writeIssue answers the issue an operation wrote, with its version as ETag.
func writeIssue(w http.ResponseWriter, r *http.Request, status int, issue *issues.IssueDetail) {
	SetETag(w, issue.Version)
	WriteJSON(w, r, status, issue)
}

// detailFromCommitted is the issue as a committing transaction left it, for
// an answer whose read-back after the commit failed: the write happened,
// so it is acknowledged with what is known (id, version, status, holder).
func detailFromCommitted(c issues.CommittedState) *issues.IssueDetail {
	return &issues.IssueDetail{
		ID: c.ID, Version: c.Version, Status: c.Status, Assignee: c.Assignee,
		LockedBy: c.LockedBy, LockedAt: c.LockedAt, StartedAt: c.StartedAt,
		ClosedAt: c.ClosedAt, CloseOutcome: c.CloseOutcome,
	}
}

// withOutcome is ctx with the before-commit hook that records the outcome
// status of an Idempotency-Key request in the write's own transaction.
func (a *issueWrites) withOutcome(ctx context.Context, status int) context.Context {
	if a.outcomes == nil {
		return ctx
	}
	return issues.WithBeforeCommit(ctx, issues.BeforeCommitHook(a.outcomes.IssueBeforeCommit(status)))
}

func (a *issueWrites) noteTarget(ctx context.Context, ref ObjectRef) {
	if a.outcomes != nil {
		a.outcomes.NoteTarget(ctx, ref)
	}
}

func (a *issueWrites) noteObjects(ctx context.Context, refs ...ObjectRef) {
	if a.outcomes != nil {
		a.outcomes.NoteObjects(ctx, refs...)
	}
}

// pathIssueArg is the {issue} of the path for an operation; a value that is
// not a hierarchical id names no issue (404).
func pathIssueArg(w http.ResponseWriter, r *http.Request) (string, bool) {
	return pathIssueID(w, r)
}

// expectedVersionArg puts the version If-Match named into the arguments.
func expectedVersionArg(r *http.Request, args issueops.Args) {
	if v, ok := ExpectedVersion(r.Context()); ok {
		args["expectedVersion"] = v
	}
}

// forceArg puts the mode of the request into the arguments of a guarded
// operation.
func forceArg(r *http.Request, args issueops.Args) {
	if ModeFromContext(r.Context()).Force {
		args["force"] = true
	} else {
		delete(args, "force")
	}
}

// dependencyInput is one inline edge of a created issue.
type dependencyInput struct {
	// ToID is the hierarchical id of the other end.
	ToID string `json:"toId"`
	// Type is blocks, waits-for, related, discovered-from, supersedes,
	// duplicates or parent-child.
	Type string `json:"type"`
	// Gate qualifies a waits-for edge.
	Gate *string `json:"gate,omitempty"`
}

// issueFields are the fields of an issue a create sets.
type issueFields struct {
	// Title, 1..500 characters. Required.
	Title *string `json:"title,omitempty"`
	// Description, markdown.
	Description *string `json:"description,omitempty"`
	// Design notes, markdown.
	Design *string `json:"design,omitempty"`
	// AcceptanceCriteria, markdown.
	AcceptanceCriteria *string `json:"acceptanceCriteria,omitempty"`
	// Notes, markdown.
	Notes *string `json:"notes,omitempty"`
	// Priority 0..4 (default 3).
	Priority *int `json:"priority,omitempty"`
	// Assignee: username, email or user UUID. Omitted: the owner's «assign
	// issues my agent creates to me» setting decides.
	Assignee *string `json:"assignee,omitempty"`
	// Unassigned leaves the issue without an assignee whatever the setting.
	Unassigned *bool `json:"unassigned,omitempty"`
	// ParentID, the hierarchical id of the parent.
	ParentID *string `json:"parentId,omitempty"`
	// ComponentIDs, UUIDs of components of the project; omitted — the
	// parent's are inherited.
	ComponentIDs []string `json:"componentIds,omitempty"`
	// Labels.
	Labels []string `json:"labels,omitempty"`
	// SourceContext, free text on where the issue came from.
	SourceContext *string `json:"sourceContext,omitempty"`
	// ReviewRequired asks the author's review once the issue is closed.
	ReviewRequired *bool `json:"reviewRequired,omitempty"`
	// Kind of the write: directed or autonomous; omitted — the origin is
	// api.
	Kind *string `json:"kind,omitempty"`
}

// createIssueBody is the body of createIssue.
type createIssueBody struct {
	issueFields
	// Type: bug, feature, task, epic, chore, decision, idea or note.
	// Required.
	Type *string `json:"type,omitempty"`
	// Standing makes the issue a standing task.
	Standing *bool `json:"standing,omitempty"`
	// Force skips the duplicate gate (it is not a privilege: right write).
	Force *bool `json:"force,omitempty"`
	// DuplicateOf names an existing issue this one duplicates: no gate, and
	// a duplicates edge to it in the same transaction.
	DuplicateOf *string `json:"duplicateOf,omitempty"`
	// IDMode overrides the project's id mode: counter, hash or custom.
	IDMode *string `json:"idMode,omitempty"`
	// ID is the id of a custom-mode issue.
	ID *string `json:"id,omitempty"`
	// Dependencies are edges written with the issue, all or nothing (at
	// most 50).
	Dependencies []dependencyInput `json:"dependencies,omitempty"`
}

// createIssue creates an issue.
//
// @Summary Create an issue
// @Description Creates an issue in the project of the path — the same operation as the MCP tool skein_create:
// @Description 1. The fields are checked; the owner must be able to write in the project (and to assign, when assignee is given).
// @Description 2. Unless force or duplicateOf, the duplicate gate looks for similar issues of the project: found — nothing is created, the answer is 422 duplicate_suspected with the candidates (duplicates; degraded when only the lexical check ran). Repeat with force=true to create anyway, or with duplicateOf to create it as a duplicate.
// @Description 3. One transaction writes the issue, its components, labels, the inline dependencies (each refusal rolls everything back) and, with duplicateOf, the duplicates edge.
// @Description
// @Description Without kind the issue's source is api; kind directed and autonomous give the agent's sources, with the same side effects as in MCP (the author's auto-watch, the notification rules, the default assignee). Accepts Idempotency-Key: a repeat within 24 h answers the first outcome without creating anything. Side effects: issue.created and the events of the edges; notifications of the assignee and the watchers. The Location header names the issue, ETag is its version.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param Idempotency-Key header string false "A key that makes a repeat of this request answer its first outcome (1..255 visible ASCII)"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body createIssueBody true "The issue"
// @Success 201 {object} issues.IssueDetail "The issue created"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous, a malformed Idempotency-Key (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write or assign in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 409 {object} ProblemDocument "The first request with this Idempotency-Key is still running (code idempotency_in_progress)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "Similar issues exist (code duplicate_suspected, with duplicates); a value is invalid (code validation_error); the parent, a dependency target or duplicateOf does not exist or is out of reach (code parent_not_found, issue_not_found, validation_error — as MCP); parent closed or in another project (parent_closed, parent_wrong_project); id taken (id_taken); components of another project (components_wrong_project); assignee may not work here (assignee_not_allowed); a cycle (dependency_cycle); a serialization race (serialization_retry, retryable); the Idempotency-Key reused with another request (idempotency_key_reused)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/issues [post]
// @ID createIssue
func (a *issueWrites) createIssue(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body createIssueBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	pa := &projectsAPI{pool: a.pool, projects: a.projects}
	projectID, ok := pa.pathProject(w, r)
	if !ok {
		return
	}
	ctx := a.withOutcome(r.Context(), http.StatusCreated)
	a.noteTarget(ctx, ObjectRef{Kind: ObjectProject, ID: projectID.String()})
	args["project"] = projectID.String()
	res, f := issueops.Create(ctx, a.ops, callerOf(r), args, issueops.OriginKey)
	if f != nil {
		writeFailure(w, f, projectID.String())
		return
	}
	if res.Decision {
		refs := make([]ObjectRef, 0, len(res.Duplicates))
		for _, c := range res.Duplicates {
			refs = append(refs, ObjectRef{Kind: ObjectIssue, ID: c.ID})
		}
		a.noteObjects(ctx, refs...)
		WriteProblem(w, Problem{Code: errcode.DuplicateSuspected, Origin: OriginDomain,
			Detail:     "similar issues exist; nothing was created — repeat with force=true or duplicateOf",
			Extensions: map[string]any{"duplicates": res.Duplicates, "degraded": res.Degraded}})
		return
	}
	issue := res.Issue
	if res.ReadBack != nil {
		issue = detailFromCommitted(res.ReadBack.State)
	}
	w.Header().Set("Location", PathPrefix+"issues/"+issue.ID)
	writeIssue(w, r, http.StatusCreated, issue)
}

// hydrateIssue is the Hydrate of the operations that answer one issue.
func (a *issueWrites) hydrateIssue(ctx context.Context, _ *http.Request, objects []ObjectRef) (any, error) {
	for _, o := range objects {
		if o.Kind == ObjectIssue {
			return a.issues.GetIssue(ctx, o.ID)
		}
	}
	return nil, errors.New("no issue among the objects of the outcome")
}

// childTemplate is one child of an epic.
type childTemplate struct {
	// Title, required.
	Title *string `json:"title,omitempty"`
	// Type of the child (default task).
	Type *string `json:"type,omitempty"`
	// Priority 0..4.
	Priority *int `json:"priority,omitempty"`
	// Description, markdown.
	Description *string `json:"description,omitempty"`
	// Design, markdown.
	Design *string `json:"design,omitempty"`
	// AcceptanceCriteria, markdown.
	AcceptanceCriteria *string `json:"acceptanceCriteria,omitempty"`
	// Notes, markdown.
	Notes *string `json:"notes,omitempty"`
	// ComponentIDs of the child; omitted — the epic's.
	ComponentIDs []string `json:"componentIds,omitempty"`
	// Labels of the child.
	Labels []string `json:"labels,omitempty"`
	// Assignee of the child.
	Assignee *string `json:"assignee,omitempty"`
	// Unassigned leaves the child without an assignee.
	Unassigned *bool `json:"unassigned,omitempty"`
}

// createEpicBody is the body of createEpic. There is no duplicate gate.
type createEpicBody struct {
	issueFields
	// ChildTemplates are the children (at most 50).
	ChildTemplates []childTemplate `json:"childTemplates,omitempty"`
}

// EpicCreated is the answer of createEpic: the epic with its children.
type EpicCreated struct {
	issues.IssueDetail
	// Children are the children created with the epic.
	Children []*issues.IssueDetail `json:"children"`
}

// createEpic creates an epic with its children.
//
// @Summary Create an epic with its children
// @Description Creates an epic and its children in one transaction — the same operation as the MCP tool skein_epic_create: all of them, or none when any child is refused. The children are parent-child children of the epic, in the order given. There is no duplicate gate (no force, no duplicateOf).
// @Description
// @Description Without kind the source is api; kind directed and autonomous give the agent's sources. Accepts Idempotency-Key. Side effects: issue.created of each issue and the parent-child edges; notifications of the assignees. The Location header names the epic, ETag is its version.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param Idempotency-Key header string false "A key that makes a repeat of this request answer its first outcome"
// @Param Prefer header string false "return=minimal answers only the epic's id and version"
// @Param body body createEpicBody true "The epic and its children"
// @Success 201 {object} EpicCreated "The epic with its children"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write or assign in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 409 {object} ProblemDocument "The first request with this Idempotency-Key is still running (code idempotency_in_progress)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "A value of the epic or of a child is invalid (code validation_error); the parent is out of reach (code parent_not_found); components of another project (components_wrong_project); assignee may not work here (assignee_not_allowed); a serialization race (serialization_retry); the Idempotency-Key reused (idempotency_key_reused)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/epics [post]
// @ID createEpic
func (a *issueWrites) createEpic(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body createEpicBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	pa := &projectsAPI{pool: a.pool, projects: a.projects}
	projectID, ok := pa.pathProject(w, r)
	if !ok {
		return
	}
	ctx := a.withOutcome(r.Context(), http.StatusCreated)
	a.noteTarget(ctx, ObjectRef{Kind: ObjectProject, ID: projectID.String()})
	args["project"] = projectID.String()
	res, f := issueops.EpicCreate(ctx, a.ops, callerOf(r), args, issueops.OriginKey)
	if f != nil {
		writeFailure(w, f, projectID.String())
		return
	}
	epic, children := res.Epic, res.Children
	if rb := res.ReadBack; rb != nil {
		epic = detailFromCommitted(rb.State)
		children = make([]*issues.IssueDetail, 0, len(rb.Children))
		for _, c := range rb.Children {
			children = append(children, detailFromCommitted(c))
		}
	}
	if children == nil {
		children = []*issues.IssueDetail{}
	}
	w.Header().Set("Location", PathPrefix+"issues/"+epic.ID)
	SetETag(w, epic.Version)
	WriteJSON(w, r, http.StatusCreated, EpicCreated{IssueDetail: *epic, Children: children})
}

// hydrateEpic is the Hydrate of createEpic: the epic as it is now, with its
// current children.
func (a *issueWrites) hydrateEpic(ctx context.Context, _ *http.Request, objects []ObjectRef) (any, error) {
	for _, o := range objects {
		if o.Kind != ObjectIssue {
			continue
		}
		epic, err := a.issues.GetIssue(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		caller, _ := auth.UserIDFrom(ctx)
		parent := epic.ID
		page, err := a.issues.ListIssues(ctx, issues.ListIssuesArgs{
			ProjectID: epic.ProjectID, ParentID: &parent, Caller: &caller, Limit: issueops.EpicChildTemplateMaxItems,
		})
		if err != nil {
			return nil, err
		}
		children := make([]*issues.IssueDetail, 0, len(page.Items))
		for _, it := range page.Items {
			child, err := a.issues.GetIssue(ctx, it.ID)
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		}
		return EpicCreated{IssueDetail: *epic, Children: children}, nil
	}
	return nil, errors.New("no issue among the objects of the outcome")
}

// updateIssueBody is the body of updateIssue: every member present is
// changed, null or "" clears a text field.
type updateIssueBody struct {
	// Title, 1..500 characters.
	Title *string `json:"title,omitempty"`
	// Description; null or "" clears it.
	Description *string `json:"description,omitempty"`
	// Type of the issue.
	Type *string `json:"type,omitempty"`
	// Priority 0..4.
	Priority *int `json:"priority,omitempty"`
	// Design; null or "" clears it.
	Design *string `json:"design,omitempty"`
	// AcceptanceCriteria; null or "" clears it.
	AcceptanceCriteria *string `json:"acceptanceCriteria,omitempty"`
	// Notes; null or "" clears it.
	Notes *string `json:"notes,omitempty"`
	// SourceContext; null or "" clears it.
	SourceContext *string `json:"sourceContext,omitempty"`
	// Assignee; "" clears it.
	Assignee *string `json:"assignee,omitempty"`
	// DueAt, RFC 3339.
	DueAt *string `json:"dueAt,omitempty"`
	// DeferUntil, RFC 3339.
	DeferUntil *string `json:"deferUntil,omitempty"`
	// ExternalRef; null or "" clears it.
	ExternalRef *string `json:"externalRef,omitempty"`
	// EstimatedMinutes, >= 0.
	EstimatedMinutes *int `json:"estimatedMinutes,omitempty"`
	// Status: open, in_progress, deferred, closed or standing.
	Status *string `json:"status,omitempty"`
	// Reason of a status change (required to close).
	Reason *string `json:"reason,omitempty"`
	// CloseOutcome: done, duplicate, superseded, wont_do — with status
	// closed, or on an already closed issue.
	CloseOutcome *string `json:"closeOutcome,omitempty"`
	// CloseTarget, the issue a duplicate or superseded one points to.
	CloseTarget *string `json:"closeTarget,omitempty"`
	// ComponentIDs replace the components.
	ComponentIDs []string `json:"componentIds,omitempty"`
	// LabelsAdd are labels to add.
	LabelsAdd []string `json:"labelsAdd,omitempty"`
	// LabelsRemove are labels to remove (each must be set).
	LabelsRemove []string `json:"labelsRemove,omitempty"`
	// ReviewRequired, sent alone, sets the author's review flag (it moves
	// no version).
	ReviewRequired *bool `json:"reviewRequired,omitempty"`
	// Kind of the write: directed or autonomous (checked; an issue keeps
	// its origin).
	Kind *string `json:"kind,omitempty"`
}

// updateIssue changes an issue.
//
// @Summary Update an issue
// @Description Changes the issue of the path in one command — the same operation as the MCP tool skein_update: fields, status (a close among them), components and labels land in one transaction, all or nothing (US-14). If-Match is required: the version of the issue (its ETag); a stale one answers 412 version_conflict with the current version, and nothing is written. reviewRequired travels alone and moves no version; labels alone move no version either.
// @Description
// @Description The lock guards a change: only the holder changes a claimed issue. force=true (the key needs manage, the owner force_unlock) bypasses another holder's claim, the version and the pin in one call. Side effects: the issue.* events of what changed; notifications of the assignee and the watchers.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param If-Match header string true "The version of the issue, as its ETag gives it"
// @Param force query bool false "The privileged mode: manage and force_unlock"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body updateIssueBody true "The changes"
// @Success 200 {object} issues.IssueDetail "The issue after the change"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, force not true|false, If-Match not one version (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write, or manage for force (code insufficient_scope); the owner may not write or assign (forbidden) or force (force_unlock_forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 412 {object} ProblemDocument "If-Match is stale (code version_conflict, currentVersion)"
// @Failure 428 {object} ProblemDocument "If-Match is missing (code precondition_required)"
// @Failure 422 {object} ProblemDocument "Locked by another holder (code locked_by_other, retryable); a value or transition is invalid (code validation_error, a standing transition code); a label not set (validation_error); pinned (pinned_no_force); the close target out of reach (issue_not_found); components of another project (components_wrong_project); a serialization race (serialization_retry)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue} [patch]
// @ID updateIssue
func (a *issueWrites) updateIssue(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body updateIssueBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["id"] = id
	expectedVersionArg(r, args)
	forceArg(r, args)
	res, f := issueops.Update(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	issue := res.Final
	if res.ReadBack != nil {
		issue = detailFromCommitted(res.ReadBack.State)
	}
	writeIssue(w, r, http.StatusOK, issue)
}

// claimBody is the body of claimIssue.
type claimBody struct {
	// Assignee claims on behalf of another user (the instance admin only).
	Assignee *string `json:"assignee,omitempty"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// claimIssue claims an issue.
//
// @Summary Claim an issue
// @Description Takes the lock of the issue of the path and starts it — the same operation as the MCP tool skein_claim: status in_progress, the holder is the executor of this request (X-Skein-Worker-Id of the key, or the key itself), the owner becomes the assignee. The check of the lock is in the claiming transaction: of two concurrent claims exactly one succeeds, the other answers already_claimed. Another holder's lock is never taken over here. Side effects: issue.claimed (and the status change); notifications of the watchers.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param X-Skein-Worker-Id header string false "The executor of the key that will hold the lock (1..64 of A-Z a-z 0-9 . _ -)"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body claimBody false "Options"
// @Success 200 {object} issues.IssueDetail "The issue, claimed"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a malformed X-Skein-Worker-Id (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not be the assignee, or claims for another user without being an admin (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "Claimed by another holder (code already_claimed, retryable); closed (cannot_claim_closed); a standing task (cannot_claim_standing); a serialization race (serialization_retry)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/claim [post]
// @ID claimIssue
func (a *issueWrites) claimIssue(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body claimBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["id"] = id
	res, f := issueops.Claim(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	writeIssue(w, r, http.StatusOK, res.Issue)
}

// claimIssuesBody is the body of claimIssues.
type claimIssuesBody struct {
	// IDs are 1..100 distinct hierarchical ids. Required.
	IDs []string `json:"ids"`
	// Assignee claims on behalf of another user (the instance admin only).
	Assignee *string `json:"assignee,omitempty"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// ClaimedIssues is the answer of claimIssues, in the order of the request.
type ClaimedIssues struct {
	// Items are the issues claimed.
	Items []*issues.IssueDetail `json:"items"`
}

// claimIssues claims several issues, all or nothing.
//
// @Summary Claim several issues
// @Description Claims 1..100 issues in one transaction — the same operation as the MCP tool skein_claim with ids: every issue is claimed, or none (any refusal rolls every lock back, and names the issue that refused it). The answer keeps the order of the request. No version is needed: the lock is its own guard. Side effects: issue.claimed of each.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param X-Skein-Worker-Id header string false "The executor of the key that will hold the locks"
// @Param body body claimIssuesBody true "The issues"
// @Success 200 {object} ClaimedIssues "The issues, claimed, in the order of the request"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, 0 or more than 100 ids (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not be the assignee (code forbidden)"
// @Failure 422 {object} ProblemDocument "A repeated or malformed id (code validation_error); an issue out of reach (issue_not_found); claimed by another holder (already_claimed); closed (cannot_claim_closed); nothing was claimed"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/claim [post]
// @ID claimIssues
func (a *issueWrites) claimIssues(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body claimIssuesBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	if n := len(body.IDs); n < 1 || n > issueops.BatchMaxItems {
		WriteParseError(w, errors.New("ids must contain 1..100 issue ids"))
		return
	}
	res, f := issueops.Claim(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusOK, ClaimedIssues{Items: res.Issues})
}

// unclaimBody is the body of unclaimIssue.
type unclaimBody struct {
	// NextStatus: in_progress (hand over), open or deferred. Required.
	NextStatus *string `json:"nextStatus,omitempty"`
	// Reason, non-empty. Required.
	Reason *string `json:"reason,omitempty"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// unclaimIssue releases the caller's claim with a next status.
//
// @Summary Unclaim an issue
// @Description Releases the lock the executor of this request holds and sets the next status — the same operation as the MCP tool skein_unclaim. The version is not checked. Only the holder unclaims; force=true (the key needs manage, the owner force_unlock) releases another holder's. To release a lock without changing the status — your own, taken by any of your keys or sessions, or another's with force_unlock — use releaseIssueLock. Side effects: issue.unclaimed and the status change.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param force query bool false "The privileged mode: manage and force_unlock"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body unclaimBody true "Next status and reason"
// @Success 200 {object} issues.IssueDetail "The issue, released"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, force not true|false (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write, or manage for force (code insufficient_scope); the owner may not write (forbidden) or force (force_unlock_forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "nextStatus or reason invalid (code validation_error); the executor does not hold the lock (not_lock_holder)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/unclaim [post]
// @ID unclaimIssue
func (a *issueWrites) unclaimIssue(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body unclaimBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["id"] = id
	forceArg(r, args)
	res, f := issueops.Unclaim(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	writeIssue(w, r, http.StatusOK, res.Issue)
}

// kindBody is the body of an operation that takes nothing but kind.
type kindBody struct {
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// releaseIssueLock releases the lock of an issue.
//
// @Summary Release the lock of an issue
// @Description Releases the lock of the issue of the path without changing its status and without bypassing its version or its pin (the UI's «release the lock»): a lock the key's owner took — with any of his keys, executors or MCP sessions — is released without a role; another holder's needs the owner's force_unlock. The version grows, so a writer holding the old version gets 412. An issue nobody holds is answered as it is, its version unchanged. There is no MCP tool of its own (MCP releases another's lock with force on skein_unclaim). Side effects: issue.unclaimed; for another's lock also issue.lock_force_released.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body kindBody false "Options"
// @Success 200 {object} issues.IssueDetail "The issue, released"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write (forbidden), or releases another holder's lock without force_unlock (force_unlock_forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "The lock changed while it was being released (code not_lock_holder)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/release-lock [post]
// @ID releaseIssueLock
func (a *issueWrites) releaseIssueLock(w http.ResponseWriter, r *http.Request) {
	a.simple(w, r, issueops.ReleaseLock, false, false)
}

// closeBody is the body of closeIssue.
type closeBody struct {
	// Reason of the close, non-empty. Required.
	Reason *string `json:"reason,omitempty"`
	// CloseOutcome: done, duplicate, superseded or wont_do.
	CloseOutcome *string `json:"closeOutcome,omitempty"`
	// CloseTarget, the issue a duplicate or superseded one points to; the
	// edge is written in the same transaction.
	CloseTarget *string `json:"closeTarget,omitempty"`
	// Commits closing the issue (at most 10), attached in the same
	// transaction.
	Commits []commitInput `json:"commits,omitempty"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// commitInput is one commit of a close or an attach.
type commitInput struct {
	// Hash, lowercase hex 7..64. Required.
	Hash string `json:"hash"`
	// RepoURL, exact match of the project's gitRepos[].url.
	RepoURL *string `json:"repoUrl,omitempty"`
	// RepoID, the UUID of the project's repo; wins over repoUrl.
	RepoID *string `json:"repoId,omitempty"`
	// URL of the commit.
	URL *string `json:"url,omitempty"`
	// Message, the subject line.
	Message *string `json:"message,omitempty"`
	// Author of the commit.
	Author *string `json:"author,omitempty"`
	// CommittedAt, RFC 3339.
	CommittedAt *string `json:"committedAt,omitempty"`
}

// closeIssue closes an issue.
//
// @Summary Close an issue
// @Description Closes the issue of the path — the same operation as the MCP tool skein_close: reason, outcome, the target edge of a duplicate or superseded close and the commits land in one transaction, all or nothing (US-14). If-Match is required (the issue's version). Only the holder closes a claimed issue; force=true (the key needs manage, the owner force_unlock) bypasses another holder's claim, the version and the pin. Closing a standing task with open children is refused.
// @Description
// @Description Accepts Idempotency-Key: a repeat within 24 h answers the first outcome without closing again. Side effects: issue.closed (and the lock release), the commit and edge events; notifications of the watchers.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param If-Match header string true "The version of the issue, as its ETag gives it"
// @Param force query bool false "The privileged mode: manage and force_unlock"
// @Param Idempotency-Key header string false "A key that makes a repeat of this request answer its first outcome"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body closeBody true "The close"
// @Success 200 {object} issues.IssueDetail "The issue, closed"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, force not true|false (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write, or manage for force (code insufficient_scope); the owner may not write (forbidden) or force (force_unlock_forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 409 {object} ProblemDocument "The first request with this Idempotency-Key is still running (code idempotency_in_progress)"
// @Failure 412 {object} ProblemDocument "If-Match is stale (code version_conflict, currentVersion)"
// @Failure 428 {object} ProblemDocument "If-Match is missing (code precondition_required)"
// @Failure 422 {object} ProblemDocument "Locked by another holder (code locked_by_other); reason, outcome or a commit invalid (validation_error, close_outcome_link_required); the close target out of reach (issue_not_found); a repo unknown or ambiguous (repo_not_found, repo_ambiguous); pinned (pinned_no_force); a standing task with open children (standing_has_open_children); a serialization race (serialization_retry); the Idempotency-Key reused (idempotency_key_reused)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/close [post]
// @ID closeIssue
func (a *issueWrites) closeIssue(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body closeBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["id"] = id
	expectedVersionArg(r, args)
	forceArg(r, args)
	ctx := a.withOutcome(r.Context(), http.StatusOK)
	ref := ObjectRef{Kind: ObjectIssue, ID: id}
	a.noteTarget(ctx, ref)
	a.noteObjects(ctx, ref)
	res, f := issueops.Close(ctx, a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	issue := res.Issue
	if res.ReadBack != nil {
		issue = detailFromCommitted(res.ReadBack.State)
	}
	writeIssue(w, r, http.StatusOK, issue)
}

// acceptIssue accepts a delegated issue.
//
// @Summary Accept an issue
// @Description The assignee acknowledges the issue — the same operation as the MCP tool skein_accept: the acceptance stamp is set, nothing else changes; a repeat is a no-op. If-Match is optional: when sent, a stale version answers 412. The lock does not guard it. Side effects: issue.accepted; the creator's outbox is updated.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param If-Match header string false "The version of the issue; checked when sent"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body kindBody false "Options"
// @Success 200 {object} issues.IssueDetail "The issue, accepted"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner is not the assignee (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 412 {object} ProblemDocument "If-Match is stale (code version_conflict)"
// @Failure 422 {object} ProblemDocument "Closed (code cannot_accept_closed); a serialization race (serialization_retry)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/accept [post]
// @ID acceptIssue
func (a *issueWrites) acceptIssue(w http.ResponseWriter, r *http.Request) {
	a.simple(w, r, issueops.Accept, true, false)
}

// reparentBody is the body of reparentIssue.
type reparentBody struct {
	// NewParentID is the new parent; "" detaches the issue. Required.
	NewParentID *string `json:"newParentId,omitempty"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// reparentIssue moves an issue under another parent.
//
// @Summary Reparent an issue
// @Description Replaces the parent of the issue of the path atomically — the same operation as the MCP tool skein_reparent; "" detaches it. If-Match is required. The new parent must be open, in the same project and not a descendant. Side effects: the parent-child edge events; the issue.reparented event.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param If-Match header string true "The version of the issue, as its ETag gives it"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body reparentBody true "The new parent"
// @Success 200 {object} issues.IssueDetail "The issue under its new parent"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 412 {object} ProblemDocument "If-Match is stale (code version_conflict)"
// @Failure 428 {object} ProblemDocument "If-Match is missing (code precondition_required)"
// @Failure 422 {object} ProblemDocument "The new parent out of reach (code issue_not_found, as skein_reparent answers it), closed (parent_closed), in another project (reparent_cross_project), the issue itself (reparent_self), a cycle (dependency_cycle); a serialization race (serialization_retry)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/reparent [post]
// @ID reparentIssue
func (a *issueWrites) reparentIssue(w http.ResponseWriter, r *http.Request) {
	var body reparentBody
	a.withBody(w, r, &body, &body.Kind, issueops.Reparent, true, false)
}

// dropDelegated dismisses a delegated issue from the caller's outbox.
//
// @Summary Drop a delegated issue
// @Description Removes the issue of the path from the key owner's outbox of delegated issues — the same operation as the MCP tool skein_drop_delegated; only the owner's own view changes, the issue does not. Side effects: none on the issue.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param body body kindBody false "Options"
// @Success 200 {object} issues.IssueDetail "The issue"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "The issue is not one the owner delegated (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/drop-delegated [post]
// @ID dropDelegated
func (a *issueWrites) dropDelegated(w http.ResponseWriter, r *http.Request) {
	a.simple(w, r, func(ctx context.Context, d issueops.Deps, u uuid.UUID, args issueops.Args) (issueops.IssueResult, *issueops.Failure) {
		return issueops.Delegation(ctx, d, u, args, true)
	}, false, false)
}

// resumeDelegated puts a dropped delegated issue back into the outbox.
//
// @Summary Resume a delegated issue
// @Description Puts the issue of the path back into the key owner's outbox of delegated issues — the same operation as the MCP tool skein_resume_delegated; the issue itself does not change.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param body body kindBody false "Options"
// @Success 200 {object} issues.IssueDetail "The issue"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "The issue is not one the owner delegated (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/resume-delegated [post]
// @ID resumeDelegated
func (a *issueWrites) resumeDelegated(w http.ResponseWriter, r *http.Request) {
	a.simple(w, r, func(ctx context.Context, d issueops.Deps, u uuid.UUID, args issueops.Args) (issueops.IssueResult, *issueops.Failure) {
		return issueops.Delegation(ctx, d, u, args, false)
	}, false, false)
}

// attachBody is the body of attachCommits.
type attachBody struct {
	// Commits to attach (1..10). Required.
	Commits []commitInput `json:"commits"`
	// Kind of the write: directed or autonomous (checked).
	Kind *string `json:"kind,omitempty"`
}

// attachCommits attaches commits to an issue.
//
// @Summary Attach commits to an issue
// @Description Links commits to the issue of the path without touching its status or version — the same operation as the MCP tool skein_commits_attach; a repeat of a hash is a no-op. The lock guards it: only the holder attaches to a claimed issue; force=true (the key needs manage, the owner force_unlock) bypasses another holder. Side effects: issue.commits_attached.
// @Tags Issues
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param force query bool false "The privileged mode: manage and force_unlock"
// @Param body body attachBody true "The commits"
// @Success 200 {object} issues.IssueDetail "The issue with its commits"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, force not true|false (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write, or manage for force (code insufficient_scope); the owner may not write (forbidden) or force (force_unlock_forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "Locked by another holder (code locked_by_other); a commit invalid (validation_error); a repo unknown or ambiguous (repo_not_found, repo_ambiguous)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/commits [post]
// @ID attachCommits
func (a *issueWrites) attachCommits(w http.ResponseWriter, r *http.Request) {
	var body attachBody
	a.withBody(w, r, &body, &body.Kind, issueops.AttachCommits, false, true)
}

// DetachedCommits is the answer of detachCommit.
type DetachedCommits struct {
	// Issue after the detach.
	Issue *issues.IssueDetail `json:"issue"`
	// Detached are the links removed.
	Detached []issues.DetachedCommitRef `json:"detached"`
	// NotFound are the hashes the issue had no link with.
	NotFound []string `json:"notFound"`
}

// detachCommit removes the links of a commit from an issue.
//
// @Summary Detach a commit from an issue
// @Description Removes every link of the issue of the path with the commit hash of the path, whatever its repo — the same operation as the MCP tool skein_commits_detach; a hash the issue has no link with is reported in notFound. The lock guards it: only the holder detaches from a claimed issue; force=true (the key needs manage, the owner force_unlock) bypasses another holder. Side effects: issue.commits_detached when something was removed.
// @Tags Issues
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param hash path string true "The commit hash, lowercase hex 7..64"
// @Param force query bool false "The privileged mode: manage and force_unlock"
// @Success 200 {object} DetachedCommits "What was detached"
// @Failure 400 {object} ProblemDocument "Unknown parameter, force not true|false (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write, or manage for force (code insufficient_scope); the owner may not write (forbidden) or force (force_unlock_forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "Locked by another holder (code locked_by_other); a malformed hash (validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/commits/{hash} [delete]
// @ID detachCommit
func (a *issueWrites) detachCommit(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body kindBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	args = issueops.Args{"issueId": id, "hashes": []any{chi.URLParam(r, "hash")}}
	forceArg(r, args)
	res, f := issueops.DetachCommits(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	out := DetachedCommits{Issue: res.Issue, Detached: []issues.DetachedCommitRef{}, NotFound: []string{}}
	if res.Detach != nil {
		if res.Detach.Detached != nil {
			out.Detached = res.Detach.Detached
		}
		if res.Detach.NotFound != nil {
			out.NotFound = res.Detach.NotFound
		}
	}
	if out.Issue != nil {
		SetETag(w, out.Issue.Version)
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// issueOp is an operation of issueops on the issue of the path.
type issueOp func(ctx context.Context, d issueops.Deps, userID uuid.UUID, a issueops.Args) (issueops.IssueResult, *issueops.Failure)

// simple serves an operation whose body holds only kind.
func (a *issueWrites) simple(w http.ResponseWriter, r *http.Request, op issueOp, version, force bool) {
	var body kindBody
	a.withBody(w, r, &body, &body.Kind, op, version, force)
}

// withBody serves an operation on the issue of the path with the body
// typed: id (or issueId for the commit tools) from the path, the expected
// version from If-Match, the mode from ?force=.
func (a *issueWrites) withBody(w http.ResponseWriter, r *http.Request, typed any, kind **string, op issueOp, version, force bool) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	args, ok := decodeArgs(w, r, typed)
	if !ok || !kindOf(w, *kind) {
		return
	}
	delete(args, "kind")
	args["id"] = id
	args["issueId"] = id
	if version {
		expectedVersionArg(r, args)
	}
	if force {
		forceArg(r, args)
	}
	res, f := op(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	writeIssue(w, r, http.StatusOK, res.Issue)
}
