package tools

import (
	"context"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/themes"
)

// themeInspectClearResponse is the wire shape returned by
// skein_theme(action="inspect_clear") — just the dropped count. We don't
// echo the cleared selections (the agent already saw them via describe;
// surfacing them again here would just bloat the response).
//
// The action flushes the caller's accumulated inspect-mode selections
// from the in-memory cache. No DB write happens, but it is the reason
// skein_theme as a whole declares destructiveHint=true: the caller cannot
// recover the cleared selections — the frontend has to re-post them on
// the next inspector click.
type themeInspectClearResponse struct {
	Cleared int `json:"cleared"`
}

func handleThemeInspectClear(inspectCache *themes.InspectCache) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		// Defensive — cache may be nil in tests that don't wire it.
		// Treat as "nothing to clear" rather than an internal error;
		// the agent's intent (flush) is already satisfied.
		cleared := 0
		if inspectCache != nil {
			cleared = inspectCache.Clear(userID)
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_theme", "action", "inspect_clear",
			"userId", userID.String(),
			"cleared", cleared,
			"latencyMs", time.Since(start).Milliseconds())

		return skmcp.ToolJSON(themeInspectClearResponse{Cleared: cleared})
	}
}
