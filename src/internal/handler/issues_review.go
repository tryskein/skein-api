package handler

// issues_review.go — sk-303.14, task T007, contract C1.
//
// REST half of two operations of the author's private bookkeeping:
// toggling the "needs review" flag and the reviewed mark.
//
// # Split of duties with issues.go
//
// The flag arrives in the general patch body (PATCH), because the client
// has nowhere else to send it and no way to recognise a mixed request;
// but it is handled HERE and goes to issues.Store.SetReviewRequired — a
// method outside the general update. The mark is its own route: it shares
// not a single field with the patch.
//
// # There is no agent tool for the mark
//
// The reviewed mark is a human action of reading the result. An agent
// tool would give "marked" without a reading, and the unreviewed number
// would stop meaning what it was introduced for. The flag exists on the
// agent change path (skein_update); the mark does not.
//
// # Refusal codes
//
// Every mark refusal is 422 (a domain business rule): not the author,
// the issue is not closed, the flag is cleared. A mixed request is 400
// (a request-shape constraint); it lives in issues.go, next to body parse.

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// markReviewedRequest is the POST /issues/{issueID}/review body.
//
// Version is passed in the body, not as an If-Match header, for the same
// reason /accept does it that way: this is a pinpoint action from the
// card, not a resource patch. The field is optional — zero means
// "do not compare".
type markReviewedRequest struct {
	ExpectedVersion int `json:"expectedVersion"`
}

// setReviewRequired is the PATCH branch a request that carries ONLY the
// review flag goes down. Called from IssuesHandler.Patch after that
// handler has confirmed project visibility and that the issue belongs to
// the project.
//
// The flag is changed outside the general update: no version bump, no
// updated-at stamp, no fan-out (INV-2). The response is the card: the
// caller here is always the author (else 422), and the author can see
// the three mechanic fields.
func (h *IssuesHandler) setReviewRequired(
	w http.ResponseWriter, r *http.Request,
	issueID string, expectedVersion int, required bool, c callerCtx,
) {
	ctx := r.Context()
	dto, err := h.issuesStore.SetReviewRequired(ctx, issues.ReviewFlagInput{
		ID:              issueID,
		ExpectedVersion: expectedVersion,
		Required:        required,
		Actor:           issueActor(c),
	})
	if err != nil {
		if writeReviewProblem(w, err) {
			return
		}
		slog.ErrorContext(ctx, "set review flag failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// MarkReviewed handles POST /api/v1/projects/{projectID}/issues/{issueID}/review.
//
// Places a reviewed mark on a closed issue the caller created.
// The mark is stored with its author and timestamp, status is NOT
// changed and nothing is gated (INV-1), the issue's version and
// last-updated stamp do not move (INV-2), no notifications are produced
// — neither to the assignee nor to the author (INV-2, notification
// feed). A repeat is idempotent: there is no second event, and the
// author and timestamp of the first mark do not change.
//
// Responses:
//   - 200 — the issue card (the caller is the author, so the three
//     mechanic fields are on it).
//   - 400 — invalid project id / JSON.
//   - 401 — unauthenticated.
//   - 404 — issue not found, not visible, or living in another project.
//   - 422 review_forbidden        — the caller is not the author (INV-4;
//     an admin is refused identically).
//   - 422 review_issue_not_closed — the issue is not closed: there is
//     nothing to mark.
//   - 422 review_flag_cleared     — the flag is cleared: the mark is
//     meaningless.
//   - 422 version_conflict        — the presented version diverged from
//     the live one.
//   - 500 — store refusal.
//
// @Summary Mark a delegated closed issue as reviewed by its author
// @Description Records the author's private "I have read this result" mark on a CLOSED issue they created. Business meaning: the mark is the author's own bookkeeping over work they delegated — it removes the issue from their unreviewed set, from the second delegated counter and from the repeat-reminder selection, all three of which are derived from one predicate.
// @Description
// @Description Algorithm: authenticate → validate the path project id → confirm the project is visible to the caller → confirm the issue exists, is visible and belongs to THIS project (a foreign issue answers 404, never 403, so ids cannot be enumerated) → check the domain rules (caller is the author, the issue is closed, the review flag is set) → write the mark with its author and timestamp inside one transaction, together with the audit event.
// @Description
// @Description Side effects, stated as what does NOT happen, because that is the contract: NO notification of any kind is produced — not to the assignee, not to the author; the issue's activity feed does not show the mark; the project live feed does not carry it; and the issue's version and updated-at stamp DO NOT move, so a watcher with the card open cannot infer that anything happened. The mark is visible only to the author, on every read surface. Status is not changed and nothing is gated by the mark.
// @Description
// @Description Repeating the call is idempotent: the second call writes no event and does not overwrite the author or the timestamp of the first mark. Two concurrent calls settle on one mark.
// @Tags Issues
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param issueID path string true "Hierarchical issue id, e.g. sk-42.1"
// @Param body body markReviewedRequest false "Optional expected version; 0 or an absent body means «do not compare»"
// @Success 200 {object} issues.IssueDetail "The issue card — the caller is the author, so it carries the three review-mechanic fields"
// @Failure 400 {object} map[string]any "invalid_id — the path project id is not a UUID; missing_issue_id; invalid_json"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr)"
// @Failure 404 {object} map[string]any "issue_not_found — absent, invisible, or living in another project (deliberately indistinguishable)"
// @Failure 422 {object} map[string]any "review_forbidden — the caller is not the author (admins are refused identically); review_issue_not_closed; review_flag_cleared; version_conflict"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues/{issueID}/review [post]
func (h *IssuesHandler) MarkReviewed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	pid, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorisation before the write — the same contract as
	// the other routes under /projects/{projectID}/issues/{issueID}: the
	// path project has been checked, the path issue has not yet.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	var req markReviewedRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
			return
		}
	}

	dto, err := h.issuesStore.MarkReviewed(ctx, issues.ReviewMarkInput{
		ID:              issueID,
		ExpectedVersion: req.ExpectedVersion,
		Actor:           issueActor(c),
	})
	if err != nil {
		if writeReviewProblem(w, err) {
			return
		}
		slog.ErrorContext(ctx, "mark reviewed failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// writeReviewProblem translates both operations' refusals into HTTP.
// Returns true if a response was written.
//
// One translator for two operations — so that a "not the author" refusal
// is literally the same response on both: INV-4 requires the right to
// the flag and the right to the mark to coincide, and two independent
// branches would drift sooner or later.
func writeReviewProblem(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, issues.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
		return true
	case errors.Is(err, issues.ErrReviewNotAuthor):
		// 422, and the text does NOT distinguish "you are the assignee" /
		// "you are an administrator": INV-4 says an admin is refused the
		// same as any non-author, and different text itself would be an
		// oracle.
		writeProblem(w, http.StatusUnprocessableEntity, "review_forbidden",
			"only the issue author may change the review flag or mark it reviewed", nil)
		return true
	case errors.Is(err, issues.ErrReviewIssueNotClosed):
		writeProblem(w, http.StatusUnprocessableEntity, "review_issue_not_closed",
			"cannot mark an unclosed issue reviewed — there is no result yet", nil)
		return true
	case errors.Is(err, issues.ErrReviewFlagCleared):
		writeProblem(w, http.StatusUnprocessableEntity, "review_flag_cleared",
			"cannot mark reviewed while the review flag is cleared", nil)
		return true
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		writeProblem(w, http.StatusUnprocessableEntity, "version_conflict",
			"the resource was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
		return true
	}
	return false
}
