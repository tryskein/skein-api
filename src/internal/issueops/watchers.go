package issueops

// watchers.go — the subscription operations (sk-878.3.1 T027, the lead's
// decision of 10.11.2026 extending 01a1c640): the actions of the MCP tool
// skein_watchers but the monitor's (not carried over to the public API),
// moved out of it with their checks and refusal codes unchanged. The key's
// scope narrows them in the store (T011).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/notifications"
	"skein-api/internal/projects"
)

const watchersTool = "skein_watchers"

// watchIssueArg is the checked issueId of watch and unwatch, and the gate:
// the issue must be visible to the caller.
func watchIssueArg(ctx context.Context, d Deps, userID uuid.UUID, a Args) (string, *Failure) {
	rawID, err := a.RequireString("issueId")
	if err != nil {
		return "", Fail(ErrCodeValidationError, "issueId is required", nil)
	}
	issueID := strings.TrimSpace(rawID)
	if !LooksLikeHierarchicalID(issueID) {
		return "", Fail(ErrCodeValidationError, "issueId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"issueId": rawID})
	}
	if _, f := ResolveVisibleIssueProject(ctx, d.Pool, watchersTool, issueID, userID); f != nil {
		return "", f
	}
	return issueID, nil
}

// WatchIssue subscribes the caller to an issue, with its subtree when asked
// (action watch; watchIssue). Watching a watched issue changes the subtree
// flag only.
func WatchIssue(ctx context.Context, d Deps, userID uuid.UUID, a Args) (string, *Failure) {
	start := time.Now()
	issueID, f := watchIssueArg(ctx, d, userID, a)
	if f != nil {
		return "", f
	}
	if err := d.Notifications.AddIssueWatch(ctx, issueID, userID, a.GetBool("subtree", false)); err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", watchersTool, "action", "watch", "reason", "add issue watch failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return "", Fail(ErrCodeInternalError, "failed to add watch", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", watchersTool, "action", "watch",
		"userId", userID.String(), "issueId", issueID,
		"latencyMs", time.Since(start).Milliseconds())
	return issueID, nil
}

// UnwatchIssue unsubscribes the caller from an issue (action unwatch;
// unwatchIssue); the explicit opt-out keeps the issue unwatched against the
// automatic subscriptions.
func UnwatchIssue(ctx context.Context, d Deps, userID uuid.UUID, a Args) (string, *Failure) {
	start := time.Now()
	issueID, f := watchIssueArg(ctx, d, userID, a)
	if f != nil {
		return "", f
	}
	if err := d.Notifications.RemoveIssueWatch(ctx, issueID, userID); err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", watchersTool, "action", "unwatch", "reason", "remove issue watch failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return "", Fail(ErrCodeInternalError, "failed to remove watch", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", watchersTool, "action", "unwatch",
		"userId", userID.String(), "issueId", issueID,
		"latencyMs", time.Since(start).Milliseconds())
	return issueID, nil
}

// ProjectWatchResult is the state of the caller's subscription to a
// project after a watch or an unwatch.
type ProjectWatchResult struct {
	ProjectID uuid.UUID
	Watching  bool
	// EventTypes are the event types set by the call; nil when it set none.
	EventTypes []string
}

// WatchProject subscribes the caller to a project, or unsubscribes him
// (actions project_watch, project_unwatch; watchProject, unwatchProject).
// project is a handle as MCP resolves it; a project out of reach is
// project_not_found. eventTypes (watch only) narrows the subscription; an
// empty list clears the narrowing. The stale argument unwatch is refused,
// and so is eventTypes on an unwatch.
func WatchProject(ctx context.Context, d Deps, userID uuid.UUID, a Args, unwatch bool) (ProjectWatchResult, *Failure) {
	start := time.Now()
	action := "project_watch"
	if unwatch {
		action = "project_unwatch"
	}
	if _, stale := a["unwatch"]; stale {
		return ProjectWatchResult{}, Fail(ErrCodeValidationError,
			"unwatch is no longer a parameter — the direction is the action itself: "+
				"use action=\"project_unwatch\" to unsubscribe, action=\"project_watch\" to subscribe",
			map[string]any{"parameter": "unwatch"})
	}
	if _, present := a["eventTypes"]; present && unwatch {
		return ProjectWatchResult{}, Fail(ErrCodeValidationError,
			"eventTypes is only read by project_watch — unsubscribing removes the whole "+
				"subscription row, filter included; to narrow an existing subscription use "+
				"action=\"project_watch\" with eventTypes",
			map[string]any{"action": "project_unwatch"})
	}
	projectArg, err := a.RequireString("project")
	if err != nil {
		return ProjectWatchResult{}, Fail(ErrCodeValidationError, "project is required", nil)
	}
	projectID, perr := ResolveProjectHandle(ctx, d.Pool, d.Projects, projectArg, userID)
	if perr != nil {
		return ProjectWatchResult{}, ProjectFailure(perr)
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, d.Pool, projectID, userID); vErr != nil {
		return ProjectWatchResult{}, ProjectFailure(&ProjectNotFoundError{Query: projectArg})
	}
	var eventTypes *[]string
	if raw, ok := a["eventTypes"]; ok {
		arr, ok := raw.([]any)
		if !ok {
			return ProjectWatchResult{}, Fail(ErrCodeValidationError, "eventTypes must be an array of strings", nil)
		}
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			s, ok := v.(string)
			if !ok {
				return ProjectWatchResult{}, Fail(ErrCodeValidationError, "eventTypes must contain only strings", nil)
			}
			// Subscription catalogue (T001 / sk-303.13): only the types that
			// deliver to a subscriber are stored.
			if !notifications.IsSubscriptionEventType(s) {
				return ProjectWatchResult{}, Fail(ErrCodeValidationError, "unknown or undeliverable event type: "+s,
					map[string]any{"eventType": s})
			}
			out = append(out, s)
		}
		eventTypes = &out
	}

	if unwatch {
		if err := d.Notifications.RemoveProjectWatch(ctx, projectID, userID); err != nil {
			slog.ErrorContext(ctx, logMessage(ctx, "error"),
				"tool", watchersTool, "action", action, "reason", "unwatch failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return ProjectWatchResult{}, Fail(ErrCodeInternalError, "failed to unwatch project", nil)
		}
		slog.InfoContext(ctx, logMessage(ctx, "ok"),
			"tool", watchersTool, "action", action, "userId", userID.String(),
			"projectId", projectID.String(), "latencyMs", time.Since(start).Milliseconds())
		return ProjectWatchResult{ProjectID: projectID}, nil
	}
	if err := d.Notifications.AddProjectWatch(ctx, projectID, userID); err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", watchersTool, "action", action, "reason", "watch failed",
			"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
		return ProjectWatchResult{}, Fail(ErrCodeInternalError, "failed to watch project", nil)
	}
	res := ProjectWatchResult{ProjectID: projectID, Watching: true}
	if eventTypes != nil {
		if err := d.Notifications.SetProjectWatcherEventTypes(ctx, projectID, userID, *eventTypes); err != nil {
			if !errors.Is(err, notifications.ErrNotFound) {
				slog.ErrorContext(ctx, logMessage(ctx, "error"),
					"tool", watchersTool, "action", action, "reason", "set event_types failed",
					"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
				return ProjectWatchResult{}, Fail(ErrCodeInternalError, "failed to set event types", nil)
			}
			// Cannot happen right after the insert; logged, not refused.
			slog.WarnContext(ctx, "project watcher row missing right after insert",
				"projectId", projectID.String(), "userId", userID.String())
		}
		res.EventTypes = *eventTypes
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", watchersTool, "action", action, "userId", userID.String(),
		"projectId", projectID.String(), "latencyMs", time.Since(start).Milliseconds())
	return res, nil
}

// Watching lists the caller's subscriptions — issues, projects and filters,
// each capped at 50 (action list). Only the subscriptions inside the key's
// scope are listed.
func Watching(ctx context.Context, d Deps, userID uuid.UUID) (*notifications.AllWatchersDTO, *Failure) {
	start := time.Now()
	all, err := d.Notifications.ListAllWatchers(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", watchersTool, "action", "list", "userId", userID.String(), "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to list watchers", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", watchersTool, "action", "list", "userId", userID.String(),
		"issues", len(all.Issues), "projects", len(all.Projects), "custom", len(all.Custom),
		"latencyMs", time.Since(start).Milliseconds())
	return all, nil
}

// WatchFilters lists the caller's subscription filters inside the key's
// scope (action filter_list; listWatchFilters): a filter over a project
// outside it, or over any project under a narrower scope, is not listed.
func WatchFilters(ctx context.Context, d Deps, userID uuid.UUID) ([]notifications.CustomWatcherDTO, *Failure) {
	start := time.Now()
	watchers, err := d.Notifications.ListCustomWatchers(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", watchersTool, "action", "filter_list", "userId", userID.String(), "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to list custom watchers", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", watchersTool, "action", "filter_list", "userId", userID.String(),
		"count", len(watchers), "latencyMs", time.Since(start).Milliseconds())
	return watchers, nil
}

// DeleteWatchFilter deletes one subscription filter of the caller (action
// filter_delete; deleteWatchFilter). A filter that is missing, another
// user's, or outside the key's scope is validation_error "custom watcher
// not found", as MCP answers it, and stays.
func DeleteWatchFilter(ctx context.Context, d Deps, userID uuid.UUID, a Args) (uuid.UUID, *Failure) {
	start := time.Now()
	rawID, err := a.RequireString("filterId")
	if err != nil {
		return uuid.Nil, Fail(ErrCodeValidationError, "filterId is required for filter_delete", nil)
	}
	watcherID, err := uuid.Parse(strings.TrimSpace(rawID))
	if err != nil {
		return uuid.Nil, Fail(ErrCodeValidationError, "filterId must be a UUID", map[string]any{"filterId": rawID})
	}
	if err := d.Notifications.DeleteCustomWatcher(ctx, userID, watcherID); err != nil {
		if errors.Is(err, notifications.ErrNotFound) {
			return uuid.Nil, Fail(ErrCodeValidationError, "custom watcher not found", map[string]any{"filterId": rawID})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", watchersTool, "action", "filter_delete", "userId", userID.String(),
			"watcherId", watcherID.String(), "error", err.Error())
		return uuid.Nil, Fail(ErrCodeInternalError, "failed to delete custom watcher", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", watchersTool, "action", "filter_delete", "userId", userID.String(),
		"watcherId", watcherID.String(), "latencyMs", time.Since(start).Milliseconds())
	return watcherID, nil
}

// CreateWatchFilter creates a subscription filter of the caller (action
// filter_create; createWatchFilter): name 1..100 characters, the filter
// object of the MCP tool. A project of the filter outside the key's scope
// is project_not_found; a filter over any project needs a key of scope all
// (forbidden otherwise); at most MaxCustomWatchersPerUser filters.
func CreateWatchFilter(ctx context.Context, d Deps, userID uuid.UUID, a Args) (*notifications.CustomWatcherDTO, *Failure) {
	start := time.Now()
	name, err := a.RequireString("name")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "name is required for filter_create", nil)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, Fail(ErrCodeValidationError, "name must not be empty", nil)
	}
	if len(name) > 100 {
		return nil, Fail(ErrCodeValidationError, "name must be 100 characters or fewer", nil)
	}
	filterRaw, ok := a["filter"]
	if !ok || filterRaw == nil {
		return nil, Fail(ErrCodeValidationError, "filter is required for filter_create", nil)
	}
	filter, ferr := DecodeCustomWatcherFilter(filterRaw)
	if ferr != nil {
		return nil, Fail(ErrCodeValidationError, ferr.Error(), nil)
	}
	dto, err := d.Notifications.AddCustomWatcher(ctx, userID, notifications.CreateCustomWatcherInput{Name: name, Filter: filter})
	if err != nil {
		switch {
		case errors.Is(err, notifications.ErrWatcherProjectNotFound):
			return nil, Fail(ErrCodeProjectNotFound, "a project of the filter was not found", nil)
		case errors.Is(err, notifications.ErrWatcherAnyProjectNeedsScopeAll):
			return nil, Fail(ErrCodeForbidden, "a filter over any project needs a key with scope all: name the projects", nil)
		case errors.Is(err, notifications.ErrTooManyWatchers):
			return nil, Fail(ErrCodeValidationError, "too_many_watchers: at most 10 custom watchers per user",
				map[string]any{"cap": notifications.MaxCustomWatchersPerUser})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", watchersTool, "action", "filter_create", "userId", userID.String(), "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to create custom watcher", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", watchersTool, "action", "filter_create", "userId", userID.String(),
		"watcherId", dto.ID.String(), "name", dto.Name,
		"latencyMs", time.Since(start).Milliseconds())
	return dto, nil
}

// DecodeCustomWatcherFilter checks the loose JSON of a filter: event_types
// of the subscription catalogue, projects and components as UUIDs, types
// and labels as strings, priority_max in [0, 1000].
func DecodeCustomWatcherFilter(raw any) (notifications.CustomWatcherFilter, error) {
	out := notifications.CustomWatcherFilter{}
	m, ok := raw.(map[string]any)
	if !ok {
		return out, errors.New("filter must be an object")
	}
	asStringSlice := func(field string, v any) ([]string, error) {
		if v == nil {
			return nil, nil
		}
		arr, ok := v.([]any)
		if !ok {
			return nil, errors.New(field + " must be an array of strings")
		}
		res := make([]string, 0, len(arr))
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, errors.New(field + " must contain only strings")
			}
			res = append(res, s)
		}
		return res, nil
	}
	uuids := func(field string, arr []string) error {
		for _, s := range arr {
			if _, err := uuid.Parse(s); err != nil {
				return errors.New(field + " must be UUIDs: " + s)
			}
		}
		return nil
	}
	if v, ok := m["event_types"]; ok {
		arr, err := asStringSlice("event_types", v)
		if err != nil {
			return out, err
		}
		for _, t := range arr {
			// Undeliverable types are refused so a filter cannot become a
			// dead toggle (T001 / sk-303.13).
			if !notifications.IsSubscriptionEventType(t) {
				return out, errors.New("unknown or undeliverable event type: " + t)
			}
		}
		out.EventTypes = arr
	}
	if v, ok := m["projects"]; ok {
		arr, err := asStringSlice("projects", v)
		if err != nil {
			return out, err
		}
		if err := uuids("projects", arr); err != nil {
			return out, err
		}
		out.Projects = arr
	}
	if v, ok := m["types"]; ok {
		arr, err := asStringSlice("types", v)
		if err != nil {
			return out, err
		}
		out.Types = arr
	}
	if v, ok := m["labels"]; ok {
		arr, err := asStringSlice("labels", v)
		if err != nil {
			return out, err
		}
		out.Labels = arr
	}
	if v, ok := m["components"]; ok {
		arr, err := asStringSlice("components", v)
		if err != nil {
			return out, err
		}
		if err := uuids("components", arr); err != nil {
			return out, err
		}
		out.Components = arr
	}
	if v, ok := m["priority_max"]; ok && v != nil {
		f, ok := v.(float64)
		if !ok {
			return out, errors.New("priority_max must be a number")
		}
		n := int(f)
		if n < 0 || n > 1000 {
			return out, errors.New("priority_max must be in [0, 1000]")
		}
		out.PriorityMax = &n
	}
	return out, nil
}
