package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/themes"
)

// themeForkResponse echoes the new ThemeDTO returned by
// skein_theme(action="fork") — copy an existing theme (or a builtin
// defaults map) into a brand-new theme row and activate it as the
// caller's preference. The agent already knows the user-pref flip
// happened, since the same call did it.
//
// Publishes `skein.user.{uid}.theme.user-pref.updated` so the originating
// frontend refetches /me/theme.
type themeForkResponse struct {
	Theme *themes.ThemeDTO `json:"theme"`
}

func handleThemeFork(store *themes.Store, publisher themes.Publisher) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		rawSource, err := req.RequireString("source")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"source is required", nil), nil
		}
		source := strings.TrimSpace(rawSource)
		if source == "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"source must be non-empty", nil), nil
		}

		rawName, err := req.RequireString("name")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"name is required", nil), nil
		}
		name, vErr := themes.ValidateName(rawName)
		if vErr != nil {
			if ve, ok := themes.IsValidationError(vErr); ok {
				return skmcp.ToolError(ErrCodeValidationError,
					ve.Message, map[string]any{"code": ve.Code}), nil
			}
			return skmcp.ToolError(ErrCodeValidationError,
				vErr.Error(), nil), nil
		}

		// Resolve source → (variables, base). Four shapes:
		//   - "active"        → caller's active theme, or builtin-light when none.
		//   - "builtin-light" → DefaultVariablesForBase("light").
		//   - "builtin-dark"  → DefaultVariablesForBase("dark").
		//   - <uuid>          → that theme's variables.
		//
		// Active fork preserves the user's existing builtinTheme
		// preference — only the customThemeId pointer changes. Other
		// source kinds set BuiltinTheme=seedBase because they ARE
		// specifying a base intent (the user explicitly asked to fork
		// builtin-dark, so the resulting custom theme inherits dark as
		// its fallback base).
		var (
			seedVariables     map[string]string
			seedBase          string
			activePrefBuiltin string // populated only in the "active" branch
			isActive          bool
		)
		switch source {
		case "active":
			isActive = true
			pref, err := store.GetUserThemePref(ctx, userID)
			if err != nil {
				if errors.Is(err, themes.ErrNotFound) {
					return skmcp.ToolError(ErrCodeUserNotFound,
						"user no longer exists", nil), nil
				}
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_theme", "action", "fork",
					"reason", "get user theme pref failed",
					"userId", userID.String(), "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError,
					"failed to load user theme preference", nil), nil
			}
			activePrefBuiltin = pref.BuiltinTheme
			if activePrefBuiltin == "" {
				activePrefBuiltin = "light"
			}
			if pref.Mode == "custom" && pref.CustomThemeID != nil && *pref.CustomThemeID != uuid.Nil {
				a, err := store.GetTheme(ctx, *pref.CustomThemeID, userID)
				switch {
				case err == nil:
					seedVariables = a.Variables
					seedBase = a.Base
				case errors.Is(err, themes.ErrNotFound):
					// Dangling reference — fall back to the builtin
					// pref the user had configured (light or dark).
					seedBase = pref.BuiltinTheme
					if seedBase == "" {
						seedBase = "light"
					}
					seedVariables = themes.DefaultVariablesForBase(seedBase)
				default:
					slog.ErrorContext(ctx, "mcp tool error",
						"tool", "skein_theme", "action", "fork",
						"reason", "get active theme failed",
						"userId", userID.String(),
						"customThemeId", pref.CustomThemeID.String(),
						"error", err.Error())
					return skmcp.ToolError(ErrCodeInternalError,
						"failed to load active theme", nil), nil
				}
			} else {
				// mode=builtin → fork the matching builtin defaults.
				seedBase = pref.BuiltinTheme
				if seedBase == "" {
					seedBase = "light"
				}
				seedVariables = themes.DefaultVariablesForBase(seedBase)
			}
		case "builtin-light":
			seedBase = "light"
			seedVariables = themes.DefaultVariablesForBase("light")
		case "builtin-dark":
			seedBase = "dark"
			seedVariables = themes.DefaultVariablesForBase("dark")
		default:
			// UUID path.
			id, err := uuid.Parse(source)
			if err != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					`source must be "active", "builtin-light", "builtin-dark", or a theme UUID`,
					map[string]any{"source": source}), nil
			}
			// Only the caller's own theme forks; another user's answers
			// as a nonexistent one (sk-674.18 T035, INV-3).
			src, err := store.GetTheme(ctx, id, userID)
			if err != nil {
				if errors.Is(err, themes.ErrNotFound) {
					return skmcp.ToolError(ErrCodeValidationError,
						"theme_not_found: source theme does not exist",
						map[string]any{"source": source}), nil
				}
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_theme", "action", "fork",
					"reason", "get source theme failed",
					"userId", userID.String(), "source", source, "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError,
					"failed to load source theme", nil), nil
			}
			seedVariables = src.Variables
			seedBase = src.Base
		}

		// Validate the seed variables defensively — the source's
		// variables were already validated when it was created, but
		// the validate-everything-at-the-boundary rule keeps the
		// CreateTheme call uniform.
		seedVariables, vErr = themes.ValidateVariables(seedVariables)
		if vErr != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_theme", "action", "fork",
				"reason", "seed variables invalid",
				"userId", userID.String(), "error", vErr.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"seed theme has invalid variables", nil), nil
		}

		dto, err := store.CreateTheme(ctx, themes.CreateThemeInput{
			Name:      name,
			Base:      seedBase,
			Variables: seedVariables,
			CreatedBy: userID,
		})
		if err != nil {
			if errors.Is(err, themes.ErrNameTaken) {
				return skmcp.ToolError(ErrCodeValidationError,
					"name_taken: theme name already in use",
					map[string]any{"name": name}), nil
			}
			if ve, ok := themes.IsValidationError(err); ok {
				return skmcp.ToolError(ErrCodeValidationError,
					ve.Message, map[string]any{"code": ve.Code}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_theme", "action", "fork",
				"reason", "create theme failed",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"failed to create theme", nil), nil
		}

		// Flip user pref to mode=custom pointing at the new id.
		//
		// BuiltinTheme handling differs per source kind:
		//   - "active"        → preserve the user's CURRENT BuiltinTheme.
		//                       The user already had a dark/light preference;
		//                       forking their active theme is a copy-and-edit
		//                       op, not a redeclaration of base intent.
		//   - otherwise       → BuiltinTheme = seedBase. The user explicitly
		//                       asked to fork e.g. builtin-dark, so the
		//                       fallback base reflects that choice.
		newID := dto.ID
		prefBuiltin := seedBase
		if isActive {
			prefBuiltin = activePrefBuiltin
		}
		if _, err := store.SetUserThemePref(ctx, userID, themes.SetThemePrefInput{
			Mode:          "custom",
			BuiltinTheme:  prefBuiltin, // see comment above
			CustomThemeID: &newID,
		}); err != nil {
			// Best-effort cleanup: the new theme row stays — the
			// agent can still pick it via skein_theme(action="describe")
			// and
			// rerun the activation step.
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_theme", "action", "fork",
				"reason", "set user theme pref failed",
				"userId", userID.String(), "themeId", newID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"failed to activate forked theme", nil), nil
		}

		// Fire-and-forget pref poke for the originating frontend. We
		// log but don't fail the call — the theme + pref are already
		// persisted, the message is just for the live-update flow.
		// See theme_set_variables for the three-way error split.
		if pErr := publisher.PublishThemeUserPrefUpdated(ctx, userID); pErr != nil {
			switch {
			case errors.Is(pErr, themes.ErrInvalidPublishTarget):
				slog.ErrorContext(ctx, "skein_theme(action=fork): invalid publish target",
					"userId", userID.String(), "error", pErr.Error())
			case errors.Is(pErr, themes.ErrNATSDisabled):
				// NATS down — frontend won't get live update, but DB is fine
			default:
				slog.WarnContext(ctx, "skein_theme(action=fork): user-pref publish failed",
					"userId", userID.String(), "error", pErr.Error())
			}
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_theme", "action", "fork",
			"userId", userID.String(),
			"source", source,
			"themeId", newID.String(),
			"name", name,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(themeForkResponse{Theme: dto})
	}
}
