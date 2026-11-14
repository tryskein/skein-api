package publicapi

// comments.go — the comment operations of the public API (spec sk-878.3.1,
// "API surface", T026): thin adapters over the operations of
// internal/issueops that the MCP tools skein_comment, skein_comment_get,
// skein_comment_update and skein_comment_delete also call (INV-4).

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/errcode"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// The operationIds of the comment writes and of getComment.
const (
	OperationCreateComment = "createComment"
	OperationGetComment    = "getComment"
	OperationUpdateComment = "updateComment"
	OperationDeleteComment = "deleteComment"
)

// commentBody is the body of createComment and updateComment.
type commentBody struct {
	// Body, markdown, 1..100000 characters after trimming. Required.
	Body *string `json:"body,omitempty"`
	// Kind of the write: directed or autonomous; omitted — the author kind
	// of a new comment is api. An edit keeps the comment's author kind.
	Kind *string `json:"kind,omitempty"`
}

// deleteCommentBody is the optional body of deleteComment.
type deleteCommentBody struct {
	// Reason, free text recorded on the comment.deleted event.
	Reason *string `json:"reason,omitempty"`
	// Kind of the write: directed or autonomous.
	Kind *string `json:"kind,omitempty"`
}

// pathComment is the {comment} of the path; a value that is not a UUID names
// no comment (404 comment_not_found).
func pathComment(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(chi.URLParam(r, "comment"))
	if _, err := uuid.Parse(raw); err != nil {
		WriteProblem(w, Problem{Code: errcode.CommentNotFound, Origin: OriginPath,
			Detail: "comment does not exist or is not visible", Extensions: map[string]any{"commentId": raw}})
		return "", false
	}
	return raw, true
}

// writeCommentRefusal answers a refusal of an edit or a delete of the
// comment of the path: the comment missing or out of reach, which MCP
// answers issue_not_found with its commentId, is comment_not_found of the
// path — one code with a malformed id and with getComment (review R8-05).
func writeCommentRefusal(w http.ResponseWriter, f *issueops.Failure, id string) {
	if f.Code == errcode.IssueNotFound.String() && f.Fields["commentId"] != nil {
		WriteProblem(w, Problem{Code: errcode.CommentNotFound, Origin: OriginPath,
			Detail: "comment does not exist or is not visible", Extensions: map[string]any{"commentId": id}})
		return
	}
	writeFailure(w, f, id)
}

// writeComment answers a comment with its version as ETag.
func writeComment(w http.ResponseWriter, r *http.Request, status int, c *issues.CommentDTO) {
	SetETag(w, c.Version)
	WriteJSON(w, r, status, c)
}

// withCommentOutcome is ctx with the before-commit hook of AddComment that
// records the outcome of an Idempotency-Key request in the comment's own
// transaction.
func (a *issueWrites) withCommentOutcome(ctx context.Context, status int) context.Context {
	if a.outcomes == nil {
		return ctx
	}
	return issues.WithCommentBeforeCommit(ctx, issues.CommentBeforeCommitHook(a.outcomes.CommentBeforeCommit(status)))
}

// createComment adds a comment to an issue.
//
// @Summary Add a comment to an issue
// @Description Adds a comment to the issue of the path — the same operation as the MCP tool skein_comment: the owner of the key must be able to write in the issue's project; the comment's author is the owner. @username mentions of members of the project are resolved, the mentioned users are notified and start watching the issue.
// @Description
// @Description Without kind the comment's author kind is api; kind directed and autonomous give the agent's kinds, as in MCP. A comment is not guarded by the issue's lock. Accepts Idempotency-Key: a repeat within 24 h answers the first outcome and writes no second comment, event or notification. Side effects: comment.created; notifications of the issue's and the project's watchers and of the mentioned users. The Location header names the comment, ETag is its version.
// @Tags Comments
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param issue path string true "The issue's hierarchical id"
// @Param Idempotency-Key header string false "A key that makes a repeat of this request answer its first outcome (1..255 visible ASCII)"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body commentBody true "The comment"
// @Success 201 {object} issues.CommentDTO "The comment added"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous, a malformed Idempotency-Key (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner may not write in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such issue within reach (code issue_not_found)"
// @Failure 409 {object} ProblemDocument "The first request with this Idempotency-Key is still running (code idempotency_in_progress)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "The body is empty or invalid (code validation_error); a serialization race, nothing written (code serialization_retry, retryable); the Idempotency-Key reused with another request (code idempotency_key_reused)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/issues/{issue}/comments [post]
// @ID createComment
func (a *issueWrites) createComment(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathIssueArg(w, r)
	if !ok {
		return
	}
	var body commentBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	ctx := a.withCommentOutcome(r.Context(), http.StatusCreated)
	a.noteTarget(ctx, ObjectRef{Kind: ObjectIssue, ID: id})
	args["issueId"] = id
	c, f := issueops.CreateComment(ctx, a.ops, callerOf(r), args, issueops.OriginKey)
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	w.Header().Set("Location", PathPrefix+"comments/"+c.ID.String())
	writeComment(w, r, http.StatusCreated, c)
}

// hydrateComment is the Hydrate of createComment: the comment as it is now.
func (a *issueWrites) hydrateComment(ctx context.Context, _ *http.Request, objects []ObjectRef) (any, error) {
	for _, o := range objects {
		if o.Kind != ObjectComment {
			continue
		}
		id, err := uuid.Parse(o.ID)
		if err != nil {
			return nil, err
		}
		c, _, err := a.issues.GetComment(ctx, id)
		return c, err
	}
	return nil, errors.New("the outcome names no comment")
}

