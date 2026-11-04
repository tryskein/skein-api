// Package delegation holds the shared selection of the Delegated lens.
package delegation

import (
	"fmt"

	"skein-api/internal/assignee"
)

// Predicate renders the existing Delegated selection for a trusted
// issue alias and caller placeholder. History is enabled only when the caller
// has a username, matching the three list paths' established fallback.
// A creator remains in the author branch even when explicitly dismissed;
// dismissal narrows only the previous-assignee branch.
func Predicate(issueAlias, callerPlaceholder string, includeHistory bool) string {
	if !includeHistory {
		return fmt.Sprintf("%s.created_by = %s", issueAlias, callerPlaceholder)
	}
	return fmt.Sprintf(
		"(%[1]s.assignee IS NOT NULL AND NOT %[3]s "+
			"AND (%[1]s.created_by = %[2]s "+
			"OR (%[2]s = ANY(%[1]s.previous_assignees) "+
			"AND NOT %[2]s = ANY(%[1]s.dismissed_by))))",
		issueAlias, callerPlaceholder, assignee.MatchesCaller(issueAlias, callerPlaceholder))
}
