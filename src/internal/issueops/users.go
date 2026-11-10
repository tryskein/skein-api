package issueops

// users.go — the user operations (sk-878.3.1 T028, the lead's decision of
// 10.11.2026 extending 01a1c640): the actions of the MCP tool skein_user,
// moved out of it with their checks and refusal codes unchanged. Every one
// needs an internal instance admin; through an API key the password of an
// existing user, the admin role and an admin's deactivation are not changed
// (INV-12; users.ErrNotByAPIKey in the store).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

const userTool = "skein_user"

// RequireAdmin refuses with not_admin, before any database call, a caller
// who is not an internal instance admin (an external one never is, whatever
// his stored role says — sk-163 S2).
func RequireAdmin(ctx context.Context) *Failure {
	if !auth.IsAdminRole(CallerRole(ctx), auth.IsExternalFrom(ctx)) {
		return Fail(ErrCodeNotAdmin, "this tool requires admin role", nil)
	}
	return nil
}

// SpacesFailure is the refusal of the spaces service as its own code; ok is
// false for an error that is not one.
func SpacesFailure(err error) (*Failure, bool) {
	var ref *spaces.Refusal
	if !errors.As(err, &ref) {
		return nil, false
	}
	return Fail(ref.Code, ref.Message, ref.Extra), true
}

// MapUsersError maps a refusal of the users store to its code; action names
// the skein_user action in the log line of an unexpected error.
func MapUsersError(ctx context.Context, err error, action, actorUserID string) *Failure {
	switch {
	case errors.Is(err, users.ErrUserNotFound):
		return Fail(ErrCodeUserNotFound, "user not found", nil)
	case errors.Is(err, users.ErrEmailTaken):
		return Fail(ErrCodeEmailTaken, "email already in use", nil)
	case errors.Is(err, users.ErrUsernameTaken):
		return Fail(ErrCodeUsernameTaken, "username already in use", nil)
	case errors.Is(err, spaces.ErrConcurrentChange):
		return Fail(ErrCodeConcurrentChange, "a concurrent membership change won; retry", nil)
	case errors.Is(err, users.ErrInvalidPassword):
		return Fail(ErrCodeValidationError, "password must be at least 8 characters", map[string]any{"field": "password"})
	case errors.Is(err, users.ErrInvalidRole):
		return Fail(ErrCodeValidationError, "role must be 'admin' or 'user'", map[string]any{"field": "role"})
	case errors.Is(err, users.ErrLastAdmin):
		return Fail(ErrCodeLastAdminCannotDemote, "would leave zero active admins", nil)
	case errors.Is(err, users.ErrInvalidCredentials):
		return Fail(ErrCodeValidationError, "current password is incorrect", nil)
	case errors.Is(err, users.ErrValidation):
		return Fail(ErrCodeValidationError, err.Error(), nil)
	case errors.Is(err, users.ErrAdminRoleDisabled):
		// sk-674.18 T037, INV-20: the admin role is disabled.
		return Fail(ErrCodeAdminRoleDisabled, err.Error(), map[string]any{"field": "role"})
	case errors.Is(err, users.ErrExternalCannotBeAdmin):
		// sk-163 S3: an external user is never an admin.
		return Fail(ErrCodeValidationError, err.Error(), map[string]any{"field": "role"})
	case errors.Is(err, users.ErrExternalSSOConflict):
		// sk-163 S5: an external account asked for an SSO-bound email.
		return Fail(ErrCodeValidationError, err.Error(), map[string]any{"field": "isExternal"})
	case errors.Is(err, users.ErrNotByAPIKey):
		// INV-12, "MCP changes" (2): password, the admin role and an admin's
		// deactivation change only in the web UI.
		return Fail(ErrCodeForbidden, err.Error()+"; nothing was changed", nil)
	case errors.Is(err, users.ErrLocalPasswordDisabled):
		// sk-163 S4a: a local password on an internal account while OIDC is on.
		return Fail(ErrCodeForbidden,
			"local password management is disabled for this account; this user authenticates via SSO",
			map[string]any{"field": "password"})
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", userTool, "action", action,
		"reason", "users store error",
		"actorUserId", actorUserID, "error", err.Error())
	return Fail(ErrCodeInternalError, "users store failed", nil)
}

// UserList lists the users of the instance (action list; listUsers):
// archived ones with archived=true, search over name, email and username,
// keyset paging by cursor, limit 50 when absent.
func UserList(ctx context.Context, d Deps, userID uuid.UUID, a Args) (*users.ListUsersResult, *Failure) {
	start := time.Now()
	if f := RequireAdmin(ctx); f != nil {
		return nil, f
	}
	archived := "false"
	if a.GetBool("archived", false) {
		archived = "true"
	}
	in := users.ListUsersInput{Archived: archived, Search: a.GetString("search", ""), Limit: a.GetInt("limit", 50)}
	if cursor := a.GetString("cursor", ""); cursor != "" {
		t, id, err := users.DecodeCursor(cursor)
		if err != nil {
			return nil, Fail(ErrCodeValidationError, "invalid cursor: "+err.Error(), map[string]any{"field": "cursor"})
		}
		in.CursorTime, in.CursorID = &t, &id
	}
	res, err := d.Users.ListUsers(ctx, in)
	if err != nil {
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", userTool, "action", "list", "reason", "list users failed",
			"actorUserId", userID.String(), "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to list users", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", userTool, "action", "list", "actorUserId", userID.String(),
		"count", len(res.Items), "hasMore", res.HasMore,
		"latencyMs", time.Since(start).Milliseconds())
	return res, nil
}

