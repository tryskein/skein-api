package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// userCreateResponse is the wire shape returned by
// skein_user(action="create") — the created UserDTO, without the
// password hash.
type userCreateResponse struct {
	User *users.UserDTO `json:"user"`
}

func handleUserCreate(store *users.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		if errResp := requireAdminTool(ctx); errResp != nil {
			return errResp, nil
		}

		email, err := req.RequireString("email")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "email is required", nil), nil
		}
		name, err := req.RequireString("name")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "name is required", nil), nil
		}
		password, err := req.RequireString("password")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "password is required", nil), nil
		}
		role, err := req.RequireString("role")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "role is required", nil), nil
		}
		username := strings.TrimSpace(req.GetString("username", ""))
		isExternal := req.GetBool("is_external", false)

		actor := UsersAgentActor(ctx, store.Pool(), userID)
		dto, err := store.CreateUser(ctx, users.CreateUserInput{
			Email:      strings.TrimSpace(strings.ToLower(email)),
			Username:   strings.ToLower(username),
			Name:       strings.TrimSpace(name),
			Password:   password,
			Role:       role,
			IsExternal: isExternal,
		}, actor)
		if err != nil {
			return mapUsersError(ctx, err, "create", userID.String()), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_user", "action", "create",
			"actorUserId", userID.String(),
			"createdUserId", dto.ID.String(),
			"role", dto.Role,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(userCreateResponse{User: dto})
	}
}

// requireAdminTool gates the skein_user actions at handler entry. Returns
// `not_admin` BEFORE any DB call when the role from ctx is not 'admin'.
// We deliberately do NOT collapse this with project visibility — the
// admin tools have their own closed-set error code distinct from
// `forbidden` so the agent can branch on it cleanly.
func requireAdminTool(ctx context.Context) *mcpgo.CallToolResult {
	// Admin-ceiling derivation (sk-163 S2): external callers are rejected
	// even when their stored role says admin.
	if !auth.IsAdminRole(callerRole(ctx), auth.IsExternalFrom(ctx)) {
		return skmcp.ToolError(ErrCodeNotAdmin,
			"this tool requires admin role", nil)
	}
	return nil
}

// mapUsersError translates the users package sentinels into MCP error
// codes. Branches must match the closed set defined in errors.go.
//
// `action` is the skein_user action that failed, not a tool name: since
// sk-255.3 folded the three admin user tools into skein_user, the tool
// half of the log line is constant and the action is the part that
// identifies where the error came from.
func mapUsersError(ctx context.Context, err error, action, actorUserID string) *mcpgo.CallToolResult {
	switch {
	case errors.Is(err, users.ErrUserNotFound):
		return skmcp.ToolError(ErrCodeUserNotFound, "user not found", nil)
	case errors.Is(err, users.ErrEmailTaken):
		return skmcp.ToolError(ErrCodeEmailTaken, "email already in use", nil)
	case errors.Is(err, users.ErrUsernameTaken):
		return skmcp.ToolError(ErrCodeUsernameTaken, "username already in use", nil)
	case errors.Is(err, spaces.ErrConcurrentChange):
		return skmcp.ToolError(ErrCodeConcurrentChange, "a concurrent membership change won; retry", nil)
	case errors.Is(err, users.ErrInvalidPassword):
		return skmcp.ToolError(ErrCodeValidationError,
			"password must be at least 8 characters",
			map[string]any{"field": "password"})
	case errors.Is(err, users.ErrInvalidRole):
		return skmcp.ToolError(ErrCodeValidationError,
			"role must be 'admin' or 'user'",
			map[string]any{"field": "role"})
	case errors.Is(err, users.ErrLastAdmin):
		return skmcp.ToolError(ErrCodeLastAdminCannotDemote,
			"would leave zero active admins", nil)
	case errors.Is(err, users.ErrInvalidCredentials):
		return skmcp.ToolError(ErrCodeValidationError,
			"current password is incorrect", nil)
	case errors.Is(err, users.ErrValidation):
		return skmcp.ToolError(ErrCodeValidationError, err.Error(), nil)
	case errors.Is(err, users.ErrAdminRoleDisabled):
		// sk-674.18 T037, INV-20: the admin role is disabled — the code REST
		// answers.
		return skmcp.ToolError(ErrCodeAdminRoleDisabled, err.Error(),
			map[string]any{"field": "role"})
	case errors.Is(err, users.ErrExternalCannotBeAdmin):
		// sk-163 S3 — external user can never be admin.
		return skmcp.ToolError(ErrCodeValidationError, err.Error(),
			map[string]any{"field": "role"})
	case errors.Is(err, users.ErrExternalSSOConflict):
		// sk-163 S5 — external account requested for an SSO-bound email.
		return skmcp.ToolError(ErrCodeValidationError, err.Error(),
			map[string]any{"field": "isExternal"})
	case errors.Is(err, users.ErrLocalPasswordDisabled):
		// sk-163 S4a — admin set a local password on an internal account
		// while OIDC is active. Authorization denial → forbidden.
		return skmcp.ToolError(ErrCodeForbidden,
			"local password management is disabled for this account; this user authenticates via SSO",
			map[string]any{"field": "password"})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_user", "action", action,
		"reason", "users store error",
		"actorUserId", actorUserID, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "users store failed", nil)
}
