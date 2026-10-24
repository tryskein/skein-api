package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
)

// IssuesHandler bundles the dependencies for every /api/v1/projects/{pid}/issues/*
// route. Constructed once at startup; methods are goroutine-safe (the
// underlying Store is stateless beyond the pool).
type IssuesHandler struct {
	pool          *pgxpool.Pool
	issuesStore   *issues.Store
	projectsStore *projects.Store
	// depsStore drives POST /issues/check-duplicates (R1.4). Same store
	// instance is reused by DependenciesHandler / ReadyHandler — the
	// underlying pool is shared.
	depsStore *dependencies.Store
}

// NewIssuesHandler wires a fresh Store for issues and reuses the projects
// Store for project metadata lookups (id_prefix, default_id_mode). The
// depsStore is supplied by main() so the same instance powers
// /dependencies, /ready, and /check-duplicates.
func NewIssuesHandler(pool *pgxpool.Pool, projectsStore *projects.Store, depsStore *dependencies.Store) *IssuesHandler {
	return &IssuesHandler{
		pool:          pool,
		issuesStore:   issues.NewStore(pool),
		projectsStore: projectsStore,
		depsStore:     depsStore,
	}
}

// ----------------- helpers -----------------

// issueActor adapts the projects.Actor produced by caller() into the
// equivalent issues.Actor. Two separate types because the issues package
// must not import projects (would risk a cycle when projects later wants
// issue counts on its DTO).
func issueActor(c callerCtx) issues.Actor {
	return issues.Actor{
		UserID:   c.UserID,
		Username: c.Username,
		Role:     "human",
	}
}

// closeTargetCaller builds the access info the store needs to check that a
// close-target is visible (sk-489, US-2).
//
// The role carried here is the SYSTEM role (users.role), not issueActor's
// "human" — the visibility key has to be the one the manual dependency
// route uses, or an admin and a plain member would get the same answer on
// an invisible target. It is supplied UNCONDITIONALLY, even when no target
// was sent: the store only consults it when CloseTarget is non-empty, and
// a caller that passes it only "when needed" is one refactor away from the
// store's `DependenciesCaller is required` fault.
func closeTargetCaller(c callerCtx) *issues.DependenciesCaller {
	return &issues.DependenciesCaller{UserID: c.UserID, Role: c.Role}
}

// derefString unwraps an optional wire string into the store's plain-string
// field, mapping "absent" and "" onto the same empty value.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// mapIssueValidation surfaces typed issues.ValidationError as 400/422.
//
// Most validation errors are 400 (caller sent something syntactically
// wrong). The exceptions are business errors whose wire shape is valid and
// whose action is fine — we just can't fulfil them under the current state.
// Those are 422 "Unprocessable Entity" per the org-wide HTTP code guideline
// (the project guidelines §HTTP codes for business errors):
//
//   - blocked_is_derived (sk-31) — `blocked` is no longer a writable status.
//   - the sk-118 standing-status guards (cannot_claim_standing,
//     cannot_work_standing_directly, cannot_promote_locked_to_standing,
//     nested_standing_forbidden) — the request is well-formed; standing
//     lifecycle rules just forbid it. Keeps the REST boundary's status code
//     consistent with the closed-state business errors (e.g. version_conflict,
//     locked_by_other in mapIssueErr, both 422).
func mapIssueValidation(w http.ResponseWriter, err error) bool {
	if ve, ok := issues.IsValidationError(err); ok {
		status := http.StatusBadRequest
		if issueValidationIsBusinessError(ve.Code) {
			status = http.StatusUnprocessableEntity
		}
		writeProblem(w, status, ve.Code, ve.Message, nil)
		return true
	}
	if ve, ok := projects.IsValidationError(err); ok {
		writeProblem(w, http.StatusBadRequest, ve.Code, ve.Message, nil)
		return true
	}
	return false
}

// issueValidationIsBusinessError reports whether a typed issues.ValidationError
// code is a business error (well-formed request the current state forbids) that
// must surface as 422 rather than the default 400. The set is closed and small;
// adding a code here is a deliberate REST-contract decision.
func issueValidationIsBusinessError(code string) bool {
	switch code {
	case "blocked_is_derived",
		// sk-118 standing-status guards. cannot_claim_standing is reached if
		// claim is ever wired to a REST route; the other three already flow
		// through PATCH /issues (UpdateStatus) where this mapper runs.
		"cannot_claim_standing",
		"cannot_work_standing_directly",
		"cannot_defer_standing",
		"cannot_promote_locked_to_standing",
		"nested_standing_forbidden",
		// sk-118 (F2e) retire-block: closing a standing issue with non-closed
		// children. Well-formed request the current state forbids → 422.
		"standing_has_open_children",
		// sk-489 (US-12): editing the close outcome of an issue that is not
		// closed (issues.Store.PatchIssueWithResult). The request itself is
		// well-formed — the issue's CURRENT STATE forbids it — so 422.
		//
		// Its sibling refusal is deliberately NOT here:
		// `close_outcome_not_a_close` (issues.Store.UpdateStatus) fires when
		// an outcome/target is sent together with a transition to a
		// non-closed status. That combination is impossible in any state, so
		// it is a malformed request → the default 400. Two codes because the
		// REST contract needs two statuses; merging them would force one.
		"close_outcome_requires_closed",
		// sk-540.2: duplicate/superseded without closeTarget. Whether it is
		// allowed depends on the issue's state (already closed with the same
		// outcome, or with the matching edge), so it is a business refusal.
		"close_target_required":
		return true
	default:
		return false
	}
}

// ensureIssueInProject is the object-level authorization gate for every
// route shaped /projects/{projectID}/issues/{issueID}/... It returns true
// when the caller may proceed; when it returns false a response has already
// been written and the handler must return immediately.
//
// # The gate this closes (sk-194.6.50 / sk-194.6.51)
//
// projects.EnsureProjectVisible answers "may this caller see the project
// named in the path". It says nothing about whether the ISSUE named in the
// path lives in that project — issue ids are globally unique, so a caller
// who legitimately sees project A can put a project-B issue id in the URL
// and, with only the project-level check in place, have the write applied
// to B. Parent-level authorization passing is not object-level
// authorization passing; this is the missing half.
//
// # Why this is called inline in each handler and not wired as middleware
//
// A chi middleware would be the tempting "can't forget it" answer, and for
// THIS route shape it is a trap. chi fills RouteContext.URLParams during
// FindRoute, which runs after the mux's own Use-chain; a middleware sees
// only the params consumed by ANCESTOR muxes' trees. Our routes nest as
// Route("/{projectID}") > Route("/issues") > Route("/{issueID}"), so a
// middleware attached inside the /issues subtree gets a populated projectID
// and an EMPTY issueID — the worst possible failure, because it looks like
// it works and silently compares against "". (Measured on the chi this repo
// pins, v5.2.5: this nesting yields projectID="p1", issueID=""; moving the
// middleware under Route("/issues/{issueID}") yields both. The behaviour is
// by design — go-chi/chi#127, still open as #892 — and is not mentioned in
// chi's routing docs.) Only a middleware attached
// inside a Route whose own pattern carries {issueID} sees it, which makes
// the guarantee depend on how the next person registers a route rather than
// on the middleware itself. An inline call is locally verifiable: the
// arguments are right there.
//
// # Call it BEFORE the mutation, always
//
// Checking after the store call is not a fix: the write has already
// committed and the 404 only hides it from the response. The handlers that
// used to compare dto.ProjectID on the RESULT of a mutation were leaking
// the write itself, not just the answer.
//
// # Why 404 and not 403
//
// 403 would be an existence oracle: walking ids through their own project's
// URL, a caller could sort "real issue somewhere else in this installation"
// from "nothing", which is exactly the enumeration BOLA exploitation starts
// with. 404 discloses neither. It is also the honest answer about the
// resource actually addressed — project A's issue X — which does not exist.
//
// The normative permission for this is RFC 9110 §15.5.4: "An origin server
// that wishes to 'hide' the current existence of a forbidden target resource
// MAY instead respond with a status code of 404 (Not Found)", reinforced by
// §15.5.5, where 404 covers a server "not willing to disclose that one
// exists". Note it is MAY, not MUST — and contrary to a widespread belief,
// OWASP does NOT mandate 404 here: API1:2023, ASVS 5.0 V8 and the IDOR
// Prevention Cheat Sheet name no status code at all, and the REST Security
// Cheat Sheet lists 403 for authenticated-but-unpermitted. The one OWASP
// text that touches the choice, the Authorization Regression Testing Cheat
// Sheet, accepts either: "The system must return a 403 Forbidden or 404 Not
// Found (to avoid information leakage about resource existence), never a
// 200 OK." So this is a deliberate project-level decision, taken for the
// disclosure reason above, and it is consistent with the anti-leak
// convention already in IssuesHandler.Get, the comment handlers and
// RemindersHandler.Create — same status, same code, same body as a
// genuinely nonexistent id.
func ensureIssueInProject(
	ctx context.Context,
	w http.ResponseWriter,
	store *issues.Store,
	issueID string,
	projectID uuid.UUID,
) bool {
	err := store.IssueBelongsToProject(ctx, issueID, projectID)
	if err == nil {
		return true
	}
	if errors.Is(err, issues.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
		return false
	}
	slog.ErrorContext(ctx, "issue/project membership check failed",
		"issueId", issueID, "projectId", projectID.String(), "error", err.Error())
	writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
	return false
}

// mapIssueErr translates store errors into the right HTTP response.
// Returns true if a response was written.
func mapIssueErr(w http.ResponseWriter, err error) bool {
	if errors.Is(err, issues.ErrAssigneeNotAllowed) {
		// sk-674.18 T028, INV-30: a choice by id that may not work in the
		// project (E-32); nothing was written.
		writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeAssigneeNotAllowed,
			"the assignee specified by id is not allowed to work in this project", nil)
		return true
	}
	if errors.Is(err, issues.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
		return true
	}
	if errors.Is(err, issues.ErrIDTaken) {
		writeProblem(w, http.StatusUnprocessableEntity, "id_taken",
			"an issue with this id already exists", nil)
		return true
	}
	if errors.Is(err, issues.ErrPinnedNoForce) {
		writeProblem(w, http.StatusUnprocessableEntity, "pinned_no_force",
			"pinned issues require ?force=true to close", nil)
		return true
	}
	if errors.Is(err, issues.ErrHashCollisionExhausted) {
		writeProblem(w, http.StatusInternalServerError, "id_generation_failed",
			"could not allocate a unique hash id; try again", nil)
		return true
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		// version_conflict is a routine concurrent-write outcome, not a server
		// fault — 422 per the project guidelines (4xx/5xx alert, 422 = normal).
		// Uniform across every issue + comment path that maps through here.
		writeProblem(w, http.StatusUnprocessableEntity, "version_conflict",
			"the resource was modified by another writer",
			map[string]any{"currentVersion": verr.CurrentVersion})
		return true
	}
	// sk-303.17 review: AddComment / UpdateComment / DeleteComment
	// exhausting their SERIALIZABLE retry budget against a genuine
	// class-40 abort used to fall through every branch here into a bare
	// 500 internal_error — the same defect shape the retry was added to
	// close, just pushed to the tail of the budget. issues.Store now
	// degrades that exhaustion to issues.ErrSerializationRetry (nothing
	// was written — not a version conflict, which would wrongly tell the
	// client to re-read a version that never moved). Same code + status
	// dependencies.ErrSerializationRetry already gets at the REST layer
	// (internal/handler/dependencies.go) — one "your write raced and
	// lost, nothing happened, retry" contract across the API.
	if errors.Is(err, issues.ErrSerializationRetry) {
		writeProblem(w, http.StatusUnprocessableEntity, "serialization_retry",
			"the write lost a serialization race and made no change — retry the request", nil)
		return true
	}
	var perr *issues.ErrParentNotEligible
	if errors.As(err, &perr) {
		writeProblem(w, http.StatusUnprocessableEntity, perr.Code, perr.Message, nil)
		return true
	}
	var cerr *issues.ErrComponentsWrongProject
	if errors.As(err, &cerr) {
		writeProblem(w, http.StatusUnprocessableEntity, "components_wrong_project",
			"some components do not belong to this project",
			map[string]any{"componentIds": cerr.ComponentIDs})
		return true
	}
	// R2.3: a writer that doesn't hold the lock on a locked issue (and isn't
	// using force=true) gets 422 with a stable code. Code mirrors the MCP
	// constant `locked_by_other` so REST and MCP agents see the same error
	// code on the same condition.
	var lerr *issues.ErrLockedByOther
	if errors.As(err, &lerr) {
		writeProblem(w, http.StatusUnprocessableEntity, "locked_by_other",
			"issue is locked by another session",
			map[string]any{"lockedBy": lerr.LockedBy})
		return true
	}
	// sk-489 (US-2) — the close-target link. Closing or editing an issue with
	// `closeTarget` creates the same `duplicates` / `supersedes` edge the
	// manual dependency route creates, in the same transaction, and can fail
	// the same two ways: the target does not exist, or it lives in a project
	// this caller cannot see.
	//
	// The answer is deliberately the one mapDepErr already gives — 422
	// `target_not_found` for BOTH — and it is produced by the same writer, so
	// the close path can neither harden nor soften the refusal contract of
	// its own surface. Splitting the two here would rebuild, on a new route,
	// exactly the existence oracle dependencies.go merged them to close
	// (CWE-203/204): a caller could point closeTarget at guessed ids and sort
	// real issues in invisible projects from imaginary ones.
	//
	// The MCP surface answers the pair with one code too, issue_not_found
	// (sk-674.18 T033, INV-3) — see internal/mcp/tools/close_errors.go.
	if isDepTargetErr(err) {
		writeTargetNotFound(w)
		return true
	}
	return false
}