// getComment reads one comment.
//
// @Summary Get a comment
// @Description Returns one comment — the same operation as the MCP tool skein_comment_get: its body, author, author kind, version, mentions and edit and delete times. A deleted comment answers as its tombstone: deletedAt set, the body empty. A comment of a project out of reach answers as a missing one. ETag is the comment's version, the value updateComment takes in If-Match. Read-only.
// @Tags Comments
// @Produce json
// @Security SkeinApiKey[read]
// @Param comment path string true "The comment's UUID"
// @Param fields query string false "Comma-separated fields to return (plus id)"
// @Success 200 {object} issues.CommentDTO "The comment"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks the read right (code insufficient_scope)"
// @Failure 404 {object} ProblemDocument "No such comment within reach (code comment_not_found)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/comments/{comment} [get]
// @ID getComment
func (a *issueWrites) getComment(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathComment(w, r)
	if !ok {
		return
	}
	c, f := issueops.GetComment(r.Context(), a.ops, callerOf(r), issueops.Args{"commentId": id})
	if f != nil {
		writeFailure(w, f, id)
		return
	}
	writeComment(w, r, http.StatusOK, c)
}

// updateComment replaces the body of a comment.
//
// @Summary Edit a comment
// @Description Replaces the body of the comment of the path — the same operation as the MCP tool skein_comment_update. Only the author edits a comment; another user, an owner or an admin included, gets 403 forbidden. If-Match is required: the version of the comment (its ETag); a stale one answers 412 version_conflict with the current version and nothing is written. A deleted comment is not edited (422 validation_error, comment_deleted). The comment keeps its author kind; kind is optional and checked. A comment of a project out of reach answers as a missing one, before authorship is looked at.
// @Description
// @Description Side effects: comment.edited; mentions added by the edit are resolved and notified.
// @Tags Comments
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param comment path string true "The comment's UUID"
// @Param If-Match header string true "The version of the comment, as its ETag gives it"
// @Param Prefer header string false "return=minimal answers only id and version"
// @Param body body commentBody true "The new body"
// @Success 200 {object} issues.CommentDTO "The comment after the edit"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous, If-Match not one version (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner is not the author or may not write in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such comment within reach, or the id is not a UUID (code comment_not_found; MCP answers issue_not_found)"
// @Failure 412 {object} ProblemDocument "If-Match is stale (code version_conflict, currentVersion)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "The body is empty or invalid, or the comment is deleted (code validation_error); a serialization race, nothing written (code serialization_retry, retryable)"
// @Failure 428 {object} ProblemDocument "If-Match is missing (code precondition_required)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/comments/{comment} [patch]
// @ID updateComment
func (a *issueWrites) updateComment(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathComment(w, r)
	if !ok {
		return
	}
	var body commentBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	args["commentId"] = id
	expectedVersionArg(r, args)
	c, f := issueops.UpdateComment(r.Context(), a.ops, callerOf(r), args, issueops.OriginKey)
	if f != nil {
		writeCommentRefusal(w, f, id)
		return
	}
	writeComment(w, r, http.StatusOK, c)
}

// deleteComment deletes a comment.
//
// @Summary Delete a comment
// @Description Soft-deletes the comment of the path — the same operation as the MCP tool skein_comment_delete: the row stays in the thread as a tombstone with its body blanked on read. The author deletes his own comment with write; another's needs the owner's role delete_any_comment on the project (the project owner, the space owner, the instance admin), else 403 forbidden. Deleting a deleted comment succeeds again without a change. A comment of a project out of reach answers as a missing one, before authorship is looked at.
// @Description
// @Description Side effects: comment.deleted with the reason.
// @Tags Comments
// @Accept json
// @Produce json
// @Security SkeinApiKey[write]
// @Param comment path string true "The comment's UUID"
// @Param body body deleteCommentBody false "Options"
// @Success 204 "The comment is a tombstone"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks write (code insufficient_scope); the owner is not the author and lacks delete_any_comment, or may not write in the project (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such comment within reach, or the id is not a UUID (code comment_not_found; MCP answers issue_not_found)"
// @Failure 413 {object} ProblemDocument "Body over 1 MiB (code payload_too_large)"
// @Failure 415 {object} ProblemDocument "Body is not application/json (code unsupported_media_type)"
// @Failure 422 {object} ProblemDocument "A serialization race, nothing written (code serialization_retry, retryable)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/comments/{comment} [delete]
// @ID deleteComment
func (a *issueWrites) deleteComment(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id, ok := pathComment(w, r)
	if !ok {
		return
	}
	var body deleteCommentBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["commentId"] = id
	if _, f := issueops.DeleteComment(r.Context(), a.ops, callerOf(r), args); f != nil {
		writeCommentRefusal(w, f, id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// commentVersion is the version of the comment of the path within the
// caller's reach: found is false for a missing comment, one of a project out
// of reach and a path that is not a UUID.
func (v IssueVersions) commentVersion(ctx context.Context, r *http.Request, raw string) (int, bool, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return 0, false, nil
	}
	meta, err := v.Issues.GetCommentMeta(ctx, id)
	if errors.Is(err, issues.ErrCommentNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if _, err := projects.EnsureProjectVisible(ctx, v.Pool, meta.ProjectID, callerOf(r)); err != nil {
		if errors.Is(err, projects.ErrProjectNotVisible) {
			return 0, false, nil
		}
		return 0, false, err
	}
	c, _, err := v.Issues.GetComment(ctx, id)
	if errors.Is(err, issues.ErrCommentNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return c.Version, true, nil
}
