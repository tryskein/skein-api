package publicapi

// personal.go — the operations of the caller's own state of the public API
// (spec sk-878.3.1, "API surface", T027): notifications, Watching,
// subscriptions and their filters, reminders. Thin adapters over the
// operations of internal/issueops that the MCP tools skein_notifications,
// skein_watchers and skein_remind also call (INV-4). A write by a key
// changes only the state inside its scope (US-3 «write acceptance»): read
// all leaves the notifications of a project outside it unread, a filter
// over such a project is neither listed nor deleted, a reminder on its issue
// answers as a missing one.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/issueops"
	"skein-api/internal/notifications"
	"skein-api/internal/spaces"
)

// The operationIds of the operations of the caller's own state.
const (
	OperationListNotifications          = "listNotifications"
	OperationMarkNotificationRead       = "markNotificationRead"
	OperationMarkAllNotificationsRead   = "markAllNotificationsRead"
	OperationArchiveNotification        = "archiveNotification"
	OperationMarkIssueNotificationsRead = "markIssueNotificationsRead"
	OperationMarkWatchingCardRead       = "markWatchingCardRead"
	OperationListWatching               = "listWatching"
	OperationWatchIssue                 = "watchIssue"
	OperationUnwatchIssue               = "unwatchIssue"
	OperationWatchProject               = "watchProject"
	OperationUnwatchProject             = "unwatchProject"
	OperationListWatchFilters           = "listWatchFilters"
	OperationCreateWatchFilter          = "createWatchFilter"
	OperationDeleteWatchFilter          = "deleteWatchFilter"
	OperationListReminders              = "listReminders"
	OperationCreateReminder             = "createReminder"
	OperationSnoozeReminder             = "snoozeReminder"
	OperationDismissReminder            = "dismissReminder"
)

// The page sizes of the lists of the caller's own state.
var (
	notificationsLimit = Limit{Min: 1, Max: 100, Default: 25}
	watchingLimit      = Limit{Min: 1, Max: 100, Default: 50}
)

// NotificationPage is a page of the caller's notifications.
type NotificationPage struct {
	List[notifications.NotificationDTO]
	// Invitations are the pending invitations to the caller, on the first
	// page of the live scopes; omitted otherwise.
	Invitations []spaces.PendingInvitation `json:"invitations,omitempty"`
}

// NotificationsMarked is the answer of a mark-read operation.
type NotificationsMarked struct {
	// Updated is how many notifications the call marked read; 0 is an
	// answer, not a refusal.
	Updated int64 `json:"updated"`
}

// NotificationArchived is the answer of archiveNotification.
type NotificationArchived struct {
	// ID of the notification.
	ID int64 `json:"id"`
	// Archived is always true.
	Archived bool `json:"archived"`
}

// IssueWatch is the answer of watchIssue and unwatchIssue.
type IssueWatch struct {
	// IssueID of the issue.
	IssueID string `json:"issueId"`
	// Watching: the caller watches the issue after the call.
	Watching bool `json:"watching"`
}

// ProjectWatch is the answer of watchProject and unwatchProject.
type ProjectWatch struct {
	// ProjectID of the project.
	ProjectID string `json:"projectId"`
	// Watching: the caller watches the project after the call.
	Watching bool `json:"watching"`
	// EventTypes the call narrowed the subscription to; omitted when it set
	// none.
	EventTypes []string `json:"eventTypes,omitempty"`
}

// WatchingLists are the caller's subscriptions, each collection a page of
// its own.
type WatchingLists struct {
	// Issues, newest subscription first; issuesCursor continues them.
	Issues List[notifications.IssueWatcherForUserDTO] `json:"issues"`
	// Projects, newest subscription first; projectsCursor continues them.
	Projects List[notifications.ProjectWatcherDTO] `json:"projects"`
	// Filters, all of them (at most 10 per user: one page).
	Filters List[notifications.CustomWatcherDTO] `json:"filters"`
}

