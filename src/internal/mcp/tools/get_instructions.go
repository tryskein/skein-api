package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/mcp/instructions"
	"skein-api/internal/projects"
)

// getInstructionsMaxResultChars is the value advertised as
// anthropic/maxResultSizeChars on skein_get_instructions.
//
// The name mirrors the wire key rather than what we measure, because the
// unit is the client's to define and we do not get to see it. The key
// says "chars"; we compare against len() in BYTES. That mismatch is
// deliberate and safe in exactly one direction: a UTF-8 string never has
// more runes than bytes, so a payload that fits the cap when counted in
// bytes also fits when counted in characters, whichever the client
// means. Counting runes instead would be the optimistic reading of the
// ambiguity — with the guide's Cyrillic and em-dashes that is ~1 KB of
// difference today — so byte-counting is the side to be wrong on.
// TestGetInstructions_PayloadFitsCap enforces it that way, and
// instructions.init() sizes initialize.txt in bytes for the same reason.
//
// The number has to stay above len(instructions.FullGuide) plus the
// worst-case "Project: <name>\n\n" prefix, because a cap below the
// payload means the client truncates the guide — and a half-delivered
// rulebook is worse than none: the agent reads the first N KB and
// silently acts as if the rest does not exist.
//
// The previous value (50000) was written when the guide really was ~6 KB
// and was nearly breached at 49.5 KB. The headroom here is deliberately
// generous and carries no obligation to keep the guide small: this is a
// client-side ceiling, not an allocation, so unused margin costs
// nothing. The guide's own size budget is a separate, much tighter
// invariant — see fullGuideBudgetBytes in get_instructions_test.go.
const getInstructionsMaxResultChars = 150000

// getInstructionsPrefixMaxBytes is the worst case of the
// "Project: <name>\n\n" header handleGetInstructions prepends when the
// caller passes `project`.
//
// Derived from projects.NameMaxLen rather than a guessed slack figure so
// the payload guard sizes the real header. Every write path that sets a
// project name — MCP skein_project_create / skein_project_update and the
// REST create/update handlers — runs ValidateProjectName, so that bound
// holds for anything this tool can read back. (The DB column is bare
// `text`, so a name written around the API by hand could exceed it; that
// is a broken invariant elsewhere, not a budget this constant should
// pad for.)
const getInstructionsPrefixMaxBytes = len("Project: ") + projects.NameMaxLen + len("\n\n")

// RegisterGetInstructions wires the skein_get_instructions tool. Returns
// the embedded long-form guide (~50 KB markdown). Optional `project`
// argument prepends a "Project: <alias>" line so the agent has the
// resolved project name visible at the top of the doc; full per-project
// rule overrides arrive in R2.2.
//
// Annotations:
//
//   - readOnlyHint: true
//   - idempotentHint: true
//   - openWorldHint: false
//
// _meta:
//
//   - anthropic/alwaysLoad: true — step 3 of the mandatory bootstrap the
//     server `instructions` prescribe (whoami → echo sessionId →
//     get_instructions). A tool the agent is ordered to call on every
//     session must be callable on every session; deferring it behind
//     Tool Search buys nothing and costs a round-trip before the agent
//     can even read the rules it is supposed to follow first. What lands
//     in the prompt is this tool's ~725-char schema, not the guide — the
//     markdown body only ever arrives as a tool result, on call.
//   - anthropic/maxResultSizeChars: getInstructionsMaxResultChars — see
//     the const for the sizing rationale, and
//     TestGetInstructions_PayloadFitsCap for the guard that keeps the
//     guide plus its project header underneath it.
func RegisterGetInstructions(s *server.MCPServer, pool *pgxpool.Pool, store *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_get_instructions",
		mcpgo.WithDescription(
			"Returns the long-form Skein agent guide (~50 KB markdown). "+
				"Call once at session start, cache for the session — content "+
				"does not change between calls. Pass `project` (alias or UUID) "+
				"to receive a project-tagged copy.",
		),
		mcpgo.WithString(
			"project",
			mcpgo.Description(
				"Optional project alias or UUID. When set, the guide is "+
					"prefixed with 'Project: <alias>' and visibility is "+
					"validated — invisible/missing projects return "+
					"error=project_not_found.",
			),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		// destructiveHint defaults to true in mcp-go; we're read-only.
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/alwaysLoad":         true,
		"anthropic/maxResultSizeChars": getInstructionsMaxResultChars,
	})

	addTool(s, tool, handleGetInstructions(pool, store))
}

func handleGetInstructions(pool *pgxpool.Pool, store *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		// Optional project arg. mcp-go's GetArguments returns map[string]any;
		// missing key → nil → empty string after the type-assertion. We
		// trim because callers commonly send "  demo-api  " from CLAUDE.md
		// templates with stray whitespace.
		var projectArg string
		if v, ok := req.GetArguments()["project"]; ok {
			if s, ok := v.(string); ok {
				projectArg = strings.TrimSpace(s)
			}
		}

		if projectArg == "" {
			// No project context — return the guide as-is.
			return mcpgo.NewToolResultText(instructions.FullGuide), nil
		}

		// Resolve `project` against the user's visible projects via the
		// shared helper — accepts UUID / name / id_prefix / alias / repo
		// URL uniformly. This is the same path every other tool with a
		// `project` parameter uses, so the contract stays symmetric.
		projectID, rerr := ResolveProjectHandle(ctx, pool, store, projectArg, userID)
		if rerr != nil {
			var pnf *ProjectNotFoundError
			if errors.As(rerr, &pnf) {
				return skmcp.ToolError("project_not_found",
					"project does not exist or is not visible",
					map[string]any{"q": projectArg}), nil
			}
			var amb *AmbiguousProjectError
			if errors.As(rerr, &amb) {
				return skmcp.ToolError("ambiguous_project",
					"project handle matched more than one project",
					map[string]any{"q": projectArg, "candidates": amb.Candidates}), nil
			}
			var ve *ValidationProjectError
			if errors.As(rerr, &ve) {
				return skmcp.ToolError("validation_error", ve.Message,
					map[string]any{"field": ve.Field}), nil
			}
			slog.ErrorContext(ctx, "get_instructions: project resolve failed",
				"q", projectArg, "error", rerr.Error())
			return skmcp.ToolError("internal_error", "failed to resolve project", nil), nil
		}

		// Look up display name for the prefix line (resolver only returns
		// UUID; we want a human-readable header in the guide body).
		var name string
		if err := pool.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, projectID).Scan(&name); err != nil {
			slog.ErrorContext(ctx, "get_instructions: project name lookup failed",
				"projectId", projectID.String(), "error", err.Error())
			return skmcp.ToolError("internal_error", "failed to look up project", nil), nil
		}

		// Prefix line + blank line, then the guide. Two newlines so the
		// first heading still parses as Markdown level-1.
		body := "Project: " + name + "\n\n" + instructions.FullGuide
		return mcpgo.NewToolResultText(body), nil
	}
}