// parseIfMatch parses the `If-Match: <int>` header into an int. Returns
// 0 + false when the header is absent, distinguishing it from a malformed
// value (which returns -1, false). Handlers map "absent" to 428 and
// "malformed" to 400.
func parseIfMatch(r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, false
	}
	// Allow optional weak-validator prefix or quotes — strip them so a
	// curl/web client that adds them gets accepted. We don't enforce
	// strong/weak distinction; version is monotonically increasing.
	raw = strings.TrimPrefix(raw, "W/")
	raw = strings.Trim(raw, `"`)
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return -1, true
	}
	return n, true
}

// requireIfMatch reads If-Match or returns the appropriate error response.
// On success returns the parsed version and true. On failure writes a 4xx
// and returns 0, false — caller short-circuits.
func requireIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	v, present := parseIfMatch(r)
	if !present {
		writeProblem(w, http.StatusPreconditionRequired, "missing_if_match",
			"If-Match header with the current version is required", nil)
		return 0, false
	}
	if v <= 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_if_match",
			"If-Match must be a positive integer", nil)
		return 0, false
	}
	return v, true
}

// ----------------- DTO request shapes -----------------

type createIssueRequest struct {
	ID                 string     `json:"id"`
	ParentID           *string    `json:"parentId"`
	Title              string     `json:"title"`
	Description        string     `json:"description"`
	Type               string     `json:"type"`
	Priority           *int       `json:"priority"`
	Source             string     `json:"source"`
	SourceContext      *string    `json:"sourceContext"`
	Assignee           *string    `json:"assignee"`
	Design             string     `json:"design"`
	AcceptanceCriteria string     `json:"acceptanceCriteria"`
	Notes              string     `json:"notes"`
	EstimatedMinutes   *int       `json:"estimatedMinutes"`
	DueAt              *time.Time `json:"dueAt"`
	DeferUntil         *time.Time `json:"deferUntil"`
	ExternalRef        *string    `json:"externalRef"`
	ComponentIDs       []string   `json:"componentIds"`
	Labels             []string   `json:"labels"`
}

type patchIssueRequest struct {
	Title              *string    `json:"title"`
	Description        *string    `json:"description"`
	Type               *string    `json:"type"`
	Priority           *int       `json:"priority"`
	Assignee           *string    `json:"assignee"`
	Design             *string    `json:"design"`
	AcceptanceCriteria *string    `json:"acceptanceCriteria"`
	Notes              *string    `json:"notes"`
	EstimatedMinutes   *int       `json:"estimatedMinutes"`
	DueAt              *time.Time `json:"dueAt"`
	DeferUntil         *time.Time `json:"deferUntil"`
	ExternalRef        *string    `json:"externalRef"`
	SourceContext      *string    `json:"sourceContext"`

	// ReviewRequired — sk-303.14 (contract C1), the "needs review" flag.
	// It IS on the patch input struct — otherwise the client has nowhere
	// to send it and no way to recognise a mixed request — but it does
	// NOT go through the general patch path: see reviewOnlyPatch and
	// issues/review_ops.go. A request that mixes it with any other field
	// of this struct is rejected with 400.
	ReviewRequired *bool `json:"reviewRequired"`

	// CloseOutcome — edit of the outcome of an ALREADY CLOSED issue (sk-489, US-3,
	// US-12): one of four values. For an issue that is not closed the edit
	// is rejected with 422 close_outcome_requires_closed — it has no outcome
	// and cannot have one (INV-1).
	CloseOutcome *string `json:"closeOutcome"`
	// CloseTarget — target of the "duplicate" / "superseded" link (US-2). Allowed
	// only together with an EXPLICIT closeOutcome duplicate / superseded; the link
	// is created in the same transaction as the edit.
	//
	// The type is a POINTER, and this is not a style choice: reviewOnlyPatch classifies
	// the edit by reflection and calls Value.IsNil on EVERY field of this
	// struct. A plain string would make it panic on the very first
	// PATCH request — see the invariant in the doc block of reviewOnlyPatch.
	CloseTarget *string `json:"closeTarget"`
}

// reviewOnlyPatch classifies a patch by one question: does it carry the
// review flag, and does it carry anything else.
//
// The check is done by reflecting over the struct itself, not by matching
// a hand-written field list. A list stays silent on a miss: a field added
// to the struct tomorrow and forgotten in the list would ride along with
// the flag, and a mixed request would execute halfway — exactly the
// outcome the 400 exists to forbid. Reflection fails safe: a new field
// is automatically treated as "ordinary".
//
// All fields of the struct are pointers, so "passed" and "not passed"
// are distinguishable and the nil check is exhaustive. This is an INVARIANT, not an observation:
// reflect.Value.IsNil panics on a non-pointer, so a field of a plain type
// would break EVERY PATCH request. A non-pointer field therefore turns every
// PATCH handler test red with a panic.
func reviewOnlyPatch(req patchIssueRequest) (hasFlag, hasOther bool) {
	v := reflect.ValueOf(req)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if v.Field(i).IsNil() {
			continue
		}
		if t.Field(i).Name == "ReviewRequired" {
			hasFlag = true
			continue
		}
		hasOther = true
	}
	return hasFlag, hasOther
}

type updateStatusRequest struct {
	Status string  `json:"status"`
	Reason *string `json:"reason"`

	// CloseOutcome — the EXPLICITLY passed close outcome (sk-489, US-1). nil
	// means "the caller said nothing about the outcome": a transition to "closed"
	// gets "done", a repeated close keeps the previous outcome
	// (INV-10). It is a pointer precisely for this distinction: "not passed" and
	// "passed an empty string" are different intents, and the second must be
	// a refusal.
	//
	// An outcome (or target) together with a status other than "closed" is
	// a 400 close_outcome_not_a_close refusal: the combination of request
	// fields is impossible itself, not the state of the issue.
	CloseOutcome *string `json:"closeOutcome"`
	// CloseTarget — target of the "duplicate" / "superseded" link (US-2). An empty string
	// and a missing key both mean "no target": there is no difference between them for a target,
	// because an empty target is its absence, not a second
	// intent (unlike the outcome above).
	CloseTarget string `json:"closeTarget"`
}

type addCommentRequest struct {
	Body string `json:"body"`
	// Kind (sk-16) — required for api-key (agent) callers, one of
	// "directed" or "autonomous". Cookie-session callers MUST omit this;
	// it is silently ignored on that path (the kind is always
	// human-driven). The wire token is the short form to match
	// skein_create's `kind` parameter — the store enum value
	// agent_directed / agent_autonomous is derived in the handler.
	Kind string `json:"kind,omitempty"`
}

// updateCommentRequest is the body for
// PUT /api/v1/projects/{projectID}/issues/{issueID}/comments/{commentID}.
//
// Body is the replacement comment text (required, non-empty after trim).
// ExpectedVersion is the optimistic-concurrency token — must equal the
// comment's live version or the edit returns 422 version_conflict.
// Kind (sk-16) mirrors addCommentRequest: required for api-key (agent)
// callers, silently ignored on the cookie path. An edit never rewrites
// authorship, so Kind only affects the comment.mention emit path for
// newly-introduced @mentions (it does not change author_kind).
type updateCommentRequest struct {
	Body            string `json:"body"`
	ExpectedVersion int    `json:"expectedVersion"`
	Kind            string `json:"kind,omitempty"`
}

// deleteCommentRequest is the body for
// DELETE /api/v1/projects/{projectID}/issues/{issueID}/comments/{commentID}.
// Reason is optional free-form audit text that lands on the
// comment.deleted event.
type deleteCommentRequest struct {
	Reason string `json:"reason,omitempty"`
}

type addLabelRequest struct {
	Label string `json:"label"`
}

type replaceComponentsRequest struct {
	ComponentIDs []string `json:"componentIds"`
}

// acceptIssueRequest is the body for POST /api/v1/issues/{issueID}/accept.
// Both fields are optional: an empty body is the common case (UI button
// click) — the assignee just clicks "Accept" without rebasing against
// the latest version. Agents that care about optimistic concurrency
// pass expectedVersion explicitly.
type acceptIssueRequest struct {
	ExpectedVersion *int `json:"expectedVersion,omitempty"`
}

// ----------------- Endpoints -----------------

// Create handles POST /api/v1/projects/{projectID}/issues.
//
// Wire shape: JSON body parsed into createIssueRequest. The handler:
//  1. Authenticates the caller via the cookie/BFF middleware (cookie-only
//     entry point — agents go through MCP).
//  2. Verifies project visibility.
//  3. Validates title / type / priority / labels / componentIds.
//  4. Forces source="human" regardless of req.Source. Phase 1 ui-redesign
//     (Q-8 = a): the field is preserved on the wire shape for backwards
//     compatibility with older clients but its value is intentionally
//     ignored — REST creates are always human-origin. Agent-origin
//     classifications (agent_directed, agent_autonomous) come exclusively
//     from the MCP path's `kind` parameter on skein_create.
//  5. Auto-accepts when assignee resolves to creator (case-insensitive
//     username match), seeding accepted_at / accepted_by inside the same
//     SERIALIZABLE transaction as the INSERT.
//  6. Hands off to issuesStore.CreateIssue and returns the freshly-loaded
//     IssueDetail with HTTP 201.
//
// Side effects (in the create transaction):
//   - issue.created event written to events
//   - creator + resolved-assignee added to issue_watchers
//   - components junction populated, labels deduplicated
//   - parent-child dep edge written to the dependencies graph
//     (sk-35 restored this as canonical; the issues.parent_id column
//     is frozen)
//   - @username mentions in description/notes/acceptance/design
//     parsed, resolved, fanned-out to project members
//
// @Summary Create an issue in a project
// @Description Creates one issue as the authenticated human. Business meaning: this is the cookie/BFF entry point for people; agents create through MCP, and the two paths are deliberately distinguishable — this one FORCES source="human" and ignores any source the body carries.
// @Description
// @Description Algorithm: authenticate -> verify project visibility -> validate title / type / priority / labels / componentIds -> force source="human" -> auto-accept when the assignee resolves to the creator (accepted_at / accepted_by are seeded inside the same SERIALIZABLE transaction as the INSERT) -> insert and return the freshly loaded card with 201.
// @Description
// @Description The review-required flag (sk-303.14): `reviewRequired` is OPTIONAL and, when omitted, takes the creator's PERSONAL DEFAULT from their review-mechanic settings — whose built-in default is "required", not "not required". Passing it explicitly wins over the personal default. The flag decides whether the closed result of this issue will later enter the author's unreviewed set; it is part of the author's private bookkeeping and is returned only to the author.
// @Description
// @Description Side effects, all inside the create transaction: an issue.created event; creator and resolved assignee added to the watchers; the components junction populated and labels deduplicated; a parent-child dependency edge written to the graph when parentId is given (the issues.parent_id column is frozen); @username mentions in description / notes / acceptanceCriteria / design parsed, resolved and fanned out to project members.
// @Tags Issues
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param body body createIssueRequest true "Issue to create"
// @Success 201 {object} issues.IssueDetail "The created issue; the three review-mechanic fields are present because the caller is its author"
// @Failure 400 {object} map[string]any "invalid_id; invalid_json; validation_error — title, type, priority, labels or componentIds refused"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but not writable by the caller"
// @Failure 404 {object} map[string]any "project_not_found — absent or invisible; parent issue absent or invisible"
// @Failure 422 {object} map[string]any "Domain refusals — e.g. an unusable parent or a component from another project; assignee_not_allowed — the assignee given as a user id names nobody who may work in this project now (INV-30; a username or e-mail is stored as text and never refused)"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues [post]
func (h *IssuesHandler) Create(w http.ResponseWriter, r *http.Request) {
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
	// sk-674.18 T066: creating an issue is ActionWrite of US-6; naming an
	// assignee is ActionAssign too (checked on the same level below).
	level, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	// Load project metadata for id-mode + prefix. We cannot rely on cached
	// values because the caller could have just patched defaultIdMode.
	project, err := h.projectsStore.GetProject(ctx, pid)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "project_not_found", "project not found", nil)
		return
	}

	var req createIssueRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if req.Assignee != nil && mapAccessErr(w, projects.RequireAction(level, authz.ActionAssign)) {
		return
	}

	title, err := issues.ValidateTitle(req.Title)
	if err != nil {
		mapIssueValidation(w, err)
		return
	}
	itype := req.Type
	if itype == "" {
		itype = issues.TypeTask
	}
	if _, err := issues.ValidateType(itype); err != nil {
		mapIssueValidation(w, err)
		return
	}
	priority := 2
	if req.Priority != nil {
		priority = *req.Priority
	}
	if err := issues.ValidatePriority(priority); err != nil {
		mapIssueValidation(w, err)
		return
	}
	// Phase 1 ui-redesign: source is server-determined for cookie-auth
	// callers — REST creates always come from a human via the BFF
	// (Web UI). req.Source is preserved on the wire shape for
	// backwards compatibility (Q-8 = a) but its value is intentionally
	// ignored. The MCP path is the only writer that lands
	// agent_directed / agent_autonomous, and it does so via the
	// `kind` parameter — never via a body-supplied `source`.
	source := issues.SourceHuman

	// Component IDs as uuids.
	componentIDs := make([]uuid.UUID, 0, len(req.ComponentIDs))
	for _, raw := range req.ComponentIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_component_id",
				"componentIds["+raw+"] is not a UUID", nil)
			return
		}
		componentIDs = append(componentIDs, id)
	}

	labels := make([]string, 0, len(req.Labels))
	for _, l := range req.Labels {
		canon, err := issues.ValidateLabel(l)
		if err != nil {
			mapIssueValidation(w, err)
			return
		}
		labels = append(labels, canon)
	}

	input := issues.CreateIssueInput{
		ID:                 req.ID,
		ParentID:           req.ParentID,
		Title:              title,
		Description:        req.Description,
		Type:               itype,
		Priority:           priority,
		Source:             source,
		SourceContext:      req.SourceContext,
		Assignee:           req.Assignee,
		Design:             req.Design,
		AcceptanceCriteria: req.AcceptanceCriteria,
		Notes:              req.Notes,
		EstimatedMinutes:   req.EstimatedMinutes,
		DueAt:              req.DueAt,
		DeferUntil:         req.DeferUntil,
		ExternalRef:        req.ExternalRef,
		ComponentIDs:       componentIDs,
		// ComponentIDsExplicit: caller supplied the JSON key.
		// `componentIds: []` is "explicit none" (suppresses parent
		// inheritance), `componentIds` absent is "fall through to
		// inheritance". encoding/json gives us nil for both an absent
		// key and `null`, so we infer presence from the parsed slice
		// being non-nil after decode.
		ComponentIDsExplicit: req.ComponentIDs != nil,
		Labels:               labels,
	}
	// sk-35: cross-project parents need the caller's (uuid, role) to
	// run the visibility check inside the SERIALIZABLE tx. The REST
	// path doesn't accept inline deps, so this is the sole trigger.
	if req.ParentID != nil && *req.ParentID != "" {
		input.DependenciesCaller = &issues.DependenciesCaller{
			UserID: c.UserID,
			Role:   c.Role,
		}
	}

	dto, err := h.issuesStore.CreateIssue(ctx, pid, project.IDPrefix, project.DefaultIDMode,
		input, c.UserID, issueActor(c))
	if err != nil {
		if mapIssueValidation(w, err) {
			return
		}
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "create issue failed",
			"userId", c.UserID.String(), "projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "issue created",
		"userId", c.UserID.String(), "projectId", pid.String(), "issueId", dto.ID)
	writeJSON(w, http.StatusCreated, dto)
}

