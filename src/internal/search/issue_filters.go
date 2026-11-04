package search

import (
	"fmt"

	"skein-api/internal/assignee"
	"skein-api/internal/authz"
	"skein-api/internal/delegation"
)

// issueFilterSQL is shared by ranked retrieval and the search-only ID paths.
// The issue alias is i everywhere; visibleRef binds the caller's current
// visible-project set. Callers append this fragment before ranking and LIMIT.
func issueFilterSQL(filters Filters, visibleRef string, addArg func(any) string) string {
	fragment := ""
	if len(filters.Statuses) > 0 {
		fragment += fmt.Sprintf(" AND i.status = ANY(%s)", addArg(filters.Statuses))
	}
	if len(filters.Priorities) > 0 {
		fragment += fmt.Sprintf(" AND i.priority = ANY(%s)", addArg(filters.Priorities))
	}
	if len(filters.Types) > 0 {
		fragment += fmt.Sprintf(" AND i.type = ANY(%s)", addArg(filters.Types))
	}
	if len(filters.Labels) > 0 {
		fragment += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ANY(%s))", addArg(filters.Labels))
	}
	if filters.Source != nil {
		fragment += fmt.Sprintf(" AND i.source = %s", addArg(*filters.Source))
	}
	if len(filters.OriginSources) > 0 {
		fragment += fmt.Sprintf(" AND i.source = ANY(%s)", addArg(filters.OriginSources))
	}
	if filters.AssigneeUserID != nil {
		fragment += " AND " + assignee.MatchesCaller("i", addArg(*filters.AssigneeUserID))
	} else if filters.Assignee != nil {
		fragment += fmt.Sprintf(" AND LOWER(i.assignee) = LOWER(%s)", addArg(*filters.Assignee))
	}
	if filters.CreatedBy != nil {
		fragment += " AND " + delegation.Predicate("i", addArg(*filters.CreatedBy), filters.DelegatedLens)
	}
	if filters.CreatedByExclude != nil {
		fragment += fmt.Sprintf(" AND i.created_by <> %s", addArg(*filters.CreatedByExclude))
	}
	if filters.AcceptedAtNull != nil && *filters.AcceptedAtNull {
		fragment += " AND i.accepted_at IS NULL"
	}
	if filters.AcceptedAtNotNull != nil && *filters.AcceptedAtNotNull {
		fragment += " AND i.accepted_at IS NOT NULL"
	}
	if filters.ComponentID != nil {
		fragment += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM issue_components ic WHERE ic.issue_id = i.id AND ic.component_id = %s)", addArg(*filters.ComponentID))
	}
	if filters.IsBlocked != nil {
		blocked := authz.ReaderBlockedExpr("i", "false", visibleRef)
		if *filters.IsBlocked {
			fragment += " AND " + blocked
		} else {
			fragment += " AND NOT " + blocked
		}
	}
	return fragment
}
