package tools

import (
	"context"
	_ "embed"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
)

// themeInstructionsBody is the canonical agent guide for theme editing.
// Embedded at compile time so the running service has zero filesystem
// dependency on the source tree — same pattern as instructions/full.md.
//
//go:embed theme_instructions.md
var themeInstructionsBody string

// handleThemeInstructions backs skein_theme(action="instructions") —
// the canonical agent guide for the fork → preview → confirm → persist
// theme-editing workflow. Static markdown; same body every call.
//
// The pointer in skein_get_instructions tells the agent to call it on
// demand when the user asks for visual / theme changes, which keeps the
// long markdown out of the always-loaded onboarding doc.
//
// This is the only handler on the MCP surface that answers with raw text
// instead of a JSON object (NewToolResultText, below). That is deliberate
// — it is a document, not a payload — and it is safe for the metrics
// classifier because classifyToolCall only probes bodies that start with
// `{` for an error envelope, so markdown scores outcome="ok". Pinned by
// TestThemeInstructions_RawMarkdownStaysOK.
func handleThemeInstructions() server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_theme", "action", "instructions",
			"userId", userID.String(),
			"bytes", len(themeInstructionsBody),
			"latencyMs", time.Since(start).Milliseconds())

		return mcpgo.NewToolResultText(themeInstructionsBody), nil
	}
}
