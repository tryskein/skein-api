package tools

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/users"
)

// userCreateResponse is the wire shape returned by
// skein_user(action="create") — the created UserDTO, without the
// password hash.
type userCreateResponse struct {
	User *users.UserDTO `json:"user"`
}

func handleUserCreate(store *users.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Users: store}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		dto, f := issueops.UserCreate(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(userCreateResponse{User: dto})
	}
}

// requireAdminTool refuses a caller who is not an internal instance admin
// with not_admin, before any database call (issueops.RequireAdmin).
func requireAdminTool(ctx context.Context) *mcpgo.CallToolResult {
	return toolFailure(issueops.RequireAdmin(ctx))
}

// mapUsersError maps a refusal of the users store to its envelope
// (issueops.MapUsersError).
func mapUsersError(ctx context.Context, err error, action, actorUserID string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapUsersError(ctx, err, action, actorUserID))
}