// List handles GET /api/v1/projects/{projectID}/issues.
//
// Unreviewed filter (sk-303.14, contract C3) — `unreviewed=true`. This is
// the PROJECT path of the delegated surface; the paired cross-project one
// lives on /me/created-by (issues_me.go). Membership is defined by
// review.UnreviewedPredicate, the single definition of the set (INV-3).
// This path has no "no closed" default, so there is nothing to lift here.
// The value is only the literal "true".
//
// @Summary List the issues of one project
// @Description Returns a keyset-paginated page of the project's issues. Business meaning: the single server-side selection behind every project-scoped board, lens and filter; the client is never expected to filter a wider answer down, and the contract forbids it.
// @Description
// @Description Algorithm: authenticate -> verify project visibility -> translate the query string into one selection (every unrecognised value is REFUSED with 400 rather than ignored, so a typo cannot silently widen the answer) -> run one query with the caller's visibility horizon applied -> return the page plus the cursor for the next one.
// @Description
// @Description The unreviewed selection (sk-303.14): `unreviewed=true` narrows the answer to the caller's unreviewed set — closed issues the caller created, delegated to somebody else, with the review flag on, no review mark, not explicitly dismissed, in a non-archived visible project, with the workflow enabled for the caller. Membership comes from ONE definition shared with the second delegated counter and with the reminder selection, so the three cannot disagree. It is a separate key rather than a value of `status` on purpose: the set spans closed issues by construction, and it does NOT inherit the wider «delegated» reach — issues where the caller is merely a previous assignee are excluded, because the caller may not mark those. Only the literal "true" is accepted; this path has no «closed are hidden» default, so there is nothing to switch off.
// @Description
// @Description Close outcome (sk-489): a row whose issue is CLOSED carries `closeOutcome` — one of done, wont_do, duplicate, superseded — the machine-readable answer to «how did it end» that the free-text close reason never gave. On an issue that is not closed the KEY IS ABSENT rather than empty: an outcome without a close is a state the model does not have. Rows of epic-typed issues also carry `epicProgress`, which since sk-489 has three further legs — `done`, `noWork`, `counted` — beside the unchanged closed/total/unavailable; progress is done/counted, and closed/total keep their old meaning for clients that already read them.
// @Description
// @Description Row freshness order (sk-622.21, US-15): every row carries `rowSeq` — the issue's freshness order, taken from a global sequence on every committed write of the issue row, including writes that do not move `version` (label add/remove, component replace). For ONE issue a greater rowSeq is a later committed state; values of different issues are not comparable. A phase-2 client substitutes an event's row into a loaded row only if the event row's rowSeq is greater than the one shown. An issue not written since the phase-2 rollout (migration 000064) has no rowSeq key (absent ≡ 0, order unknown); any event row with a positive rowSeq is fresher than it.
// @Description
// @Description Side effects: none — the call is a pure read and writes no events.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param status query string false "Comma-separated statuses; every element must be a known status"
// @Param assignee query string false "me resolves to the caller; any other value is a username equality filter"
// @Param createdBy query string false "me or not-me; anything else is refused" Enums(me, not-me)
// @Param unreviewed query string false "Only true: narrow to the caller's unreviewed set (sk-303.14)" Enums(true)
// @Param acceptedAt query string false "null (not yet accepted) or not-null (accepted)" Enums(null, not-null)
// @Param origin query string false "Origin group of the issue's source"
// @Param priority query int false "Exact priority"
// @Param parentId query string false "Hierarchical id of the parent issue"
// @Param componentId query string false "Component UUID"
// @Param pinned query bool false "Pinned only / unpinned only"
// @Param isBlocked query bool false "Blocked only / unblocked only. Blocked = an open, in-progress or deferred blocks / waits-for target in a project the caller can see; blockers outside the caller's horizon never count (sk-423) — the same rule as item.isBlocked"
// @Param text query string false "Free-text match over the issue's texts"
// @Param source query string false "Canonical source classification"
// @Param labels query string false "Comma-separated labels; all must be present"
// @Param sort query string false "Sort key; an unknown key is refused"
// @Param cursor query string false "Keyset cursor from the previous page"
// @Param limit query int false "Page size, 1..500"
// @Success 200 {object} issues.ListResult "One page plus the next cursor"
// @Failure 400 {object} map[string]any "invalid_id; validation_error; invalid_priority; invalid_component_id; invalid_pinned; invalid_isBlocked; invalid_sort; invalid_cursor; invalid_limit"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr)"
// @Failure 404 {object} map[string]any "project_not_found — absent or invisible"
// @Failure 422 {object} map[string]any "blocked_is_derived — status=blocked is a DERIVED state and cannot be selected as a status; use isBlocked=true. Raised by issues.ValidateStatus through mapIssueValidation"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues [get]
func (h *IssuesHandler) List(w http.ResponseWriter, r *http.Request) {
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
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	q := r.URL.Query()
	args := issues.ListIssuesArgs{
		ProjectID: pid,
		Caller:    &c.UserID,
		// sk-34: CallerUsername drives the extended Delegated predicate
		// when createdBy=me is set below. Empty username degrades the
		// store to the strict pre-sk-34 predicate (no widening to
		// previous_assignees).
		CallerUsername: c.Username,
	}

	if raw := q.Get("status"); raw != "" {
		statuses := strings.Split(raw, ",")
		validated := make([]string, 0, len(statuses))
		for _, s := range statuses {
			canon, err := issues.ValidateStatus(strings.TrimSpace(s))
			if err != nil {
				mapIssueValidation(w, err)
				return
			}
			validated = append(validated, canon)
		}
		args.Statuses = validated
	}
	if raw := q.Get("assignee"); raw != "" {
		// "me" resolves to the caller's user id (matched via LEFT JOIN
		// to users with LOWER(username) — same path as /me/assigned-to).
		// Any other value passes through as a free-form username equality
		// filter for backwards compatibility (Phase 2 ui-redesign — B5).
		if raw == "me" {
			id := c.UserID
			args.AssigneeUserID = &id
		} else {
			v := raw
			args.Assignee = &v
		}
	}
	// Phase 2 ui-redesign — B5/sk-30: createdBy=me filter for the Outbox
	// view, createdBy=not-me filter for the "On me" lens. Any other value
	// rejected — accepting raw UUIDs here would bypass our visibility
	// model (caller could enumerate someone else's issues across projects
	// they share).
	if raw := q.Get("createdBy"); raw != "" {
		id := c.UserID
		switch raw {
		case "me":
			args.CreatedBy = &id
		case "not-me":
			args.CreatedByExclude = &id
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"createdBy must be \"me\" or \"not-me\"", nil)
			return
		}
	}
	// sk-303.14 T012 — unreviewed filter (contract C3), the project path
	// of the delegated surface.
	//
	// The same parameter and the same strictness as on the cross-project
	// path (issues_me.go): a separate key, not a value in the closed
	// `status` set (FOR-PLAN 5), and only the literal "true".
	//
	// This path has no "no closed" default — empty `status` here means
	// "any status" — so there is nothing to lift; the lift is recorded
	// on the path that actually applies the default.
	if raw := q.Get("unreviewed"); raw != "" {
		if raw != "true" {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"unreviewed accepts only the literal value \"true\"", nil)
			return
		}
		args.Unreviewed = true
	}
	// Phase 2 ui-redesign — B5/sk-30: acceptedAt=null for the Inbox
	// surface (unaccepted prefix), acceptedAt=not-null for the "On me"
	// surface (accepted-and-working prefix).
	if raw := q.Get("acceptedAt"); raw != "" {
		t := true
		switch raw {
		case "null":
			args.AcceptedAtNull = &t
		case "not-null":
			args.AcceptedAtNotNull = &t
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"acceptedAt accepts only the literal values \"null\" or \"not-null\"", nil)
			return
		}
	}
	// Phase 2 ui-redesign — B5: origin groups source values for the
	// "agent firehose" toggle. Validated whitelist only. Multi-select —
	// sk-273: comma-separated list, same CSV convention as status.
	if raw := q.Get("origin"); raw != "" {
		parts := strings.Split(raw, ",")
		groups := make([]string, 0, len(parts))
		for _, s := range parts {
			v := strings.TrimSpace(s)
			switch v {
			case issues.OriginHumanDriven, issues.OriginAgentAutonomous, issues.OriginAll:
				groups = append(groups, v)
			default:
				writeProblem(w, http.StatusBadRequest, "validation_error",
					"origin must be a comma-separated list of human-driven, agent-autonomous, all", nil)
				return
			}
		}
		args.OriginGroups = groups
	}
	// sk-273: multi-select priority — comma-separated list of integers.
	if raw := q.Get("priority"); raw != "" {
		parts := strings.Split(raw, ",")
		ps := make([]int, 0, len(parts))
		for _, s := range parts {
			p, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				writeProblem(w, http.StatusBadRequest, "invalid_priority",
					"priority must be a comma-separated list of integers", nil)
				return
			}
			if err := issues.ValidatePriority(p); err != nil {
				mapIssueValidation(w, err)
				return
			}
			ps = append(ps, p)
		}
		args.Priorities = ps
	}
	// sk-273: type filter — new on this handler, same CSV-validate shape
	// as status/priority above.
	if raw := q.Get("type"); raw != "" {
		parts := strings.Split(raw, ",")
		validated := make([]string, 0, len(parts))
		for _, s := range parts {
			canon, err := issues.ValidateType(strings.TrimSpace(s))
			if err != nil {
				mapIssueValidation(w, err)
				return
			}
			validated = append(validated, canon)
		}
		args.Types = validated
	}
	if raw := q.Get("parentId"); raw != "" {
		v := raw
		args.ParentID = &v
	}
	// Component filter — single UUID. Pushed into the issue_components
	// EXISTS subquery in the store so the SQL stays a strict subset of
	// the project's issues. We do NOT validate the component belongs to
	// the project here: a UUID for some other project simply matches
	// nothing (no rows in issue_components linking those two), which is
	// the desired empty result. Keeping this lax also avoids a second
	// round-trip on every list request.
	if raw := q.Get("componentId"); raw != "" {
		cid, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_component_id",
				"componentId is not a UUID", nil)
			return
		}
		args.ComponentID = &cid
	}
	if raw := q.Get("pinned"); raw != "" {
		switch raw {
		case "true":
			t := true
			args.Pinned = &t
		case "false":
			f := false
			args.Pinned = &f
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_pinned", "pinned must be true or false", nil)
			return
		}
	}
	// sk-31: derived blocked-ness filter. Strict whitelist — server
	// computes from the dep graph, no boolean coercion of stray values.
	if raw := q.Get("isBlocked"); raw != "" {
		switch raw {
		case "true":
			t := true
			args.IsBlocked = &t
		case "false":
			f := false
			args.IsBlocked = &f
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_isBlocked",
				"isBlocked must be true or false", nil)
			return
		}
	}
	if raw := q.Get("text"); raw != "" {
		v := raw
		args.Text = &v
	}
	// source filter (vision §829 — F24/F25 separation of human vs agent
	// origin). Whitelist enforced here so the store can inline the value
	// into the WHERE clause without revalidating.
	if raw := strings.TrimSpace(q.Get("source")); raw != "" {
		canon, err := issues.ValidateSource(raw)
		if err != nil {
			mapIssueValidation(w, err)
			return
		}
		args.Source = &canon
	}
	// Labels: support both repeated (?label=a&label=b) and CSV (?label=a,b).
	if labels, ok := q["label"]; ok {
		args.Labels = make([]string, 0, 8)
		for _, l := range labels {
			if l == "" {
				continue
			}
			for _, sub := range strings.Split(l, ",") {
				if t := strings.TrimSpace(sub); t != "" {
					args.Labels = append(args.Labels, t)
				}
			}
		}
	}
	if raw := q.Get("sort"); raw != "" {
		switch raw {
		case "updated", "created":
			args.SortBy = raw
		default:
			writeProblem(w, http.StatusBadRequest, "invalid_sort",
				"sort must be 'updated' or 'created'", nil)
			return
		}
	}
	if cursor := q.Get("cursor"); cursor != "" {
		t, id, err := issues.DecodeCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", err.Error(), nil)
			return
		}
		args.CursorTime = &t
		args.CursorID = &id
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 500 {
			writeProblem(w, http.StatusBadRequest, "invalid_limit",
				"limit must be in [1, 500]", nil)
			return
		}
		args.Limit = n
	}

	res, err := h.issuesStore.ListIssues(ctx, args)
	if err != nil {
		slog.ErrorContext(ctx, "list issues failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Tree handles GET /api/v1/projects/{projectID}/issues/tree — sk-36
// layered tree view, project-scoped variant. Returns a flat list of
// {item, depth, parentRef, isReference, firstSeenAt} rows pre-order-
// traversed from each project root, recursing across whichever
// dependency types the caller toggled on via `types[]`.
//
// Query params:
//
//	types[]   — repeated, validated against the seven-element
//	            whitelist (parent-child + the six non-hierarchical
//	            types). Empty defaults to ['parent-child'] so the
//	            spine is always rendered.
//	rootId    — optional. When set, the walk is anchored at the
//	            specified issue id rather than the forest of every
//	            issue without a parent-child edge.
//	includeBlockers — optional, exactly `true` or `false` (sk-421.2.8
//	            R-8, parseTreeIncludeBlockers). `true` adds `blockers` to
//	            every visible blocked row, classified by the caller's
//	            horizon; absent, empty or `false` leaves the response as
//	            it was.
//
// Response: {items: IssueTreeNode[]}.
//
// @Summary Layered tree of one project's issues
// @Description Returns the project's issues as a FLAT, pre-order (DFS) list of rows — {item, depth, parentRef, isReference, firstSeenAt} — so a client renders a tree without recursing itself: `depth` gives the indent, the order gives the structure. Business meaning: this is the project-scoped tree surface; its cross-project twin is GET /api/v1/issues/tree.
// @Description
// @Description Algorithm: authenticate -> parse projectID -> verify project visibility -> parse `types[]` (validated against the seven-element whitelist; empty defaults to ['parent-child'] so the spine is always rendered) -> walk the dependency graph with a recursive CTE from every project root, or from `rootId` when given -> redact rows the caller may not see -> order siblings by the id rule -> emit.
// @Description
// @Description Rows the caller cannot see: an issue living in a project outside the caller's horizon surfaces as a STUB — title "<hidden>", isReference=true, and EMPTY status and type (sk-421.2 INV-2: the status of an invisible task is card content and must not travel; the keys stay so older clients keep parsing). For every caller (internal and external alike) stubs are emitted after every visible sibling, so their position among visible siblings leaks nothing about their real id; among themselves they follow the id the caller receives: the real id by the id-order rule for an internal caller, the synthetic id (hidden-1 before hidden-2) for an external one. External callers additionally receive a prefix-free synthetic id and an empty project id.
// @Description
// @Description Blockers (sk-421.2.8, sk-423): with `includeBlockers=true` every VISIBLE row whose item.isBlocked is true carries `blockers` — one entry {id, title, status, projectId} per gating blocks / waits-for edge whose target is inside the CALLER's horizon, the same entries in the same order as the issue detail. Entries are classified by the caller's horizon, NOT by this project: a blocker in another project the caller can see is an entry. An edge to an issue outside the horizon is never an entry and never makes the row blocked; the canonical visible row instead carries `unavailableBlockingLinks` — the number of such edges, whatever their status, closed and standing included — and the key is absent when that number is 0. ⚠ Contract change in sk-423: the redacted entries {id, redacted:true} / {redacted:true} of sk-421.2.8 are gone. Stubs and reference rows carry neither key; unblocked rows carry no `blockers`. Without the flag item.isBlocked follows the same rule (blockers the caller can see); with it, isBlocked of every visible row is derived from that same list. Without the parameter, empty or `false`, the response has neither key.
// @Description
// @Description Branch progress (sk-461): `item.epicProgress` is present on every VISIBLE row that has parent-child children, whatever its type — not only on epic-typed rows, as the list endpoints still do. The six numbers count the row's DIRECT children: `total` excludes visible standing children and KEEPS children outside the caller's horizon, `closed` counts only visible closed children, `unavailable` counts the out-of-horizon ones, and — since sk-489 — `done` counts the visible children closed WITH the outcome `done`, `noWork` the visible children closed with any other outcome (wont_do, duplicate, superseded), and `counted` is the denominator `total - noWork`. `closed` keeps its old meaning, «closed by status», so an existing client reading closed/total sees exactly what it saw before; a client that wants progress reads done/counted, which is the one rule every Skein surface shares. Children outside the horizon stay in `counted`: «cannot say» is not «dropped out» — the same reading of "unavailable" as `epic.unavailableChildren` on the issue card, so a branch bar and an epic ring on one screen cannot disagree. Children are parent-child children regardless of `types[]`. A row with no children keeps the previous behaviour: an epic gets {closed:0,total:0,unavailable:0,done:0,noWork:0,counted:0}, any other type gets no key. Stubs for inaccessible rows never carry it.
// @Description
// @Description Close outcome (sk-489): a row whose issue is CLOSED carries `closeOutcome` — one of done, wont_do, duplicate, superseded — the machine-readable answer to «how did it end» that the free-text close reason never gave. On an issue that is not closed the KEY IS ABSENT rather than empty: an outcome without a close is a state the model does not have. Stubs for rows the caller may not see never carry it — it is card content about a task outside the horizon.
// @Description
// @Description Ordering (sk-421.2 US-6): siblings are ordered by one strict total rule over the id — digit segments compare by numeric value of any length, so `x.2` precedes `x.10`. The block "node plus all its descendants" stays contiguous.
// @Description
// @Description Row freshness order (sk-622.21, US-15): every row carries `rowSeq` — the issue's freshness order, taken from a global sequence on every committed write of the issue row, including writes that do not move `version` (label add/remove, component replace). For ONE issue a greater rowSeq is a later committed state; values of different issues are not comparable. A phase-2 client substitutes an event's row into a loaded row only if the event row's rowSeq is greater than the one shown. An issue not written since the phase-2 rollout (migration 000064) has no rowSeq key (absent ≡ 0, order unknown); any event row with a positive rowSeq is fresher than it. Redacted `<hidden>` rows carry no rowSeq.
// @Description
// @Description Side effects: none, the endpoint is read-only.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param types query []string false "Dependency types to recurse across; repeated. Defaults to ['parent-child']"
// @Param rootId query string false "Anchor the walk at this issue id instead of the project roots"
// @Param maxDepth query int false "Stop the walk at this depth, counted from the seed row (depth 0); 1 returns the roots plus their direct children. Omitted means the server ceiling"
// @Param includeBlockers query bool false "Exactly true or false. true adds `blockers` (blockers the caller can see) and `unavailableBlockingLinks` (links to issues outside the caller's horizon, when > 0) to visible rows; omitted, empty or false adds neither"
// @Success 200 {object} issues.IssueTreeResult "Flat pre-order row list"
// @Failure 400 {object} map[string]any "invalid_id — projectID is not a UUID; invalid_dep_type — types[] carried an unknown value; invalid_max_depth — maxDepth is not a whole number, is below 1, or exceeds the server ceiling; invalid_include_blockers — includeBlockers is neither true nor false"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project exists but the caller may not read it"
// @Failure 404 {object} map[string]any "project_not_found — absent or invisible to the caller"
// @Failure 422 {object} map[string]any "tree_too_large — the walk exceeded the node limit (extra: limit)"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues/tree [get]
func (h *IssuesHandler) Tree(w http.ResponseWriter, r *http.Request) {
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
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	// EnsureProjectVisible above authorized this caller for this project;
	// nodes in other projects are classified by the caller's scope.
	args := issues.IssueTreeArgs{
		Caller:    c.UserID,
		Scope:     authz.FromContext(ctx),
		ProjectID: &pid,
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("rootId")); raw != "" {
		args.RootID = &raw
	}

	types, ok := parseTreeTypes(w, r)
	if !ok {
		return
	}
	args.Types = types

	maxDepth, ok := parseTreeMaxDepth(w, r)
	if !ok {
		return
	}
	args.MaxDepth = maxDepth

	includeBlockers, ok := parseTreeIncludeBlockers(w, r)
	if !ok {
		return
	}
	args.IncludeBlockers = includeBlockers

	res, err := h.issuesStore.IssueTree(ctx, args)
	if err != nil {
		var tooLarge *issues.ErrTreeTooLarge
		if errors.As(err, &tooLarge) {
			writeProblem(w, http.StatusUnprocessableEntity, "tree_too_large",
				"tree exceeds node limit; narrow the filters",
				map[string]any{"limit": tooLarge.Limit})
			return
		}
		slog.ErrorContext(ctx, "issue tree failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// parseTreeMaxDepth reads the optional `?maxDepth=N` query parameter of
// both tree endpoints. Returns (0, true) when absent — the store reads
// zero as "unset" and falls back to its defensive ceiling, so the response
// without the parameter is byte-for-byte the one callers got before
// sk-421.2 (INV-3).
//
// Refusals are 400, not 422: this is range validation of a query
// parameter, the same class as invalid_dep_type next door. The business
// limit — a walk too large to answer — stays 422 tree_too_large and is
// untouched.
//
// A value above the server ceiling is REFUSED rather than clamped. Silently
// answering with a shallower tree than asked for is indistinguishable, to
// the caller, from a tree that genuinely ends there.
func parseTreeMaxDepth(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("maxDepth"))
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_max_depth",
			"maxDepth must be a whole number",
			map[string]any{"got": raw})
		return 0, false
	}
	if n < 1 {
		writeProblem(w, http.StatusBadRequest, "invalid_max_depth",
			"maxDepth must be at least 1",
			map[string]any{"got": n})
		return 0, false
	}
	if n > issues.IssueTreeMaxDepth {
		writeProblem(w, http.StatusBadRequest, "invalid_max_depth",
			"maxDepth exceeds the server ceiling",
			map[string]any{"got": n, "max": issues.IssueTreeMaxDepth})
		return 0, false
	}
	return n, true
}

// parseTreeIncludeBlockers reads the optional `?includeBlockers=` query
// parameter of both tree endpoints (sk-421.2.8 US-4, R-8). Exactly `true`
// and `false` are accepted; absent or empty reads as false, like an absent
// maxDepth, so the response without the parameter is byte-for-byte the
// previous one.
//
// Anything else — `1`, `t`, `TRUE`, `yes` — is refused with 400
// invalid_include_blockers, the same class as invalid_max_depth next door.
// strconv.ParseBool is deliberately not used: it accepts several of those
// silently, and «an invalid value» would stop meaning anything.
func parseTreeIncludeBlockers(w http.ResponseWriter, r *http.Request) (bool, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("includeBlockers"))
	switch raw {
	case "", "false":
		return false, true
	case "true":
		return true, true
	}
	writeProblem(w, http.StatusBadRequest, "invalid_include_blockers",
		"includeBlockers must be true or false",
		map[string]any{"got": raw})
	return false, false
}

// parseTreeTypes reads the repeated `?types[]=...` (or `?types=...`)
// query param, validates each value against the seven-element
// whitelist, and ensures `parent-child` is always present (the tree
// spine is mandatory-on per the sk-36 spec). Returns the validated
// type slice and `true`; on any unknown value writes 400
// invalid_dep_type and returns `false`.
//
// Accepts both wire forms — the typed-array `types[]=a&types[]=b` PHP
// convention and the plain `types=a&types=b` form — so callers can use
// either shape.
func parseTreeTypes(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	q := r.URL.Query()
	raw := q["types[]"]
	if len(raw) == 0 {
		raw = q["types"]
	}
	if len(raw) == 0 {
		return []string{dependencies.DepParentChild}, true
	}

	seen := make(map[string]struct{}, len(raw)+1)
	out := make([]string, 0, len(raw)+1)
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !issues.IsAllowedTreeType(t) {
			writeProblem(w, http.StatusBadRequest, "invalid_dep_type",
				"types must be one of parent-child, blocks, waits-for, related, "+
					"discovered-from, supersedes, duplicates",
				map[string]any{"got": t})
			return nil, false
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if _, ok := seen[dependencies.DepParentChild]; !ok {
		// Mandatory-on: prepend parent-child so the spine is always
		// walked. Matches the F1 setter invariant on the frontend.
		out = append([]string{dependencies.DepParentChild}, out...)
	}
	return out, true
}

// Get handles GET /api/v1/projects/{projectID}/issues/{issueID}. Project
// permission is checked via the path projectID — the caller must already
// be visible to the project. We then verify the issue belongs to that
// project to prevent ID-leakage across projects.
//
// sk-303.14 (contract C1): the card's field set depends on whether the
// caller is the author. The three review-mechanic fields — reviewRequired,
// reviewedAt, reviewedBy — arrive ONLY for the issue author and are ABSENT
// for everyone else (not `null`, not `false` — the key is not there). The
// gate is not here, it is in the single place the card is assembled, so it
// applies identically to every return of the card — patch, status
// transition, create, labels; rationale and predicate live in
// issues/review_visibility.go.
//
// @Summary Get one issue card
// @Description Returns the full card of one issue. Business meaning: the detail view behind the web UI and behind every read that needs more than a list row.
// @Description
// @Description Algorithm: authenticate -> validate the path project id -> verify project visibility -> load the issue by its hierarchical id -> confirm it belongs to THE PROJECT IN THE PATH and answer 404 if it does not, exactly as for an absent issue, so ids cannot be enumerated across projects.
// @Description
// @Description Field set depends on the reader (sk-303.14): the three review-mechanic fields — reviewRequired, reviewedAt, reviewedBy — are returned ONLY to the issue's author. For every other reader the KEYS ARE ABSENT: not null, not false, absent — so that "not supplied to you" stays distinguishable from "this issue has no mark". The gate lives in the single place the card is assembled, so it applies identically to every surface that returns a card: this read, the patch, the status transition, create, labels.
// @Description
// @Description Blockers (sk-421.1, sk-423): blockers[] lists every gating blocker of the issue that the READER can see — the target of an outgoing blocks / waits-for edge whose status is neither closed nor standing AND whose project is inside the reader's horizon (a member, or an internal admin; external collaborators never get the admin horizon) — ordered by id under the id-order rule of sk-421.2 US-6 (digit segments compare by numeric value, so `x.2` precedes `x.10`; ⚠ sk-426: it was string order before). Every entry is {id, title, status}. isBlocked is true if and only if blockers[] of the same response is non-empty. An edge to an issue OUTSIDE the horizon is never an entry and never makes the issue blocked: unavailableBlockingLinks counts such edges WHATEVER their status, closed and standing included, so it states that a link exists and reveals nothing about the hidden issue (the same rule as depsCounts' unavailable bucket and epic.unavailableChildren). ⚠ Contract change in sk-423: the redacted entry {id, redacted:true} of sk-421.1 is gone — its mere presence said the hidden issue was not closed. The horizon is the one the epic rollup on the same card uses; a request without a reader sees no project. Readiness (skein_ready) still counts every blocker, so an issue with isBlocked=false and unavailableBlockingLinks > 0 may be absent from the ready list.
// @Description
// @Description Close outcome (sk-489): a CLOSED issue carries `closeOutcome` — one of done, wont_do, duplicate, superseded — the machine-readable answer to «how did it end». On an issue that is not closed the KEY IS ABSENT rather than empty: an outcome without a close is a state the model does not have, held by the schema itself rather than by this handler. On an epic the `epic` rollup carries three further counters beside the status buckets: `doneChildren` (visible children closed with outcome done), `noWorkChildren` (visible children closed with any other outcome) and `countedChildren` (the progress denominator, totalChildren minus noWorkChildren). The existing buckets are untouched — closedChildren still means «closed by status» — so a client reading them sees what it always saw; a client that wants progress reads doneChildren/countedChildren. Children outside the reader's horizon stay in countedChildren: «cannot say» is not «dropped out».
// @Description
// @Description Lock holder (sk-421.1, US-15): on a locked issue lockedByUsername is the username of the user who PERFORMED the claim that holds the lock now — for an admin claiming on behalf of someone else, the admin, not the assignee. It is derived on read from that claim's issue.claimed audit event (the session it wrote into the lock and the lock moment), never stored. The key is absent when the issue is not locked or that event is not found (the card still answers 200), and it disappears with the lock on unclaim, close, stale-lock auto-release or forced release. A request that does not change the lock cannot change the name, whichever session it presents. lockedBy keeps the MCP session id; lockedByUsername never repeats it.
// @Description
// @Description Row freshness order (sk-622.21, US-15): the card carries `rowSeq` — the same value as the issue's list row, taken from a global sequence on every committed write of the issue row, including writes that do not move `version` (label add/remove, component replace). A phase-2 client substitutes an event's row into the open card only if the event row's rowSeq is greater than the card's. An issue not written since the phase-2 rollout (migration 000064) has no rowSeq key (absent ≡ 0, order unknown); any event row with a positive rowSeq is fresher than it. The card's labels and components are read in the same statement as rowSeq, so the card is exactly the state its rowSeq names.
// @Description
// @Description Side effects: none.
// @Description
// @Description parentIdCurrent (sk-674.18 T009): the canonical parent (first parent-child edge by creation) is named only when its project is visible to the caller; a parent the caller cannot see leaves the key absent — the card never carries the id of an inaccessible issue.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param issueID path string true "Hierarchical issue id, e.g. sk-42.1"
// @Success 200 {object} issues.IssueDetail "The issue card, narrowed to what this reader may see"
// @Failure 400 {object} map[string]any "invalid_id; missing_issue_id"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr)"
// @Failure 404 {object} map[string]any "issue_not_found — absent, invisible, or in another project"
// @Failure 422 {object} map[string]any "business refusals of the shared issue mappers — mapIssueValidation raises a validation code to 422 (for example a derived filter such as blocked), mapIssueErr raises id_taken / pinned_no_force"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues/{issueID} [get]
func (h *IssuesHandler) Get(w http.ResponseWriter, r *http.Request) {
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
	level, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	dto, err := h.issuesStore.GetIssue(ctx, issueID)
	if err != nil {
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "get issue failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if dto.ProjectID != pid {
		// Cross-project URL: pretend the issue doesn't exist (don't leak).
		writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
		return
	}
	canForce, err := h.canForceUnlock(ctx, level, issueID, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get issue: claim holder failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	dto.CanForceUnlock = &canForce
	writeJSON(w, http.StatusOK, dto)
}

// canForceUnlock is the per-reader flag of the card (US-16, K-28, row 145):
// the reader's force_unlock on the project, and the claim is not his own —
// the holder releases his claim as his own, the force action is not his).
// Without a claim the flag is the right alone.
func (h *IssuesHandler) canForceUnlock(ctx context.Context, level authz.Level, issueID string, reader uuid.UUID) (bool, error) {
	if !authz.Allows(level, authz.ActionForceUnlock) {
		return false, nil
	}
	holder, err := h.issuesStore.LockHolder(ctx, issueID)
	if err != nil {
		return false, err
	}
	return holder != reader, nil
}

// Unlock handles POST /api/v1/projects/{projectID}/issues/{issueID}/unlock.
//
// @Summary Release the claim on one issue
// @Description The explicit "release claim" action (US-16, sk-674.18 T034): clears the claim on the issue and leaves its status as it is — unlike unclaim, which moves the status too. Business meaning: a stuck claim (an agent that died, a person who went away) is released by someone responsible for the project instead of waiting for the automatic release by term.
// @Description
// @Description Algorithm: authenticate -> validate the path project id -> verify project visibility -> confirm the issue belongs to THE PROJECT IN THE PATH (404 otherwise, as for an absent issue) -> resolve the caller's force_unlock on the project (the project owner, the space owner, the instance admin while the flag is on) -> in one SERIALIZABLE transaction: no claim -> nothing to do; the caller's own claim (held by the caller's user) -> released; another holder's claim -> released only with force_unlock, otherwise 422 force_unlock_forbidden and nothing written -> return the reloaded card with canForceUnlock.
// @Description
// @Description Side effects: on a release, issue.unclaimed (as every release of a claim) and — when the claim was another holder's — issue.lock_force_released, whose new_data.by names the releaser; version + 1; the personal signal of a claim change. The status, the assignee and every other field stay.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param issueID path string true "Hierarchical issue id, e.g. sk-42.1"
// @Success 200 {object} issues.IssueDetail "The issue card, as its GET returns it, with canForceUnlock"
// @Failure 400 {object} map[string]any "invalid_id; missing_issue_id"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 404 {object} map[string]any "not_found — project not visible; issue_not_found — absent, invisible, or in another project"
// @Failure 422 {object} map[string]any "force_unlock_forbidden — the claim is another holder's and the caller may not release it, nothing written; not_lock_holder — the claim changed while it was being released, re-read and retry"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues/{issueID}/unlock [post]
func (h *IssuesHandler) Unlock(w http.ResponseWriter, r *http.Request) {
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
	level, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}
	canForce := authz.Allows(level, authz.ActionForceUnlock)
	dto, err := h.issuesStore.ReleaseLock(ctx, issues.ReleaseLockInput{
		ID:             issueID,
		CanForceUnlock: canForce,
		Actor:          issueActor(c),
	})
	if err != nil {
		if errors.Is(err, issues.ErrForceUnlockForbidden) {
			requireForceUnlock(w, level)
			return
		}
		if errors.Is(err, issues.ErrNotLockHolder) {
			writeProblem(w, http.StatusUnprocessableEntity, "not_lock_holder",
				"the claim changed while it was being released; re-read the issue and retry", nil)
			return
		}
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "release lock failed", "issueId", issueID, "userId", c.UserID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	after, err := h.canForceUnlock(ctx, level, issueID, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "unlock: claim holder failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	dto.CanForceUnlock = &after
	writeJSON(w, http.StatusOK, dto)
}

// Patch handles PATCH /api/v1/projects/{projectID}/issues/{issueID}.
// Requires If-Match: <version>; missing → 428, stale → 422 version_conflict.
//
// Path scoping: the issue must belong to the project in the path, checked
// BEFORE PatchIssue runs. Until sk-194.6.50 the comparison happened on the
// store's return value, which 404'd the response but left the write
// committed against the foreign project. A cross-project issue id now reads
// as 404 issue_not_found with nothing written.
//
// @Summary Update one issue (optimistic concurrency)
// @Description Applies a partial update to one issue. Business meaning: the single edit path for a person; it requires the version the caller has seen, so two editors cannot silently overwrite each other.
// @Description
// @Description Algorithm: authenticate -> validate the path project id -> verify project visibility -> require If-Match with the version (absent -> 428) -> decode the body -> SPLIT on the review flag (below) -> confirm the issue belongs to the project in the path BEFORE anything is written -> apply the update against the presented version and return the reloaded card.
// @Description
// @Description The review-required flag (sk-303.14) does NOT travel the ordinary update path. A body carrying `reviewRequired` TOGETHER with any other field is refused whole, with 400 review_flag_not_patchable — nothing is written, and the request is never executed as two acts. A body carrying `reviewRequired` ALONE is routed to the flag operation instead: only the author may change it (any other caller, admin included, is refused with 422), and the change deliberately does NOT bump the issue version, does NOT move the updated-at stamp, does NOT appear in the issue's activity feed and is NOT fanned out — the flag is the author's private bookkeeping and must not be observable to the assignee through any channel. Clearing the flag removes the issue from the unreviewed set and is NOT a review mark.
// @Description
// @Description Editing the close outcome (sk-489, US-12): `closeOutcome` re-answers «how did it end» on an ALREADY CLOSED issue, without reopening it — the correction a person makes when a task turns out to have been a duplicate all along. On an issue that is not closed it is refused 422 close_outcome_requires_closed: the request is well-formed, the issue's state forbids it. `closeTarget` names the issue this one duplicates or was superseded by and creates that dependency edge in the SAME transaction, indistinguishable from an edge made by hand and never duplicated if one already exists; it is accepted only together with an explicit closeOutcome of duplicate or superseded, and never pointing at the issue itself. Changing the outcome is NOT part of the issue.updated diff: it has its own event and its own notification reason, and folding it in would notify the author twice for one act. Since sk-540.2 an outcome change to duplicate or superseded WITHOUT `closeTarget` is accepted only when the issue already has the matching edge (`duplicates` from it, `supersedes` to it); otherwise 422 close_target_required. Re-sending the outcome the issue already has needs no target.
// @Description
// @Description Side effects of an ordinary update: an issue.updated event; watcher fan-out and notifications per the reason registry; newly-appeared @username mentions resolved and notified. An outcome change adds issue.outcome_changed, notifies the author of the new reason and resets their review mark; a named target adds the dependency edge.
// @Tags Issues
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param issueID path string true "Hierarchical issue id, e.g. sk-42.1"
// @Param If-Match header string true "Version the caller has seen"
// @Param body body patchIssueRequest true "Fields to change; reviewRequired may only travel alone"
// @Success 200 {object} issues.IssueDetail "The updated card, narrowed to what this reader may see"
// @Failure 400 {object} map[string]any "invalid_id; missing_issue_id; invalid_json; review_flag_not_patchable — reviewRequired was combined with other fields and NOTHING was written"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr)"
// @Failure 404 {object} map[string]any "issue_not_found — absent, invisible, or in another project; nothing written"
// @Failure 422 {object} map[string]any "version_conflict — the presented version is stale; validation and domain refusals; review_forbidden — only the author may change the review flag; close_target_required — duplicate/superseded without closeTarget and without an existing matching edge; assignee_not_allowed — the assignee given as a user id names nobody who may work in this project now (INV-30), nothing is written"
// @Failure 428 {object} map[string]any "If-Match is missing"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues/{issueID} [patch]
func (h *IssuesHandler) Patch(w http.ResponseWriter, r *http.Request) {
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
	level, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	version, ok := requireIfMatch(w, r)
	if !ok {
		return
	}

	var req patchIssueRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	// sk-674.18 T066: changing the assignee is ActionAssign of US-6.
	if req.Assignee != nil && mapAccessErr(w, projects.RequireAction(level, authz.ActionAssign)) {
		return
	}

	// sk-303.14 T007 (contract C1) — the review flag does NOT go through
	// the general patch path.
	//
	// A mixed request is rejected by the shape, not executed as two acts.
	// The reason is INV-2: with two acts the version effect would start
	// depending on the field set in the request — the same call would
	// sometimes bump the version and sometimes not — and from that
	// difference the assignee would learn that the author touched the
	// flag. The refusal is checkable by one class and cannot be
	// implemented halfway.
	//
	// Code 400, not 422: this is a REQUEST-SHAPE constraint (the field
	// is not part of the general patch), not a domain business rule.
	hasReviewFlag, hasOtherFields := reviewOnlyPatch(req)
	if hasReviewFlag && hasOtherFields {
		writeProblem(w, http.StatusBadRequest, "review_flag_not_patchable",
			"reviewRequired cannot be combined with other fields in one request — "+
				"send it alone", nil)
		return
	}
	if hasReviewFlag {
		h.setReviewRequired(w, r, issueID, version, *req.ReviewRequired, c)
		return
	}

	// Validate enum-bearing fields if provided.
	if req.Type != nil {
		if _, err := issues.ValidateType(*req.Type); err != nil {
			mapIssueValidation(w, err)
			return
		}
	}
	if req.Priority != nil {
		if err := issues.ValidatePriority(*req.Priority); err != nil {
			mapIssueValidation(w, err)
			return
		}
	}
	if req.Title != nil {
		v, err := issues.ValidateTitle(*req.Title)
		if err != nil {
			mapIssueValidation(w, err)
			return
		}
		req.Title = &v
	}

	dto, err := h.issuesStore.PatchIssue(ctx, issueID, issues.PatchIssueInput{
		ExpectedVersion:    version,
		Title:              req.Title,
		Description:        req.Description,
		Type:               req.Type,
		Priority:           req.Priority,
		Assignee:           req.Assignee,
		Design:             req.Design,
		AcceptanceCriteria: req.AcceptanceCriteria,
		Notes:              req.Notes,
		EstimatedMinutes:   req.EstimatedMinutes,
		DueAt:              req.DueAt,
		DeferUntil:         req.DeferUntil,
		ExternalRef:        req.ExternalRef,
		SourceContext:      req.SourceContext,
		// sk-489 (US-12, US-2). There are deliberately NO outcome checks here: both
		// the resolution of the value and all target checks live in one place for
		// both writers (issues.DecideCloseOutcome), and the target's visibility is in
		// the same transaction as the link. The handler is responsible only for the
		// request shape and routing.
		CloseOutcome:       req.CloseOutcome,
		CloseTarget:        derefString(req.CloseTarget),
		DependenciesCaller: closeTargetCaller(c),
	}, issueActor(c))
	if err != nil {
		if mapIssueValidation(w, err) {
			return
		}
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "patch issue failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// The cross-project guard used to sit HERE, comparing dto.ProjectID
	// against pid on the result of PatchIssue. That 404'd the response but
	// the write had already committed to the foreign project — it hid the
	// breach instead of preventing it. The check now runs pre-flight above
	// (ensureIssueInProject), so by this point the pair is known good.
	writeJSON(w, http.StatusOK, dto)
}

// UpdateStatus handles POST /api/v1/projects/{projectID}/issues/{issueID}/transitions.
//
// Transitions the issue's status (open / in_progress / deferred / closed /
// standing) under optimistic concurrency: If-Match carries the version the
// caller observed. `?force=true` overrides the pinned-close and lock guards
// the store enforces.
//
// Path scoping: the issue must belong to the project in the path. An issue
// id from another project reads as 404 issue_not_found — identical to a
// nonexistent id, so the response never confirms that a foreign issue
// exists (sk-194.6.50).
//
// Responses:
//   - 200 — the updated IssueDetail.
//   - 400 — invalid project id / JSON.
//   - 401 — unauthenticated.
//   - 404 — project not visible.
//   - 403 — forbidden: the caller may not write issues in the project
//     (US-6 ActionWrite; since sk-741 every level of a visible project has
//     it, so the answer stays for a narrower level of the action table).
//   - 404 — issue not found, or not in this project (issue_not_found).
//   - 428 — If-Match missing; 422 — version_conflict / business rule;
//     422 force_unlock_forbidden — ?force=true without force_unlock
//     (sk-674.18 T034, K-28).
//   - 500 — store fault.
//
// @Summary Move one issue to another status
// @Description Moves one issue into a new lifecycle status under optimistic concurrency. Business meaning: the single place a person changes where a task stands — picked up, put aside, finished — with the version they were looking at, so two editors cannot silently overwrite each other's decision.
// @Description
// @Description Algorithm: authenticate -> validate the path project id -> verify project visibility -> confirm the issue belongs to the project in the path BEFORE anything is written -> require If-Match with the version (absent -> 428) -> decode the body strictly (an unknown key is a 400, never a silent drop) -> hand status, reason, close outcome and close target to the store, which routes a transition to `closed` through the close writer and every other status through the transition writer -> return the reloaded card.
// @Description
// @Description Close outcome (sk-489): a transition to `closed` may name WHY it ended — `closeOutcome` is one of done, wont_do, duplicate, superseded. Omitted, a transition gets `done`; a re-close of an already closed issue keeps whatever outcome it had, so attaching commits or rewriting a reason never erases it. `closeTarget` names the issue this one duplicates or was superseded by, and creates that dependency edge in the SAME transaction as the close — indistinguishable from an edge made by hand, and never a second copy if one already exists. A target is accepted ONLY together with an explicit closeOutcome of duplicate or superseded, never with done or wont_do, and never pointing at the issue being closed. Conversely (sk-540.2) duplicate and superseded REQUIRE a target — 422 close_target_required — except on an issue that is already closed: re-closing with the outcome it already has, or changing its outcome when the matching edge (`duplicates` from it, `supersedes` to it) already exists. Sending either field with a status other than `closed` is refused 400 close_outcome_not_a_close: no state of any issue makes that combination meaningful.
// @Description
// @Description Side effects: an issue.status_changed event and, on a close, issue.closed plus the release of any lock; the close-target edge when one was named; on a re-close that changes the outcome, issue.outcome_changed and the reset of the author's review mark; watcher fan-out and notifications per the reason registry.
// @Tags Issues
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param projectID path string true "Project UUID"
// @Param issueID path string true "Hierarchical issue id, e.g. sk-42.1"
// @Param If-Match header string true "Version the caller has seen"
// @Param force query bool false "Bypass the pinned-close and lock guards. Needs force_unlock on the project (project owner, space owner, instance admin); otherwise 422 force_unlock_forbidden"
// @Param body body updateStatusRequest true "New status, optional reason, and — on a close — the outcome and its target"
// @Success 200 {object} issues.IssueDetail "The updated card, narrowed to what this reader may see"
// @Failure 400 {object} map[string]any "invalid_id; missing_issue_id; invalid_json; close_outcome_not_a_close — an outcome or target travelled with a non-closed status; invalid_close_outcome — the outcome is not one of the four; close_target_requires_outcome; close_target_not_allowed; close_target_self"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr)"
// @Failure 404 {object} map[string]any "issue_not_found — absent, invisible, or in another project; nothing written"
// @Failure 422 {object} map[string]any "force_unlock_forbidden — force=true without the right to release another holder's claim, nothing written; version_conflict — the presented version is stale; pinned_no_force; locked_by_other; standing lifecycle refusals; close_target_required — duplicate/superseded without closeTarget where no exception applies; target_not_found — closeTarget does not exist OR is in a project this caller cannot see, answered identically on purpose"
// @Failure 428 {object} map[string]any "If-Match is missing"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/projects/{projectID}/issues/{issueID}/transitions [post]
func (h *IssuesHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
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
	level, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}
	force := r.URL.Query().Get("force") == "true"
	if force && !requireForceUnlock(w, level) {
		return
	}

	version, ok := requireIfMatch(w, r)
	if !ok {
		return
	}

	var req updateStatusRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	dto, err := h.issuesStore.UpdateStatus(ctx, issueID, issues.UpdateStatusInput{
		ExpectedVersion: version,
		Status:          req.Status,
		Reason:          req.Reason,
		Force:           force,
		// sk-489 (US-1, US-2) — see the note at Patch above: the handler
		// routes, the storage decides.
		CloseOutcome:       req.CloseOutcome,
		CloseTarget:        req.CloseTarget,
		DependenciesCaller: closeTargetCaller(c),
	}, issueActor(c))
	if err != nil {
		if mapIssueValidation(w, err) {
			return
		}
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "update status failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// Pin handles POST /api/v1/projects/{projectID}/issues/{issueID}/pin.
// Requires If-Match. Cross-project path → 404 issue_not_found (see
// pinHandler / ensureIssueInProject).
func (h *IssuesHandler) Pin(w http.ResponseWriter, r *http.Request) {
	h.pinHandler(w, r, true)
}

// Unpin handles POST /api/v1/projects/{projectID}/issues/{issueID}/unpin.
// Requires If-Match. Cross-project path → 404 issue_not_found (see
// pinHandler / ensureIssueInProject).
func (h *IssuesHandler) Unpin(w http.ResponseWriter, r *http.Request) {
	h.pinHandler(w, r, false)
}

// pinHandler is the shared body of Pin/Unpin.
//
// Path scoping: the issue must belong to the project in the path; an issue
// id from another project is answered 404 issue_not_found, indistinguishable
// from a nonexistent id, and no write happens (sk-194.6.50).
//
// Responses: 200 IssueDetail · 400 invalid id · 401 unauthenticated ·
// 404 project not visible · 403 forbidden — the caller
// may not write issues in the project (US-6 ActionWrite) · 404 issue not found / not in this project ·
// 428 If-Match missing · 422 version_conflict · 500 store fault.
func (h *IssuesHandler) pinHandler(w http.ResponseWriter, r *http.Request, pin bool) {
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
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}
	version, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	var dto *issues.IssueDetail
	if pin {
		dto, err = h.issuesStore.Pin(ctx, issueID, version, issueActor(c))
	} else {
		dto, err = h.issuesStore.Unpin(ctx, issueID, version, issueActor(c))
	}
	if err != nil {
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "pin toggle failed", "issueId", issueID, "pin", pin, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// ReplaceComponents handles PUT /api/v1/projects/{projectID}/issues/{issueID}/components.
// Body: {componentIds: [uuid...]} — replaces the issue's component set
// wholesale (empty array clears it).
//
// Path scoping: the issue must belong to the project in the path. A
// cross-project issue id is answered 404 issue_not_found before any write,
// so components of a foreign project's issue cannot be rewritten through a
// project the caller happens to see (sk-194.6.50).
//
// The componentIds in the BODY are a separate cross-project question and
// already have their own guard: the store rejects any component that does
// not belong to the issue's project with components_wrong_project (422),
// writing nothing. This path scoping and that body validation are
// complementary — one fixes which issue is addressed, the other what may be
// attached to it.
//
// Responses: 200 IssueDetail · 400 invalid project/component id, invalid
// JSON · 401 unauthenticated · 404 project not visible · 403 forbidden — the caller
// may not write issues in the project (US-6 ActionWrite) · 404 issue not
// found / not in this project · 422 components_wrong_project /
// locked_by_other / version_conflict · 500 store fault.
func (h *IssuesHandler) ReplaceComponents(w http.ResponseWriter, r *http.Request) {
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
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}
	var req replaceComponentsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	cids := make([]uuid.UUID, 0, len(req.ComponentIDs))
	for _, raw := range req.ComponentIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_component_id",
				"componentIds["+raw+"] is not a UUID", nil)
			return
		}
		cids = append(cids, id)
	}
	dto, err := h.issuesStore.ReplaceComponents(ctx, issueID, cids, issueActor(c), issues.LockOpts{})
	if err != nil {
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "replace components failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// AddLabel handles POST /api/v1/projects/{projectID}/issues/{issueID}/labels.
// Body: {label: "..."}. Idempotent — re-adding an existing label is a no-op.
//
// Path scoping: cross-project issue id → 404 issue_not_found, checked before
// the write (sk-194.6.50).
//
// Responses: 200 IssueDetail · 400 invalid id / JSON / label ·
// 401 unauthenticated · 404 project not visible · 403 forbidden — the caller
// may not write issues in the project (US-6 ActionWrite) · 404 issue not found /
// not in this project · 500 store fault.
func (h *IssuesHandler) AddLabel(w http.ResponseWriter, r *http.Request) {
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
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}
	var req addLabelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	dto, err := h.issuesStore.AddLabel(ctx, issueID, req.Label, issueActor(c), issues.LockOpts{})
	if err != nil {
		if mapIssueValidation(w, err) {
			return
		}
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "add label failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// RemoveLabel handles DELETE /api/v1/projects/{projectID}/issues/{issueID}/labels/{label}.
//
// Path scoping: cross-project issue id → 404 issue_not_found, checked before
// the write (sk-194.6.50). Note the two distinct 404 codes on this route:
// issue_not_found (no such issue in this project) and label_not_found (the
// issue is right, the label is not set on it).
//
// Responses: 200 IssueDetail · 400 invalid id / missing label ·
// 401 unauthenticated · 404 project not visible · 403 forbidden — the caller
// may not write issues in the project (US-6 ActionWrite) · 404 issue_not_found /
// label_not_found · 500 store fault.
func (h *IssuesHandler) RemoveLabel(w http.ResponseWriter, r *http.Request) {
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
	label := chi.URLParam(r, "label")
	if label == "" {
		writeProblem(w, http.StatusBadRequest, "missing_label", "label is required", nil)
		return
	}
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Object-level authorization, BEFORE the write: EnsureProjectAction
	// cleared the project in the path, not the issue in the path. Rationale
	// and the 404-not-403 choice: ensureIssueInProject.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}
	dto, err := h.issuesStore.RemoveLabel(ctx, issueID, label, issueActor(c), issues.LockOpts{})
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			// Specifically the "label not found on this issue" case maps to
			// 404 — we still return 404 if the issue itself was missing.
			writeProblem(w, http.StatusNotFound, "label_not_found",
				"label is not set on this issue", nil)
			return
		}
		if mapIssueValidation(w, err) {
			return
		}
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "remove label failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// ListProjectLabels handles GET /api/v1/projects/{projectID}/labels.
func (h *IssuesHandler) ListProjectLabels(w http.ResponseWriter, r *http.Request) {
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
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	out, err := h.issuesStore.ListLabelsInProject(ctx, pid)
	if err != nil {
		slog.ErrorContext(ctx, "list project labels failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListComments handles GET /api/v1/projects/{projectID}/issues/{issueID}/comments.
//
// Optional query `origin=human-driven|agent-autonomous|all` (default
// `all`) filters by comments.author_kind: "human-driven" returns
// human + agent_directed (the "a human is behind this" lens), and
// "agent-autonomous" returns only agent_autonomous (self-spawned agent
// notes). Mirrors the same wire token already used by skein_list and
// the project counters. Invalid values → 400 invalid_origin.
func (h *IssuesHandler) ListComments(w http.ResponseWriter, r *http.Request) {
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

	// sk-16 — parse + validate `origin` before any DB work. "all" and an
	// empty value both mean "no filter" (zero value of CommentOriginFilter
	// is treated as no filter by the store).
	originRaw := strings.TrimSpace(r.URL.Query().Get("origin"))
	var origin issues.CommentOriginFilter
	switch originRaw {
	case "", "all":
		origin = issues.CommentOriginAll
	case "human-driven":
		origin = issues.CommentOriginHumanDriven
	case "agent-autonomous":
		origin = issues.CommentOriginAgentAutonomous
	default:
		writeProblem(w, http.StatusBadRequest, "invalid_origin",
			"origin must be one of: human-driven, agent-autonomous, all", nil)
		return
	}

	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Verify issue belongs to project — same anti-leak rule as Get.
	dto, err := h.issuesStore.GetIssue(ctx, issueID)
	if err != nil {
		if mapIssueErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if dto.ProjectID != pid {
		writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
		return
	}
	// Zero opts = full chronological thread as bare JSON array (US-6 /
	// INV-8). total is discarded — REST does not wrap list responses.
	out, _, err := h.issuesStore.ListComments(ctx, issueID, origin, issues.ListCommentsOpts{})
	if err != nil {
		slog.ErrorContext(ctx, "list comments failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// AddComment handles POST /api/v1/projects/{projectID}/issues/{issueID}/comments.
//
// sk-16 — request carries an optional `kind` field. Routing of the
// effective comments.author_kind depends on the caller's auth mode:
//
//   - cookie session  → author_kind is forced to "human" regardless of
//     the `kind` field (silently ignored; a debug log is emitted if a
//     non-empty value was sent).
//   - api-key (agent) → `kind` is REQUIRED, must be one of "directed"
//     or "autonomous". Missing or unknown values → 400 validation_error.
//
// REST callers never write agent_type — that column stays "human" on
// the REST path; only the MCP skein_comment tool writes agent_type='agent'.
// This handler only resolves author_kind.
//
// Authorization is two-step and both steps precede the insert:
// EnsureProjectAction(ActionWrite) on the path project (404 when not
// visible, 403 when visible but not writable — US-6), then
// ensureIssueInProject on the path issue (404 issue_not_found when the issue lives in another project —
// same answer as an id that does not exist).
func (h *IssuesHandler) AddComment(w http.ResponseWriter, r *http.Request) {
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
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// sk-16 review fix #6 — cap upload size at the HTTP layer so a
	// malicious 1 MiB body never opens the SERIALIZABLE tx downstream.
	// 200 KiB = 2× the store-side `commentBodyMaxBytes` cap (100 KiB),
	// matching the same headroom the MCP comment tool uses.
	r.Body = http.MaxBytesReader(w, r.Body, 200<<10)
	var req addCommentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	// sk-16 review fix #6 (cont.) — validate body shape BEFORE the
	// issue/project lookup + AddComment tx. Mirrors the validate-early
	// pattern the MCP `skein_comment` handler uses; bounces malformed
	// requests with O(1) work instead of O(DB roundtrip).
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeProblem(w, http.StatusBadRequest, "validation_error", "body is required", nil)
		return
	}
	if len(body) > 100000 {
		writeProblem(w, http.StatusBadRequest, "validation_error", "body must be 100000 characters or fewer", nil)
		return
	}
	// Object-level gate: the issue named in the path must belong to the
	// project named in the path. Scoped predicate rather than
	// GetIssue-then-compare — see ensureIssueInProject. The foreign row is
	// never hydrated, so "no such issue" and "issue of another project"
	// leave through one query plan and one response.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	// sk-16 — derive author_kind from caller mode + request kind.
	// Mode classification mirrors MCP skein_create's callerOriginMode:
	// presence of APIKeyID in ctx ⇒ agent caller. Cookie session ⇒ no
	// APIKeyID, force human and ignore the `kind` field.
	kindArg := strings.TrimSpace(req.Kind)
	apiKeyID, hasAPIKey := auth.APIKeyIDFrom(ctx)
	isAgent := hasAPIKey && apiKeyID != uuid.Nil
	var authorKind string
	if isAgent {
		switch kindArg {
		case "directed":
			authorKind = issues.CommentAuthorKindAgentDirected
		case "autonomous":
			authorKind = issues.CommentAuthorKindAgentAutonomous
		case "":
			writeProblem(w, http.StatusBadRequest, "validation_error",
				`kind is required for agent callers; pass "directed" (user-requested) or "autonomous" (side-quest)`, nil)
			return
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"kind must be one of: directed, autonomous", nil)
			return
		}
	} else {
		authorKind = issues.CommentAuthorKindHuman
		if kindArg != "" {
			slog.DebugContext(ctx, "cookie-session caller passed kind on comment POST; ignoring",
				"issueId", issueID, "kind", kindArg)
		}
	}

	dto, err := h.issuesStore.AddComment(ctx, issueID, req.Body,
		c.UserID, c.Username,
		issues.CommentAuthorTypeHuman, authorKind,
		issueActor(c))
	if err != nil {
		if mapIssueValidation(w, err) {
			return
		}
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "add comment failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusCreated, dto)
}

// UpdateComment handles
// PUT /api/v1/projects/{projectID}/issues/{issueID}/comments/{commentID}.
//
// Edits a comment's body. sk-57. Algorithm:
//  1. Authenticate + parse path ids (projectID, issueID, commentID).
//  2. EnsureProjectAction(ActionWrite) — non-members get 404 (anti-leak,
//     vision §10); a visible project the caller may not write in — 403.
//  3. Decode the body (body required, expectedVersion the OCC token).
//  4. ensureIssueInProject — the path issue must belong to the path
//     project, else 404 issue_not_found (sk-194.6.50; scoped predicate,
//     so a foreign comment's metadata is never read).
//  5. Load comment meta; verify it belongs to the issue + the issue
//     belongs to the project (404 comment_not_found on mismatch — the
//     comment is a different object from the issue gated in step 4).
//  6. AUTHOR-ONLY gate: caller user id must equal the comment's
//     author_user_id, else 403 forbidden.
//  7. Store.UpdateComment runs in a SERIALIZABLE tx: version check,
//     deleted-comment gate, body replace, version bump, edited_at set,
//     comment.edited event + newly-introduced @mention fan-out.
//
// Error mapping: version mismatch → 422 version_conflict (a routine
// concurrent-edit situation, as in the accept handler); editing a deleted
// comment → 422 comment_deleted;
// not the author → 403 forbidden; missing/blank body → 400; unknown
// comment → 404.
//
// Cookie callers force author_kind=human-equivalent semantics (kind is
// ignored); api-key (agent) callers MUST send kind=directed|autonomous —
// it does not change authorship but mirrors the AddComment contract.
func (h *IssuesHandler) UpdateComment(w http.ResponseWriter, r *http.Request) {
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
	commentID, err := parseUUIDParam(r, "commentID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if _, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Cap upload size at the HTTP layer (same 200 KiB headroom as AddComment).
	r.Body = http.MaxBytesReader(w, r.Body, 200<<10)
	var req updateCommentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeProblem(w, http.StatusBadRequest, "validation_error", "body is required", nil)
		return
	}
	if len(body) > 100000 {
		writeProblem(w, http.StatusBadRequest, "validation_error", "body must be 100000 characters or fewer", nil)
		return
	}
	if req.ExpectedVersion < 1 {
		writeProblem(w, http.StatusBadRequest, "validation_error", "expectedVersion must be a positive integer", nil)
		return
	}

	// Object-level gate on the PATH issue, before the comment row is
	// resolved: an issueID that does not belong to this project is answered
	// without ever reading a foreign comment's metadata. The meta compare
	// below still stands — it authorizes the COMMENT against the issue,
	// which is a different object and a different question.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	// Resolve comment → issue + author for the ownership + project checks.
	meta, err := h.issuesStore.GetCommentMeta(ctx, commentID)
	if err != nil {
		if errors.Is(err, issues.ErrCommentNotFound) {
			writeProblem(w, http.StatusNotFound, "comment_not_found", "comment not found", nil)
			return
		}
		slog.ErrorContext(ctx, "load comment meta failed", "commentId", commentID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if meta.IssueID != issueID || meta.ProjectID != pid {
		// Anti-leak: a comment that exists but not under this issue/project
		// reads as "not found" rather than confirming cross-tree existence.
		writeProblem(w, http.StatusNotFound, "comment_not_found", "comment not found", nil)
		return
	}
	// AUTHOR-ONLY.
	if meta.AuthorUserID == nil || *meta.AuthorUserID != c.UserID {
		writeProblem(w, http.StatusForbidden, "forbidden", "only the comment author can edit this comment", nil)
		return
	}

	dto, err := h.issuesStore.UpdateComment(ctx, commentID, req.Body, req.ExpectedVersion, issueActor(c))
	if err != nil {
		if mapIssueValidation(w, err) {
			return
		}
		// version_conflict (→ 422) and the rest are handled uniformly by mapIssueErr.
		if mapIssueErr(w, err) {
			return
		}
		if errors.Is(err, issues.ErrCommentNotFound) {
			writeProblem(w, http.StatusNotFound, "comment_not_found", "comment not found", nil)
			return
		}
		slog.ErrorContext(ctx, "update comment failed", "commentId", commentID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// DeleteComment handles
// DELETE /api/v1/projects/{projectID}/issues/{issueID}/comments/{commentID}.
//
// Soft-deletes a comment (sk-57). Algorithm:
//  1. Authenticate + parse path ids.
//  2. EnsureProjectAction(ActionWrite) (404 on non-member, anti-leak;
//     403 when the project is visible but not writable, US-6).
//  3. Decode the optional {reason} body.
//  4. ensureIssueInProject — the path issue must belong to the path
//     project, else 404 issue_not_found (sk-194.6.50).
//  5. Load comment meta; verify issue + project membership (404 mismatch).
//  6. AUTHOR-OR-OWNER gate: caller is the comment author OR an
//     admin/owner of the project (authz.ActionDeleteAnyComment on the
//     level resolved in step 2 — the owner's rights, an instance admin's
//     included, US-6). Otherwise 403 forbidden.
//  7. Store.DeleteComment soft-deletes (idempotent) + emits
//     comment.deleted. The body stays in the DB; ListComments returns
//     the row as a blanked tombstone.
//
// Returns 200 with the tombstone DTO so the client can update in place.
func (h *IssuesHandler) DeleteComment(w http.ResponseWriter, r *http.Request) {
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
	commentID, err := parseUUIDParam(r, "commentID")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	accessLevel, err := projects.EnsureProjectAction(ctx, h.pool, pid, c.UserID, authz.ActionWrite)
	if err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Reason is optional; an empty body is the common UI case. Cap the
	// body so a malformed/huge payload can't open the tx downstream.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req deleteCommentRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
			return
		}
	}

	// Object-level gate on the PATH issue, before the comment row is
	// resolved — see UpdateComment for the reasoning.
	if !ensureIssueInProject(ctx, w, h.issuesStore, issueID, pid) {
		return
	}

	meta, err := h.issuesStore.GetCommentMeta(ctx, commentID)
	if err != nil {
		if errors.Is(err, issues.ErrCommentNotFound) {
			writeProblem(w, http.StatusNotFound, "comment_not_found", "comment not found", nil)
			return
		}
		slog.ErrorContext(ctx, "load comment meta failed", "commentId", commentID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if meta.IssueID != issueID || meta.ProjectID != pid {
		writeProblem(w, http.StatusNotFound, "comment_not_found", "comment not found", nil)
		return
	}
	// AUTHOR-OR-OWNER: author always passes; otherwise require the
	// project owner's rights (an instance admin has them, US-6, sk-741).
	isAuthor := meta.AuthorUserID != nil && *meta.AuthorUserID == c.UserID
	isOwner := authz.Allows(accessLevel, authz.ActionDeleteAnyComment)
	if !isAuthor && !isOwner {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"only the comment author, the project owner, the space owner or the instance admin can delete this comment", nil)
		return
	}

	if err := h.issuesStore.DeleteComment(ctx, commentID, strings.TrimSpace(req.Reason), issueActor(c)); err != nil {
		if errors.Is(err, issues.ErrCommentNotFound) {
			writeProblem(w, http.StatusNotFound, "comment_not_found", "comment not found", nil)
			return
		}
		// sk-303.17 review: unlike AddComment/UpdateComment above, this
		// handler never called mapIssueErr at all — every non-not-found
		// error, version_conflict and serialization_retry included, fell
		// straight to the generic 500 below. DeleteComment can now return
		// issues.ErrSerializationRetry on an exhausted retry budget; route
		// it (and any future mapIssueErr-handled error) through the same
		// shared mapper AddComment/UpdateComment already use.
		if mapIssueErr(w, err) {
			return
		}
		slog.ErrorContext(ctx, "delete comment failed", "commentId", commentID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// Return the tombstone DTO (blanked body) so the client can replace
	// the row in place. Re-load via the list path's single-comment shape:
	// fetch meta is not enough (no version / timestamps), so we re-read
	// the row through GetCommentMeta + a lightweight projection. Simpler:
	// return a minimal tombstone built from meta.
	writeJSON(w, http.StatusOK, deleteCommentResponse{
		OK:      true,
		ID:      commentID,
		IssueID: meta.IssueID,
		Deleted: true,
	})
}

// deleteCommentResponse is the 200 body for DeleteComment — typed so we
// avoid map[string]any boxing on the hot response path.
type deleteCommentResponse struct {
	OK      bool      `json:"ok"`
	ID      uuid.UUID `json:"id"`
	IssueID string    `json:"issueId"`
	Deleted bool      `json:"deleted"`
}

// checkDuplicatesRequest is the wire shape for POST /check-duplicates.
type checkDuplicatesRequest struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// checkDuplicatesResponse is the wire shape for /check-duplicates.
//
// `degraded` mirrors skein_search's flag and skein_create's (sk-228):
// true means the semantic layer could not be consulted, so an empty
// `candidates` is "we could not compare", not "we compared and found
// nothing". `degradedReason` is prose for a human; branch on `degraded`.
type checkDuplicatesResponse struct {
	Candidates     []dependencies.DuplicateCandidate `json:"candidates"`
	Degraded       bool                              `json:"degraded"`
	DegradedReason string                            `json:"degradedReason,omitempty"`
}

// CheckDuplicates handles POST /api/v1/projects/{projectID}/issues/check-duplicates.
//
// Two-layer duplicate detection (pg_trgm on the title + semantic vector
// rerank) against the supplied project — the same dependencies.Store
// path skein_create's gate uses, so the UI hint and the MCP refusal can
// never disagree. Does NOT create an issue, does NOT write events. The
// endpoint exists to power "did you mean ...?" hints in the create-issue
// UI before the user actually submits.
//
// Response carries `degraded` (sk-228): when true the semantic layer was
// unavailable and an empty `candidates` means "not compared", not
// "compared and clean".
//
// Permission: caller must see the project (route group enforces
// RequireUser; we add EnsureProjectVisible to surface 404 on the
// "exists-but-hidden" branch instead of leaking the project's existence).
func (h *IssuesHandler) CheckDuplicates(w http.ResponseWriter, r *http.Request) {
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
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
		if mapAccessErr(w, err) {
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	var req checkDuplicatesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	title, err := issues.ValidateTitle(req.Title)
	if err != nil {
		mapIssueValidation(w, err)
		return
	}

	report, err := h.depsStore.CheckDuplicates(ctx, pid, title, req.Description)
	if err != nil {
		slog.ErrorContext(ctx, "check duplicates failed",
			"projectId", pid.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, checkDuplicatesResponse{
		Candidates:     report.Candidates,
		Degraded:       report.Degraded,
		DegradedReason: report.DegradedReason,
	})
}

// Accept handles POST /api/v1/issues/{issueID}/accept.
//
// Phase 2 of the ui-redesign acceptance flow: explicit ack from the
// current assignee that the task has been seen and is being taken.
// Sets accepted_at = now() and accepted_by = caller, bumps version,
// writes one issue.accepted event, and fans out a notification to
// the issue's creator + watchers.
//
// Permission: caller MUST be the issue's current assignee. Only the
// assignee accepts; the project owner's rights and role=admin do not let
// anyone accept for another (US-6, sk-741) — everyone else gets 422
// forbidden, otherwise non-assignees could pre-accept tasks for others.
//
// Idempotent: re-calling on an already-accepted issue is a silent
// no-op — returns 200 with the current IssueDetail without writing
// a new event row or bumping version.
//
// Body (all fields optional — empty body is OK for UI flows):
//
//	{ "expectedVersion": 7 }
//
// Path is non-project-scoped (the issue id alone identifies the row);
// the handler resolves project_id from the issues table and runs the
// standard EnsureProjectVisible check before delegating to the store.
//
// Errors (with stable codes — see the project guidelines, "HTTP codes
// for business errors"):
//   - 404 issue_not_found       — issue missing or not visible
//   - 422 cannot_accept_closed  — issue is in status=closed (sk-25)
//   - 422 forbidden             — caller is not the assignee (owner
//     rights and role=admin do not count)
//   - 422 version_conflict      — expectedVersion mismatch
//   - 401 unauthenticated       — cookie missing
//   - 500 internal_error        — DB / store fault
func (h *IssuesHandler) Accept(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}

	// Empty body is allowed; decodeJSON returns an "empty body" error
	// in that case which we treat as the zero-value request shape.
	// A separate read of r.ContentLength would also work but the
	// shorter path is to attempt the decode and tolerate EOF.
	var req acceptIssueRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
			return
		}
	}

	// Resolve project from issue id so we can run visibility check.
	// We deliberately do not lean on store.GetIssue here — it does
	// extra side queries (components, labels, comments count) that
	// the access gate doesn't need.
	issue, err := h.issuesStore.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "accept: load issue failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, h.pool, issue.ProjectID, c.UserID); vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			// 404 — do not leak project existence to non-members.
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "accept: visibility check failed",
			"issueId", issueID, "error", vErr.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	dto, err := h.issuesStore.Accept(ctx, issues.AcceptInput{
		ID:              issueID,
		ExpectedVersion: req.ExpectedVersion,
		Actor:           issueActor(c),
	})
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		if errors.Is(err, issues.ErrCannotAcceptClosed) {
			writeProblem(w, http.StatusUnprocessableEntity, "cannot_accept_closed",
				"cannot accept a closed issue — reopen it first", nil)
			return
		}
		if errors.Is(err, issues.ErrForbidden) {
			writeProblem(w, http.StatusUnprocessableEntity, "forbidden",
				"only the issue's assignee can accept", nil)
			return
		}
		var verr *issues.ErrVersionConflict
		if errors.As(err, &verr) {
			writeProblem(w, http.StatusUnprocessableEntity, "version_conflict",
				"the resource was modified by another writer",
				map[string]any{"currentVersion": verr.CurrentVersion})
			return
		}
		slog.ErrorContext(ctx, "accept failed", "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// Dismiss handles POST /api/v1/issues/{issueID}/dismiss.
//
// sk-34 — caller opts out of the issue's Delegated lens. Side effects:
//
//   - issues.dismissed_by += caller (idempotent — re-call is no-op)
//   - the caller's subscription is stopped as by DELETE /watch
//     (sk-622.21, US-1): IF the caller has one, it is deleted with an
//     opt-out, so the automatic paths do not re-subscribe them; without
//     one nothing is recorded (R3). Either way the issue's Watching card
//     is cleared
//   - issue.delegation_dropped event written, audit-only (no FanOut —
//     broadcasting the drop would undo the bell-silence we just set)
//
// Visibility check is the standard EnsureProjectVisible against the
// issue's project_id.
//
// The "Drop from Delegated" affordance in the new shell calls this
// endpoint; the symmetric Undismiss below takes the caller out of
// dismissed_by only — it does NOT re-add a watcher nor remove the opt-out
// (that's a separate user intent surfaced through
// skein_watchers(action="watch") / POST /watch).
//
// Errors:
//   - 404 issue_not_found    — issue missing or not visible
//   - 401 unauthenticated    — cookie missing
//   - 422 not_in_chain       — caller is neither the creator nor a past assignee
//   - 500 internal_error     — DB / store fault
//
// @Summary Drop an issue from the caller's Delegated lens (and stop watching it)
// @Description Declines the CALLER's part in a delegation chain (sk-34; watch side reworked by sk-622.21, US-1): "I handed this task on and do not want to track it". Only the issue's creator or one of its previous assignees may drop it.
// @Description
// @Description Algorithm: authenticate -> load the issue (absent -> 404) -> check the caller can see its project (not visible -> 404, answered as absent) -> one SERIALIZABLE transaction (a serialization conflict or deadlock is replayed, up to 3 attempts in all; a conflict that outlives the attempts is answered 500 internal_error and nothing is written): check the caller is in the chain (creator or previous assignee, else 422 not_in_chain) -> run the explicit unwatch -> if the caller was not yet in dismissed_by, append them and write the event -> return the reloaded card.
// @Description
// @Description Explicit unwatch, exactly as DELETE /api/v1/issues/{issueID}/watch: IF the caller is subscribed to the issue (any reasons, manual included), the subscription is deleted and an opt-out is recorded — the automatic paths (assignment, reopen) will not subscribe the caller again; if the caller is NOT subscribed (e.g. auto-unsubscribed when the issue closed), no opt-out is recorded, so a later reopen or assignment subscribes them again. In both cases every notification of the caller whose Watching card is this issue, archived ones included, loses the card.
// @Description
// @Description Side effects: the caller's user id joins issues.dismissed_by, so the issue leaves the caller's Delegated lens; an issue.delegation_dropped event is written for the audit trail only — no fan-out, no notifications (broadcasting the drop would undo the silence). Idempotent: when the caller is already in dismissed_by there is no event and no version bump, but the explicit unwatch still runs — a subscription made by hand after an earlier drop is stopped again.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param issueID path string true "Full hierarchical issue id, e.g. sk-42 or sk-42.1"
// @Success 200 {object} issues.IssueDetail "The issue card after the drop"
// @Failure 400 {object} map[string]any "missing_issue_id"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 404 {object} map[string]any "issue_not_found — absent, or in a project the caller cannot see; nothing written"
// @Failure 422 {object} map[string]any "not_in_chain — the caller is neither the creator nor a previous assignee; nothing written"
// @Failure 500 {object} map[string]any "internal_error — a DB fault, or a serialization conflict that outlived the 3 attempts"
// @Router /api/v1/issues/{issueID}/dismiss [post]
func (h *IssuesHandler) Dismiss(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	issue, err := h.issuesStore.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "dismiss: load issue failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, h.pool, issue.ProjectID, c.UserID); vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "dismiss: visibility check failed",
			"issueId", issueID, "error", vErr.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	dto, err := h.issuesStore.DismissDelegated(ctx, issues.DelegationActionInput{
		ID:    issueID,
		Actor: issueActor(c),
	})
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		// sk-34 review Go-W2: chain-membership rejection. Surface as
		// 422 validation_error so the frontend can branch on the code
		// rather than treating it as a 500.
		if vErr, ok := issues.IsValidationError(err); ok {
			writeProblem(w, http.StatusUnprocessableEntity, vErr.Code, vErr.Message, nil)
			return
		}
		slog.ErrorContext(ctx, "dismiss failed",
			"issueId", issueID, "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// Undismiss handles POST /api/v1/issues/{issueID}/undismiss.
//
// sk-34 — symmetric inverse of Dismiss. Removes caller's user_id from
// issues.dismissed_by, emits issue.delegation_resumed. Does NOT re-add
// a watcher row — the unsubscribe done by Dismiss (and its opt-out, when
// one was recorded) persists until they explicitly call POST /watch
// (skein_watchers(action="watch")).
//
// Idempotent: caller not in dismissed_by → silent no-op.
//
// Errors:
//   - 404 issue_not_found    — issue missing or not visible
//   - 401 unauthenticated    — cookie missing
//   - 500 internal_error     — DB / store fault
//
// @Summary Bring an issue back into the caller's Delegated lens
// @Description The inverse of POST /api/v1/issues/{issueID}/dismiss (sk-34): "show me this delegated task again". Algorithm: authenticate -> load the issue (absent -> 404) -> check the caller can see its project (not visible -> 404, answered as absent) -> one SERIALIZABLE transaction, single attempt (a serialization conflict is answered 500 internal_error, nothing written): if the caller is in issues.dismissed_by, remove them and write an issue.delegation_resumed event -> return the reloaded card.
// @Description
// @Description Watching is NOT restored: the subscription stopped by the drop stays stopped and an opt-out it recorded stays in place — watching again is a separate intent, POST /api/v1/issues/{issueID}/watch (or MCP skein_watchers action=watch), which also removes the opt-out.
// @Description
// @Description Side effects: the event is audit-only — no fan-out, no notifications. Idempotent: a caller not in dismissed_by gets the current card, with no event and no version bump.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param issueID path string true "Full hierarchical issue id, e.g. sk-42 or sk-42.1"
// @Success 200 {object} issues.IssueDetail "The issue card after the resume"
// @Failure 400 {object} map[string]any "missing_issue_id"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 404 {object} map[string]any "issue_not_found — absent, or in a project the caller cannot see; nothing written"
// @Failure 500 {object} map[string]any "internal_error — a DB fault or a serialization conflict"
// @Router /api/v1/issues/{issueID}/undismiss [post]
func (h *IssuesHandler) Undismiss(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueID")
	if issueID == "" {
		writeProblem(w, http.StatusBadRequest, "missing_issue_id", "issue id is required", nil)
		return
	}
	issue, err := h.issuesStore.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "undismiss: load issue failed",
			"issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, h.pool, issue.ProjectID, c.UserID); vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "undismiss: visibility check failed",
			"issueId", issueID, "error", vErr.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	dto, err := h.issuesStore.ResumeDelegated(ctx, issues.DelegationActionInput{
		ID:    issueID,
		Actor: issueActor(c),
	})
	if err != nil {
		if errors.Is(err, issues.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "issue_not_found", "issue not found", nil)
			return
		}
		slog.ErrorContext(ctx, "undismiss failed",
			"issueId", issueID, "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// requireForceUnlock answers a bypass of the claim guards (force=true) for a
// level already resolved on the issue's project (sk-674.18 T034, K-28):
// force_unlock of US-6 — the project owner, the space owner, the instance
// admin. Otherwise 422 force_unlock_forbidden, the same code as MCP, and
// false. One rule for every REST path with force.
func requireForceUnlock(w http.ResponseWriter, level projects.AccessLevel) bool {
	if authz.Allows(level, authz.ActionForceUnlock) {
		return true
	}
	writeProblem(w, http.StatusUnprocessableEntity, spaces.CodeForceUnlockForbidden,
		"releasing another holder's claim needs the project owner, the space owner or the instance admin", nil)
	return false
}
