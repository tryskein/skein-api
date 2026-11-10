package issueops

// comments.go — the comment operations (sk-878.3.1 T026, the owner's
// decision 01a1c640): create, read, edit and delete a comment, moved out of
// the MCP tools skein_comment, skein_comment_get, skein_comment_update and
// skein_comment_delete with their checks and refusal codes unchanged.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// msgCommentNotVisible is the message of a comment that does not exist or
// is not visible — one answer for both (INV-3).
const msgCommentNotVisible = "comment does not exist or is not visible"

// commentAuthorKind is the author_kind of a new comment: on the MCP path a
// session caller writes as a human and an agent must name kind; on the
// public API no kind is api (issues.CommentAuthorKindForKeyWrite).
func commentAuthorKind(ctx context.Context, tool string, userID uuid.UUID, kind string, o Origin) (authorKind, authorType string, f *Failure) {
	if o == OriginKey {
		k, err := issues.CommentAuthorKindForKeyWrite(kind)
		if err != nil {
			return "", "", Fail(ErrCodeValidationError, "kind must be one of: directed, autonomous",
				map[string]any{"kind": kind})
		}
		return k, issues.CommentAuthorTypeAgent, nil
	}
	mode, ok := CallerOriginMode(ctx)
	if !ok {
		slog.ErrorContext(ctx, "callerOriginMode could not classify caller — neither api-key nor cookie signal present",
			"tool", tool, "userId", userID)
		return "", "", Fail(ErrCodeInternalError, "ambiguous caller mode; cannot determine comment kind", nil)
	}
	switch mode {
	case "human":
		// kind is silently ignored on the cookie path — the REST handler's
		// semantics.
		return issues.CommentAuthorKindHuman, issues.CommentAuthorTypeHuman, nil
	case "agent":
		switch kind {
		case "directed":
			return issues.CommentAuthorKindAgentDirected, issues.CommentAuthorTypeAgent, nil
		case "autonomous":
			return issues.CommentAuthorKindAgentAutonomous, issues.CommentAuthorTypeAgent, nil
		case "":
			return "", "", Fail(ErrCodeValidationError,
				`kind is required for agent callers; pass "directed" (user-requested) or "autonomous" (side-quest)`, nil)
		default:
			return "", "", Fail(ErrCodeValidationError, "kind must be one of: directed, autonomous",
				map[string]any{"kind": kind})
		}
	}
	slog.ErrorContext(ctx, "callerOriginMode returned unexpected mode",
		"tool", tool, "mode", mode, "userId", userID)
	return "", "", Fail(ErrCodeInternalError, "unknown caller mode; cannot determine comment kind",
		map[string]any{"mode": mode})
}

// commentBody is the trimmed, checked body argument.
func commentBody(a Args) (string, *Failure) {
	raw, err := a.RequireString("body")
	if err != nil {
		return "", Fail(ErrCodeValidationError, "body is required", nil)
	}
	body := strings.TrimSpace(raw)
	if body == "" {
		return "", Fail(ErrCodeValidationError, "body must be non-empty after trim", nil)
	}
	if vErr := issues.ValidateProse("body", body); vErr != nil {
		return "", ValidationFailure(vErr)
	}
	return body, nil
}

