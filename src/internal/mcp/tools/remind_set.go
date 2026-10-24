package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
)

// handleRemindSet backs skein_remind(action="set") (sk-62.2). Creates a
// personal reminder on an issue for the calling user. When
// remindAt passes, the scheduler materialises a bell notification.
//
// Until sk-255.3 this was its own tool, skein_remind_set; the schema now
// lives in remind_group.go and the handler is reached through the
// `action` discriminator. Behaviour is unchanged.
func handleRemindSet(pool *pgxpool.Pool, store *reminders.Store, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		rawID, err := req.RequireString("issueId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "issueId is required", nil), nil
		}
		issueID := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"sk-62\"",
				map[string]any{"issueId": rawID}), nil
		}

		rawRemind, err := req.RequireString("remindAt")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "remindAt is required", nil), nil
		}
		remindAt, perr := time.Parse(time.RFC3339, strings.TrimSpace(rawRemind))
		if perr != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"remindAt must be an RFC3339 timestamp", map[string]any{"remindAt": rawRemind}), nil
		}
		note := strings.TrimSpace(req.GetString("note", ""))

		// Visibility gate — issue must exist and be visible to the caller.
		issue, err := issuesStore.GetIssue(ctx, issueID)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": issueID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error", "tool", "skein_remind", "action", "set",
				"reason", "load issue failed", "userId", userID.String(),
				"issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, issue.ProjectID, userID); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": issueID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error", "tool", "skein_remind", "action", "set",
				"reason", "visibility check failed", "userId", userID.String(),
				"issueId", issueID, "error", vErr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "visibility check failed", nil), nil
		}

		actor := RemindersAgentActor(ctx, pool, userID)
		rem, err := store.Create(ctx, issueID, userID, remindAt, note, actor)
		if err != nil {
			if errors.Is(err, reminders.ErrIssueNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": issueID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error", "tool", "skein_remind", "action", "set",
				"reason", "store create failed", "userId", userID.String(),
				"issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to create reminder", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok", "tool", "skein_remind", "action", "set",
			"userId", userID.String(), "issueId", issueID,
			"reminderId", rem.ID.String(), "latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(rem)
	}
}
