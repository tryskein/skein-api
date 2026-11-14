package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/spaces"
)

// RegisterWhoami wires the skein_whoami tool. Returns the resolved user,
// the current MCP session (agent), and the projects the user can see.
//
// Annotations:
//
//   - readOnlyHint: true   — never writes (the agents UPSERT happens in
//     the MCP middleware before tool dispatch, so the tool itself is
//     read-only as far as agents see).
//   - idempotentHint: true — same input always yields same shape.
//   - openWorldHint: false — closed world (Skein DB only).
//
// _meta:
//
//   - anthropic/alwaysLoad: true — every Claude Code session needs the
//     identity context up-front; deferring through Tool Search would
//     pay an extra round-trip on every conversation.
func RegisterWhoami(s *server.MCPServer, pool *pgxpool.Pool) {
	tool := mcpgo.NewTool(
		"skein_whoami",
		mcpgo.WithDescription(
			"Returns the current MCP session identity: the resolved user, the "+
				"agent (this MCP session), the spaces the user is in (their "+
				"short ids are the `space` of skein_project_create) and the "+
				"projects the user can see with their role and space, both within the API key's scope; "+
				"canCreateProject also needs a key that may create projects. Read-only, idempotent. Call once at "+
				"session start so future tool calls have the user's permission "+
				"context cached on your side.",
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		// destructiveHint defaults to true in mcp-go (matching MCP spec
		// "default safe" semantics for unannotated tools). We're read-
		// only — explicitly set false so clients don't trigger
		// confirmation prompts.
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	// _meta is set on the Tool struct — mcp-go does not currently expose a
	// fluent ToolOption for vendor-extension fields.
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/alwaysLoad": true,
	})

	addTool(s, tool, handleWhoami(pool))
}

// whoamiProjectsCap caps the projects[] slice. A wide reach — an
// instance admin's typically — can be thousands of projects; dumping all
// of them pollutes the agent's context (on an instance with several
// thousand projects the uncapped payload was measured at 917 KB / ~200K
// tokens). The cap is by response size and the same for every caller,
// whatever its rights: an ordinary member, whose list
// agents rely on as "my projects", fits under it.
const whoamiProjectsCap = 100

// WhoamiResponse is the JSON shape the tool returns (and getMe of the
// public API, sk-878.3.1 T022). We keep it stable
// across releases because agents may parse it field-by-field. New
// fields are additive only; never rename or remove.
type WhoamiResponse struct {
	User     WhoamiUser      `json:"user"`
	Agent    *WhoamiAgent    `json:"agent,omitempty"`
	Projects []WhoamiProject `json:"projects"`
	// ProjectsTotal is the count of projects visible to the caller.
	// >= len(Projects); greater when the projects[] slice was capped.
	// Always populated so callers can tell whether they got the full list.
	ProjectsTotal int `json:"projectsTotal"`
	// ProjectsTruncated is true when projects[] was capped
	// (whoamiProjectsCap).
	ProjectsTruncated bool   `json:"projectsTruncated,omitempty"`
	ProjectsHint      string `json:"projectsHint,omitempty"`
	// Spaces are the spaces the caller sees (sk-674.18 T024, US-12)
	// within the API key's scope (sk-878.3.1 US-3), in the order of the
	// projects column, capped like projects[].
	Spaces          []spaces.AgentSpace `json:"spaces"`
	SpacesTotal     int                 `json:"spacesTotal"`
	SpacesTruncated bool                `json:"spacesTruncated,omitempty"`
	IsAdmin         bool                `json:"isAdmin"`
}

// WhoamiUser is the caller of a whoami answer.
type WhoamiUser struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}

