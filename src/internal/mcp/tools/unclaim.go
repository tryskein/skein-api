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

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterUnclaim wires the skein_unclaim tool. Releases the caller's
// lock on an issue AND (sk-19) atomically transitions the status to the
// caller-declared `nextStatus`.
//
// Required parameters:
//
//   - id          — full hierarchical issue id
//   - nextStatus — one of: "in_progress" (handoff), "open" (give up),
//     "deferred" (punt). "closed" is rejected; closing has its own
//     tool (skein_close) which carries reason + commits[].
//   - reason — non-empty after trim, lands on the issue.unclaimed
//     event AND (when status actually changed) the
//     issue.status_changed event.
//
// Predicate:
//
//   - locked_by IS NULL → idempotent no-op (returns the existing
//     IssueDetail unchanged; nextStatus is ignored on a free issue,
//     because "unclaim" semantically means "release my lock" — there's
//     no lock to release, nothing to do. Use skein_update to flip status
//     without a lock).
//   - locked_by = caller's session → success.
//   - locked_by = another session AND force=false → `not_lock_holder`.
//   - locked_by = another session AND force=true (force_unlock) → success
//     with event reason="<reason> (force-overridden)".
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false
//   - idempotentHint: false (first call mutates state; we keep the
//     conservative default for consistency with other write tools)
//   - openWorldHint: false
func RegisterUnclaim(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_unclaim",
		mcpgo.WithDescription(withAckAnswerNote(
			"Release the caller's lock on an issue AND atomically "+
				"transition status to the caller-declared `nextStatus`. "+
				"Three valid intents: `nextStatus=\"in_progress\"` "+
				"= handoff (status stays in_progress so a teammate can "+
				"claim and continue; pair with a comment describing "+
				"what's done); `nextStatus=\"open\"` = give up (status "+
				"reverts to open so the issue is visible again in "+
				"skein_ready); `nextStatus=\"deferred\"` = punt (status "+
				"→ deferred; set defer_until via a follow-up "+
				"skein_update if needed). `nextStatus=\"closed\"` is "+
				"rejected — use skein_close instead, which carries "+
				"reason + commits[]. `reason` is required (non-empty "+
				"after trim) and lands on both the issue.unclaimed "+
				"event and (when status actually changed) the "+
				"issue.status_changed event. Idempotent on an "+
				"already-unlocked issue (returns the issue post-state "+
				"unchanged; nextStatus is ignored — use skein_update "+
				"directly if you want to flip status without a lock). "+
				"Caller must hold the lock or pass `force=true` "+
				"(force_unlock: project owner, space owner, instance admin; "+
				"others get `force_unlock_forbidden`); event reason "+
				"becomes \"<reason> (force-overridden)\" on the force "+
				"path.",
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\")."),
		),
		mcpgo.WithString("nextStatus",
			mcpgo.Required(),
			mcpgo.Description("Required intent declaration: \"in_progress\" (handoff), \"open\" (give up), or \"deferred\" (punt). \"closed\" is rejected — use skein_close."),
			mcpgo.Enum("in_progress", "open", "deferred"),
		),
		mcpgo.WithString("reason",
			mcpgo.Required(),
			mcpgo.Description("Free-form reason annotation (non-empty after trim). Lands on issue.unclaimed.reason and, when status changed, issue.status_changed.reason."),
		),
		mcpgo.WithBoolean("force",
			mcpgo.Description("Bypass the lock-holder predicate. Needs force_unlock on the issue's project (project owner, space owner, instance admin); others get `force_unlock_forbidden`. Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleUnclaim(pool, issuesStore))
}

func handleUnclaim(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		rawID, err := req.RequireString("id")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "id is required", nil), nil
		}
		id := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(id) {
			return skmcp.ToolError(ErrCodeValidationError,
				"id must be a full hierarchical id like \"demo-42\"",
				map[string]any{"id": rawID}), nil
		}

		force := req.GetBool("force", false)
		// sk-674.18 T034 (K-28): force is force_unlock of US-6, checked
		// on the issue's project (forceUnlockGate), not by the role.

		// sk-19: nextStatus + reason are required. Validate at the
		// MCP boundary so failures short-circuit before the visibility
		// check + the SERIALIZABLE tx the store layer would open.
		nextStatusRaw, err := req.RequireString("nextStatus")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"nextStatus is required — one of: in_progress, open, deferred",
				nil), nil
		}
		nextStatus := strings.TrimSpace(nextStatusRaw)
		switch nextStatus {
		case "in_progress", "open", "deferred":
		case "closed":
			return skmcp.ToolError(ErrCodeValidationError,
				"nextStatus=closed is rejected — use skein_close to close an issue",
				map[string]any{"nextStatus": nextStatus}), nil
		default:
			return skmcp.ToolError(ErrCodeValidationError,
				"nextStatus must be one of: in_progress, open, deferred",
				map[string]any{"nextStatus": nextStatus}), nil
		}

		reasonRaw, err := req.RequireString("reason")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError,
				"reason is required (non-empty after trim)", nil), nil
		}
		reason := strings.TrimSpace(reasonRaw)
		if reason == "" {
			return skmcp.ToolError(ErrCodeValidationError,
				"reason is required (non-empty after trim)", nil), nil
		}
		if vErr := issues.ValidateProse("reason", reason); vErr != nil {
			return validationToolError(vErr), nil
		}

		// Visibility check.
		issue, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_unclaim",
				"reason", "load issue failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}
		// sk-674.18 T066: releasing one's own claim moves the issue's
		// status — ActionWrite of US-6. force=true releases ANOTHER
		// holder's claim: that is force_unlock (T034, K-28) — the project
		// owner, the space owner, the instance admin.
		if force {
			if res := forceUnlockGate(ctx, pool, "skein_unclaim", "id", id, issue.ProjectID, userID); res != nil {
				return res, nil
			}
		} else if res := issueActionGate(ctx, pool, "skein_unclaim", "id", id, issue.ProjectID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}

		sessionID := auth.SessionIDFrom(ctx)
		actor := IssuesAgentActor(ctx, pool, userID)

		dto, err := issuesStore.Unclaim(ctx, issues.UnclaimInput{
			ID:         id,
			SessionID:  sessionID,
			Force:      force,
			NextStatus: nextStatus,
			Reason:     reason,
			Actor:      actor,
		})
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			if errors.Is(err, issues.ErrNotLockHolder) {
				return skmcp.ToolError(ErrCodeNotLockHolder,
					"caller session does not hold the lock on this issue",
					map[string]any{"id": id}), nil
			}
			if vErr, ok := issues.IsValidationError(err); ok {
				return skmcp.ToolError(ErrCodeValidationError, vErr.Message,
					map[string]any{"code": vErr.Code}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_unclaim",
				"reason", "store call failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "unclaim failed", nil), nil
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_unclaim",
			"userId", userID.String(), "issueId", id,
			"version", dto.Version,
			"latencyMs", time.Since(start).Milliseconds())
		return writeIssueAck(ctx, "skein_unclaim", userID.String(), id,
			dto, req.GetBool("verbose", false))
	}
}
