package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/notifications"
)

// Custom watchers (filter-based subscriptions) used to be one tool,
// skein_filter_watch, with its own `op` discriminator (create|delete|list).
// sk-255.3 folded it into skein_watchers, and a discriminator inside a
// discriminator is not a schema an agent should have to reason about — so
// `op` was flattened into the top-level `action` enum as filter_create /
// filter_delete / filter_list, and the single switch below was split into
// the three handlers those actions bind to.
//
// This is the only handler split in the consolidation: everywhere else the
// per-action handler already existed as its own function.

type filterWatchListResponse struct {
	Watchers []notifications.CustomWatcherDTO `json:"watchers"`
}

type filterWatchCreateResponse struct {
	Watcher *notifications.CustomWatcherDTO `json:"watcher"`
}

type filterWatchDeleteResponse struct {
	ID string `json:"id"`
}

// handleFilterWatchList backs skein_watchers(action="filter_list") —
// returns the caller's custom_watchers rows. Takes no arguments.
func handleFilterWatchList(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		watchers, err := notifStore.ListCustomWatchers(ctx, userID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_watchers", "action", "filter_list", "userId", userID.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list custom watchers", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_watchers", "action", "filter_list", "userId", userID.String(),
			"count", len(watchers),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(filterWatchListResponse{Watchers: watchers})
	}
}

// handleFilterWatchDelete backs skein_watchers(action="filter_delete") —
// removes one custom watcher by id. The id arrives as `filterId`, not
// `id`: in the consolidated schema `issueId` is the issue-side identifier
// and this one is a UUID from a different table entirely.
func handleFilterWatchDelete(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		rawID, err := req.RequireString("filterId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "filterId is required for filter_delete", nil), nil
		}
		watcherID, err := uuid.Parse(strings.TrimSpace(rawID))
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"filterId must be a UUID",
				map[string]any{"filterId": rawID}), nil
		}
		if err := notifStore.DeleteCustomWatcher(ctx, userID, watcherID); err != nil {
			if errors.Is(err, notifications.ErrNotFound) {
				return skmcp.ToolError(ErrCodeValidationError,
					"custom watcher not found",
					map[string]any{"filterId": rawID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_watchers", "action", "filter_delete", "userId", userID.String(),
				"watcherId", watcherID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to delete custom watcher", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_watchers", "action", "filter_delete", "userId", userID.String(),
			"watcherId", watcherID.String(),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(filterWatchDeleteResponse{ID: watcherID.String()})
	}
}

// handleFilterWatchCreate backs skein_watchers(action="filter_create") —
// creates one custom watcher from `name` + `filter`. Per-user cap: 10.
func handleFilterWatchCreate(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		name, err := req.RequireString("name")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "name is required for filter_create", nil), nil
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return skmcp.ToolError(ErrCodeValidationError, "name must not be empty", nil), nil
		}
		if len(name) > 100 {
			return skmcp.ToolError(ErrCodeValidationError, "name must be 100 characters or fewer", nil), nil
		}

		filterRaw, ok := req.GetArguments()["filter"]
		if !ok || filterRaw == nil {
			return skmcp.ToolError(ErrCodeValidationError, "filter is required for filter_create", nil), nil
		}
		filter, ferr := decodeCustomWatcherFilter(filterRaw)
		if ferr != nil {
			return skmcp.ToolError(ErrCodeValidationError, ferr.Error(), nil), nil
		}

		dto, err := notifStore.AddCustomWatcher(ctx, userID, notifications.CreateCustomWatcherInput{
			Name:   name,
			Filter: filter,
		})
		if err != nil {
			if errors.Is(err, notifications.ErrTooManyWatchers) {
				return skmcp.ToolError(ErrCodeValidationError,
					"too_many_watchers: at most 10 custom watchers per user",
					map[string]any{"cap": notifications.MaxCustomWatchersPerUser}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_watchers", "action", "filter_create", "userId", userID.String(),
				"error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to create custom watcher", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_watchers", "action", "filter_create", "userId", userID.String(),
			"watcherId", dto.ID.String(), "name", dto.Name,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(filterWatchCreateResponse{Watcher: dto})
	}
}

// decodeCustomWatcherFilter validates the loose JSON arrived from the
// MCP arguments map into a CustomWatcherFilter, applying the same
// validation as the REST handler (closed event-type set, UUID parsing,
// priority bounds).
func decodeCustomWatcherFilter(raw any) (notifications.CustomWatcherFilter, error) {
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

	if v, ok := m["event_types"]; ok {
		arr, err := asStringSlice("event_types", v)
		if err != nil {
			return out, err
		}
		for _, t := range arr {
			// Subscription catalogue (T001 / sk-303.13) — undeliverable
			// types are refused so a filter cannot become a dead toggle.
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
		for _, p := range arr {
			if _, err := uuid.Parse(p); err != nil {
				return out, errors.New("projects must be UUIDs: " + p)
			}
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
		for _, c := range arr {
			if _, err := uuid.Parse(c); err != nil {
				return out, errors.New("components must be UUIDs: " + c)
			}
		}
		out.Components = arr
	}
	if v, ok := m["priority_max"]; ok && v != nil {
		// JSON numbers arrive as float64.
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
