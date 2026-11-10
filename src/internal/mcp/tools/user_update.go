package tools

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/users"
)

// userUpdateResponse is the wire shape returned by
// skein_user(action="update") — the user after the write.
//
// The one action covers patch + deactivate + reactivate; `deactivated`
// dispatches inside the handler:
//
//   - deactivated == true  → users.Store.Deactivate (cascades api keys)
//   - deactivated == false → users.Store.Reactivate
//   - deactivated unset    → users.Store.UpdateUser (name/role/password)
type userUpdateResponse struct {
	User *users.UserDTO `json:"user"`
}

func handleUserUpdate(store *users.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Users: store}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		dto, f := issueops.UserUpdate(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		return skmcp.ToolJSON(userUpdateResponse{User: dto})
	}
}
