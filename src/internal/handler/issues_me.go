package handler

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
)

// IssuesMeHandler hosts the /api/v1/me/assigned-by, /me/assigned-to and
// /me/created-by endpoints. Read-only, caller-scoped, no path params
// (everything is driven by the auth context). Lives in handler so it
// can reuse caller() + writeJSON / writeProblem helpers.
type IssuesMeHandler struct {
	pool        *pgxpool.Pool
	issuesStore *issues.Store
}

// NewIssuesMeHandler builds the handler — pool + issues store are
// injected by main.go to share with the rest of the issues stack.
func NewIssuesMeHandler(pool *pgxpool.Pool, issuesStore *issues.Store) *IssuesMeHandler {
	return &IssuesMeHandler{pool: pool, issuesStore: issuesStore}
}

// AssignedBy handles GET /api/v1/me/assigned-by — issues created by the
// caller across every project they can see. Same wire surface as
// CreatedBy (the predicate is identical: i.created_by = caller); kept
// as a distinct route for backwards compatibility with R3.3 callers.
//
// Optional query filters (Phase 2 ui-redesign — B7):
//   - includeClosed=true  — surface closed issues (default: open-only).
//   - status=a,b          — multi-select status filter; takes precedence
//     over includeClosed when both are present.
//   - acceptedAt=null     — restrict to issues whose assignee hasn't
//     accepted yet ("My delegated, awaiting ack" view).
//   - origin=human-driven|agent-autonomous|all
//     — origin grouping (matches ListIssues).
//   - unreviewed=true     — unreviewed filter (sk-303.14, contract C3):
//     closed results the caller created, delegated, and has not yet
//     marked as reviewed. Membership is defined by review.UnreviewedPredicate —
//     the single definition of the set (INV-3). The filter LIFTS the default
//     "no closed" (the set is made of closed issues); an explicit `status`
//     still wins. Breadth is strictly authorship: issues where the caller
//     is only the previous assignee do not enter the filter (INV-13, SC-018).
//     Any value other than the literal "true" is 400.
//
// Cursor-paginated; same shape as the project-scoped list.
//
// @Summary List issues the caller created, across every visible project
// @Description Returns a keyset-paginated page of the issues whose creator is the caller, across every project they can see. Business meaning: the cross-project «what I originated» surface; the visibility horizon is applied server-side, so the answer never contains a project the caller may not see.
// @Description
// @Description Algorithm: authenticate -> resolve the caller's visible-project set (an org-admin gets «no project filter») -> translate the query string into one selection, refusing any unrecognised value with 400 rather than ignoring it -> run one query -> return the page plus the next cursor. The default is OPEN-ONLY; `includeClosed` and `status` widen it.
// @Description
// @Description The unreviewed selection (sk-303.14): `unreviewed=true` narrows the answer to closed results the caller created, delegated to somebody else and has not yet marked as read. Membership comes from ONE definition shared with the second delegated counter and with the reminder selection, so list, number and reminders cannot disagree. The selection LIFTS the open-only default by itself, because the set consists of closed issues; an explicit `status` still wins. Its reach is strictly authorial: issues where the caller is merely a PREVIOUS assignee are excluded, unlike the wider delegated surface — otherwise the list would carry issues the caller has no right to mark. Any value other than the literal "true" is refused with 400.
// @Description
// @Description Close outcome (sk-489): a row whose issue is CLOSED carries `closeOutcome` — one of done, wont_do, duplicate, superseded — the machine-readable answer to «how did it end» that the free-text close reason never gave. On an issue that is not closed the KEY IS ABSENT rather than empty. Rows of epic-typed issues also carry `epicProgress`, which since sk-489 has three further legs — `done`, `noWork`, `counted` — beside the unchanged closed/total/unavailable; progress is done/counted.
// @Description
// @Description Row freshness order (sk-622.21, US-15): every row carries `rowSeq` — the issue's freshness order, taken from a global sequence on every committed write of the issue row, including writes that do not move `version` (label add/remove, component replace). For ONE issue a greater rowSeq is a later committed state; values of different issues are not comparable. A phase-2 client substitutes an event's row into a loaded row only if the event row's rowSeq is greater than the one shown. An issue not written since the phase-2 rollout (migration 000064) has no rowSeq key (absent ≡ 0, order unknown); any event row with a positive rowSeq is fresher than it.
// @Description
// @Description Side effects: none — a pure read.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param space query string false "Optional space short id; only issues of that space's projects (the space of the issue's project). An unknown or unseen space yields nothing"
// @Param includeClosed query bool false "Surface closed issues too; an explicit status wins over it. The unreviewed selection lifts it by itself"
// @Param unreviewed query string false "Only the literal true: narrow to the caller's unreviewed set (sk-303.14). Refused on /me/assigned-to" Enums(true)
// @Param status query string false "Comma-separated statuses; every element must be a known status"
// @Param acceptedAt query string false "null (not yet accepted) or not-null (accepted)" Enums(null, not-null)
// @Param createdBy query string false "me or not-me" Enums(me, not-me)
// @Param origin query string false "Origin grouping" Enums(human-driven, agent-autonomous, all)
// @Param isBlocked query bool false "Blocked only / unblocked only. Blocked = an open, in-progress or deferred blocks / waits-for target in a project the caller can see; blockers outside the caller's horizon never count (sk-423) — the same rule as item.isBlocked"
// @Param cursor query string false "Keyset cursor from the previous page"
// @Param limit query int false "Page size"
// @Success 200 {object} issues.ListResult "One page plus the next cursor"
// @Failure 400 {object} map[string]any "validation_error — an unrecognised value for any filter, including an unreviewed other than true; invalid_isBlocked; invalid_limit"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 422 {object} map[string]any "blocked_is_derived — status=blocked is a DERIVED state and cannot be selected as a status; use isBlocked=true. Raised by issues.ValidateStatus inside the shared runMine driver, through mapIssueValidation"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/me/assigned-by [get]
func (h *IssuesMeHandler) AssignedBy(w http.ResponseWriter, r *http.Request) {
	h.runMine(w, r, issues.ModeAssignedBy)
}