// CreateComment adds a comment to an issue (skein_comment; the public API's
// createComment). The issue must be one the caller may write in; the
// comment's author is the caller. With issues.WithCommentBeforeCommit on
// ctx the hook runs in the comment's own transaction.
func CreateComment(ctx context.Context, d Deps, userID uuid.UUID, a Args, o Origin) (*issues.CommentDTO, *Failure) {
	const tool = "skein_comment"
	start := time.Now()
	rawIssueID, err := a.RequireString("issueId")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "issueId is required", nil)
	}
	issueID := strings.TrimSpace(rawIssueID)
	if !LooksLikeHierarchicalID(issueID) {
		return nil, Fail(ErrCodeValidationError,
			"issueId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"issueId": rawIssueID})
	}
	body, f := commentBody(a)
	if f != nil {
		return nil, f
	}
	authorKind, authorType, f := commentAuthorKind(ctx, tool, userID, strings.TrimSpace(a.GetString("kind", "")), o)
	if f != nil {
		return nil, f
	}

	// Visibility: a non-member sees issue_not_found, as on REST.
	issue, err := d.Issues.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			return nil, Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible",
				map[string]any{"issueId": issueID})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", tool, "reason", "load issue failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to load issue", nil)
	}
	if f := IssueActionGate(ctx, d.Pool, tool, "issueId", issueID, issue.ProjectID, userID, authz.ActionWrite); f != nil {
		return nil, f
	}

	actor := IssuesAgentActor(ctx, d.Pool, userID)
	// comments.author is the actor's resolved username (never empty).
	dto, err := d.Issues.AddComment(ctx, issueID, body, userID, actor.Username, authorType, authorKind, actor)
	if err != nil {
		return nil, MapAddCommentError(ctx, err, userID.String(), issueID)
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", tool,
		"userId", userID.String(), "issueId", issueID,
		"commentId", dto.ID.String(),
		"latencyMs", time.Since(start).Milliseconds())
	return dto, nil
}

// MapAddCommentError maps a refusal of issues.Store.AddComment to its code.
//
// An exhausted serialization retry is serialization_retry: AddComment is an
// INSERT, nothing was written, so the call is repeated verbatim (sk-303.17).
func MapAddCommentError(ctx context.Context, err error, userID, issueID string) *Failure {
	if errors.Is(err, issues.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"the comment write lost a serialization race and made no change — retry the call",
			map[string]any{"issueId": issueID})
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		return Fail(ErrCodeValidationError, vErr.Message, map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, issues.ErrNotFound) {
		return Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible",
			map[string]any{"issueId": issueID})
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", "skein_comment",
		"reason", "add comment failed",
		"userId", userID, "issueId", issueID, "error", err.Error())
	return Fail(ErrCodeInternalError, "failed to add comment", nil)
}

// GetComment reads one comment, a deleted one as its tombstone
// (skein_comment_get; getComment). A comment that does not exist and one of
// a project out of reach are one answer, comment_not_found (INV-3); the
// project is checked before the body is read.
func GetComment(ctx context.Context, d Deps, userID uuid.UUID, a Args) (*issues.CommentDTO, *Failure) {
	const tool = "skein_comment_get"
	start := time.Now()
	raw, err := a.RequireString("commentId")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "commentId is required", nil)
	}
	commentID, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "commentId must be a valid uuid",
			map[string]any{"commentId": raw})
	}
	notVisible := Fail(ErrCodeCommentNotFound, msgCommentNotVisible, map[string]any{"commentId": commentID.String()})
	meta, err := d.Issues.GetCommentMeta(ctx, commentID)
	if err != nil {
		if errors.Is(err, issues.ErrCommentNotFound) {
			return nil, notVisible
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", tool, "reason", "get comment meta failed",
			"userId", userID.String(), "commentId", commentID.String(), "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to load comment", nil)
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, d.Pool, meta.ProjectID, userID); vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			return nil, notVisible
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", tool, "reason", "visibility check failed",
			"userId", userID.String(), "commentId", commentID.String(), "error", vErr.Error())
		return nil, Fail(ErrCodeInternalError, "visibility check failed", nil)
	}
	dto, _, err := d.Issues.GetComment(ctx, commentID)
	if err != nil {
		// Deleted between the meta and the full read: still not found.
		if errors.Is(err, issues.ErrCommentNotFound) {
			return nil, notVisible
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", tool, "reason", "get comment failed",
			"userId", userID.String(), "commentId", commentID.String(), "error", err.Error())
		return nil, Fail(ErrCodeInternalError, "failed to load comment", nil)
	}
	if dto.Mentions == nil {
		dto.Mentions = []issues.MentionRef{}
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", tool,
		"userId", userID.String(), "commentId", commentID.String(),
		"latencyMs", time.Since(start).Milliseconds())
	return dto, nil
}