// WatchFilterDeleted is the answer of deleteWatchFilter.
type WatchFilterDeleted struct {
	// ID of the filter.
	ID string `json:"id"`
	// Deleted is always true.
	Deleted bool `json:"deleted"`
}

// ReminderPage is the caller's reminders.
type ReminderPage struct {
	List[issueops.ReminderListItem]
	// RingingTaskCount is the badge: issues with a ringing reminder across
	// projects, of the key's scope (the project filter does not narrow it).
	RingingTaskCount int `json:"ringingTaskCount"`
}

// ReminderDismissed is the answer of dismissReminder.
type ReminderDismissed struct {
	// ReminderID of the reminder.
	ReminderID string `json:"reminderId"`
	// Dismissed is always true.
	Dismissed bool `json:"dismissed"`
}

// The bodies of the writes.
type (
	// markAllBody is the body of markAllNotificationsRead.
	markAllBody struct {
		// ProjectID narrows the sweep to one project (UUID).
		ProjectID *string `json:"projectId,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
	// watchIssueBody is the body of watchIssue.
	watchIssueBody struct {
		// Subtree also watches the descendants' events on this card.
		Subtree *bool `json:"subtree,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
	// watchProjectBody is the body of watchProject.
	watchProjectBody struct {
		// EventTypes narrows the subscription to these event types; [] clears
		// the narrowing; omitted keeps it.
		EventTypes []string `json:"eventTypes,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
	// watchFilterBody is the body of createWatchFilter.
	watchFilterBody struct {
		// Name, 1..100 characters, unique per user. Required.
		Name *string `json:"name,omitempty"`
		// Filter: projects (UUIDs), event_types, types, labels, components
		// (UUIDs), priority_max. No projects — any project, which needs a key
		// of scope all. Required.
		Filter map[string]any `json:"filter,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
	// createReminderBody is the body of createReminder.
	createReminderBody struct {
		// RemindAt, RFC 3339. Required.
		RemindAt *string `json:"remindAt,omitempty"`
		// Note shown when it rings.
		Note *string `json:"note,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
	// snoozeBody is the body of snoozeReminder: exactly one of by and until.
	snoozeBody struct {
		// By, a duration from now (1h, 30m, 2d).
		By *string `json:"by,omitempty"`
		// Until, RFC 3339.
		Until *string `json:"until,omitempty"`
		// Reason recorded on the event.
		Reason *string `json:"reason,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
	// dismissBody is the body of dismissReminder.
	dismissBody struct {
		// Note recorded on the event.
		Note *string `json:"note,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
)

// kindArgs decodes a body that carries only kind (or nothing) and checks
// kind; the arguments of the operation are built by the caller.
func kindArgs(w http.ResponseWriter, r *http.Request) bool {
	var body kindBody
	_, ok := decodeArgs(w, r, &body)
	return ok && kindOf(w, body.Kind)
}

// pathNotification is the {notification} of the path as the operation's
// notificationId (a JSON number, as MCP takes it); a value that is not a
// positive integer is refused as MCP refuses it.
func pathNotification(r *http.Request) issueops.Args {
	raw := chi.URLParam(r, "notification")
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return issueops.Args{"notificationId": float64(n)}
	}
	return issueops.Args{"notificationId": raw}
}

// listNotifications reads a page of the caller's notifications.
//
// @Summary List the caller's notifications
// @Description Returns a page of the caller's notifications, newest first — the same operation as the MCP tool skein_notifications, action inbox: scope unread (default), all or archived; projectId narrows to one project. Only the notifications of the key's scope are listed. Keyset paging: pass nextCursor back as cursor while hasMore is true. The first page of a live scope also carries the caller's pending invitations. Read-only.
// @Tags Me
// @Produce json
// @Security SkeinApiKey[read]
// @Param scope query string false "unread (default), all or archived"
// @Param projectId query string false "A project UUID"
// @Param limit query int false "Page size, 1..100 (default 25)"
// @Param cursor query string false "The nextCursor of the previous page"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} NotificationPage "A page of notifications"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field, limit out of range, a foreign cursor (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "An unknown scope or a projectId that is not a UUID (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/notifications [get]
// @ID listNotifications
func (a *issueWrites) listNotifications(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "scope", "projectId", "limit", "cursor") {
		return
	}
	limit, err := ParseLimit(r, notificationsLimit)
	if err != nil {
		WriteParseError(w, err)
		return
	}
	q := r.URL.Query()
	cursor := q.Get("cursor")
	if cursor != "" {
		if _, err := notifications.DecodeCursor(cursor); err != nil {
			WriteParseError(w, ErrBadCursor)
			return
		}
	}
	args := issueops.Args{"limit": float64(limit), "cursor": cursor, "projectId": q.Get("projectId")}
	if s := q.Get("scope"); s != "" {
		args["scope"] = s
	}
	res, f := issueops.Inbox(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f)
		return
	}
	out := NotificationPage{List: List[notifications.NotificationDTO]{Items: res.Page.Items, NextCursor: res.Page.NextCursor, HasMore: res.Page.HasMore}}
	if out.Items == nil {
		out.Items = []notifications.NotificationDTO{}
	}
	if res.FirstPage {
		out.Invitations = res.Invitations
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// markNotificationRead marks one notification read.
//
// @Summary Mark a notification read
// @Description Marks the notification of the path read — the same operation as the MCP tool skein_notifications, action mark_read. A notification that is not the caller's, missing or outside the key's scope is refused with the code MCP answers, 422 validation_error. Side effects: the notification's read time; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param notification path int true "The notification's id"
// @Param body body kindBody false "Options"
// @Success 200 {object} NotificationsMarked "Marked read"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "No such notification of the caller within reach, or the id is not a positive integer (code validation_error, as MCP)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/notifications/{notification}/read [post]
// @ID markNotificationRead
func (a *issueWrites) markNotificationRead(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) || !kindArgs(w, r) {
		return
	}
	if f := issueops.MarkNotificationRead(r.Context(), a.ops, callerOf(r), pathNotification(r)); f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusOK, NotificationsMarked{Updated: 1})
}

// markAllNotificationsRead marks every unread notification read.
//
// @Summary Mark all notifications read
// @Description Marks every unread, unarchived notification of the caller read, optionally of one project — the same operation as the MCP tool skein_notifications, action mark_read_all. A key of a narrower scope marks only the notifications inside its scope: the others stay unread. Answers how many were marked. Side effects: the notifications' read time; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param body body markAllBody false "Options"
// @Success 200 {object} NotificationsMarked "Marked read"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "projectId is not a UUID (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/notifications/read-all [post]
// @ID markAllNotificationsRead
func (a *issueWrites) markAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body markAllBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	n, f := issueops.MarkAllNotificationsRead(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusOK, NotificationsMarked{Updated: n})
}

// archiveNotification archives one notification.
//
// @Summary Archive a notification
// @Description Moves the notification of the path into the archive — the same operation as the MCP tool skein_notifications, action archive. A notification that is not the caller's, missing or outside the key's scope is refused with the code MCP answers, 422 validation_error. Side effects: the notification's archive time; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param notification path int true "The notification's id"
// @Param body body kindBody false "Options"
// @Success 200 {object} NotificationArchived "Archived"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "No such notification of the caller within reach, or the id is not a positive integer (code validation_error, as MCP)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/notifications/{notification}/archive [post]
// @ID archiveNotification
func (a *issueWrites) archiveNotification(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) || !kindArgs(w, r) {
		return
	}
	id, f := issueops.ArchiveNotification(r.Context(), a.ops, callerOf(r), pathNotification(r))
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusOK, NotificationArchived{ID: id, Archived: true})
}

// markIssueNotificationsRead marks the notifications of an issue read.
//
// @Summary Mark the notifications of an issue read
// @Description Marks read every notification of the caller tied to the issue of the path, those of its comments included — the same operation as the MCP tool skein_notifications, action mark_read_issue. Answers how many were marked; 0 is an answer. Side effects: the notifications' read time; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param body body kindBody false "Options"
// @Success 200 {object} NotificationsMarked "Marked read"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "The path names no issue (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/notifications/read [post]
// @ID markIssueNotificationsRead
func (a *issueWrites) markIssueNotificationsRead(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok || !kindArgs(w, r) {
		return
	}
	n, f := issueops.MarkIssueNotificationsRead(r.Context(), a.ops, callerOf(r), issueops.Args{"issueId": id})
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, NotificationsMarked{Updated: n})
}

// markWatchingCardRead reads a Watching card.
//
// @Summary Read a Watching card
// @Description Marks read every unread notification carded to the issue of the path in the caller's Watching — with a subtree subscription, the descendants' events included — the same operation as the MCP tool skein_notifications, action mark_read_card. A key of a narrower scope marks only the events of issues inside its scope. A card that is not the caller's, missing or with nothing unread answers marked 0. Side effects: the notifications' read time; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The card's issue, hierarchical id"
// @Param body body kindBody false "Options"
// @Success 200 {object} notifications.MarkCardReadResult "Marked read"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "The path names no issue (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/me/watching/issues/{issue}/read [post]
// @ID markWatchingCardRead
func (a *issueWrites) markWatchingCardRead(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok || !kindArgs(w, r) {
		return
	}
	res, f := issueops.MarkWatchingCardRead(r.Context(), a.ops, callerOf(r), issueops.Args{"issueId": id})
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, res)
}

// watchingCursor is the keyset position of a page of one collection of
// listWatching.
type watchingCursor struct {
	Since time.Time `json:"s"`
	ID    string    `json:"i"`
}

// listWatching lists the caller's subscriptions.
//
// @Summary List the caller's subscriptions
// @Description Returns the caller's subscriptions — issues, projects and filters, each collection its own page with its own cursor (issuesCursor, projectsCursor; the filters are at most 10 and come whole). Issues and projects run newest subscription first. Only the subscriptions inside the key's scope are listed: a filter over a project outside it, or over any project under a narrower scope, is not. The MCP tool skein_watchers, action list, answers the first 50 of each instead. Read-only.
// @Tags Me
// @Produce json
// @Security SkeinApiKey[read]
// @Param limit query int false "Page size of each collection, 1..100 (default 50)"
// @Param issuesCursor query string false "The nextCursor of the previous page of issues"
// @Param projectsCursor query string false "The nextCursor of the previous page of projects"
// @Param fields query string false "Comma-separated members to return"
// @Success 200 {object} WatchingLists "The subscriptions"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field, limit out of range, a foreign cursor (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/me/watching [get]
// @ID listWatching
func (a *issueWrites) listWatching(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "limit", "issuesCursor", "projectsCursor") {
		return
	}
	limit, err := ParseLimit(r, watchingLimit)
	if err != nil {
		WriteParseError(w, err)
		return
	}
	args := notifications.WatchersPageArgs{Limit: limit}
	for name, dst := range map[string]**notifications.WatchersCursor{"issuesCursor": &args.IssuesAfter, "projectsCursor": &args.ProjectsAfter} {
		if c := r.URL.Query().Get(name); c != "" {
			var cur watchingCursor
			if err := DecodeCursor(c, &cur); err != nil {
				WriteParseError(w, err)
				return
			}
			*dst = &notifications.WatchersCursor{Since: cur.Since, ID: cur.ID}
		}
	}
	page, f := issueops.WatchingPage(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f)
		return
	}
	next := func(more bool, since time.Time, id string) (*string, bool) {
		if !more {
			return nil, true
		}
		c, err := EncodeCursor(watchingCursor{Since: since, ID: id})
		if err != nil {
			WriteInternalError(w, r, "encoding a cursor", err)
			return nil, false
		}
		return &c, true
	}
	out := WatchingLists{
		Issues:   List[notifications.IssueWatcherForUserDTO]{Items: page.Issues, HasMore: page.IssuesMore},
		Projects: List[notifications.ProjectWatcherDTO]{Items: page.Projects, HasMore: page.ProjectsMore},
		Filters:  List[notifications.CustomWatcherDTO]{Items: page.Custom},
	}
	if out.Filters.Items == nil {
		out.Filters.Items = []notifications.CustomWatcherDTO{}
	}
	if n := len(page.Issues); n > 0 {
		var ok bool
		if out.Issues.NextCursor, ok = next(page.IssuesMore, page.Issues[n-1].Since, page.Issues[n-1].IssueID); !ok {
			return
		}
	}
	if n := len(page.Projects); n > 0 {
		var ok bool
		if out.Projects.NextCursor, ok = next(page.ProjectsMore, page.Projects[n-1].Since, page.Projects[n-1].ProjectID.String()); !ok {
			return
		}
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// watchIssue subscribes the caller to an issue.
//
// @Summary Watch an issue
// @Description Subscribes the caller to the issue of the path — the same operation as the MCP tool skein_watchers, action watch; subtree also carries the descendants' events to its Watching card. Watching a watched issue only sets subtree. Side effects: the subscription; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param body body watchIssueBody false "Options"
// @Success 200 {object} IssueWatch "Watching"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/watch [put]
// @ID watchIssue
func (a *issueWrites) watchIssue(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body watchIssueBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	args = issueops.Args{"issueId": id, "subtree": body.Subtree != nil && *body.Subtree}
	if _, f := issueops.WatchIssue(r.Context(), a.ops, callerOf(r), args); f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, IssueWatch{IssueID: id, Watching: true})
}

// unwatchIssue unsubscribes the caller from an issue.
//
// @Summary Stop watching an issue
// @Description Unsubscribes the caller from the issue of the path — the same operation as the MCP tool skein_watchers, action unwatch; the explicit opt-out keeps the issue unwatched against the automatic subscriptions. Side effects: the opt-out; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param body body kindBody false "Options"
// @Success 200 {object} IssueWatch "Not watching"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/watch [delete]
// @ID unwatchIssue
func (a *issueWrites) unwatchIssue(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok || !kindArgs(w, r) {
		return
	}
	if _, f := issueops.UnwatchIssue(r.Context(), a.ops, callerOf(r), issueops.Args{"issueId": id}); f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, IssueWatch{IssueID: id, Watching: false})
}

// projectWatch serves watchProject and unwatchProject.
func (a *issueWrites) projectWatch(w http.ResponseWriter, r *http.Request, unwatch bool) {
	if !CheckQuery(w, r) {
		return
	}
	pa := &projectsAPI{pool: a.pool, projects: a.projects}
	projectID, ok := pa.pathProject(w, r)
	if !ok {
		return
	}
	args := issueops.Args{"project": projectID.String()}
	if unwatch {
		if !kindArgs(w, r) {
			return
		}
	} else {
		var body watchProjectBody
		raw, ok := decodeArgs(w, r, &body)
		if !ok || !kindOf(w, body.Kind) {
			return
		}
		if v, present := raw["eventTypes"]; present {
			args["eventTypes"] = v
		}
	}
	res, f := issueops.WatchProject(r.Context(), a.ops, callerOf(r), args, unwatch)
	if f != nil {
		writeFailure(w, f, projectID.String())
		return
	}
	WriteJSON(w, r, http.StatusOK, ProjectWatch{ProjectID: res.ProjectID.String(), Watching: res.Watching, EventTypes: res.EventTypes})
}

// watchProject subscribes the caller to a project.
//
// @Summary Watch a project
// @Description Subscribes the caller to the project of the path — the same operation as the MCP tool skein_watchers, action project_watch; eventTypes narrows the subscription to those event types of the subscription catalogue, [] clears the narrowing. Side effects: the subscription; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param body body watchProjectBody false "Options"
// @Success 200 {object} ProjectWatch "Watching"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 422 {object} ProblemDocument "An unknown or undeliverable event type (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/watch [put]
// @ID watchProject
func (a *issueWrites) watchProject(w http.ResponseWriter, r *http.Request) {
	a.projectWatch(w, r, false)
}

// unwatchProject unsubscribes the caller from a project.
//
// @Summary Stop watching a project
// @Description Unsubscribes the caller from the project of the path — the same operation as the MCP tool skein_watchers, action project_unwatch: the subscription row goes, its narrowing included. Side effects: no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param project path string true "The project's UUID, prefix or alias (URL-encoded)"
// @Param body body kindBody false "Options"
// @Success 200 {object} ProjectWatch "Not watching"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such project within reach (code project_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/projects/{project}/watch [delete]
// @ID unwatchProject
func (a *issueWrites) unwatchProject(w http.ResponseWriter, r *http.Request) {
	a.projectWatch(w, r, true)
}

// listWatchFilters lists the caller's subscription filters.
//
// @Summary List the caller's subscription filters
// @Description Returns the caller's subscription filters — the same operation as the MCP tool skein_watchers, action filter_list. A filter over a project outside the key's scope, or over any project under a narrower scope, is not listed. At most 10 per user: one page. Read-only.
// @Tags Me
// @Produce json
// @Security SkeinApiKey[read]
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[notifications.CustomWatcherDTO] "The filters"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/me/watch-filters [get]
// @ID listWatchFilters
func (a *issueWrites) listWatchFilters(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	filters, f := issueops.WatchFilters(r.Context(), a.ops, callerOf(r))
	if f != nil {
		writeFailure(w, f)
		return
	}
	if filters == nil {
		filters = []notifications.CustomWatcherDTO{}
	}
	WriteJSON(w, r, http.StatusOK, List[notifications.CustomWatcherDTO]{Items: filters})
}

// createWatchFilter creates a subscription filter.
//
// @Summary Create a subscription filter
// @Description Creates a subscription filter of the caller — the same operation as the MCP tool skein_watchers, action filter_create. A project of the filter outside the key's scope, or missing, is 422 project_not_found (a reference of the body); a filter over any project (no projects) needs a key of scope all, else 403 forbidden; at most 10 filters (422 validation_error, cap). Side effects: the filter; no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param body body watchFilterBody true "The filter"
// @Success 201 {object} notifications.CustomWatcherDTO "The filter created"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); a filter over any project under a narrower scope (code forbidden)"
// @Failure 422 {object} ProblemDocument "A project of the filter is missing or out of reach (code project_not_found); the name or the filter is invalid, or the cap is reached (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/me/watch-filters [post]
// @ID createWatchFilter
func (a *issueWrites) createWatchFilter(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body watchFilterBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	dto, f := issueops.CreateWatchFilter(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusCreated, dto)
}

// deleteWatchFilter deletes a subscription filter.
//
// @Summary Delete a subscription filter
// @Description Deletes the subscription filter of the path — the same operation as the MCP tool skein_watchers, action filter_delete. A filter that is missing, another user's or outside the key's scope stays and is refused with the code MCP answers, 422 validation_error. Side effects: no event.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param filter path string true "The filter's UUID"
// @Param body body kindBody false "Options"
// @Success 200 {object} WatchFilterDeleted "Deleted"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "No such filter of the caller within reach, or the id is not a UUID (code validation_error, as MCP)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/me/watch-filters/{filter} [delete]
// @ID deleteWatchFilter
func (a *issueWrites) deleteWatchFilter(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) || !kindArgs(w, r) {
		return
	}
	id, f := issueops.DeleteWatchFilter(r.Context(), a.ops, callerOf(r), issueops.Args{"filterId": chi.URLParam(r, "filter")})
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusOK, WatchFilterDeleted{ID: id.String(), Deleted: true})
}

// listReminders lists the caller's reminders.
//
// @Summary List the caller's reminders
// @Description Returns the caller's reminders, ringing before armed — the same operation as the MCP tool skein_remind, action list: state ringing, armed or all (default); project, a handle, narrows the list. ringingTaskCount is the badge across projects. Reminders on issues outside the key's scope are neither listed nor counted. One page: hasMore is false. Read-only.
// @Tags Me
// @Produce json
// @Security SkeinApiKey[read]
// @Param state query string false "ringing, armed or all (default)"
// @Param project query string false "A project handle"
// @Param fields query string false "Comma-separated fields of each item to return"
// @Success 200 {object} ReminderPage "The reminders"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 422 {object} ProblemDocument "An unknown state (code validation_error); the project is missing or out of reach (code project_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/reminders [get]
// @ID listReminders
func (a *issueWrites) listReminders(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "state", "project") {
		return
	}
	q := r.URL.Query()
	res, f := issueops.RemindList(r.Context(), a.ops, callerOf(r), issueops.Args{"state": q.Get("state"), "project": q.Get("project")})
	if f != nil {
		writeFailure(w, f)
		return
	}
	WriteJSON(w, r, http.StatusOK, ReminderPage{List: List[issueops.ReminderListItem]{Items: res.Reminders}, RingingTaskCount: res.RingingTaskCount})
}

// createReminder sets a reminder on an issue.
//
// @Summary Set a reminder on an issue
// @Description Sets a reminder of the caller on the issue of the path — the same operation as the MCP tool skein_remind, action set: it rings at remindAt in the caller's inbox. Side effects: reminder.set.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param body body createReminderBody true "The reminder"
// @Success 201 {object} reminders.Reminder "The reminder"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 422 {object} ProblemDocument "remindAt is missing or not RFC 3339 (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/reminders [post]
// @ID createReminder
func (a *issueWrites) createReminder(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body createReminderBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["issueId"] = id
	rem, f := issueops.RemindSet(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusCreated, rem)
}

// snoozeReminder moves a reminder later.
//
// @Summary Snooze a reminder
// @Description Moves the reminder of the path later — the same operation as the MCP tool skein_remind, action snooze: exactly one of by (a duration from now) and until (RFC 3339). A reminder that is not the caller's, missing or on an issue outside the key's scope is 404 reminder_not_found. Side effects: reminder.snoozed.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param reminder path string true "The reminder's UUID"
// @Param body body snoozeBody true "When"
// @Success 200 {object} reminders.Reminder "The reminder"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such reminder of the caller within reach (code reminder_not_found)"
// @Failure 422 {object} ProblemDocument "Neither or both of by and until, or one is invalid; the id is not a UUID (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/reminders/{reminder}/snooze [post]
// @ID snoozeReminder
func (a *issueWrites) snoozeReminder(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body snoozeBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	id := strings.TrimSpace(chi.URLParam(r, "reminder"))
	args["reminderId"] = id
	rem, f := issueops.RemindSnooze(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, rem)
}

// dismissReminder dismisses a reminder.
//
// @Summary Dismiss a reminder
// @Description Dismisses the reminder of the path — the same operation as the MCP tool skein_remind, action dismiss. A reminder that is not the caller's, missing or on an issue outside the key's scope is 404 reminder_not_found. Side effects: reminder.dismissed.
// @Tags Me
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param reminder path string true "The reminder's UUID"
// @Param body body dismissBody false "Options"
// @Success 200 {object} ReminderDismissed "Dismissed"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such reminder of the caller within reach (code reminder_not_found)"
// @Failure 422 {object} ProblemDocument "The id is not a UUID (code validation_error)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/reminders/{reminder} [delete]
// @ID dismissReminder
func (a *issueWrites) dismissReminder(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body dismissBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	id := strings.TrimSpace(chi.URLParam(r, "reminder"))
	args["reminderId"] = id
	res, f := issueops.RemindDismiss(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	WriteJSON(w, r, http.StatusOK, ReminderDismissed{ReminderID: res.String(), Dismissed: true})
}