// AssignedTo handles GET /api/v1/me/assigned-to — issues whose assignee
// resolves (case-insensitive username match) to the caller. Same paging
// + filter shape as AssignedBy.
//
// Phase 2 ui-redesign Inbox surface: combine acceptedAt=null + status=open
// to produce the "needs my acknowledgement" set across projects.
//
// `unreviewed` is NOT accepted here (400 validation_error): the unreviewed
// set is work the caller created and handed to someone else, and this
// route filters by "assignee is the caller". See runMine.
//
// @Summary List issues assigned to the caller, across every visible project
// @Description Returns a keyset-paginated page of the issues whose assignee resolves to the caller (case-insensitive username match), across every project they can see. Business meaning: the cross-project inbox surface — combine `acceptedAt=null` with `status=open` to get «needs my acknowledgement».
// @Description
// @Description Algorithm: identical to /me/assigned-by apart from the scope predicate — authenticate -> resolve the visible-project set -> translate the query string, refusing unrecognised values -> one query -> page plus cursor. The default is OPEN-ONLY.
// @Description
// @Description `unreviewed` is REFUSED here with 400 (sk-303.14), and the refusal is the contract rather than an omission: the unreviewed set is work the caller CREATED AND GAVE AWAY, while this route selects by «the assignee is the caller». Accepting the key would answer a question about a different set with the same word.
// @Description
// @Description Close outcome (sk-489): a row whose issue is CLOSED carries `closeOutcome` — one of done, wont_do, duplicate, superseded — the machine-readable answer to «how did it end» that the free-text close reason never gave. On an issue that is not closed the KEY IS ABSENT rather than empty. Rows of epic-typed issues also carry `epicProgress`, which since sk-489 has three further legs — `done`, `noWork`, `counted` — beside the unchanged closed/total/unavailable; progress is done/counted.
// @Description
// @Description Row freshness order (sk-622.21, US-15): every row carries `rowSeq` — the issue's freshness order, taken from a global sequence on every committed write of the issue row, including writes that do not move `version` (label add/remove, component replace). For ONE issue a greater rowSeq is a later committed state; values of different issues are not comparable. A phase-2 client substitutes an event's row into a loaded row only if the event row's rowSeq is greater than the one shown. An issue not written since the phase-2 rollout (migration 000064) has no rowSeq key (absent ≡ 0, order unknown); any event row with a positive rowSeq is fresher than it.
// @Description
// @Description Side effects: none — a pure read.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param includeClosed query bool false "Surface closed issues too; an explicit status wins over it. The unreviewed selection lifts it by itself"
// @Param unreviewed query string false "Only the literal true: narrow to the caller's unreviewed set (sk-303.14). Refused on /me/assigned-to" Enums(true)
// @Param status query string false "Comma-separated statuses; every element must be a known status"
// @Param acceptedAt query string false "null (not yet accepted) or not-null (accepted)" Enums(null, not-null)
// @Param createdBy query string false "me or not-me" Enums(me, not-me)
// @Param origin query string false "Origin grouping" Enums(human-driven, agent-autonomous, all)
// @Param isBlocked query bool false "Blocked only / unblocked only. Blocked = an open, in-progress or deferred blocks / waits-for target in a project the caller can see; blockers outside the caller's horizon never count (sk-423) — the same rule as item.isBlocked"
// @Param cursor query string false "Keyset cursor from the previous page"
// @Param limit query int false "Page size"
// @Success 200 {object} issues.ListResult "One page plus the next cursor"
// @Failure 400 {object} map[string]any "validation_error — an unrecognised value for any filter, including an unreviewed other than true; invalid_isBlocked; invalid_limit"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 422 {object} map[string]any "blocked_is_derived — status=blocked is a DERIVED state and cannot be selected as a status; use isBlocked=true. Raised by issues.ValidateStatus inside the shared runMine driver, through mapIssueValidation"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/me/assigned-to [get]
func (h *IssuesMeHandler) AssignedTo(w http.ResponseWriter, r *http.Request) {
	h.runMine(w, r, issues.ModeAssignedTo)
}

