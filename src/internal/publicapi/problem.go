package publicapi

// problem.go — the error format of the public API (RFC 9457) and the closed
// mapping of an errcode code to an HTTP status.

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/errcode"
	"skein-api/internal/spaces"
)

// ProblemContentType is the media type of every error response of the public
// API (RFC 9457, section 3).
const ProblemContentType = "application/problem+json"

// Origin is where a failure comes from. For most codes the status does not
// depend on it; for two classes it does:
//
//   - a "not found" code answers 404 when the object is the one the path
//     names, and 422 with the same code when the object is a reference in
//     the request body (a parent, a dependency, a close target, duplicateOf,
//     the issues of a batch, the project of a filter) — the code is the one
//     MCP answers with;
//   - validation_error answers 400 when the request parsing layer refuses
//     the request (decoder, form, parameters, limit) and 422 when the domain
//     operation refuses it.
//
// A handler passes the origin explicitly; there is no default.
type Origin uint8

// The origins.
const (
	// OriginPath — the object named by the path of the request.
	OriginPath Origin = iota + 1
	// OriginBody — an object referenced from the request body or query.
	OriginBody
	// OriginParse — the layer that parses the request.
	OriginParse
	// OriginDomain — the domain operation the request runs.
	OriginDomain
)

// String names the origin.
func (o Origin) String() string {
	switch o {
	case OriginPath:
		return "path"
	case OriginBody:
		return "body"
	case OriginParse:
		return "parse"
	case OriginDomain:
		return "domain"
	}
	return "unknown"
}

