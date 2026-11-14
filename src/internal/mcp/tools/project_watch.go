package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
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
	d := issueops.Deps{Pool: pool, Projects: projectsStore, Notifications: notifStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.WatchProject(ctx, d, userID, issueops.Args(req.GetArguments()), unwatch)
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(projectWatchResponse{ProjectID: res.ProjectID.String(), Watching: res.Watching, EventTypes: res.EventTypes})
	}
}
