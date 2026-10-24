package issues

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/assignee"
	"skein-api/internal/authz"
	"skein-api/internal/review"
	"skein-api/internal/status"
)

// Cross-project mode discriminators for ListByUser.
//
// Historical note: ModeAssignedBy uses the same SQL predicate as
// ModeCreatedBy (`i.created_by = caller`). The "assigned_by" name has
// been the wire identifier on /me/assigned-by since R3.3 — kept here
// for backwards compatibility. The Phase 2 ui-redesign (Outbox) added
// /me/created-by with the same scope but a more accurate name; the
// two endpoints share the same predicate and only differ in the URL.
const (
	ModeAssignedBy = "assigned_by"
	ModeAssignedTo = "assigned_to"
	ModeCreatedBy  = "created_by"
)

// ListByUserArgs captures the query for the /me/assigned-by,
// /me/assigned-to and /me/created-by endpoints. Mode discriminates
// the scope predicate:
//
//   - "assigned_by"  → i.created_by = caller (legacy wire name)
//   - "created_by"   → i.created_by = caller (Phase 2 ui-redesign)
//   - "assigned_to"  → resolved assignee user id = caller
//
// Scope is the caller's visibility horizon (authz): the store keeps only
// rows of projects the access relation grants it, so cross-project
// queries respect access.
//
// The endpoint powers cross-project Inbox/Outbox/My-Delegated views in
// the UI redesign (Phase 2 — B5). Filter shape mirrors the project-
// scoped /projects/{pid}/issues path: status set + origin group +
// acceptedAt-null sentinel. Pagination is id-cursor, same as ListIssues.
type ListByUserArgs struct {
	// Space narrows the list to one space's projects (its short id); ""
	// — every space (sk-674.18 T032).
	Space  string
	UserID uuid.UUID
	Mode   string // "assigned_by" | "assigned_to" | "created_by"
	Scope  authz.Scope

	// CallerUsername is the caller's `users.username` (lowercased here
	// at the boundary). Drives the sk-34 Delegated chain predicate in
	// ModeAssignedBy / ModeCreatedBy — when populated, the lens widens
	// to "I'm somewhere in this issue's chain of responsibility" rather
	// than the strict pre-sk-34 "I'm the creator". Empty degrades to
	// the strict predicate.
	//
	// It GATES that widening; the "assigned to somebody other than me"
	// leg inside it resolves the handle through assignee.MatchesCaller on
	// the caller's id, matching all three accepted forms (sk-523).
	CallerUsername string

	// IncludeClosed is the legacy "give me closed issues too" knob (R3.3).
	// Kept for backwards compatibility with existing UI calls. When
	// Statuses is non-empty it takes precedence and IncludeClosed is
	// ignored (the explicit set already covers the open/closed split).
	IncludeClosed bool

	// Statuses is the multi-select status filter. Empty = no filter (subject
	// to IncludeClosed). Non-empty = exact match against the validated set.
	// Phase 2 ui-redesign — B5.
	Statuses []string

	// AcceptedAtNull, when non-nil and true, restricts the result to
	// issues with `accepted_at IS NULL` — the Inbox surface (assignee
	// hasn't acknowledged yet). Pointer so nil = no filter; false is
	// rejected at the handler boundary (only `null` is meaningful).
	// Phase 2 ui-redesign — B5.
	AcceptedAtNull *bool

	// AcceptedAtNotNull is the symmetric inverse of AcceptedAtNull —
	// restricts to `accepted_at IS NOT NULL`. Powers the "On me" lens
	// (assigned-to=caller, created_by != caller, already accepted).
	AcceptedAtNotNull *bool

	// CreatedByExclude restricts to issues whose created_by differs
	// from the supplied user id. In ModeAssignedTo this carves out the
	// "On me" lens: tasks assigned to the caller by SOMEONE ELSE. nil =
	// no filter. Mutually independent from the Mode predicate.
	CreatedByExclude *uuid.UUID

	// OriginGroup maps the wire-level origin filter to a set of
	// issues.source values. Validated at the handler boundary to one of
	// {human-driven, agent-autonomous, all}; the store inlines the
	// matching IN-list:
	//   - "human-driven"     → source IN ('human', 'agent_directed')
	//   - "agent-autonomous" → source IN ('agent_autonomous')
	//   - "all" / nil        → no filter
	// Pointer so nil = no filter. Phase 2 ui-redesign — B5.
	OriginGroup *string

	// IsBlocked filters by the derived blocked-ness flag (sk-31). Nil
	// = no filter. Same EXISTS predicate as the project-scoped list.
	IsBlocked *bool

	// Unreviewed — the unreviewed filter (sk-303.14, contract C3). This
	// is the CROSS-PROJECT path of the Delegated surface: /me/created-by
	// and /me/assigned-by (one predicate, two historical references). The
	// second path — project-scoped — lives in ListIssues.
	//
	// The predicate is taken whole from review.UnreviewedPredicate, the
	// single definition of the set (INV-3); there is no local copy. The
	// filter is a separate query parameter, not a new value in a closed
	// status set (FOR-PLAN 5).
	//
	// LIFTING THE "WITHOUT CLOSED" DEFAULT IS RECORDED HERE, because the
	// default belongs to this path: empty Statuses without IncludeClosed
	// narrows the list to active work. The unreviewed set consists
	// EXACTLY of closed issues, so without the lift the filter would
	// always return empty — silently and inexplicably. The project path
	// has no such default, and there is nothing to lift there (matrix
	// row 54 checks both halves). An explicitly passed Statuses still
	// wins: a person who asked for open unreviewed issues gets empty —
	// that is their own query, not a system default.
	Unreviewed bool

	Cursor *string
	Limit  int
}

