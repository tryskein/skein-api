package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

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
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		actorUserID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		if errResp := requireAdminTool(ctx); errResp != nil {
			return errResp, nil
		}

		rawID, err := req.RequireString("userId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "userId is required", nil), nil
		}
		targetID, err := uuid.Parse(strings.TrimSpace(rawID))
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"userId is not a valid UUID",
				map[string]any{"userId": rawID}), nil
		}

		actor := UsersAgentActor(ctx, store.Pool(), actorUserID)

		// Branch on `deactivated` first — when set it overrides the patch
		// fields. Default-not-present must NOT be treated as `false` (that
		// would force an implicit reactivation on every update).
		args := req.GetArguments()
		if rawDeact, ok := args["deactivated"]; ok {
			if deactivated, isBool := rawDeact.(bool); isBool {
				if deactivated {
					if err := store.Deactivate(ctx, targetID, actor); err != nil {
						return mapUsersError(ctx, err, "update", actorUserID.String()), nil
					}
				} else {
					if err := store.Reactivate(ctx, targetID, actor); err != nil {
						return mapUsersError(ctx, err, "update", actorUserID.String()), nil
					}
				}
				dto, gerr := store.GetUser(ctx, targetID)
				if gerr != nil {
					return mapUsersError(ctx, gerr, "update", actorUserID.String()), nil
				}
				slog.InfoContext(ctx, "mcp tool ok",
					"tool", "skein_user", "action", "update",
					"actorUserId", actorUserID.String(),
					"userId", targetID.String(),
					"branch", "deactivation",
					"deactivated", deactivated,
					"latencyMs", time.Since(start).Milliseconds())
				return skmcp.ToolJSON(userUpdateResponse{User: dto})
			}
			return skmcp.ToolError(ErrCodeValidationError,
				"deactivated must be a boolean",
				map[string]any{"field": "deactivated"}), nil
		}

		// Patch path — name / role / password.
		var in users.UpdateUserInput
		if v := strings.TrimSpace(req.GetString("name", "")); v != "" {
			in.Name = &v
		}
		if v := strings.TrimSpace(req.GetString("role", "")); v != "" {
			in.Role = &v
		}
		if v := req.GetString("password", ""); v != "" {
			in.Password = &v
		}
		if in.Name == nil && in.Role == nil && in.Password == nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"at least one of name/role/password/deactivated must be set", nil), nil
		}

		dto, err := store.UpdateUser(ctx, targetID, in, actor)
		if err != nil {
			return mapUsersError(ctx, err, "update", actorUserID.String()), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_user", "action", "update",
			"actorUserId", actorUserID.String(),
			"userId", targetID.String(),
			"branch", "patch",
			"changedRole", in.Role != nil,
			"changedPassword", in.Password != nil,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(userUpdateResponse{User: dto})
	}
}