// Status is the HTTP status of code when the failure comes from origin. It is
// the closed mapping of the "Response codes of the public API" table by the
// class of the code; a class outside the closed set (a programmer error in
// errcode) answers 500.
func Status(code errcode.Code, origin Origin) int {
	switch code.Class() {
	case errcode.ClassNotFound:
		if origin == OriginPath {
			return http.StatusNotFound
		}
		return http.StatusUnprocessableEntity
	case errcode.ClassFormat:
		if origin == OriginParse {
			return http.StatusBadRequest
		}
		return http.StatusUnprocessableEntity
	case errcode.ClassUnauthenticated:
		return http.StatusUnauthorized
	case errcode.ClassPermission:
		return http.StatusForbidden
	case errcode.ClassVersion:
		return http.StatusPreconditionFailed
	case errcode.ClassUnavailable:
		return http.StatusServiceUnavailable
	case errcode.ClassInternal:
		return http.StatusInternalServerError
	case errcode.ClassBusiness:
		return http.StatusUnprocessableEntity
	case errcode.ClassMethod:
		return http.StatusMethodNotAllowed
	case errcode.ClassPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case errcode.ClassMediaType:
		return http.StatusUnsupportedMediaType
	case errcode.ClassRateLimited:
		return http.StatusTooManyRequests
	case errcode.ClassPreconditionRequired:
		return http.StatusPreconditionRequired
	case errcode.ClassConflict:
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// Problem is one error response. The members type, title and status are
// those of RFC 9457; code is the errcode code (the same one MCP answers
// with); retryable is present, and true, only for a transient code — the
// same request may succeed later without any change by the client.
// Extensions carries the members a code adds (the required right of
// insufficient_scope, the current version of version_conflict, …); it never
// overrides the members above.
type Problem struct {
	Code       errcode.Code
	Origin     Origin
	Detail     string
	Extensions map[string]any
}

// ProblemDocument is the body of every error response, as the OpenAPI
// document describes it (WriteProblem writes it). Beyond the RFC 9457
// members and code it declares the members refusals of several operations
// share; a refusal may carry more members, named in the operation's
// description. Every extension member is optional: a refusal carries only
// the ones of its code.
type ProblemDocument struct {
	// Type is always "about:blank": the code says what happened.
	Type string `json:"type"`
	// Title is the phrase of the status.
	Title string `json:"title"`
	// Status is the HTTP status.
	Status int `json:"status"`
	// Code is the machine code of the refusal — the same one MCP answers.
	Code string `json:"code"`
	// Detail explains the refusal to a human.
	Detail string `json:"detail,omitempty"`
	// Retryable is true for a transient code: the same request may succeed
	// later without any change.
	Retryable bool `json:"retryable,omitempty"`
	// Validation names the case of a validation_error refused by the
	// domain: the validator's own code (invalid_title,
	// close_target_required, nul_character, …).
	Validation string `json:"validation,omitempty"`
	// Field names the refused value: the path of a body member
	// (labels[1], dependencies[0].reason), a query parameter, or path.
	Field string `json:"field,omitempty"`
	// CurrentVersion is the current version of the object of a
	// version_conflict.
	CurrentVersion int `json:"currentVersion,omitempty"`
	// LockedBy is the holder of the lock of locked_by_other and
	// already_claimed.
	LockedBy string `json:"lockedBy,omitempty"`
	// Duplicates are the similar issues of duplicate_suspected.
	Duplicates []dependencies.DuplicateCandidate `json:"duplicates,omitempty"`
	// Degraded is true on duplicate_suspected when only the lexical check
	// ran.
	Degraded bool `json:"degraded,omitempty"`
	// Candidates are the projects an ambiguous_project handle matched.
	Candidates []spaces.AmbiguousProjectCandidate `json:"candidates,omitempty"`
	// RequiredRight is the key right insufficient_scope requires: read,
	// write, manage or admin.
	RequiredRight string `json:"requiredRight,omitempty"`
	// RequiredScope is "spaces or all" when insufficient_scope also
	// requires a wide key scope.
	RequiredScope string `json:"requiredScope,omitempty"`
	// OperationID is the operation insufficient_scope refused.
	OperationID string `json:"operationId,omitempty"`
	// The members below name the object a "not found" refusal is about,
	// one member per code whichever operation refused (objectMembers).
	// IssueID is the issue of issue_not_found.
	IssueID string `json:"issueId,omitempty"`
	// Project is the project of project_not_found and ambiguous_project,
	// as the request named it (UUID, prefix, alias or name).
	Project string `json:"project,omitempty"`
	// User is the user of user_not_found and member_not_found, as the
	// request named it (UUID or username).
	User string `json:"user,omitempty"`
	// CommentID is the comment of comment_not_found.
	CommentID string `json:"commentId,omitempty"`
	// ComponentID is the component of component_not_found.
	ComponentID string `json:"componentId,omitempty"`
	// AttachmentID is the attachment of attachment_not_found.
	AttachmentID string `json:"attachmentId,omitempty"`
	// NotificationID is the notification of notification_not_found.
	NotificationID string `json:"notificationId,omitempty"`
	// FilterID is the watch filter of watch_filter_not_found.
	FilterID string `json:"filterId,omitempty"`
	// ReminderID is the reminder of reminder_not_found.
	ReminderID string `json:"reminderId,omitempty"`
	// Space is the short id of the space of space_not_found.
	Space string `json:"space,omitempty"`
	// ParentID is the parent of parent_not_found.
	ParentID string `json:"parentId,omitempty"`
}

// problemType is the RFC 9457 type of every problem: the code, not a URI,
// says what happened, so the type is "about:blank" and the title is the
// status phrase (RFC 9457, section 4.2.1).
const problemType = "about:blank"

// WriteProblem writes p as an application/problem+json response with the
// status Status(p.Code, p.Origin).
func WriteProblem(w http.ResponseWriter, p Problem) {
	status := Status(p.Code, p.Origin)
	body := make(map[string]any, len(p.Extensions)+6)
	for k, v := range p.Extensions {
		body[k] = v
	}
	nameObject(p.Code, body)
	body["type"] = problemType
	body["title"] = http.StatusText(status)
	body["status"] = status
	body["code"] = p.Code.String()
	if p.Detail != "" {
		body["detail"] = p.Detail
	} else {
		delete(body, "detail")
	}
	if p.Code.Transient() {
		body["retryable"] = true
	} else {
		delete(body, "retryable")
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("public API: writing a problem response failed",
			"code", p.Code.String(), "status", status, "error", err.Error())
	}
}

// objectMembers is the one member that names the object of a refusal of
// code, and the members the shared operations (issueops, the MCP tools)
// name it by, which the public API answers under that one member. A client
// reads the object of issue_not_found from issueId whichever operation
// refused it; MCP keeps its own members.
var objectMembers = map[errcode.Code]struct {
	member  string
	aliases []string
}{
	errcode.IssueNotFound:        {"issueId", []string{"id"}},
	errcode.ProjectNotFound:      {"project", []string{"q", "projectId", "id"}},
	errcode.AmbiguousProject:     {"project", []string{"q"}},
	errcode.CommentNotFound:      {"commentId", []string{"id"}},
	errcode.ComponentNotFound:    {"componentId", []string{"id"}},
	errcode.AttachmentNotFound:   {"attachmentId", []string{"id"}},
	errcode.NotificationNotFound: {"notificationId", []string{"id"}},
	errcode.WatchFilterNotFound:  {"filterId", []string{"id"}},
	errcode.ReminderNotFound:     {"reminderId", []string{"id"}},
	errcode.UserNotFound:         {"user", []string{"userId", "username", "id"}},
	errcode.MemberNotFound:       {"user", []string{"userId", "id"}},
	errcode.SpaceNotFound:        {"space", []string{"spaceId", "id"}},
	errcode.ParentNotFound:       {"parentId", []string{"id"}},
}

// nameObject moves the object of a refusal of code from an alias member of
// body to the member objectMembers names for code. The first alias present
// wins when the member itself is absent; every alias is dropped, so no
// refusal carries the object twice under two names.
func nameObject(code errcode.Code, body map[string]any) {
	m, ok := objectMembers[code]
	if !ok {
		return
	}
	for _, alias := range m.aliases {
		v, present := body[alias]
		if !present {
			continue
		}
		if _, set := body[m.member]; !set {
			body[m.member] = v
		}
		delete(body, alias)
	}
}

// WriteInternalError answers a failure inside the service while serving the
// route of r, as writeInternal does: logged, then 503 database_unavailable
// or 500 internal_error.
func WriteInternalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	rt, _ := RouteFromContext(r.Context())
	writeInternal(w, r, rt, what, err)
}

// WriteInsufficientScope answers a key that does not meet req, the
// requirement of rt: 403 insufficient_scope naming the required right (and
// the required scope of a wide-scope operation), counted by
// ScopeRefusalsTotal.
func WriteInsufficientScope(w http.ResponseWriter, rt Route, req authz.KeyRequirement) {
	ScopeRefusalsTotal.WithLabelValues(req.Right.String()).Inc()
	ext := map[string]any{"requiredRight": req.Right.String(), "operationId": rt.OperationID}
	if req.WideScope {
		ext["requiredScope"] = "spaces or all"
	}
	WriteProblem(w, Problem{Code: errcode.InsufficientScope, Origin: OriginDomain, Extensions: ext})
}

// writeInternal logs a failure inside the service and answers it: 503
// database_unavailable when the database could not be reached, 500
// internal_error otherwise.
func writeInternal(w http.ResponseWriter, r *http.Request, rt Route, what string, err error) {
	code := errcode.InternalError
	if isDatabaseUnavailable(err) {
		code = errcode.DatabaseUnavailable
	}
	slog.ErrorContext(r.Context(), "public API: "+what+" failed",
		"operationId", rt.OperationID, "code", code.String(), "error", err.Error())
	WriteProblem(w, Problem{Code: code, Origin: OriginDomain})
}