// CreatedBy handles GET /api/v1/me/created-by — issues whose
// created_by column equals the caller, across every project they can
// see. Powers the cross-project Outbox view in the Phase 2 ui-redesign
// (Sc2): "tasks I originated".
//
// Same scope predicate as AssignedBy (the codebase records the
// originator on i.created_by); the two routes coexist so the UI can
// pick the URL that matches the surface name without an extra
// ?mode= knob. Same filter shape as AssignedBy.
//
// @Summary List issues the caller created (cross-project outbox surface)
// @Description Returns a keyset-paginated page of the issues whose `created_by` is the caller, across every project they can see. Business meaning: the cross-project outbox view — «tasks I originated». The scope predicate is the same as /me/assigned-by; the two routes coexist so a surface can use the URL that matches its name without an extra mode parameter.
// @Description
// @Description Algorithm: authenticate -> resolve the caller's visible-project set -> translate the query string into one selection, refusing any unrecognised value with 400 -> one query -> page plus cursor. The default is OPEN-ONLY.
// @Description
// @Description The unreviewed selection (sk-303.14): `unreviewed=true` narrows the answer to closed results the caller created, delegated to somebody else and has not yet marked as read. Membership comes from ONE definition shared with the second delegated counter and with the reminder selection, so list, number and reminders cannot disagree. The selection LIFTS the open-only default by itself, because the set consists of closed issues; an explicit `status` still wins. Its reach is strictly authorial: issues where the caller is merely a PREVIOUS assignee are excluded, unlike the wider delegated surface — otherwise the list would carry issues the caller has no right to mark. Any value other than the literal "true" is refused with 400.
// @Description
// @Description This is the CROSS-PROJECT half of the unreviewed selection; the project-scoped half is `GET /api/v1/projects/{projectID}/issues?unreviewed=true`. Both consume the same predicate.
// @Description
// @Description Close outcome (sk-489): a row whose issue is CLOSED carries `closeOutcome` — one of done, wont_do, duplicate, superseded — the machine-readable answer to «how did it end» that the free-text close reason never gave. On an issue that is not closed the KEY IS ABSENT rather than empty. Rows of epic-typed issues also carry `epicProgress`, which since sk-489 has three further legs — `done`, `noWork`, `counted` — beside the unchanged closed/total/unavailable; progress is done/counted.
// @Description
// @Description Row freshness order (sk-622.21, US-15): every row carries `rowSeq` — the issue's freshness order, taken from a global sequence on every committed write of the issue row, including writes that do not move `version` (label add/remove, component replace). For ONE issue a greater rowSeq is a later committed state; values of different issues are not comparable. A phase-2 client substitutes an event's row into a loaded row only if the event row's rowSeq is greater than the one shown. An issue not written since the phase-2 rollout (migration 000064) has no rowSeq key (absent ≡ 0, order unknown); any event row with a positive rowSeq is fresher than it.
// @Description
// @Description Side effects: none — a pure read.
// @Tags Issues
// @Produce json
// @Security ApiKeyAuth
// @Param includeClosed query bool false "Surface closed issues too; an explicit status wins over it. The unreviewed selection lifts it by itself"
// @Param unreviewed query string false "Only the literal true: narrow to the caller's unreviewed set (sk-303.14). Refused on /me/assigned-to" Enums(true)
// @Param status query string false "Comma-separated statuses; every element must be a known status"
// @Param acceptedAt query string false "null (not yet accepted) or not-null (accepted)" Enums(null, not-null)
// @Param createdBy query string false "me or not-me" Enums(me, not-me)
// @Param origin query string false "Origin grouping" Enums(human-driven, agent-autonomous, all)
// @Param isBlocked query bool false "Blocked only / unblocked only. Blocked = an open, in-progress or deferred blocks / waits-for target in a project the caller can see; blockers outside the caller's horizon never count (sk-423) — the same rule as item.isBlocked"
// @Param cursor query string false "Keyset cursor from the previous page"
// @Param limit query int false "Page size"
// @Success 200 {object} issues.ListResult "One page plus the next cursor"
// @Failure 400 {object} map[string]any "validation_error — an unrecognised value for any filter, including an unreviewed other than true; invalid_isBlocked; invalid_limit"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 422 {object} map[string]any "blocked_is_derived — status=blocked is a DERIVED state and cannot be selected as a status; use isBlocked=true. Raised by issues.ValidateStatus inside the shared runMine driver, through mapIssueValidation"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/me/created-by [get]
func (h *IssuesMeHandler) CreatedBy(w http.ResponseWriter, r *http.Request) {
	h.runMine(w, r, issues.ModeCreatedBy)
}

