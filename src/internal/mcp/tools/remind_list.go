package tools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
)

// remindListItem is the slim per-reminder shape returned by
// skein_remind(action="list") — issue id/title + remind_at + derived state +
// reminderId + snoozeCount, the fields an agent needs to reason about
// "what's ringing / armed".
type remindListItem struct {
	ReminderID  string `json:"reminderId"`
	IssueID     string `json:"issueId"`
	IssueTitle  string `json:"issueTitle"`
	ProjectID   string `json:"projectId"`
	RemindAt    string `json:"remindAt"`
	State       string `json:"state"`
	SnoozeCount int    `json:"snoozeCount"`
	NotifiedAt  string `json:"notifiedAt,omitempty"`
}

// remindListResult is the tool payload: the (optionally project-filtered)
// list + the cross-project ringing-task badge count.
type remindListResult struct {
	Reminders        []remindListItem `json:"reminders"`
	RingingTaskCount int              `json:"ringingTaskCount"`
}

// handleRemindList backs skein_remind(action="list") (sk-62.2) — the
// caller's "what's ringing / armed" lens across projects, plus the
// ringing-task badge count. Rows come back most-urgent-first: ringing
// (due) before armed (future). Pure read.
//
// Until sk-255.3 this was its own tool, skein_remind_list; the schema now
// lives in remind_group.go, where it is the one read-only action of a
// tool whose annotations are coarsened to readOnly=false.
func handleRemindList(pool *pgxpool.Pool, store *reminders.Store, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		filter, ok := reminders.ParseListFilter(strings.TrimSpace(req.GetString("state", "")))
		if !ok {
			return skmcp.ToolError(ErrCodeValidationError,
				"state must be one of ringing | armed | all", nil), nil
		}

		// Optional project filter — resolve the handle to a UUID and keep
		// only reminders on issues in that project.
		var projectFilter *uuid.UUID
		if raw := strings.TrimSpace(req.GetString("project", "")); raw != "" {
			pid, err := ResolveProjectHandle(ctx, pool, projectsStore, raw, userID)
			if err != nil {
				return projectErrorResult(err)
			}
			projectFilter = &pid
		}

		resp, err := store.ListAndCount(ctx, userID, filter)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error", "tool", "skein_remind", "action", "list",
				"reason", "store list failed", "userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list reminders", nil), nil
		}

		items := make([]remindListItem, 0, len(resp.Reminders))
		for _, r := range resp.Reminders {
			if projectFilter != nil && r.ProjectID != *projectFilter {
				continue
			}
			item := remindListItem{
				ReminderID:  r.ID.String(),
				IssueID:     r.IssueID,
				IssueTitle:  r.IssueTitle,
				ProjectID:   r.ProjectID.String(),
				RemindAt:    r.RemindAt.Format(time.RFC3339),
				State:       string(r.State),
				SnoozeCount: r.SnoozeCount,
			}
			if r.NotifiedAt != nil {
				item.NotifiedAt = r.NotifiedAt.Format(time.RFC3339)
			}
			items = append(items, item)
		}

		out := remindListResult{
			Reminders:        items,
			RingingTaskCount: resp.RingingTaskCount,
		}
		slog.InfoContext(ctx, "mcp tool ok", "tool", "skein_remind", "action", "list",
			"userId", userID.String(), "count", len(items),
			"ringingTaskCount", out.RingingTaskCount, "latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(out)
	}
}
