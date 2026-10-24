package authz

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// SQL fragments over the access relation (migration 000066).
//
// Every fragment takes the placeholders of the caller's own query, because
// every query in the codebase numbers its own parameters: the unrestricted
// flag first, then the scope's visible set — bind them from Scope.Bind in
// that order. The unrestricted flag exists only for declared internal work;
// a caller's scope always binds false and the relation's set decides, an
// admin included.

// ProjectVisibleExpr renders the SQL boolean "the project of row alias is
// visible to the scope", for a row alias that has a project_id column:
//
//	($u::boolean OR alias.project_id = ANY($v::uuid[]))
//
// $v is the scope's visible set (Scope.Bind) — the access relation read
// once per request and bound as a value, so the planner sees exact
// selectivity, can use the index on project_id, and pays nothing per row
// (sk-674.18 T009: the relation inlined into big queries, or called per row
// through skein_project_visible, was measured far slower on the 10k stand).
func ProjectVisibleExpr(alias, unrestrictedPlaceholder, visiblePlaceholder string) string {
	return VisibleExpr(alias+".project_id", unrestrictedPlaceholder, visiblePlaceholder)
}

// VisibleExpr is ProjectVisibleExpr for any SQL expression naming a project
// id — the projects table's own `p.id`, an aliases row's project.
func VisibleExpr(projectIDExpr, unrestrictedPlaceholder, visiblePlaceholder string) string {
	return "(" + unrestrictedPlaceholder + "::boolean OR " + projectIDExpr +
		" = ANY(" + visiblePlaceholder + "::uuid[]))"
}

// VisibleSetExpr renders the same visible set as a semi-join:
//
//	projectIDExpr IN (SELECT unnest($v::uuid[]))
//
// Which to take: VisibleExpr (= ANY) by default. VisibleSetExpr only where
// the query ranks the issues table through its GIN indexes of tsv_ru /
// tsv_en while the set is small — full-text search over issues
// (search.SearchIssues). There `= ANY` lets the planner AND the array with
// the FTS bitmaps (tens of thousands of rows each) instead of looping over
// the visible projects; the semi-join keeps that loop, as the membership
// sub-select it replaces did. It is not a general improvement: where the
// selective index is the other table's (full-text over comments), the
// semi-join drives from the set through every issue of every visible
// project and loses to `= ANY` by orders of magnitude (sk-674.18 T010, the
// EXPLAIN gate of search, measured both ways). Same set, same source —
// Scope.Bind, once per request (K-1).
//
// It has no unrestricted flag: an OR with a bound flag would stop the
// planner from turning the sub-select into a semi-join. It is for a caller's
// scope; declared internal work binds an empty set and sees nothing here.
func VisibleSetExpr(projectIDExpr, visiblePlaceholder string) string {
	return projectIDExpr + " IN (SELECT unnest(" + visiblePlaceholder + "::uuid[]))"
}

// HiddenCountableExpr renders the SQL boolean «the project projectIDExpr is
// hidden from the scope AND lies in one of its own non-personal spaces» —
// the only hidden projects whose issues may count in an unavailable*
// number (INV-28 (4): "there is something hidden, how many" only inside one's own
// non-personal space; a hidden issue in another's space or another's
// "Personal" counts nowhere). The first two placeholders are Bind's, the
// third is BindSpaces's:
//
//	(NOT <VisibleExpr> AND EXISTS (projects hc: hc.id = projectIDExpr
//	                               AND hc.space_id = ANY($s)))
func HiddenCountableExpr(projectIDExpr, unrestrictedPlaceholder, visiblePlaceholder, spacesPlaceholder string) string {
	return "(NOT " + VisibleExpr(projectIDExpr, unrestrictedPlaceholder, visiblePlaceholder) +
		" AND EXISTS (SELECT 1 FROM projects hc WHERE hc.id = " + projectIDExpr +
		" AND hc.space_id = ANY(" + spacesPlaceholder + "::uuid[])))"
}