// runMine is the shared driver — the three endpoints differ only in the
// mode passed to issues.Store.ListByUser. Query parsing is centralised
// here so the validation rules stay in lockstep with the project-scoped
// /projects/{pid}/issues path.
func (h *IssuesMeHandler) runMine(w http.ResponseWriter, r *http.Request, mode string) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	// Visibility horizon — the caller's scope (authz): rows only of the
	// projects the access relation grants. This is the ONLY mechanism that
	// stops the /me/created-by endpoint from leaking issues whose
	// created_by happens to match the caller in projects they were later
	// removed from (or were never a member of via legacy data).
	q := r.URL.Query()
	args := issues.ListByUserArgs{
		UserID: c.UserID,
		Mode:   mode,
		Scope:  authz.FromContext(ctx),
		// sk-674.18 T032: the page filter — one space's projects.
		Space: strings.TrimSpace(q.Get("space")),
		// sk-34: CallerUsername drives the extended Delegated chain
		// predicate in ModeAssignedBy / ModeCreatedBy. Empty username
		// degrades the store to the strict pre-sk-34 predicate.
		CallerUsername: c.Username,
	}
	if raw := q.Get("includeClosed"); raw == "true" {
		args.IncludeClosed = true
	}
	// sk-303.14 T012 — unreviewed filter (contract C3), the cross-project
	// path of the delegated surface.
	//
	// A separate parameter, not a value of `status`: the status set is closed
	// and checked by ValidateStatus, and "unreviewed" is not an issue state
	// but its relation to the caller (FOR-PLAN 5). The value is only the
	// literal "true"; anything else is rejected, not interpreted, so a typo
	// does not look like "the filter ran and found nothing".
	//
	//
	// The filter exists ONLY on the two paths of the delegated surface
	// (/me/assigned-by, /me/created-by). On /me/assigned-to the store
	// forcibly sets "assignee is the caller", and the single set predicate
	// requires the opposite: assignee is set and is NOT the caller
	// (INV-13). The intersection is identically empty, for any data.
	// Accepting the parameter there would, first, present a call error as
	// a correct empty response, and second — introduce a third transport
	// consumer of the set with an extra condition on top, exactly what
	// INV-3 avoids. So not "empty list", but a request-shape refusal.
	if raw := q.Get("unreviewed"); raw != "" {
		if mode == issues.ModeAssignedTo {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"unreviewed is not supported on /me/assigned-to: the unreviewed set is "+
					"work the caller created and delegated to someone else — use "+
					"/me/created-by or /me/assigned-by", nil)
			return
		}
		if raw != "true" {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"unreviewed accepts only the literal value \"true\"", nil)
			return
		}
		args.Unreviewed = true
	}
	// Phase 2 ui-redesign — B7: same parsing rules as IssuesHandler.List
	// for the project-scoped path, so query strings produced by the same
	// frontend code work on both endpoints.
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
	// sk-30: createdBy=not-me carves the "On me" lens on /me/assigned-to
	// (assignee=caller AND created_by != caller). createdBy=me is not
	// meaningful on /me/* paths — the Mode discriminator already routes
	// by creator vs assignee. Only the inverted value is accepted here.
	if raw := q.Get("createdBy"); raw != "" {
		if raw != "not-me" {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"createdBy on /me/* accepts only \"not-me\"", nil)
			return
		}
		id := c.UserID
		args.CreatedByExclude = &id
	}
	if raw := q.Get("origin"); raw != "" {
		switch raw {
		case issues.OriginHumanDriven, issues.OriginAgentAutonomous, issues.OriginAll:
			v := raw
			args.OriginGroup = &v
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"origin must be one of human-driven, agent-autonomous, all", nil)
			return
		}
	}
	// sk-31: derived blocked-ness filter; same shape as other list paths.
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
	if cursor := q.Get("cursor"); cursor != "" {
		args.Cursor = &cursor
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

	res, err := h.issuesStore.ListByUser(ctx, args)
	if err != nil {
		slog.ErrorContext(ctx, "list by user failed",
			"userId", c.UserID.String(), "mode", mode, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
