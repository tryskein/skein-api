package tools

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/themes"
)

// themeDescribeResponse is the wire shape returned by
// skein_theme(action="describe") — the agent's one-call view of "what can
// I change, and what is the user looking at right now?".
//
// Stable contract; additive changes only. Field order follows the same
// "static schema → caller-specific → ephemeral hint" arc as the agent
// is expected to read it.
type themeDescribeResponse struct {
	// Schema is the static TOKEN_SECTIONS mirror. Same shape every call.
	Schema []themes.SectionDTO `json:"schema"`

	// ActiveTheme is non-nil iff the caller's mode='custom' AND the
	// referenced themeId exists. nil for mode='builtin' OR
	// customThemeId pointing at a deleted row.
	ActiveTheme *themes.ThemeDTO `json:"activeTheme"`

	// BuiltinPref is "light" or "dark" — the caller's fallback when
	// no custom theme is active. Mirrors users.builtin_theme.
	BuiltinPref string `json:"builtinPref"`

	// Themes is the caller's own themes (sk-674.18 T035) — the same
	// list as GET /api/v1/themes, sorted updated_at DESC.
	Themes []themes.ThemeDTO `json:"themes"`

	// SelectedTargets is the caller's inspect-mode selections from the
	// in-memory cache (up to 20 most-recent clicks within the last 10
	// minutes), oldest first. Always an array — empty when the user
	// hasn't clicked anything recently OR every entry has expired.
	//
	// The agent should call skein_theme(action="inspect_clear") after
	// processing the list so the next describe call returns a fresh
	// window rather than accumulating selections across turns.
	//
	// sk-10.6 — was singular `selectedTarget` (overwrite-on-write).
	SelectedTargets []themes.InspectSelection `json:"selectedTargets"`
}

func handleThemeDescribe(store *themes.Store, inspectCache *themes.InspectCache) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		// Resolve the caller's pref. ErrNotFound here means the user
		// row vanished mid-request — extremely unlikely (the api-key
		// middleware lookup just succeeded), but we surface it cleanly
		// rather than returning a partial-shape response.
		pref, err := store.GetUserThemePref(ctx, userID)
		if err != nil {
			if errors.Is(err, themes.ErrNotFound) {
				return skmcp.ToolError(ErrCodeUserNotFound,
					"user resolved from the API key no longer exists", nil), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_theme", "action", "describe",
				"reason", "get user theme pref failed",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"failed to load user theme preference", nil), nil
		}

		// Active theme lookup is optional — only when mode=custom AND
		// a customThemeId is set.
		var active *themes.ThemeDTO
		if pref.Mode == "custom" && pref.CustomThemeID != nil && *pref.CustomThemeID != uuid.Nil {
			a, err := store.GetTheme(ctx, *pref.CustomThemeID, userID)
			switch {
			case err == nil:
				active = a
			case errors.Is(err, themes.ErrNotFound):
				// Theme was deleted between the SetUserThemePref and
				// now. Frontend behaviour is to fall back to the
				// builtin; we mirror that by returning activeTheme=null
				// (NOT an error). Log at warn so a dangling-ref
				// regression is observable in Loki.
				slog.WarnContext(ctx, "skein_theme(action=describe): active theme missing",
					"userId", userID.String(),
					"customThemeId", pref.CustomThemeID.String())
			default:
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_theme", "action", "describe",
					"reason", "get active theme failed",
					"userId", userID.String(),
					"customThemeId", pref.CustomThemeID.String(),
					"error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError,
					"failed to load active theme", nil), nil
			}
		}

		// The caller's own themes — used by the agent to pick one to
		// fork. A theme is its author's (sk-674.18 T035): another
		// user's never appears. Few per person, so no pagination.
		themesList, err := store.ListThemes(ctx, userID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_theme", "action", "describe",
				"reason", "list themes failed",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"failed to list themes", nil), nil
		}

		// Inspector selections (if any). Cache may be absent in tests
		// that don't wire it; treat that as "no selections". Always
		// emit a non-nil slice so the JSON wire shape is consistently
		// an array (never null) — easier for the agent to iterate.
		selections := []themes.InspectSelection{}
		if inspectCache != nil {
			if got, ok := inspectCache.Get(userID); ok && len(got) > 0 {
				selections = got
			}
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_theme", "action", "describe",
			"userId", userID.String(),
			"mode", pref.Mode,
			"themesCount", len(themesList),
			"selectionCount", len(selections),
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(themeDescribeResponse{
			Schema:          themes.TokenSchema,
			ActiveTheme:     active,
			BuiltinPref:     pref.BuiltinTheme,
			Themes:          themesList,
			SelectedTargets: selections,
		})
	}
}
