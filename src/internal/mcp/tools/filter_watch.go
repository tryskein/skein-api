package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
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
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		watchers, f := issueops.WatchFilters(ctx, d, userID)
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(filterWatchListResponse{Watchers: watchers})
	}
}

func handleFilterWatchDelete(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		id, f := issueops.DeleteWatchFilter(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(filterWatchDeleteResponse{ID: id.String()})
	}
}

func handleFilterWatchCreate(_ *pgxpool.Pool, notifStore *notifications.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		dto, f := issueops.CreateWatchFilter(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(filterWatchCreateResponse{Watcher: dto})
	}
}
