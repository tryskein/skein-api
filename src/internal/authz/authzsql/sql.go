// Package authzsql holds the recipient fragments over the access relation
// (migrations 000066, 000067) — the dependency-free half of package authz
// (sk-674.18, K-1, D-1). It imports nothing of the application, so a package
// below internal/auth in the import graph (monitorcreds) can read the one rule
// too. Everything here is re-exported by authz; code that can import authz
// uses authz.
package authzsql

// RecipientVisibleExpr renders the SQL boolean "the user userExpr sees the
// project projectExpr" — the access relation read for an arbitrary user, not
// the request's caller: fan-out candidates, digest rows, mention targets, a
// monitor credential's owner. userExpr and projectExpr are SQL expressions of
// the caller's query (a column, a placeholder with its cast). It calls
// skein_project_visible (migration 000067), the per-row boolean form of the
// relation, which the planner keeps an opaque call.
//
// It is the recipient filter of D-1: a notification, a digest line or a
// mention reaches a user only if this holds for the event's project, so a
// stale subscription row or a mention of an outsider grants nothing.
func RecipientVisibleExpr(userExpr, projectExpr string) string {
	return "skein_project_visible(" + userExpr + ", " + projectExpr + ")"
}

// RecipientEligibleExpr renders the SQL boolean "the user userExpr may
// receive what happens in the project projectExpr": the user is active
// (users.deactivated_at IS NULL — a deactivated account gets nothing, INV-26)
// AND sees the project (RecipientVisibleExpr). It is THE recipient filter of
// the fan-out — every arm, the role recipients, the personal signal's
// subscribers (sk-674.18 T010) — and the one place a recipient's activity is
// checked: when activity moves into the relation's body (T017/T036) it moves
// here, not into the callers.
func RecipientEligibleExpr(userExpr, projectExpr string) string {
	return "(EXISTS (SELECT 1 FROM users u_rcp WHERE u_rcp.id = " + userExpr +
		" AND u_rcp.deactivated_at IS NULL) AND " + RecipientVisibleExpr(userExpr, projectExpr) + ")"
}

// WorkerEligibleExpr renders the SQL boolean "the user userExpr may work in
// the project projectExpr" (INV-30, sk-674.18 T028): the user sees the
// project now — any level of the relation, an active user on its paths
// a–d of INV-1. An instance admin who sees a non-personal project by his
// role alone (level admin) may work in it too: he has the owner's rights
// there (US-6 as changed by the owner 2026-10-17, sk-741). It is the
// delivery rule of an assignee: assigneeUserId, the «on me» lenses, Inbox,
// reminders and counters resolve an assignee handle to a user only while
// this holds. The relation is read for an arbitrary user
// (skein_project_access_set is an inlinable SQL function, so the project
// predicate reaches its index probes).
func WorkerEligibleExpr(userExpr, projectExpr string) string {
	return "EXISTS (SELECT 1 FROM skein_project_access_set(" + userExpr + ") va_w" +
		" WHERE va_w.project_id = " + projectExpr + ")"
}

// VisibleNowSetExpr renders the SQL boolean "the project projectExpr is
// visible to the user userExpr now" in the set form: the user's access set
// (skein_project_access_set) is evaluated once per statement and probed per
// row, so a counter over a person's whole queue does not pay a point call per
// row (sk-674.18 T029, INV-11). A row with no project (projectExpr NULL)
// belongs to no project and is not filtered by it.
func VisibleNowSetExpr(userExpr, projectExpr string) string {
	return "(" + projectExpr + " IS NULL OR " + projectExpr +
		" IN (SELECT va_n.project_id FROM skein_project_access_set(" + userExpr + ") va_n))"
}
