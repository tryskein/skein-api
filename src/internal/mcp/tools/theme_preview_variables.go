package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/themes"
)

// themePreviewResponse is the wire shape returned by
// skein_theme(action="preview_variables") — an ok marker, plus the
// complaint about any key that will not paint. We intentionally don't
// echo the variables payload (the agent already has it).
//
// The action pushes an ephemeral preview overlay to the caller's frontend
// WITHOUT touching any theme row (subject
// `skein.user.{userId}.theme.preview`). No DB write happens, but the push
// to the user's open tabs is a visible side effect, which is part of why
// skein_theme cannot declare readOnlyHint=true.
//
// ── Warning, not refusal — and not a silent drop either (sk-285.7.4) ──
//
// A preview is an ephemeral overlay carrying an EXPLICIT map from the
// agent. Migrating it — substituting a retired role's successor — would
// be rewriting the command rather than migrating data, and that decision
// is recorded on the issue: the values go out exactly as sent. But a
// name outside the current dictionary paints nothing, so an agent that
// asked for `--bg-base: #fff`, expected the page background to change
// and got a bare `{"ok":true}` had no way to learn it had asked for
// nothing. Dropping such keys instead would only trade one silent
// failure for another.
//
// So both fields are present or both absent: `Warning` reads in prose,
// `Roles` carries the same facts structurally (key, why, successor) for
// an agent that would rather branch than parse. `omitempty` on each is
// what keeps a clean call's response byte-identical to what it was
// before this field existed.
type themePreviewResponse struct {
	OK      bool               `json:"ok"`
	Warning string             `json:"warning,omitempty"`
	Roles   []themes.RoleIssue `json:"roles,omitempty"`
}

// The `preview_unavailable` code this tool returns is declared with every
// other business code in errors.go (ErrCodePreviewUnavailable).

func handleThemePreviewVariables(publisher themes.Publisher) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

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
		if len(patch) == 0 {
			return skmcp.ToolError(ErrCodeValidationError,
				"variables must contain at least one key", nil), nil
		}
		// Validate values — defence in depth so a preview can't ship
		// CSS-injection payloads via the NATS firehose to other
		// admin-tab subscribers. Same rules as the persisted path.
		patch, valErr := themes.ValidateVariables(patch)
		if valErr != nil {
			if ve, ok := themes.IsValidationError(valErr); ok {
				return skmcp.ToolError(ErrCodeValidationError,
					ve.Message, map[string]any{"code": ve.Code}), nil
			}
			return skmcp.ToolError(ErrCodeValidationError,
				valErr.Error(), nil), nil
		}

		// Classify BEFORE publishing so the log line below can report
		// what went out; the result changes nothing about what is sent.
		// See the docblock on themePreviewResponse for why this warns
		// rather than refusing or filtering.
		issues := themes.ClassifyRoles(patch)

		if err := publisher.PublishThemePreview(ctx, userID, patch); err != nil {
			if errors.Is(err, themes.ErrNATSDisabled) {
				return skmcp.ToolError(ErrCodePreviewUnavailable,
					"NATS is not configured; live preview unavailable", nil), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_theme", "action", "preview_variables",
				"reason", "publish failed",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError,
				"failed to publish preview", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_theme", "action", "preview_variables",
			"userId", userID.String(),
			"patchSize", len(patch),
			"unpaintedKeys", len(issues),
			"latencyMs", time.Since(start).Milliseconds())
		resp := themePreviewResponse{OK: true}
		if len(issues) > 0 {
			resp.Warning = fmt.Sprintf(
				"%d of %d previewed keys are not roles of the current dictionary and "+
					"will paint nothing: %s. The values were sent EXACTLY as supplied — "+
					"a preview is your map, not a migration — so re-send under a live "+
					"role name to see a change. skein_theme(action=\"describe\") lists them.",
				len(issues), len(patch), themes.FormatRoleIssues(issues))
			resp.Roles = issues
		}
		return skmcp.ToolJSON(resp)
	}
}
