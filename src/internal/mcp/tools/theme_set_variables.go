package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/themes"
)

// themeSetVariablesResponse is the wire shape returned by
// skein_theme(action="set_variables") — merge a partial variables map
// into an existing theme, persist it, publish
// `skein.user.{authorId}.theme.updated` with the full merged map so the
// author's connected frontends refresh — nobody else's (sk-674.18 T035,
// K-31).
type themeSetVariablesResponse struct {
	Theme *themes.ThemeDTO `json:"theme"`
}

func handleThemeSetVariables(store *themes.Store, publisher themes.Publisher) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		rawID, err := req.RequireString("themeId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"themeId is required", nil), nil
		}
		themeID, err := uuid.Parse(strings.TrimSpace(rawID))
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"themeId must be a UUID",
				map[string]any{"themeId": rawID}), nil
		}

		// `variables` arrives as an arbitrary JSON object. The MCP
		// argument-decoding path gives it to us as map[string]any;
		// flatten to map[string]string with per-value validation.
		args := req.GetArguments()
		raw, ok := args["variables"]
		if !ok || raw == nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"variables is required", nil), nil
		}
		patch, vErr := decodeStringMap(raw, "variables")
		if vErr != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				vErr.Error(), nil), nil
		}
		// Reject an empty patch — the call would otherwise be a no-op
		// merge that still bumps updated_at + publishes an event. The
		// frontend's "apply variable X" button always sends at least
		// one key.
		if len(patch) == 0 {
			return skmcp.ToolError(ErrCodeValidationError,
				"variables must contain at least one key", nil), nil
		}
		patch, valErr := themes.ValidateVariables(patch)
		if valErr != nil {
			if ve, ok := themes.IsValidationError(valErr); ok {
				return skmcp.ToolError(ErrCodeValidationError,
					ve.Message, map[string]any{"code": ve.Code}), nil
			}
			return skmcp.ToolError(ErrCodeValidationError,
				valErr.Error(), nil), nil
		}

		// ── Refuse a key the dictionary does not declare (sk-285.7.5) ──
		//
		// This is the only writer on the MCP side, and it MERGES, while
		// the frontend's one-time migration write-back REPLACES the map
		// wholesale. That difference is what made a dead key here worse
		// than useless. Written, it sat in the row painting nothing (the
		// read-path migration builds its output from the current
		// dictionary, so an unknown name is dropped before anything is
		// applied) AND it re-armed the frontend's `needsMigration`, whose
		// next write-back — by the theme's owner, on their next page load
		// — replaced the map and deleted the key again. Two writers, one
		// row, and the agent lost silently every time.
		//
		// Refusing at the boundary is the fix that makes the loss
		// impossible rather than merely recoverable: nothing is written,
		// `updated_at` does not move, no `theme.updated` goes out, and
		// the caller is told which names it used and what replaced them.
		// The alternative on the table — a "migrated" marker on the row
		// so a legacy key stops arming the rewrite — preserves the key
		// but not its effect, i.e. it makes the silent no-op permanent
		// instead of transient. That is a worse answer to the same
		// question.
		//
		// This does NOT touch the keys already in the row. A theme that
		// still holds pre-redesign names is migrated by the frontend on
		// read; refusing to merge into it would strand it.
		if issues := themes.ClassifyRoles(patch); len(issues) > 0 {
			return skmcp.ToolError(ErrCodeUnknownThemeRole,
				fmt.Sprintf(
					"nothing was written: %d of %d keys are not roles of the current "+
						"dictionary and would paint nothing — %s. "+
						"skein_theme(action=\"describe\") lists every live role.",
					len(issues), len(patch), themes.FormatRoleIssues(issues)),
				map[string]any{"roles": issues}), nil
		}

		// Load the existing row so we can merge the patch on top.
		// Race against a concurrent PATCH /themes/{id} is acceptable:
		// last-writer-wins is the documented semantic for themes (no
		// version optimistic concurrency in the schema).
		// Only the author edits a theme; another user's answers as a
		// nonexistent one (sk-674.18 T035, INV-3).
		existing, err := store.GetTheme(ctx, themeID, userID)
		if err != nil {
			if errors.Is(err, themes.ErrNotFound) {
				return skmcp.ToolError(ErrCodeValidationError,
					"theme_not_found",
					map[string]any{"themeId": themeID.String()}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_theme", "action", "set_variables",
				"reason", "get theme failed",
				"userId", userID.String(), "themeId", themeID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"failed to load theme", nil), nil
		}

		merged := make(map[string]string, len(existing.Variables)+len(patch))
		for k, v := range existing.Variables {
			merged[k] = v
		}
		for k, v := range patch {
			merged[k] = v
		}

		updated, err := store.UpdateTheme(ctx, themeID, themes.UpdateThemeInput{
			Variables: &merged,
			UpdatedBy: userID,
		})
		if err != nil {
			if errors.Is(err, themes.ErrNotFound) {
				return skmcp.ToolError(ErrCodeValidationError,
					"theme_not_found",
					map[string]any{"themeId": themeID.String()}), nil
			}
			if ve, ok := themes.IsValidationError(err); ok {
				return skmcp.ToolError(ErrCodeValidationError,
					ve.Message, map[string]any{"code": ve.Code}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_theme", "action", "set_variables",
				"reason", "update theme failed",
				"userId", userID.String(), "themeId", themeID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"failed to update theme", nil), nil
		}

		// Publish the full merged map. Log-on-error: the DB update
		// already succeeded, so we must NOT fail the call just because
		// NATS is down — the next deploy reconciles via /me/theme on
		// the frontend's natural refresh.
		//
		// Branch on the publish error so the three failure modes land at
		// distinct log levels:
		//   - ErrInvalidPublishTarget → Error (we passed a zero uuid;
		//     programmer error, would never recover on retry)
		//   - ErrNATSDisabled         → silent (operator already knows;
		//     repeatedly warning would spam logs)
		//   - any other               → Warn (transient NATS issue)
		if pErr := publisher.PublishThemeUpdated(ctx, userID, themeID, updated.Variables); pErr != nil {
			switch {
			case errors.Is(pErr, themes.ErrInvalidPublishTarget):
				slog.ErrorContext(ctx, "skein_theme(action=set_variables): invalid publish target",
					"userId", userID.String(),
					"themeId", themeID.String(), "error", pErr.Error())
			case errors.Is(pErr, themes.ErrNATSDisabled):
				// NATS down — frontend won't get live update, but DB is fine
			default:
				slog.WarnContext(ctx, "skein_theme(action=set_variables): publish failed",
					"userId", userID.String(),
					"themeId", themeID.String(), "error", pErr.Error())
			}
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_theme", "action", "set_variables",
			"userId", userID.String(),
			"themeId", themeID.String(),
			"patchSize", len(patch),
			"mergedSize", len(updated.Variables),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(themeSetVariablesResponse{Theme: updated})
	}
}

// decodeStringMap turns the loose JSON object the MCP arg path
// surfaces (map[string]any with arbitrary value types) into
// map[string]string, rejecting non-string values with a clear error
// rather than coercing them. Used by both set_variables and
// preview_variables.
func decodeStringMap(raw any, field string) (map[string]string, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%q] must be a string", field, k)
		}
		out[k] = s
	}
	return out, nil
}