// ListByUser runs the caller-scoped issues query for the cross-project
// /me/assigned-by, /me/assigned-to and /me/created-by views. Returns the
// same IssueSummary shape as ListIssues — including AcceptedAt and the
// derived IAssigned flag — so the UI can render Inbox/Outbox rows from a
// single response shape regardless of the source endpoint.
func (s *Store) ListByUser(ctx context.Context, args ListByUserArgs) (*ListResult, error) {
	h, hErr := s.horizon(ctx)
	if hErr != nil {
		return nil, hErr
	}
	if args.Limit < 1 {
		args.Limit = 500
	}
	if args.Limit > 500 {
		args.Limit = 500
	}
	if args.Mode != ModeAssignedBy && args.Mode != ModeAssignedTo && args.Mode != ModeCreatedBy {
		return nil, fmt.Errorf("invalid mode %q", args.Mode)
	}

	clauses := []string{}
	params := []any{}
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}
	// sk-423: the blocked flag is the reader's; bound on first use.
	blockedExpr := readerBlockedBinder(h, addParam)

	switch args.Mode {
	case ModeAssignedBy, ModeCreatedBy:
		// Both modes scope to the caller's Delegated lens. The two
		// endpoints exist for distinct UI surfaces (My delegated vs
		// Outbox) but the underlying SQL predicate is identical.
		//
		// sk-34 — Delegated lens extension. Pre-sk-34 the predicate
		// was the strict `i.created_by = $caller`. After sk-34 the
		// lens widens to "I'm somewhere in this issue's chain of
		// responsibility":
		//
		//   created_by = me
		//   OR (me ∈ previous_assignees
		//       AND me ∉ dismissed_by
		//       AND LOWER(assignee) <> me_username)
		//
		// CallerUsername gates the assignee-mismatch clause; when
		// empty (rare — caller has no username) we degrade to strict
		// so the lens never widens without a username to compare
		// against.
		callerPh := addParam(args.UserID)
		if args.CallerUsername != "" {
			// Trailing `assignee != me` applies to BOTH branches —
			// Delegated is the "tasks not currently on my plate"
			// lens (sk-34 T6).
			clauses = append(clauses, fmt.Sprintf(
				"(i.assignee IS NOT NULL AND NOT %[2]s "+
					"AND (i.created_by = %[1]s "+
					"OR (%[1]s = ANY(i.previous_assignees) "+
					"AND NOT %[1]s = ANY(i.dismissed_by))))",
				callerPh, assignee.MatchesCaller("i", callerPh)))
		} else {
			clauses = append(clauses, fmt.Sprintf("i.created_by = %s", callerPh))
		}
	case ModeAssignedTo:
		// assignee is free-form text on the issue; the issues delivered to
		// the user are those assignee.MatchesCaller names — the same person
		// the row query's LeftJoin brings in as ua. Issues whose assignee
		// doesn't map to a real user by ANY of the three handle forms are
		// silently excluded (consistent with the auto-watch path).
		clauses = append(clauses, assignee.MatchesCaller("i", addParam(args.UserID)))
	}

	// Callers only see rows of the projects their scope sees.
	//
	// Visibility note for ModeCreatedBy: caller might appear as
	// `created_by` on a row in a project they were later removed from.
	// The relation predicate guarantees those rows stay hidden — the
	// caller can only see their own creations within their current
	// access horizon.
	scopeUnrestricted, scopeCaller, hErr := args.Scope.Bind(ctx, s.pool)
	if hErr != nil {
		return nil, hErr
	}
	clauses = append(clauses, authz.ProjectVisibleExpr("i", addParam(scopeUnrestricted), addParam(scopeCaller)))
	// sk-674.18 T032: the page filter `space=` (the issue's project's space).
	if args.Space != "" {
		clauses = append(clauses, fmt.Sprintf(
			"i.project_id IN (SELECT pj_s.id FROM projects pj_s JOIN spaces sp_s ON sp_s.id = pj_s.space_id WHERE sp_s.short_id = %s)",
			addParam(args.Space)))
	}

	// Status filter precedence: explicit Statuses set wins over the
	// legacy IncludeClosed flag. When Statuses is empty and IncludeClosed
	// is false, retain the historical "open by default" behaviour.
	if len(args.Statuses) > 0 {
		clauses = append(clauses, fmt.Sprintf("i.status = ANY(%s)", addParam(args.Statuses)))
	} else if !args.IncludeClosed && !args.Unreviewed {
		// sk-303.14 T012 — the third term of the condition IS the lift of
		// the default. The unreviewed set consists of closed issues: a
		// leftover "active work only" default would cut it out entirely,
		// and the filter would silently return empty.
		clauses = append(clauses, status.ActiveWork("i"))
	}

	if args.Unreviewed {
		// sk-303.14 T012 — the unreviewed filter, cross-project path.
		// The single definition of the set; this path does not invent
		// its own predicate (INV-3). The fragment is strictly authorial,
		// so intersection with the widened Delegated lens (the previous-
		// assignee chain, sk-34) leaves only what the caller created —
		// INV-13 / SC-018.
		clauses = append(clauses, review.UnreviewedPredicate(addParam, review.UnreviewedArgs{
			Alias:    "i",
			CallerID: args.UserID,
		}))
	}

	if args.AcceptedAtNull != nil && *args.AcceptedAtNull {
		clauses = append(clauses, "i.accepted_at IS NULL")
	}
	if args.AcceptedAtNotNull != nil && *args.AcceptedAtNotNull {
		clauses = append(clauses, "i.accepted_at IS NOT NULL")
	}
	if args.CreatedByExclude != nil {
		clauses = append(clauses, fmt.Sprintf("i.created_by <> %s", addParam(*args.CreatedByExclude)))
	}
	if args.OriginGroup != nil {
		switch *args.OriginGroup {
		case OriginHumanDriven:
			clauses = append(clauses, fmt.Sprintf(
				"i.source = ANY(%s)",
				addParam([]string{SourceHuman, SourceAgentDirected})))
		case OriginAgentAutonomous:
			clauses = append(clauses, fmt.Sprintf(
				"i.source = %s",
				addParam(SourceAgentAutonomous)))
		case OriginAll:
			// Explicit "no filter" sentinel — drop through.
		}
	}
	if args.IsBlocked != nil {
		if *args.IsBlocked {
			clauses = append(clauses, blockedExpr())
		} else {
			clauses = append(clauses, "NOT "+blockedExpr())
		}
	}

	// Snapshot the where-without-cursor clauses for the COUNT query
	// before adding the cursor clause (count is unfiltered by cursor).
	countClauses := append([]string(nil), clauses...)
	countParams := append([]any(nil), params...)

	if args.Cursor != nil && *args.Cursor != "" {
		// Cursor format mirrors ListIssues: opaque string with timestamp
		// + id. We decode and apply the same tuple comparator. The
		// helper sits in the same package so we reuse EncodeCursor /
		// DecodeCursor.
		t, idStr, err := DecodeCursor(*args.Cursor)
		if err != nil {
			return nil, fmt.Errorf("decoding cursor: %w", err)
		}
		clauses = append(clauses, fmt.Sprintf(
			"(i.updated_at, i.id) < (%s, %s)",
			addParam(t), addParam(idStr),
		))
	}

	limitPh := addParam(args.Limit + 1)

	// IAssigned is "caller created the issue and assigned it to someone
	// else". Same expression shape as ListIssues so the projection stays
	// uniform; the caller is always known on this code path (the endpoint
	// is auth-gated), so we don't need the unauthenticated FALSE branch.
	callerPh := addParam(args.UserID)
	iAssignedExpr := fmt.Sprintf(
		"(i.created_by = %s AND ua.id IS NOT NULL AND ua.id <> %s)",
		callerPh, callerPh)

	// sk-291 — epic progress is scoped to the caller's horizon.
	epicUnrestricted, epicCaller := h.unrestricted, h.visible
	epicProgressCols := epicProgressProjection(addParam(epicUnrestricted), addParam(epicCaller), addParam(h.spaces))
	parentCol := parentIDForReader(h, addParam)

	sql := fmt.Sprintf(`
SELECT i.id, i.project_id, `+parentCol+`, i.version, i.title,
       i.type, i.priority, i.status, i.pinned, i.assignee,
       i.created_by, uc.username, ua.id, `+issueSummaryPersonCols+`,
       i.created_at, i.updated_at, i.row_seq,
       `+rowComponentsLabelsCols+`,
       i.accepted_at, i.close_outcome,
       %s AS i_assigned,
       %s AS is_blocked,
       %s
  FROM issues i
  LEFT JOIN users uc ON uc.id = i.created_by
  `+assignee.LeftJoin("ua", "i")+`
 WHERE %s
 ORDER BY i.updated_at DESC, i.id DESC
 LIMIT %s`, iAssignedExpr, blockedExpr(), epicProgressCols,
		strings.Join(clauses, " AND "), limitPh)

	rows, err := s.pool.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("listing by user: %w", err)
	}
	defer rows.Close()

	items := make([]IssueSummary, 0, args.Limit)
	for rows.Next() {
		var (
			item              IssueSummary
			parentID          *string
			assignee          *string
			createdByUsername *string
			assigneeUserID    *uuid.UUID
			acceptedAt        *time.Time
			closeOutcome      *string
			iAssigned         bool
			isBlocked         bool
			epicTotal         *int
			epicClosed        *int
			epicUnavailable   *int
			epicNoWork        *int
		)
		if err := rows.Scan(
			&item.ID, &item.ProjectID, &parentID, &item.Version, &item.Title,
			&item.Type, &item.Priority, &item.Status, &item.Pinned, &assignee,
			&item.CreatedBy, &createdByUsername, &assigneeUserID, &item.AssigneeStatus, &item.CreatedByStatus, &item.SpaceID,
			&item.CreatedAt, &item.UpdatedAt, &item.RowSeq,
			&item.ComponentIDs, &item.Labels,
			&acceptedAt, &closeOutcome,
			&iAssigned,
			&isBlocked,
			&epicTotal, &epicClosed, &epicUnavailable, &epicNoWork,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		item.ParentID = parentID
		item.Assignee = assignee
		item.CreatedByUsername = createdByUsername
		item.AssigneeUserID = assigneeUserID
		item.AcceptedAt = acceptedAt
		item.CloseOutcome = closeOutcome
		item.IAssigned = iAssigned
		item.IsBlocked = isBlocked
		item.EpicProgress = epicProgressFrom(epicTotal, epicClosed, epicUnavailable, epicNoWork)
		normalizeComponentsLabels(&item.ComponentIDs, &item.Labels)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasMore := false
	if len(items) > args.Limit {
		hasMore = true
		items = items[:args.Limit]
	}
	// Components and labels arrived with the rows (rowComponentsLabelsCols),
	// the same snapshot as each row's rowSeq (R26 item 2).
	var nextCursor *string
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		c := EncodeCursor(last.UpdatedAt, last.ID)
		nextCursor = &c
	}

	// Total — exact COUNT(*) over the same WHERE clause (sans cursor).
	// Re-uses countListIssues; no clause names the users join.
	total, err := s.countListIssues(ctx, countClauses, countParams)
	if err != nil {
		return nil, fmt.Errorf("counting list-by-user issues: %w", err)
	}
	return &ListResult{Items: items, Total: total, NextCursor: nextCursor, HasMore: hasMore}, nil
}