// MemberExpr renders the SQL boolean "the caller holds an explicit
// membership of the project projectIDExpr" — "mine", not "visible": the
// lens of the caller's own memberships (projects list MineOnly), which an
// instance admin's reach does not widen. Combine it with VisibleExpr; it
// grants nothing on its own.
func MemberExpr(projectIDExpr, callerPlaceholder string) string {
	return "EXISTS (SELECT 1 FROM project_members pm_mine" +
		" WHERE pm_mine.project_id = " + projectIDExpr +
		" AND pm_mine.user_id = " + callerPlaceholder + "::uuid)"
}

// VisibleJoin renders a JOIN of the scope's visible projects to the row
// alias, exposing <as>.project_id and <as>.access (a Level: the relation's
// owner / admin_member / member / admin, or internal)
// to the rest of the query. Unlike the boolean fragments it reads the
// relation itself, so its second placeholder is the caller id
// (Scope.CallerID), not the visible set:
//
//	JOIN ( <every project as 'internal', when $u>
//	       UNION ALL
//	       <skein_project_access_set($c), when NOT $u> ) <as>
//	  ON <as>.project_id = <alias>.project_id
//
// The relation is computed once per query as a set and joined by
// project_id (K-1); exactly one leg of the UNION is live for a given scope.
func VisibleJoin(alias, unrestrictedPlaceholder, callerPlaceholder, as string) string {
	return "JOIN (" +
		"SELECT p_all.id AS project_id, 'internal'::text AS access FROM projects p_all" +
		" WHERE " + unrestrictedPlaceholder + "::boolean" +
		" UNION ALL " +
		"SELECT va_rel.project_id, va_rel.access FROM skein_project_access_set(" + callerPlaceholder + "::uuid) va_rel" +
		" WHERE NOT " + unrestrictedPlaceholder + "::boolean" +
		") " + as + " ON " + as + ".project_id = " + alias + ".project_id"
}

// visibleAmongSQL keeps the ids of $1 the scope sees.
var visibleAmongSQL = `
SELECT x.project_id
  FROM unnest($1::uuid[]) AS x(project_id)
 WHERE ` + ProjectVisibleExpr("x", "$2", "$3")

// VisibleAmong returns which of projectIDs the scope sees, as a set — for
// Go code that already holds rows of several projects and must redact the
// ones out of reach (tree stubs). It checks only the ids given, through the
// relation; it never materialises the scope's whole visible set.
func VisibleAmong(ctx context.Context, q RowsQuerier, s Scope, projectIDs []uuid.UUID) (map[uuid.UUID]struct{}, error) {
	out := make(map[uuid.UUID]struct{}, len(projectIDs))
	if len(projectIDs) == 0 {
		return out, nil
	}
	unrestricted, visible, err := s.Bind(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, visibleAmongSQL, projectIDs, unrestricted, visible)
	if err != nil {
		return nil, fmt.Errorf("checking visibility of %d projects: %w", len(projectIDs), err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning visible project: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading visible projects: %w", err)
	}
	return out, nil
}

// VisibleProjectIDs materialises the scope's whole visible set: every
// project for declared internal work (all = true, ids nil), otherwise the
// project ids of the relation — an admin's included, there is no Go
// shortcut. For surfaces that hand a set to code outside SQL (the activity
// feed, the dashboard); a query should JOIN the relation instead
// (VisibleJoin / ProjectVisibleExpr).
func VisibleProjectIDs(ctx context.Context, q RowsQuerier, s Scope) (ids map[uuid.UUID]struct{}, all bool, err error) {
	unrestricted, visible, err := s.Bind(ctx, q)
	if err != nil {
		return nil, false, err
	}
	if unrestricted {
		return nil, true, nil
	}
	ids = make(map[uuid.UUID]struct{}, len(visible))
	for _, id := range visible {
		ids[id] = struct{}{}
	}
	return ids, false, nil
}
