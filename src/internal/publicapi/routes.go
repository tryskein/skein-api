package publicapi

// routes.go — the route table of the public API (plan D-2): the one
// declaration from which the router, the key-right check, the OCC and
// Idempotency middlewares and the re-check of the owner's role on a replay
// are built. The product table is ProductRoutes; tests build routers from
// tables of their own, so a test route never reaches the product.

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/search"
	"skein-api/internal/users"
)

// PathPrefix is the prefix of every route of the public API. The public port
// serves nothing outside it (INV-10).
const PathPrefix = "/api/v1/"

// Auth says how a route authenticates.
type Auth uint8

// The authentication modes.
const (
	// AuthKey — the request authenticates with an API key; the key must hold
	// the route's right. Every operation of the API but the document of the
	// API itself.
	AuthKey Auth = iota + 1
	// AuthNone — no key: only getOpenApi.
	AuthNone
)

// IfMatch says whether a route checks the version of its resource.
type IfMatch uint8

// The If-Match rules (spec, "General rules of the surface").
const (
	// IfMatchNone — the route does not check a version; an If-Match header
	// has no effect (unclaimIssue, routes of resources without a version).
	IfMatchNone IfMatch = iota
	// IfMatchRequired — the route refuses a request without If-Match (428)
	// and a stale one (412 version_conflict): updateIssue, closeIssue,
	// reparentIssue, updateComment.
	IfMatchRequired
	// IfMatchOptional — If-Match is checked when present (acceptIssue).
	IfMatchOptional
)

// ActionObject is the object the owner's effective authz actions of a route
// are checked on.
type ActionObject uint8

// The objects.
const (
	// ObjectNone — the route checks no project action (reads of the caller's
	// own state, user operations decided by the admin right).
	ObjectNone ActionObject = iota
	// ObjectPathProject — the project named by the path.
	ObjectPathProject
	// ObjectPathIssueProject — the project of the issue named by the path.
	ObjectPathIssueProject
	// ObjectUser — the user named by the path or the body.
	ObjectUser
)

// Route is one operation of the public API.
type Route struct {
	// OperationID is the operationId of the operation in the OpenAPI
	// document; refusals of the key right name it.
	OperationID string
	// Method and Path route the request; Path is a chi pattern under
	// PathPrefix.
	Method string
	Path   string
	// Auth is how the request authenticates.
	Auth Auth
	// Key is what an ordinary call requires of the key.
	Key authz.KeyRequirement
	// Guarded marks an operation whose force=true mode bypasses another
	// holder's claim, the version check and the pin in one call: in that
	// mode the key needs manage and the owner force_unlock on the project of
	// the target (spec, "Rights matrix"; ADR-3).
	Guarded bool
	// Actions are the owner's authz actions an ordinary call needs on
	// Object. They are re-checked before a stored Idempotency response is
	// given out (US-7).
	Actions []authz.Action
	// Object is what Actions are checked on.
	Object ActionObject
	// IfMatch is the version rule of the route.
	IfMatch IfMatch
	// Idempotent marks an operation that accepts Idempotency-Key; a request
	// with the header to any other operation is refused with 400
	// idempotency_not_supported.
	Idempotent bool
	// Hydrate reads the response of an Idempotent operation again from the
	// current state of its objects, for a repeat that finds the outcome
	// recorded without its body (US-7). Required when Idempotent is set.
	Hydrate Hydrator
	// Prefer marks an operation whose response honours Prefer:
	// return=minimal (a single operation returning one resource with its
	// own version).
	Prefer bool
	// Handler serves the operation.
	Handler http.Handler
}

// ObjectKind is the kind of an object a response refers to.
type ObjectKind string

// The kinds of response objects.
const (
	ObjectIssue   ObjectKind = "issue"
	ObjectComment ObjectKind = "comment"
	ObjectProject ObjectKind = "project"
)

// ObjectRef names one object of a response: an issue by its hierarchical
// id, a comment or a project by its UUID.
type ObjectRef struct {
	Kind ObjectKind `json:"kind"`
	ID   string     `json:"id"`
}

