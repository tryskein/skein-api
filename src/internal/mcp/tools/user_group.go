package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// userActions is the action table of skein_user — the enum of the
// schema, the runtime allowlist and the `allowedActions` an agent gets
// back on a miss, all from one slice (see actionSet).
//
// Order is the admin's working sequence, not alphabetical: find out who
// already exists, add someone, then edit them. `list` sits first because
// it is the action that answers "does this person already have an
// account?", which is the question that precedes both of the others.
var userActions = actionSet{order: []string{
	"list", "create", "update", "space_add",
}}

// RegisterUser wires skein_user — the three R3.1 admin user tools
// (skein_user_create / _list / _update) folded into one tool with an
// `action` discriminator (sk-255.3).
//
// The per-action handlers are untouched and still live in their own
// files (handleUserCreate and friends); this file only supplies the
// schema and the dispatch table. That is what keeps the ~520 lines of
// user_*_test.go, which call those handlers directly, green without an
// edit, and it keeps the admin gate, the users-sentinel mapping and the
// tri-state `deactivated` read out of one long switch.
//
// Schema shape (sk-255.3, forced by the platform): `action` is the only
// required parameter — unlike skein_component and skein_project_members
// there is no field every action needs — and every other field is
// optional at the JSON Schema level with its condition stated in prose in
// the parameter description. Anthropic's API rejects a top-level
// oneOf/allOf/anyOf in input_schema with HTTP 400, so per-action required
// sets cannot be expressed structurally at all.
//
// No parameter had to be renamed here. `name`, `role` and `password`
// appear in both create and update, and in both they are the same type
// with the same meaning (the display name / the role / the password of
// the account being written), which is the condition for sharing one
// property; the read-side filters (`archived`, `search`, `limit`,
// `cursor`) and the write-side identifiers (`email`, `username`,
// `userId`, `deactivated`, `is_external`) do not overlap at all.
//
// `is_external` is left in snake_case deliberately. It is the only
// snake_case parameter on the whole MCP surface and folding these tools
// would have been a free moment to rename it to `isExternal` — the
// consumers are being rewritten in sk-255.4 anyway — but that is a
// product decision, not a mechanical one, so it stays until someone makes
// it. Flagged here rather than silently fixed.
//
// Annotations are coarsened, as everywhere in this consolidation: `list`
// is a pure read, but MCP annotations are per tool, so the tool declares
// readOnly=false. destructive stays false — deactivation is a soft flag
// that action="update" with deactivated=false reverses. The "Read-only
// actions:" line in the description is the compensation for a client that
// filters on readOnlyHint.
//
// sk-674.18 T024 adds action="space_add" (US-12, INV-17): the instance
// admin's agent adds a user to a space at once, as the admin does in the web
// (POST /spaces/{id}/members) — the same service, the same codes. Inviting,
// accepting and declining are not on MCP at all.
func RegisterUser(s *server.MCPServer, store *users.Store, pool *pgxpool.Pool, spacesSvc *spaces.Service) {
	tool := mcpgo.NewTool(
		"skein_user",
		mcpgo.WithDescription(
			"Instance-wide user accounts. ADMIN ONLY — every action returns "+
				"`not_admin` to a non-admin caller, before any database "+
				"access. action=\"update\" covers both the patch "+
				"(name / role / password) and the deactivate / reactivate "+
				"flip: deactivating cascades into API key revocation in the "+
				"same transaction and is reversible with deactivated=false. "+
				"Role admin is refused with `admin_role_disabled` while the "+
				"instance admin role is disabled on the installation. "+
				"action=\"space_add\" makes the user a member of a space at once "+
				"(no invitation, no consent). "+
				"Read-only actions: list.",
		),
		mcpgo.WithString(actionParam,
			mcpgo.Required(),
			mcpgo.Enum(userActions.enum()...),
			mcpgo.Description(
				"What to do:\n"+
					"  - \"list\" → users of the instance, cursor-paginated, optionally filtered by `search`.\n"+
					"  - \"create\" → a new account; the user can sign in immediately with the supplied password.\n"+
					"  - \"update\" → patch name / role / password, or flip `deactivated`.\n"+
					"  - \"space_add\" → add `userId` to the space `space` at once.",
			),
		),
		mcpgo.WithString("email",
			mcpgo.Description("User email address. Must match a basic email regex; uniqueness is enforced on lower(email). Required for create."),
		),
		mcpgo.WithString("username",
			mcpgo.Description("Desired username (lowercase, 3..30 chars, [a-z0-9][a-z0-9_-]*). Only read by create; derived from the email local-part when omitted."),
		),
		mcpgo.WithString("name",
			mcpgo.Description("Display name (free-form, trimmed). Required for create; on update, omit to leave it unchanged."),
		),
		mcpgo.WithString("password",
			mcpgo.Description("Password, at least 8 characters. Required for create; on update, set it to reset the user's password. The user should rotate an admin-supplied password via POST /api/v1/me/password on first sign-in."),
		),
		mcpgo.WithString("role",
			mcpgo.Description("Role of the account. Required for create; on update, omit to leave it unchanged. Demoting the last active admin returns `last_admin_cannot_demote`; a concurrent change after three attempts returns `concurrent_change` (retry)."),
			mcpgo.Enum("admin", "user"),
		),
		mcpgo.WithBoolean("is_external",
			mcpgo.Description("Only read by create: make the account an EXTERNAL collaborator. "+
				"External users are visibility-scoped to their own projects and are "+
				"permanently capped at role='user' — combining is_external=true with "+
				"role='admin' is rejected (external_cannot_be_admin). Defaults to false "+
				"(internal user). Set only at create time; there is no conversion path."),
		),
		mcpgo.WithString("userId",
			mcpgo.Description("UUID of the user to write to, as returned by list or create. Required for update and space_add."),
		),
		mcpgo.WithString("space",
			mcpgo.Description("Only read by space_add: short id of the space (skein_whoami lists the spaces)."),
		),
		mcpgo.WithBoolean("deactivated",
			mcpgo.Description("Only read by update: true → soft-deactivate (cascades into API key revocation), false → reactivate (does not un-revoke the previous keys). When set, name/role/password are ignored. Deactivating the last active admin returns `last_admin_cannot_demote`; a concurrent change after three attempts returns `concurrent_change` (retry)."),
		),
		mcpgo.WithBoolean("archived",
			mcpgo.Description("Only read by list: list deactivated users instead of active ones. Default false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithString("search",
			mcpgo.Description("Only read by list: case-insensitive substring filter on email, username and name."),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Only read by list: maximum rows to return, 1..100. Default 50."),
			mcpgo.Min(1),
			mcpgo.Max(100),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Only read by list: opaque cursor token from a prior response's nextCursor."),
		),
		// Coarsened on purpose — see the doc comment above.
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addActionTool(s, tool, userActions.with(map[string]server.ToolHandlerFunc{
		"list":      handleUserList(store),
		"create":    handleUserCreate(store),
		"update":    handleUserUpdate(store),
		"space_add": handleUserSpaceAdd(pool, spacesSvc),
	}))
}

// userSpaceAddResponse is the answer of action="space_add".
type userSpaceAddResponse struct {
	SpaceID string             `json:"spaceId"`
	Member  spaces.SpaceMember `json:"member"`
}

// handleUserSpaceAdd is action="space_add": spaces.Service.AddMember on the
// agent's behalf. The admin gate runs before any database access, like the
// other actions; the service then reads the admin right from the database.
func handleUserSpaceAdd(pool *pgxpool.Pool, svc *spaces.Service) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		if res := requireAdminTool(ctx); res != nil {
			return res, nil
		}
		target, err := uuid.Parse(strings.TrimSpace(req.GetString("userId", "")))
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "userId must be a UUID",
				map[string]any{"field": "userId"}), nil
		}
		space := strings.TrimSpace(req.GetString("space", ""))
		if space == "" {
			return skmcp.ToolError(ErrCodeValidationError, "space is required",
				map[string]any{"field": "space"}), nil
		}
		a := ProjectsAgentActor(ctx, pool, userID)
		m, err := svc.AddMember(ctx, spaces.Caller{
			UserID:   userID,
			Username: a.Username,
			Actor:    spaces.Actor{UserID: a.UserID, Username: a.Username, SessionID: a.SessionID, Type: "agent"},
		}, space, target)
		if err != nil {
			if res, ok := spacesToolError(err); ok {
				return res, nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_user", "action", "space_add",
				"userId", userID.String(), "spaceId", space, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to add the user to the space", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_user", "action", "space_add",
			"userId", userID.String(), "spaceId", space)
		return skmcp.ToolJSON(userSpaceAddResponse{SpaceID: space, Member: m})
	}
}

// spacesToolError renders a refusal of the spaces code registry
// (spaces.Refusal) as the MCP error envelope — the same `error` code and
// extra fields as REST (SC-012). false when err is not one.
func spacesToolError(err error) (*mcpgo.CallToolResult, bool) {
	var ref *spaces.Refusal
	if !errors.As(err, &ref) {
		return nil, false
	}
	return skmcp.ToolError(ref.Code, ref.Message, ref.Extra), true
}