// UserCreate creates a user with an initial password (action create;
// createUser): email, name, password, role required; username and
// is_external optional. A key never creates an admin (the store refuses).
func UserCreate(ctx context.Context, d Deps, userID uuid.UUID, a Args) (*users.UserDTO, *Failure) {
	start := time.Now()
	if f := RequireAdmin(ctx); f != nil {
		return nil, f
	}
	email, err := a.RequireString("email")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "email is required", nil)
	}
	name, err := a.RequireString("name")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "name is required", nil)
	}
	password, err := a.RequireString("password")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "password is required", nil)
	}
	role, err := a.RequireString("role")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "role is required", nil)
	}
	username := strings.TrimSpace(a.GetString("username", ""))
	dto, err := d.Users.CreateUser(ctx, users.CreateUserInput{
		Email:      strings.TrimSpace(strings.ToLower(email)),
		Username:   strings.ToLower(username),
		Name:       strings.TrimSpace(name),
		Password:   password,
		Role:       role,
		IsExternal: a.GetBool("is_external", false),
	}, UsersAgentActor(ctx, d.Users.Pool(), userID))
	if err != nil {
		return nil, MapUsersError(ctx, err, "create", userID.String())
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", userTool, "action", "create", "actorUserId", userID.String(),
		"createdUserId", dto.ID.String(), "role", dto.Role,
		"latencyMs", time.Since(start).Milliseconds())
	return dto, nil
}

// UserUpdate changes a user (action update; updateUser): deactivated, when
// present, deactivates or reactivates and overrides the rest; otherwise
// name, role and password — at least one. Deactivating a user revokes his
// keys; reactivating does not bring them back.
func UserUpdate(ctx context.Context, d Deps, actorUserID uuid.UUID, a Args) (*users.UserDTO, *Failure) {
	start := time.Now()
	if f := RequireAdmin(ctx); f != nil {
		return nil, f
	}
	rawID, err := a.RequireString("userId")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "userId is required", nil)
	}
	targetID, err := uuid.Parse(strings.TrimSpace(rawID))
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "userId is not a valid UUID", map[string]any{"userId": rawID})
	}
	actor := UsersAgentActor(ctx, d.Users.Pool(), actorUserID)

	// deactivated first: when present it overrides the patch; absent is not
	// false (that would reactivate on every update).
	if rawDeact, ok := a["deactivated"]; ok {
		deactivated, isBool := rawDeact.(bool)
		if !isBool {
			return nil, Fail(ErrCodeValidationError, "deactivated must be a boolean", map[string]any{"field": "deactivated"})
		}
		if deactivated {
			err = d.Users.Deactivate(ctx, targetID, actor)
		} else {
			err = d.Users.Reactivate(ctx, targetID, actor)
		}
		if err != nil {
			return nil, MapUsersError(ctx, err, "update", actorUserID.String())
		}
		dto, gerr := d.Users.GetUser(ctx, targetID)
		if gerr != nil {
			return nil, MapUsersError(ctx, gerr, "update", actorUserID.String())
		}
		slog.InfoContext(ctx, logMessage(ctx, "ok"),
			"tool", userTool, "action", "update", "actorUserId", actorUserID.String(),
			"userId", targetID.String(), "branch", "deactivation", "deactivated", deactivated,
			"latencyMs", time.Since(start).Milliseconds())
		return dto, nil
	}

	var in users.UpdateUserInput
	if v := strings.TrimSpace(a.GetString("name", "")); v != "" {
		in.Name = &v
	}
	if v := strings.TrimSpace(a.GetString("role", "")); v != "" {
		in.Role = &v
	}
	if v := a.GetString("password", ""); v != "" {
		in.Password = &v
	}
	if in.Name == nil && in.Role == nil && in.Password == nil {
		return nil, Fail(ErrCodeValidationError, "at least one of name/role/password/deactivated must be set", nil)
	}
	dto, err := d.Users.UpdateUser(ctx, targetID, in, actor)
	if err != nil {
		return nil, MapUsersError(ctx, err, "update", actorUserID.String())
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", userTool, "action", "update", "actorUserId", actorUserID.String(),
		"userId", targetID.String(), "branch", "patch",
		"changedRole", in.Role != nil, "changedPassword", in.Password != nil,
		"latencyMs", time.Since(start).Milliseconds())
	return dto, nil
}

// UserSpaceAdd makes a user a participant of a space (action space_add;
// addSpaceMember): space is the space's short id. The refusals of the
// spaces service keep their codes.
func UserSpaceAdd(ctx context.Context, d Deps, userID uuid.UUID, a Args) (spaces.SpaceMember, *Failure) {
	var none spaces.SpaceMember
	if f := RequireAdmin(ctx); f != nil {
		return none, f
	}
	target, err := uuid.Parse(strings.TrimSpace(a.GetString("userId", "")))
	if err != nil {
		return none, Fail(ErrCodeValidationError, "userId must be a UUID", map[string]any{"field": "userId"})
	}
	space := strings.TrimSpace(a.GetString("space", ""))
	if space == "" {
		return none, Fail(ErrCodeValidationError, "space is required", map[string]any{"field": "space"})
	}
	act := ProjectsAgentActor(ctx, d.Pool, userID)
	m, err := d.Spaces.AddMember(ctx, spaces.Caller{
		UserID:   userID,
		Username: act.Username,
		Actor:    spaces.Actor{UserID: act.UserID, Username: act.Username, SessionID: act.SessionID, Type: "agent"},
	}, space, target)
	if err != nil {
		if f, ok := SpacesFailure(err); ok {
			return none, f
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", userTool, "action", "space_add",
			"userId", userID.String(), "spaceId", space, "error", err.Error())
		return none, Fail(ErrCodeInternalError, "failed to add the user to the space", nil)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", userTool, "action", "space_add", "userId", userID.String(), "spaceId", space)
	return m, nil
}