// Hydrator builds the response of an operation from the current state of
// objects, the objects its first execution answered with. The result is
// encoded as JSON; its id and version members are replaced by the recorded
// ones.
type Hydrator func(ctx context.Context, r *http.Request, objects []ObjectRef) (any, error)

// Mode is the part of a request that changes what an operation requires.
type Mode struct {
	// Force — the request asks for force=true.
	Force bool
}

// KeyRequirement is what a call of the route in mode m requires of the key:
// force=true on a guarded operation raises the right to manage.
func (rt Route) KeyRequirement(m Mode) authz.KeyRequirement {
	req := rt.Key
	if m.Force && rt.Guarded {
		req.Right = authz.KeyRightManage
	}
	return req
}

// EffectiveActions are the owner's authz actions a call of the route in mode
// m needs on its Object: force=true on a guarded operation adds
// force_unlock.
func (rt Route) EffectiveActions(m Mode) []authz.Action {
	out := slices.Clone(rt.Actions)
	if m.Force && rt.Guarded && !slices.Contains(out, authz.ActionForceUnlock) {
		out = append(out, authz.ActionForceUnlock)
	}
	return out
}

// validRoute reports the first defect of a table row, or "" when it has
// none. A defective table is a programmer error: NewRouter panics on it.
func validRoute(rt Route) string {
	switch {
	case rt.OperationID == "":
		return "route without operationId"
	case !strings.HasPrefix(rt.Path, PathPrefix):
		return rt.OperationID + ": path " + rt.Path + " outside " + PathPrefix
	case rt.Method == "":
		return rt.OperationID + ": no method"
	case rt.Auth != AuthKey && rt.Auth != AuthNone:
		return rt.OperationID + ": no authentication mode"
	case rt.Handler == nil:
		return rt.OperationID + ": no handler"
	case rt.Idempotent && rt.Hydrate == nil:
		return rt.OperationID + ": accepts Idempotency-Key without a hydrator"
	}
	return ""
}

// Deps are what the operations of the product table read and write
// through: the same stores the MCP tools use (R-3).
type Deps struct {
	// Doc is the OpenAPI document served by getOpenApi.
	Doc *OpenAPIDocument
	// Pool is the database.
	Pool *pgxpool.Pool
	// Projects is the projects store.
	Projects *projects.Store
	// Users is the users store (username lookups).
	Users *users.Store
	// ProjectsEmbedder reranks listProjects by meaning; nil — full-text
	// search only.
	ProjectsEmbedder projects.QueryEmbedder
	// DefaultVisibility is SKEIN_PROJECT_DEFAULT_VISIBILITY.
	DefaultVisibility string
	// Issues is the issues store.
	Issues *issues.Store
	// Dependencies is the dependencies store.
	Dependencies *dependencies.Store
	// Search is the search service.
	Search *search.Service
	// IssuesEmbedder reranks listIssues by meaning; nil — full-text only.
	IssuesEmbedder issues.QueryEmbedder
	// Outcomes records the outcome of an operation that accepts
	// Idempotency-Key (idempotency.Hooks).
	Outcomes Outcomes
	// Notifications and Reminders are the stores of the caller's own state.
	Notifications *notifications.Store
	Reminders     *reminders.Store
	// Attachments are the attachment store and object storage, as the MCP
	// tool skein_attachments gets them; S3 nil — storage not configured.
	// Its Pool defaults to Pool.
	Attachments issueops.AttachmentDeps
}

