// Package person holds the person marks of the spaces contract (sk-674.18
// T027, spaces/contract.go «every person object»): a person who left the
// space of the context is shown greyed, a deleted account by its permanent
// pseudonym (US-13, INV-18, E-22). It is a leaf package, so every DTO that
// embeds a person can carry the same marks; who belongs to a context is the
// rule of package authz (INV-9), read here, not restated.
package person

import (
	"skein-api/internal/assignee"
	"skein-api/internal/authz"
)

// Status is the marks of one person object. Every key is present only when
// it marks something — absent is «nothing to mark» (not departed, not
// deleted, no pseudonym), which the client reads exactly as false / null.
type Status struct {
	// Departed — the person has left the space of this context (the
	// issue's project); shown greyed. Never set together with Deleted.
	Departed bool `json:"departed,omitempty"`
	// Deleted — the account was deleted (INV-18).
	Deleted bool `json:"deleted,omitempty"`
	// Pseudonym — the stable alias of a deleted account
	// (users.pseudonym, `deleted-<8hex>`); absent otherwise.
	Pseudonym *string `json:"pseudonym,omitempty"`
}

// IsZero reports whether s marks nothing.
func (s Status) IsZero() bool { return !s.Departed && !s.Deleted && s.Pseudonym == nil }

// StatusInProjectExpr renders a SQL jsonb expression of the marks of the
// person row userAlias (a users row of the query, possibly NULL from a LEFT
// JOIN) in the context of the project projectExpr: NULL when there is
// nothing to mark (no person, or a person of the context); {"deleted":
// true, "pseudonym": …} for a deleted account; {"departed": true} for a
// person outside the context (authz.PersonInProjectExpr). Scan it into a
// *Status: NULL leaves the pointer nil.
func StatusInProjectExpr(userAlias, projectExpr string) string {
	return "CASE WHEN " + userAlias + ".id IS NULL THEN NULL" +
		" WHEN " + userAlias + ".deleted_at IS NOT NULL THEN jsonb_build_object('deleted', true, 'pseudonym', " + userAlias + ".pseudonym)" +
		" WHEN NOT " + authz.PersonInProjectExpr(userAlias+".id", projectExpr) + " THEN jsonb_build_object('departed', true)" +
		" END"
}

// StatusExpr is StatusInProjectExpr without a context: only the deletion
// marks — for person objects that belong to no project (the directory, a
// profile, the people of a space or of a project, whose rows are current by
// construction).
func StatusExpr(userAlias string) string {
	return "CASE WHEN " + userAlias + ".deleted_at IS NOT NULL" +
		" THEN jsonb_build_object('deleted', true, 'pseudonym', " + userAlias + ".pseudonym) END"
}

// AssigneeStatusExpr renders the marks of the assignee of the issue row
// issueAlias as a SQL jsonb expression (sk-674.18 T028, INV-30 display): the
// person the handle names, if he may be shown — he belongs to the issue's
// context (INV-9) or has a historical relation to the issue
// (authz.PersonLinkedToIssueExpr) — marked departed or deleted as
// StatusInProjectExpr does. NULL for free text that names nobody showable:
// the text is shown as typed and becomes neither a name nor an id (E-33,
// E-37). Delivery (assigneeUserId) is a separate rule — assignee.LeftJoin.
func AssigneeStatusExpr(issueAlias string) string {
	project := issueAlias + ".project_id"
	return "(SELECT " + StatusInProjectExpr("uad", project) +
		" FROM users uad WHERE " + issueAlias + ".assignee IS NOT NULL AND " + assignee.Names("uad", issueAlias) +
		" AND (" + authz.PersonInProjectExpr("uad.id", project) +
		" OR " + authz.PersonLinkedToIssueExpr("uad.id", issueAlias) + "))"
}
