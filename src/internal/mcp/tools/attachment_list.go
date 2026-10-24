package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
)

// handleAttachmentList backs skein_attachments(action="list").
//
// Inputs: issueId (required, string) — full hierarchical id.
// Output: { items: [...], total: N }. Only 'ready' attachments are
// returned; pending rows are an internal lifecycle state.
func handleAttachmentList(deps attachmentDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		role := callerRole(ctx)

		raw, err := req.RequireString("issueId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "issueId is required", nil), nil
		}
		issueID := strings.TrimSpace(raw)
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"issueId": raw}), nil
		}

		// Visibility — does NOT require S3 (the list is metadata
		// only), so we don't gate on requireAttachmentsAvailable here.
		// Listing is a read that works even if S3 is offline.
		if _, errResp := loadAndCheckIssueVisibility(ctx, deps.pool, issueID, userID, role); errResp != nil {
			return errResp, nil
		}

		res, err := deps.store.ListByIssue(ctx, issueID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_attachments", "action", "list",
				"reason", "list failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list attachments", nil), nil
		}
		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_attachments", "action", "list",
			"userId", userID.String(), "issueId", issueID,
			"count", len(res.Items),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(res)
	}
}
