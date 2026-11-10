package tools

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/users"
)

// userListResponse is the wire shape returned by
// skein_user(action="list") — a cursor-paginated page of users.
type userListResponse struct {
	Users      []users.UserDTO `json:"users"`
	NextCursor *string         `json:"nextCursor,omitempty"`
	HasMore    bool            `json:"hasMore"`
}

func handleUserList(store *users.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Users: store}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.UserList(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(userListResponse{Users: res.Items, NextCursor: res.NextCursor, HasMore: res.HasMore})
	}
}