// resolveCommentForWrite is the preamble of an edit or a delete: it parses
// commentId, reads who wrote the comment and where, and checks that the
// caller may write in its project — returning the caller's level there. A
// comment of a project out of reach answers as a missing one. Who may edit
// or delete is the caller's decision.
func resolveCommentForWrite(ctx context.Context, d Deps, tool string, userID uuid.UUID, rawCommentID string) (*issues.CommentMeta, projects.AccessLevel, *Failure) {
	commentID, err := uuid.Parse(strings.TrimSpace(rawCommentID))
	if err != nil {
		return nil, "", Fail(ErrCodeValidationError, "commentId must be a valid uuid",
			map[string]any{"commentId": rawCommentID})
	}
	notVisible := Fail(ErrCodeIssueNotFound, msgCommentNotVisible, map[string]any{"commentId": commentID.String()})
	meta, err := d.Issues.GetCommentMeta(ctx, commentID)
	if err != nil {
		if errors.Is(err, issues.ErrCommentNotFound) {
			return nil, "", notVisible
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", tool, "reason", "load comment meta failed",
			"userId", userID.String(), "commentId", commentID.String(), "error", err.Error())
		return nil, "", Fail(ErrCodeInternalError, "failed to load comment", nil)
	}
	// Editing or deleting a comment is ActionWrite of US-6 (sk-674.18 T066).
	level, vErr := projects.EnsureProjectAction(ctx, d.Pool, meta.ProjectID, userID, authz.ActionWrite)
	if vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			return nil, "", notVisible
		}
		if errors.Is(vErr, projects.ErrProjectActionForbidden) {
			return nil, "", Fail(ErrCodeForbidden, MsgActionForbidden, map[string]any{"commentId": commentID.String()})
		}
		slog.ErrorContext(ctx, logMessage(ctx, "error"),
			"tool", tool, "reason", "visibility check failed",
			"userId", userID.String(), "commentId", commentID.String(), "error", vErr.Error())
		return nil, "", Fail(ErrCodeInternalError, "visibility check failed", nil)
	}
	return meta, level, nil
}

// UpdateComment replaces the body of a comment (skein_comment_update;
// updateComment): only its author edits it, at the version the caller
// names (expectedVersion; a stale one is version_conflict and writes
// nothing). An edit keeps the comment's author_kind (INV-5); on the MCP path
// an agent still names kind, on the public API kind is optional.
func UpdateComment(ctx context.Context, d Deps, userID uuid.UUID, a Args, o Origin) (*issues.CommentDTO, *Failure) {
	const tool = "skein_comment_update"
	start := time.Now()
	rawCommentID, err := a.RequireString("commentId")
	if err != nil {
		return nil, Fail(ErrCodeValidationError, "commentId is required", nil)
	}
	body, f := commentBody(a)
	if f != nil {
		return nil, f
	}
	expectedVersion := a.GetInt("expectedVersion", 0)
	if expectedVersion < 1 {
		return nil, Fail(ErrCodeValidationError, "expectedVersion is required and must be >= 1", nil)
	}
	if f := editKind(ctx, tool, userID, strings.TrimSpace(a.GetString("kind", "")), o); f != nil {
		return nil, f
	}

	meta, _, f := resolveCommentForWrite(ctx, d, tool, userID, rawCommentID)
	if f != nil {
		return nil, f
	}
	if meta.AuthorUserID == nil || *meta.AuthorUserID != userID {
		return nil, Fail(ErrCodeForbidden, "only the comment author can edit this comment", nil)
	}

	actor := IssuesAgentActor(ctx, d.Pool, userID)
	dto, err := d.Issues.UpdateComment(ctx, meta.CommentID, body, expectedVersion, actor)
	if err != nil {
		return nil, MapCommentWriteError(ctx, err, tool, userID.String(), meta.CommentID.String())
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", tool,
		"userId", userID.String(), "commentId", meta.CommentID.String(),
		"version", dto.Version,
		"latencyMs", time.Since(start).Milliseconds())
	return dto, nil
}

