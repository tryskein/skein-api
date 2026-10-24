package tools

import (
	"context"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/google/uuid"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/reminders"
)

// reminderRef is the slim shape surfaced in a reminder_target_ambiguous
// error so the agent can pick the right reminderId.
type reminderRef struct {
	ReminderID  string `json:"reminderId"`
	IssueID     string `json:"issueId"`
	IssueTitle  string `json:"issueTitle"`
	RemindAt    string `json:"remindAt"`
	State       string `json:"state"`
	SnoozeCount int    `json:"snoozeCount"`
}

// resolveReminderTarget implements the shared snooze/dismiss targeting
// rule for the MCP tools:
//
//   - reminderId supplied → parse it and use it directly.
//   - else issueId supplied → look up the caller's reminders on that
//     issue. Exactly one → target it. Zero → reminder_not_found. More than
//     one → reminder_target_ambiguous with the candidate list.
//   - neither → validation_error.
//
// Returns the resolved reminder uuid, or a ready-to-return error result.
// store + ownerID scope every lookup to the caller.
func resolveReminderTarget(
	ctx context.Context,
	store *reminders.Store,
	ownerID uuid.UUID,
	rawReminderID, rawIssueID string,
) (uuid.UUID, *mcpgo.CallToolResult) {
	reminderID := strings.TrimSpace(rawReminderID)
	issueID := strings.TrimSpace(rawIssueID)

	if reminderID != "" {
		id, err := uuid.Parse(reminderID)
		if err != nil {
			return uuid.Nil, skmcp.ToolError(ErrCodeValidationError,
				"reminderId must be a UUID", map[string]any{"reminderId": rawReminderID})
		}
		return id, nil
	}

	if issueID == "" {
		return uuid.Nil, skmcp.ToolError(ErrCodeValidationError,
			"supply either reminderId or issueId", nil)
	}
	if !looksLikeHierarchicalID(issueID) {
		return uuid.Nil, skmcp.ToolError(ErrCodeValidationError,
			"issueId must be a full hierarchical id like \"sk-62\"",
			map[string]any{"issueId": rawIssueID})
	}

	now := time.Now().UTC()
	list, err := store.ListForUserIssue(ctx, ownerID, issueID, now)
	if err != nil {
		return uuid.Nil, skmcp.ToolError(ErrCodeInternalError,
			"failed to resolve reminders for issue", nil)
	}
	switch len(list) {
	case 0:
		return uuid.Nil, skmcp.ToolError(ErrCodeReminderNotFound,
			"you have no reminder on this issue",
			map[string]any{"issueId": issueID})
	case 1:
		return list[0].ID, nil
	default:
		refs := make([]reminderRef, 0, len(list))
		for _, r := range list {
			refs = append(refs, reminderRef{
				ReminderID:  r.ID.String(),
				IssueID:     r.IssueID,
				IssueTitle:  r.IssueTitle,
				RemindAt:    r.RemindAt.Format(time.RFC3339),
				State:       string(r.State),
				SnoozeCount: r.SnoozeCount,
			})
		}
		return uuid.Nil, skmcp.ToolError(ErrCodeReminderTargetAmbiguous,
			"more than one reminder on this issue — pass reminderId to disambiguate",
			map[string]any{"issueId": issueID, "reminders": refs})
	}
}
