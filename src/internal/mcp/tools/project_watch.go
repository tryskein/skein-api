package tools

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
)

type projectWatchResponse struct {
	ProjectID  string   `json:"projectId"`
	Watching   bool     `json:"watching"`
	EventTypes []string `json:"eventTypes,omitempty"`
}

// handleProjectWatch backs skein_watchers(action="project_watch") and
// action="project_unwatch" — project-level subscriptions.
//
// Which direction it runs is a Go argument, not a request field. The tool
// used to carry a boolean `unwatch` discriminator of its own; sk-255.3
// flattened it into the top-level `action` enum, because two levels of
// discriminator in one flat schema is precisely the shape an agent
// mis-fills. The handler stays one function — the two directions share
// project resolution, the visibility gate and the eventTypes decode, and
// splitting them would duplicate all three for a four-line difference.
//
// `eventTypes` is the optional whitelist on the underlying
// project_watchers row and applies to the watch direction only: nil /
// omitted → no change to the existing filter (or "all notifiable types"
// on first subscribe); empty array clears the filter back to "all".
func handleProjectWatch(pool *pgxpool.Pool, projectsStore *projects.Store, notifStore *notifications.Store, unwatch bool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		// Stale-discriminator guards. `unwatch` was a boolean parameter of
		// skein_project_watch five commits ago and `eventTypes` only ever
		// applied to the watch direction; both are exactly what a cached
		// schema, an out-of-date consumer or a model prior still sends.
		// Ignoring them silently is what makes this pair dangerous:
		// action="project_watch" + unwatch=true reads as "unsubscribe" to
		// the caller and SUBSCRIBES, which is the same class of inverted
		// intent the mark_read family rejects rather than swallows (see the
		// header comment in mark_read.go). Both checks run before project
		// resolution — a rejected request must not cost a database round
		// trip.
		if _, stale := req.GetArguments()["unwatch"]; stale {
			return skmcp.ToolError(ErrCodeValidationError,
				"unwatch is no longer a parameter — the direction is the action itself: "+
					"use action=\"project_unwatch\" to unsubscribe, action=\"project_watch\" to subscribe",
				map[string]any{"parameter": "unwatch"}), nil
		}
		if _, present := req.GetArguments()["eventTypes"]; present && unwatch {
			return skmcp.ToolError(ErrCodeValidationError,
				"eventTypes is only read by project_watch — unsubscribing removes the whole "+
					"subscription row, filter included; to narrow an existing subscription use "+
					"action=\"project_watch\" with eventTypes",
				map[string]any{"action": "project_unwatch"}), nil
		}

		projectArg, err := req.RequireString("project")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "project is required", nil), nil
		}
		projectID, perr := ResolveProjectHandle(ctx, pool, projectsStore, projectArg, userID)
		if perr != nil {
			return projectErrorResult(perr)
		}
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, projectID, userID); vErr != nil {
			return projectErrorResult(&ProjectNotFoundError{Query: projectArg})
		}

		// Read eventTypes — nil means "no change". Empty array means "clear".
		var eventTypes *[]string
		if raw, ok := req.GetArguments()["eventTypes"]; ok {
			arr, ok := raw.([]any)
			if !ok {
				return skmcp.ToolError(ErrCodeValidationError,
					"eventTypes must be an array of strings", nil), nil
			}
			out := make([]string, 0, len(arr))
			for _, v := range arr {
				s, ok := v.(string)
				if !ok {
					return skmcp.ToolError(ErrCodeValidationError,
						"eventTypes must contain only strings", nil), nil
				}
				// Subscription catalogue (T001 / sk-303.13): only types
				// that really deliver to a subscriber may be stored.
				if !notifications.IsSubscriptionEventType(s) {
					return skmcp.ToolError(ErrCodeValidationError,
						"unknown or undeliverable event type: "+s,
						map[string]any{"eventType": s}), nil
				}
				out = append(out, s)
			}
			eventTypes = &out
		}

		if unwatch {
			if err := notifStore.RemoveProjectWatch(ctx, projectID, userID); err != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_watchers", "action", "project_unwatch", "reason", "unwatch failed",
					"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to unwatch project", nil), nil
			}
			slog.InfoContext(ctx, "mcp tool ok",
				"tool", "skein_watchers", "action", "project_unwatch", "userId", userID.String(),
				"projectId", projectID.String(),
				"latencyMs", time.Since(start).Milliseconds())
			return skmcp.ToolJSON(projectWatchResponse{
				ProjectID: projectID.String(),
				Watching:  false,
			})
		}

		if err := notifStore.AddProjectWatch(ctx, projectID, userID); err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_watchers", "action", "project_watch", "reason", "watch failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to watch project", nil), nil
		}
		if eventTypes != nil {
			if err := notifStore.SetProjectWatcherEventTypes(ctx, projectID, userID, *eventTypes); err != nil {
				if errors.Is(err, notifications.ErrNotFound) {
					// Should never hit because we just inserted; defensive log.
					slog.WarnContext(ctx, "project watcher row missing right after insert",
						"projectId", projectID.String(), "userId", userID.String())
				} else {
					slog.ErrorContext(ctx, "mcp tool error",
						"tool", "skein_watchers", "action", "project_watch", "reason", "set event_types failed",
						"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
					return skmcp.ToolError(ErrCodeInternalError, "failed to set event types", nil), nil
				}
			}
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_watchers", "action", "project_watch", "userId", userID.String(),
			"projectId", projectID.String(),
			"latencyMs", time.Since(start).Milliseconds())

		out := projectWatchResponse{
			ProjectID: projectID.String(),
			Watching:  true,
		}
		if eventTypes != nil {
			out.EventTypes = *eventTypes
		}
		return skmcp.ToolJSON(out)
	}
}