// ProductRoutes is the route table of the public API. Every operation of
// the API is a row here; the resource operations are added by their own
// changes.
func ProductRoutes(d Deps) []Route {
	pa := &projectsAPI{
		pool: d.Pool, projects: d.Projects, users: d.Users,
		embedder: d.ProjectsEmbedder, defaultVisibility: d.DefaultVisibility,
	}
	ia := &issuesAPI{
		pool: d.Pool, issues: d.Issues, deps: d.Dependencies, projects: d.Projects,
		search: d.Search, embedder: d.IssuesEmbedder,
	}
	att := d.Attachments
	if att.Pool == nil {
		att.Pool = d.Pool
	}
	wa := &issueWrites{
		issuesAPI: ia,
		ops: issueops.Deps{Pool: d.Pool, Issues: d.Issues, Deps: d.Dependencies, Projects: d.Projects,
			Notifications: d.Notifications, Reminders: d.Reminders},
		outcomes:    d.Outcomes,
		attachments: att,
	}
	read := authz.KeyRequirement{Right: authz.KeyRightRead}
	manage := authz.KeyRequirement{Right: authz.KeyRightManage}
	key := func(id, method, path string, req authz.KeyRequirement, obj ActionObject, actions []authz.Action, h http.HandlerFunc) Route {
		return Route{
			OperationID: id, Method: method, Path: PathPrefix + path, Auth: AuthKey,
			Key: req, Object: obj, Actions: actions, Handler: h,
		}
	}
	view := []authz.Action{authz.ActionView}
	manageProject := []authz.Action{authz.ActionManageProject}
	manageMembers := []authz.Action{authz.ActionManageMembers}
	return []Route{
		{
			OperationID: OperationGetOpenAPI,
			Method:      http.MethodGet,
			Path:        OpenAPIPath,
			Auth:        AuthNone,
			Key:         authz.KeyRequirement{Right: authz.KeyRightNone},
			Handler:     d.Doc,
		},

		// Projects (T022).
		key(OperationGetMe, http.MethodGet, "me", authz.KeyRequirement{Right: authz.KeyRightNone}, ObjectNone, nil, pa.getMe),
		key(OperationListProjects, http.MethodGet, "projects", read, ObjectNone, nil, pa.listProjects),
		key(OperationResolveProject, http.MethodGet, "projects/resolve", read, ObjectNone, nil, pa.resolveProject),
		key(OperationCreateProject, http.MethodPost, "projects",
			authz.KeyRequirement{Right: authz.KeyRightManage, WideScope: true}, ObjectNone, nil, pa.createProject),
		key(OperationGetProject, http.MethodGet, "projects/{project}", read, ObjectPathProject, view, pa.getProject),
		key(OperationUpdateProject, http.MethodPatch, "projects/{project}", manage, ObjectPathProject, manageProject, pa.updateProject),
		key(OperationListProjectMembers, http.MethodGet, "projects/{project}/members", read, ObjectPathProject, view, pa.listProjectMembers),
		key(OperationPutProjectMember, http.MethodPut, "projects/{project}/members/{user}", manage, ObjectPathProject, manageMembers, pa.putProjectMember),
		key(OperationDeleteProjectMember, http.MethodDelete, "projects/{project}/members/{user}", manage, ObjectPathProject, manageMembers, pa.deleteProjectMember),
		key(OperationListComponents, http.MethodGet, "projects/{project}/components", read, ObjectPathProject, view, pa.listComponents),
		key(OperationCreateComponent, http.MethodPost, "projects/{project}/components", manage, ObjectPathProject, manageProject, pa.createComponent),
		key(OperationUpdateComponent, http.MethodPatch, "projects/{project}/components/{component}", manage, ObjectPathProject, manageProject, pa.updateComponent),
		key(OperationArchiveComponent, http.MethodPost, "projects/{project}/components/{component}/archive", manage, ObjectPathProject, manageProject, pa.archiveComponent),

		// Issue reads (T023).
		key(OperationListIssues, http.MethodGet, "issues", read, ObjectNone, nil, ia.listIssues),
		key(OperationListReadyIssues, http.MethodGet, "issues/ready", read, ObjectNone, nil, ia.listReadyIssues),
		key(OperationGetIssue, http.MethodGet, "issues/{issue}", read, ObjectPathIssueProject, view, ia.getIssue),
		key(OperationListIssueEvents, http.MethodGet, "issues/{issue}/events", read, ObjectPathIssueProject, view, ia.listIssueEvents),
		key(OperationListDependencies, http.MethodGet, "issues/{issue}/dependencies", read, ObjectPathIssueProject, view, ia.listDependencies),
		key(OperationListComments, http.MethodGet, "issues/{issue}/comments", read, ObjectPathIssueProject, view, ia.listComments),
		key(OperationGetIssueMovePlan, http.MethodGet, "issues/{issue}/move-plan", read, ObjectPathIssueProject,
			[]authz.Action{authz.ActionWrite}, ia.getIssueMovePlan),
		key(OperationSearch, http.MethodGet, "search", read, ObjectNone, nil, ia.searchAll),

		// Issue writes (T024).
		writeRoute(OperationCreateIssue, http.MethodPost, "projects/{project}/issues", ObjectPathProject, wa.createIssue,
			routeFlags{prefer: true, idempotent: true, hydrate: wa.hydrateIssue}),
		writeRoute(OperationCreateEpic, http.MethodPost, "projects/{project}/epics", ObjectPathProject, wa.createEpic,
			routeFlags{prefer: true, idempotent: true, hydrate: wa.hydrateEpic}),
		writeRoute(OperationUpdateIssue, http.MethodPatch, "issues/{issue}", ObjectPathIssueProject, wa.updateIssue,
			routeFlags{prefer: true, guarded: true, ifMatch: IfMatchRequired}),
		writeRoute(OperationClaimIssue, http.MethodPost, "issues/{issue}/claim", ObjectPathIssueProject, wa.claimIssue,
			routeFlags{prefer: true, actions: []authz.Action{authz.ActionAssign}}),
		writeRoute(OperationClaimIssues, http.MethodPost, "issues/claim", ObjectNone, wa.claimIssues, routeFlags{noActions: true}),
		writeRoute(OperationUnclaimIssue, http.MethodPost, "issues/{issue}/unclaim", ObjectPathIssueProject, wa.unclaimIssue,
			routeFlags{prefer: true, guarded: true}),
		writeRoute(OperationReleaseIssueLock, http.MethodPost, "issues/{issue}/release-lock", ObjectPathIssueProject, wa.releaseIssueLock,
			routeFlags{prefer: true}),
		writeRoute(OperationCloseIssue, http.MethodPost, "issues/{issue}/close", ObjectPathIssueProject, wa.closeIssue,
			routeFlags{prefer: true, guarded: true, ifMatch: IfMatchRequired, idempotent: true, hydrate: wa.hydrateIssue}),
		writeRoute(OperationAcceptIssue, http.MethodPost, "issues/{issue}/accept", ObjectPathIssueProject, wa.acceptIssue,
			routeFlags{prefer: true, ifMatch: IfMatchOptional}),
		writeRoute(OperationReparentIssue, http.MethodPost, "issues/{issue}/reparent", ObjectPathIssueProject, wa.reparentIssue,
			routeFlags{prefer: true, ifMatch: IfMatchRequired}),
		writeRoute(OperationDropDelegated, http.MethodPost, "issues/{issue}/drop-delegated", ObjectPathIssueProject, wa.dropDelegated, routeFlags{}),
		writeRoute(OperationResumeDelegated, http.MethodPost, "issues/{issue}/resume-delegated", ObjectPathIssueProject, wa.resumeDelegated, routeFlags{}),
		writeRoute(OperationAttachCommits, http.MethodPost, "issues/{issue}/commits", ObjectPathIssueProject, wa.attachCommits,
			routeFlags{guarded: true}),
		writeRoute(OperationDetachCommit, http.MethodDelete, "issues/{issue}/commits/{hash}", ObjectPathIssueProject, wa.detachCommit,
			routeFlags{guarded: true}),

		// Batches (T025).
		writeRoute(OperationBulkUpdateIssues, http.MethodPost, "issues/bulk-update", ObjectNone, wa.bulkUpdateIssues,
			routeFlags{guarded: true, noActions: true}),
		writeRoute(OperationBulkCloseIssues, http.MethodPost, "issues/bulk-close", ObjectNone, wa.bulkCloseIssues,
			routeFlags{guarded: true, noActions: true}),
		writeRoute(OperationBulkAddDependencies, http.MethodPost, "issues/dependencies/bulk-add", ObjectNone, wa.bulkAddDependencies,
			routeFlags{noActions: true}),

		// Comments, dependencies, attachments (T026). A comment or an
		// attachment of the path is checked by its operation, as in MCP: the
		// route names no project of its own.
		writeRoute(OperationCreateComment, http.MethodPost, "issues/{issue}/comments", ObjectPathIssueProject, wa.createComment,
			routeFlags{prefer: true, idempotent: true, hydrate: wa.hydrateComment}),
		key(OperationGetComment, http.MethodGet, "comments/{comment}", read, ObjectNone, nil, wa.getComment),
		writeRoute(OperationUpdateComment, http.MethodPatch, "comments/{comment}", ObjectNone, wa.updateComment,
			routeFlags{prefer: true, ifMatch: IfMatchRequired, noActions: true}),
		writeRoute(OperationDeleteComment, http.MethodDelete, "comments/{comment}", ObjectNone, wa.deleteComment, routeFlags{noActions: true}),
		writeRoute(OperationAddDependency, http.MethodPost, "issues/{issue}/dependencies", ObjectPathIssueProject, wa.addDependency, routeFlags{}),
		writeRoute(OperationRemoveDependency, http.MethodDelete, "issues/{issue}/dependencies/{toIssue}/{type}", ObjectPathIssueProject,
			wa.removeDependency, routeFlags{}),
		key(OperationListAttachments, http.MethodGet, "issues/{issue}/attachments", read, ObjectPathIssueProject, view, wa.listAttachments),
		writeRoute(OperationCreateAttachmentUpload, http.MethodPost, "issues/{issue}/attachments", ObjectPathIssueProject,
			wa.createAttachmentUpload, routeFlags{}),
		writeRoute(OperationConfirmAttachment, http.MethodPost, "attachments/{attachment}/confirm", ObjectNone, wa.confirmAttachment,
			routeFlags{noActions: true}),
		key(OperationGetAttachment, http.MethodGet, "attachments/{attachment}", read, ObjectNone, nil, wa.getAttachment),
		key(OperationGetAttachmentDownloadURL, http.MethodGet, "attachments/{attachment}/download-url", read, ObjectNone, nil,
			wa.getAttachmentDownloadURL),
		writeRoute(OperationDeleteAttachment, http.MethodDelete, "attachments/{attachment}", ObjectNone, wa.deleteAttachment,
			routeFlags{noActions: true}),

		// The caller's own state (T027): the operation checks what it acts on,
		// as in MCP; nothing here takes an action of a project.
		key(OperationListNotifications, http.MethodGet, "notifications", read, ObjectNone, nil, wa.listNotifications),
		writeRoute(OperationMarkNotificationRead, http.MethodPost, "notifications/{notification}/read", ObjectNone,
			wa.markNotificationRead, routeFlags{noActions: true}),
		writeRoute(OperationMarkAllNotificationsRead, http.MethodPost, "notifications/read-all", ObjectNone,
			wa.markAllNotificationsRead, routeFlags{noActions: true}),
		writeRoute(OperationArchiveNotification, http.MethodPost, "notifications/{notification}/archive", ObjectNone,
			wa.archiveNotification, routeFlags{noActions: true}),
		writeRoute(OperationMarkIssueNotificationsRead, http.MethodPost, "issues/{issue}/notifications/read", ObjectNone,
			wa.markIssueNotificationsRead, routeFlags{noActions: true}),
		writeRoute(OperationMarkWatchingCardRead, http.MethodPost, "me/watching/issues/{issue}/read", ObjectNone,
			wa.markWatchingCardRead, routeFlags{noActions: true}),
		key(OperationListWatching, http.MethodGet, "me/watching", read, ObjectNone, nil, wa.listWatching),
		writeRoute(OperationWatchIssue, http.MethodPut, "issues/{issue}/watch", ObjectNone, wa.watchIssue, routeFlags{noActions: true}),
		writeRoute(OperationUnwatchIssue, http.MethodDelete, "issues/{issue}/watch", ObjectNone, wa.unwatchIssue, routeFlags{noActions: true}),
		writeRoute(OperationWatchProject, http.MethodPut, "projects/{project}/watch", ObjectNone, wa.watchProject, routeFlags{noActions: true}),
		writeRoute(OperationUnwatchProject, http.MethodDelete, "projects/{project}/watch", ObjectNone, wa.unwatchProject,
			routeFlags{noActions: true}),
		key(OperationListWatchFilters, http.MethodGet, "me/watch-filters", read, ObjectNone, nil, wa.listWatchFilters),
		writeRoute(OperationCreateWatchFilter, http.MethodPost, "me/watch-filters", ObjectNone, wa.createWatchFilter,
			routeFlags{noActions: true}),
		writeRoute(OperationDeleteWatchFilter, http.MethodDelete, "me/watch-filters/{filter}", ObjectNone, wa.deleteWatchFilter,
			routeFlags{noActions: true}),
		key(OperationListReminders, http.MethodGet, "reminders", read, ObjectNone, nil, wa.listReminders),
		writeRoute(OperationCreateReminder, http.MethodPost, "issues/{issue}/reminders", ObjectNone, wa.createReminder,
			routeFlags{noActions: true}),
		writeRoute(OperationSnoozeReminder, http.MethodPost, "reminders/{reminder}/snooze", ObjectNone, wa.snoozeReminder,
			routeFlags{noActions: true}),
		writeRoute(OperationDismissReminder, http.MethodDelete, "reminders/{reminder}", ObjectNone, wa.dismissReminder,
			routeFlags{noActions: true}),
	}
}