// WhoamiAgent intentionally omits the api_key prefix — the agent already
// has the plain key locally and a server-side echo would add noise plus
// a small info-leak surface (audit logs that include the prefix). The
// SessionID is the cross-system join key: it is also written to
// events.actor_session_id for every action this agent performs, so any
// external system that captures the agent's conversation can join its
// logs to skein audit by string-matching on this id.
type WhoamiAgent struct {
	SessionID   string    `json:"sessionId"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
}

// WhoamiProject is one project of a whoami answer.
type WhoamiProject struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IDPrefix string `json:"idPrefix"`
	// Role is the caller's access level on this project, as the access
	// relation gives it (authz): owner / member, or "admin" for an
	// instance admin's reach (we do NOT pretend an admin is an "owner" on
	// every project — distinguishing the two matters for audit).
	Role string `json:"role"`
	// SpaceID is the short id of the project's space — it tells the
	// same-named projects of different spaces apart.
	SpaceID string `json:"spaceId"`
}

func handleWhoami(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		resp, err := LoadWhoami(ctx, pool, userID)
		if errors.Is(err, ErrWhoamiUserGone) {
			return skmcp.ToolError(ErrCodeUserNotFound,
				"user resolved from the API key no longer exists", nil), nil
		}
		if err != nil {
			slog.ErrorContext(ctx, "whoami: load failed",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load the identity", nil), nil
		}
		if resp.ProjectsTruncated {
			resp.ProjectsHint = "this list is capped to the first projects by name. " +
				"Use skein_project_resolve(q=...) for a specific project, " +
				"or skein_project_list for a paginated browse."
		}
		return skmcp.ToolJSON(resp)
	}
}

// ErrWhoamiUserGone — the user the request authenticated as no longer
// exists (deleted between the key lookup and the read).
var ErrWhoamiUserGone = errors.New("whoami: the caller's user no longer exists")

// LoadWhoami reads the identity of the caller userID: the user, the agent of
// the request (the MCP session, or the REST executor of the public API —
// auth.SessionIDFrom names it in both), the projects and the spaces the
// caller sees through the scope of the request (the API key's included).
// ProjectsHint is left empty: it names MCP tools, so the tool adds it.
// ErrWhoamiUserGone when the user is gone; any other error is internal.
func LoadWhoami(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) (WhoamiResponse, error) {
	// Fetch user core fields. ErrNoRows here means the user was deleted
	// between the api-key middleware's lookup and now — extremely unlikely
	// but we still return a clean error.
	var u WhoamiUser
	var dbIsExternal bool
	err := pool.QueryRow(ctx, `
SELECT id::text, email, username, name, role, is_external
  FROM users
 WHERE id = $1
`, userID).Scan(&u.ID, &u.Email, &u.Username, &u.Name, &u.Role, &dbIsExternal)
	if errors.Is(err, pgx.ErrNoRows) {
		return WhoamiResponse{}, ErrWhoamiUserGone
	}
	if err != nil {
		return WhoamiResponse{}, fmt.Errorf("loading the user: %w", err)
	}

	// Admin-ceiling derivation (sk-163 S2): is_external is re-read from the
	// same users row as role; an external user is never admin. Response
	// metadata only — the project list below does not branch on it.
	isAdmin := auth.IsAdminRole(u.Role, dbIsExternal)

	// Project list: the caller's visible projects through authz, capped by
	// size with the total, so truncation is signalled.
	projects, total, truncated, err := loadProjects(ctx, pool, userID)
	if err != nil {
		return WhoamiResponse{}, fmt.Errorf("loading the projects: %w", err)
	}
	limit, err := keySpaceLimit(ctx, pool)
	if err != nil {
		return WhoamiResponse{}, fmt.Errorf("reading the spaces of the key scope: %w", err)
	}
	agentSpaces, err := spaces.AgentSpacesWithin(ctx, pool, userID, limit)
	if err != nil {
		return WhoamiResponse{}, fmt.Errorf("loading the spaces: %w", err)
	}
	spacesTotal := len(agentSpaces)
	if spacesTotal > whoamiProjectsCap {
		agentSpaces = agentSpaces[:whoamiProjectsCap]
	}

	// Agent block — present only when the request names its agent. The
	// very first request of an MCP session (initialize) has none; callers
	// who tool/call before initialize completes (which is invalid per spec)
	// get a payload without `agent`.
	var agent *WhoamiAgent
	if sid := auth.SessionIDFrom(ctx); sid != "" {
		a, err := loadAgent(ctx, pool, userID, sid)
		if err != nil {
			slog.WarnContext(ctx, "whoami: agent lookup failed",
				"userId", userID.String(), "sessionId", sid,
				"error", err.Error())
			// Soft-fail: identity payload is still useful without it.
		} else {
			agent = a
		}
	}

	return WhoamiResponse{
		User:              u,
		Agent:             agent,
		Projects:          projects,
		ProjectsTotal:     total,
		ProjectsTruncated: truncated,
		Spaces:            agentSpaces,
		SpacesTotal:       spacesTotal,
		SpacesTruncated:   spacesTotal > len(agentSpaces),
		IsAdmin:           isAdmin,
	}, nil
}

// keySpaceLimit is the narrowing of the spaces of a whoami answer by the API
// key of the request (sk-878.3.1 US-3, INV-2): scope "projects" keeps the
// spaces of its projects, "spaces" the chosen spaces, "all" every space; a
// key request without its grant keeps none. canCreateProject stays only for
// a key that may create a project — the right manage and a scope of spaces
// or all, as createProject requires. A request without a key is not
// narrowed.
func keySpaceLimit(ctx context.Context, pool *pgxpool.Pool) (spaces.AgentSpaceLimit, error) {
	if _, isKey := auth.APIKeyIDFrom(ctx); !isKey {
		return spaces.AgentSpaceLimit{}, nil
	}
	g, ok := auth.KeyGrantFrom(ctx)
	if !ok {
		return spaces.AgentSpaceLimit{Limited: true, NoCreate: true}, nil
	}
	limit := spaces.AgentSpaceLimit{
		NoCreate: !authz.KeyMeets(g, authz.KeyRequirement{Right: authz.KeyRightManage, WideScope: true}),
	}
	switch g.Scope.Mode {
	case apikeys.ScopeAll:
	case apikeys.ScopeSpaces:
		limit.Limited, limit.Spaces = true, g.Scope.Spaces
	case apikeys.ScopeProjects:
		limit.Limited = true
		rows, err := pool.Query(ctx, `SELECT DISTINCT space_id FROM projects WHERE id = ANY($1::uuid[])`, g.Scope.Projects)
		if err != nil {
			return spaces.AgentSpaceLimit{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return spaces.AgentSpaceLimit{}, err
			}
			limit.Spaces = append(limit.Spaces, id)
		}
		if err := rows.Err(); err != nil {
			return spaces.AgentSpaceLimit{}, err
		}
	default:
		// A mode outside the closed set reaches nothing.
		limit.Limited = true
	}
	return limit, nil
}

// loadProjects reads the non-archived projects visible to userID — the
// access relation through authz (VisibleJoin), with the caller's level on
// each — ordered by name, and returns (projects, total, truncated, err).
// The role is the caller's own: an admin's member row (admin_member) reads
// as member, his reach alone as admin.
// The list is capped at whoamiProjectsCap whatever the caller's rights;
// total counts every visible project, truncated tells the cap applied
// (no admin / member branch of its own).
func loadProjects(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) ([]WhoamiProject, int, bool, error) {
	s := authz.ScopeFor(ctx, userID)
	// VisibleJoin reads the relation by the caller's id; the key's scope
	// narrows it here, as Scope.Bind narrows the bound set (D-3).
	args := []any{s.IsUnrestricted(), s.CallerID(), whoamiProjectsCap}
	keyed := s.KeyLimit().Expr("p.project_id", authz.Placeholders(&args))
	rows, err := pool.Query(ctx, `
SELECT p.project_id::text, p.name, p.id_prefix,
       CASE va.access WHEN 'admin_member' THEN 'member' ELSE va.access END,
       (SELECT sp.short_id FROM spaces sp WHERE sp.id = p.space_id),
       count(*) OVER ()
  FROM (SELECT id AS project_id, name, id_prefix, space_id FROM projects WHERE archived_at IS NULL) p
  `+authz.VisibleJoin("p", "$1", "$2", "va")+`
 WHERE `+keyed+`
 ORDER BY lower(p.name), p.project_id
 LIMIT $3
`, args...)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()

	out := make([]WhoamiProject, 0, 8)
	total := 0
	for rows.Next() {
		var p WhoamiProject
		if err := rows.Scan(&p.ID, &p.Name, &p.IDPrefix, &p.Role, &p.SpaceID, &total); err != nil {
			return nil, 0, false, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	return out, total, total > len(out), nil
}

// loadAgent reads the agents row for (userID, sessionID). The row is
// guaranteed to exist (the MCP middleware UPSERTs before dispatch), but
// we still tolerate ErrNoRows defensively — a race on registry GC, or a
// future feature toggle that disables the registry, must not crash this
// tool.
//
// Timestamps are scanned as time.Time and rendered as RFC3339-with-nanos
// by json.Marshal. We deliberately do NOT format them in SQL (to_char) —
// that drops sub-second precision, drifts away from the rest of the
// codebase, and forces every consumer to know the magic format string.
func loadAgent(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, sessionID string) (*WhoamiAgent, error) {
	var a WhoamiAgent
	a.SessionID = sessionID
	err := pool.QueryRow(ctx, `
SELECT first_seen_at, last_seen_at
  FROM agents
 WHERE user_id = $1 AND session_id = $2
`, userID, sessionID).Scan(&a.FirstSeenAt, &a.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row yet — return what we have without timestamps.
		return &WhoamiAgent{SessionID: sessionID}, nil
	}
	if err != nil {
		return nil, err
	}
	a.FirstSeenAt = a.FirstSeenAt.UTC()
	a.LastSeenAt = a.LastSeenAt.UTC()
	return &a, nil
}