// editKind checks the kind of an edit: an agent on the MCP path must name
// directed or autonomous; on the public API it is optional. It does not
// change the stored author_kind.
func editKind(ctx context.Context, tool string, userID uuid.UUID, kind string, o Origin) *Failure {
	if o == OriginKey {
		if _, err := issues.CommentAuthorKindForKeyWrite(kind); err != nil {
			return Fail(ErrCodeValidationError, "kind must be one of: directed, autonomous", map[string]any{"kind": kind})
		}
		return nil
	}
	mode, ok := CallerOriginMode(ctx)
	if !ok {
		slog.ErrorContext(ctx, "callerOriginMode could not classify caller",
			"tool", tool, "userId", userID)
		return Fail(ErrCodeInternalError, "ambiguous caller mode; cannot determine edit kind", nil)
	}
	if mode != "agent" {
		return nil
	}
	switch kind {
	case "directed", "autonomous":
		return nil
	case "":
		return Fail(ErrCodeValidationError, `kind is required for agent callers; pass "directed" or "autonomous"`, nil)
	}
	return Fail(ErrCodeValidationError, "kind must be one of: directed, autonomous", map[string]any{"kind": kind})
}

// CommentDeleted is the acknowledgement of a deleted comment.
type CommentDeleted struct {
	ID      uuid.UUID
	IssueID string
}

// DeleteComment soft-deletes a comment (skein_comment_delete;
// deleteComment): its author may, and so may whoever holds
// delete_any_comment on the project (the project owner, the space owner,
// the instance admin). Deleting a deleted comment is a no-op success; the
// row stays as a tombstone. A comment of a project out of reach answers as
// a missing one, before authorship is looked at.
func DeleteComment(ctx context.Context, d Deps, userID uuid.UUID, a Args) (CommentDeleted, *Failure) {
	const tool = "skein_comment_delete"
	start := time.Now()
	rawCommentID, err := a.RequireString("commentId")
	if err != nil {
		return CommentDeleted{}, Fail(ErrCodeValidationError, "commentId is required", nil)
	}
	reason := strings.TrimSpace(a.GetString("reason", ""))
	meta, level, f := resolveCommentForWrite(ctx, d, tool, userID, rawCommentID)
	if f != nil {
		return CommentDeleted{}, f
	}
	isAuthor := meta.AuthorUserID != nil && *meta.AuthorUserID == userID
	if !isAuthor && !authz.Allows(level, authz.ActionDeleteAnyComment) {
		return CommentDeleted{}, Fail(ErrCodeForbidden,
			"only the comment author, the project owner, the space owner or the instance admin can delete this comment", nil)
	}
	actor := IssuesAgentActor(ctx, d.Pool, userID)
	if err := d.Issues.DeleteComment(ctx, meta.CommentID, reason, actor); err != nil {
		return CommentDeleted{}, MapCommentWriteError(ctx, err, tool, userID.String(), meta.CommentID.String())
	}
	slog.InfoContext(ctx, logMessage(ctx, "ok"),
		"tool", tool,
		"userId", userID.String(), "commentId", meta.CommentID.String(),
		"latencyMs", time.Since(start).Milliseconds())
	return CommentDeleted{ID: meta.CommentID, IssueID: meta.IssueID}, nil
}

// MapCommentWriteError maps a refusal of issues.Store.UpdateComment or
// DeleteComment to its code: serialization_retry (checked first — nothing
// was written, the version did not move), version_conflict with the live
// version, a validator's refusal (comment_deleted among them), a missing
// comment or issue.
func MapCommentWriteError(ctx context.Context, err error, tool, userID, commentID string) *Failure {
	if errors.Is(err, issues.ErrSerializationRetry) {
		return Fail(ErrCodeSerializationRetry,
			"the comment write lost a serialization race and made no change — retry the call",
			map[string]any{"commentId": commentID})
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		return Fail(ErrCodeVersionConflict, "the comment was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
	}
	if vErr, ok := issues.IsValidationError(err); ok {
		return Fail(ErrCodeValidationError, vErr.Message, map[string]any{"code": vErr.Code})
	}
	if errors.Is(err, issues.ErrCommentNotFound) {
		return Fail(ErrCodeIssueNotFound, msgCommentNotVisible, map[string]any{"commentId": commentID})
	}
	if errors.Is(err, issues.ErrNotFound) {
		return Fail(ErrCodeIssueNotFound, "issue does not exist or is not visible", nil)
	}
	slog.ErrorContext(ctx, logMessage(ctx, "error"),
		"tool", tool, "reason", "comment write failed",
		"userId", userID, "commentId", commentID, "error", err.Error())
	return Fail(ErrCodeInternalError, "comment operation failed", nil)
}