// Outcomes are the hooks an operation that accepts Idempotency-Key records
// its outcome through (spec US-7, plan D-8): the idempotency package
// implements them (idempotency.Hooks) and main wires it, so this package
// does not import its own middleware. Each hook does nothing for a request
// without Idempotency-Key.
type Outcomes interface {
	// IssueBeforeCommit is the before-commit hook of an issue write that
	// answers status with the issue it wrote: it records the committed id
	// and version in the write's own transaction (D-8 rule 2).
	IssueBeforeCommit(status int) func(ctx context.Context, tx pgx.Tx, st issues.CommittedState) error
	// CommentBeforeCommit is the before-commit hook of createComment, which
	// answers status with the comment it wrote: it records the comment's id
	// and version in the comment's own transaction.
	CommentBeforeCommit(status int) func(ctx context.Context, tx pgx.Tx, c *issues.CommentDTO) error
	// NoteTarget names the object the operation acts on.
	NoteTarget(ctx context.Context, ref ObjectRef)
	// NoteObjects names the objects of a response produced without a
	// writing transaction (the candidates of duplicate_suspected).
	NoteObjects(ctx context.Context, refs ...ObjectRef)
}

// routeFlags are the request rules of a write route.
type routeFlags struct {
	prefer, guarded, idempotent bool
	ifMatch                     IfMatch
	hydrate                     Hydrator
	// actions are the owner's actions on the object; nil — write.
	actions []authz.Action
	// noActions — the route checks no action on one object (a batch).
	noActions bool
}

// writeRoute is a row of an operation that needs the key right write.
func writeRoute(id, method, path string, obj ActionObject, h http.HandlerFunc, f routeFlags) Route {
	actions := f.actions
	if actions == nil && !f.noActions {
		actions = []authz.Action{authz.ActionWrite}
	}
	return Route{
		OperationID: id, Method: method, Path: PathPrefix + path, Auth: AuthKey,
		Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Guarded: f.guarded,
		Actions: actions, Object: obj, IfMatch: f.ifMatch, Idempotent: f.idempotent,
		Hydrate: f.hydrate, Prefer: f.prefer, Handler: h,
	}
}
