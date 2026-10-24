// Package assignee centralizes the SQL that maps `issues.assignee` — free-form
// text on the issue row — onto a real `users` row.
//
// # Why the column needs resolving at all
//
// `issues.assignee` stores a HANDLE verbatim, and the write surface documents
// three accepted forms: a username, an e-mail, or a user UUID (skein_create's
// `assignee` is described as "username, email, or user UUID — stored verbatim,
// resolved on read"). The column carries no CHECK that narrows this. sk-294
// already brought the WRITE side into line: notifications.ResolveAssigneeUserID
// matches all three, and every write-time predicate built on it — the claim
// gate, the accept gate, auto-accept-on-self-assign, auto-watch — treats the
// three forms as one person.
//
// The READ side did not follow. Every projection and every "mine" counter
// spelled its own one-legged `LOWER(ua.username) = LOWER(i.assignee)`, so an
// issue assigned by e-mail or by UUID resolved to NOBODY: `assigneeUserId` came
// back NULL and the person was absent from their own counters. sk-523 measured
// exactly that against the live schema — username resolved, UUID and e-mail
// both NULL — which is why this package exists and why the literals it replaces
// are now forbidden (see TestNoRawAssigneeMatch).
//
// # Why resolution stays DERIVED rather than materialized
//
// A stored `issues.assignee_user_id` column would be faster and would make both
// helpers below trivial. It is nonetheless wrong for this column, because
// resolution here is deliberately LATE:
//
//   - assigning to the e-mail of somebody who has not signed up yet is legal,
//     and the issue becomes theirs the moment that account exists;
//   - a username or e-mail change re-points existing issues at the same person.
//
// A value frozen at write time heals in neither case, and nothing would report
// the drift. The join is re-evaluated on every read precisely so that identity
// follows the `users` table rather than a snapshot of it.
//
// # Why matching three forms at once cannot fan out or collide
//
// The three namespaces are provably disjoint, so at most one user can match a
// given handle and a LEFT JOIN on the condition below can never multiply rows:
//
//   - `users.username` is CHECK-constrained to `^[a-z0-9][a-z0-9_-]{2,29}$`, so
//     it is at most 30 characters and can never contain '@';
//   - a UUID in text form is 36 characters — longer than any username — and
//     also has no '@';
//   - an e-mail contains '@', which the username charset forbids.
//
// `idx_users_username_lower` and `idx_users_email_lower` (migration 000001) are
// UNIQUE, so neither leg can match twice on its own either. This is the same
// argument notifications.ResolveAssigneeUserID makes for its `LIMIT 1`; here it
// is what lets the join stay a plain LEFT JOIN with no de-duplication.
//
// # Resolution is an index lookup, not a filter over users (sk-712)
//
// The handle is resolved by userIDOf — three probes of the unique indexes
// (`idx_users_username_lower`, `idx_users_email_lower`, `users_pkey`) under
// UNION ALL ... LIMIT 1 — and every helper compares a users row's id with
// that one value. The earlier shape, an OR of the three LOWER() comparisons
// against the users row itself, could use no index: a join on it compared
// each issue with every user (the cross product of sk-712), and the planner
// charged every per-user condition AND-ed to it — the access check of
// the delivery join, the marks of package person — once per users row, which inflated
// the estimate of a one-issue read past the JIT thresholds (sk-674.18,
// EXPLAIN gate). Both helpers mean the same person: the namespaces
// are disjoint (above), so the order of the legs and LIMIT 1 decide
// nothing.
//
// # The id leg casts only a canonical UUID
//
// `u.id = CASE WHEN LOWER(h) ~ <canonical> THEN LOWER(h)::uuid END`: a
// free-form handle ("the docs team") fed to `::uuid` unguarded raises 22P02
// and turns a read into a 500, which on a JOIN over many rows would fire on
// the first unparseable assignee in the table. The guard is the canonical
// text form of a uuid (lower-case, hyphenated) — exactly the strings
// `u.id::text` produces — so the leg matches the same handles the earlier
// text comparison `u.id::text = LOWER(h)` did: any upper-case spelling of a
// user's id, and nothing else (no braces, no hyphen-less form).
package assignee

import (
	"fmt"

	"skein-api/internal/authz/authzsql"
)

// canonicalUUID is the text form `uuid::text` produces: the only spelling
// of an id the id leg accepts (after LOWER), so the cast it guards cannot fail.
const canonicalUUID = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// userIDOf renders the id of the user `<issueAlias>.assignee` names, in any
// of the three forms, as a scalar subquery: NULL when the handle is NULL or
// names nobody. Each leg is a probe of a unique index.
func userIDOf(issueAlias string) string {
	return fmt.Sprintf(
		`(SELECT hu.id FROM users hu WHERE LOWER(hu.username) = LOWER(%[1]s.assignee)`+
			` UNION ALL SELECT hu.id FROM users hu WHERE LOWER(hu.email) = LOWER(%[1]s.assignee)`+
			` UNION ALL SELECT hu.id FROM users hu WHERE hu.id = CASE WHEN LOWER(%[1]s.assignee) ~ '%[2]s'`+
			` THEN LOWER(%[1]s.assignee)::uuid END`+
			` LIMIT 1)`,
		issueAlias, canonicalUUID)
}

// Names returns the condition "the users row userAlias is the one
// `<issueAlias>.assignee` names" — the identity of the handle alone, with no
// access condition. It is for showing an assignee (INV-30's display
// grounds are applied by the caller, person.AssigneeStatusExpr), never for
// delivering to one: delivery is LeftJoin / Join / MatchesCaller. NULL (not
// true) for a NULL handle or one that names nobody.
func Names(userAlias, issueAlias string) string {
	return "(" + userAlias + ".id = " + userIDOf(issueAlias) + ")"
}

// LeftJoin returns the whole join clause that brings in, as `<userAlias>`,
// the users row `<issueAlias>.assignee` names AND who may work in the
// issue's project now (authzsql.WorkerEligibleExpr — INV-30, sk-674.18
// T028); every column of users, NULL when there is no such person. It is
// the DELIVERY resolution of an assignee: assigneeUserId, the «on me»
// lenses, Inbox, reminders and counters see the person only while he has
// that access; a handle of anybody else stays text and resolves to nobody
// (E-33, E-37).
//
// Use it in place of a join:
//
//	"FROM issues i " + assignee.LeftJoin("ua", "i") + " WHERE …"
//
// It is a LATERAL subquery on purpose (sk-712): the lookup is per issue
// row, so the only plan is a nested loop over the issues read, and an
// ordered read keeps its order. An ON condition `u.id = <lookup>` would be
// a hashable equality, and on a small users table the planner hashes it —
// the order of the issues read is lost and a Sort lands above it
// (TestUnreviewed_ReadsUseTheIndexes). LATERAL alone does not hold: a
// simple subquery is pulled up into the join tree and its WHERE becomes that
// same hashable join condition. LIMIT 1 keeps it a subquery — and says what
// is meant, at most one person (users.id is unique, so nothing is cut).
func LeftJoin(userAlias, issueAlias string) string {
	return "LEFT " + Join(userAlias, issueAlias)
}

// Join is LeftJoin as an inner join: only issues whose assignee is
// delivered to somebody stay.
func Join(userAlias, issueAlias string) string {
	return "JOIN LATERAL (SELECT au.* FROM users au WHERE au.id = " + userIDOf(issueAlias) +
		" AND " + authzsql.WorkerEligibleExpr("au.id", issueAlias+".project_id") +
		" LIMIT 1) " + userAlias + " ON true"
}

// handlesOf renders the handles that name the user whose id is userIDExpr,
// as a text[] of their lowered forms — lower(username), lower(email),
// id::text. LOWER(h) is in it exactly when userIDOf resolves h to that user:
// the namespaces are disjoint (above), and the id leg accepts precisely the
// canonical text of an id, which is what id::text is. Empty when there is
// no such user.
func handlesOf(userIDExpr string) string {
	return "ARRAY(SELECT hv.h FROM users hx, LATERAL (VALUES (LOWER(hx.username)), (LOWER(hx.email)), (hx.id::text)) hv(h)" +
		" WHERE hx.id = " + userIDExpr + ")"
}

// namesUser is the identity half of MatchesCaller / NamesUser: the handle
// of the issue row names the user userIDExpr. It reads from the issue's
// side — `LOWER(assignee) = ANY(<his handles>)` under `assignee IS NOT
// NULL`, the shape of idx_issues_assignee_lower — so a filter "assigned to
// X" finds X's issues by that index instead of resolving the handle of
// every issue it could see (sk-712: /me/assigned-to and the cross-project
// «on me» list, EXPLAIN gate). Never NULL: a NULL handle stops at
// the first conjunct, and ANY over an array without NULLs is true or false.
func namesUser(issueAlias, userIDExpr string) string {
	return "(" + issueAlias + ".assignee IS NOT NULL AND LOWER(" + issueAlias + ".assignee) = ANY(" +
		handlesOf(userIDExpr) + "))"
}

// MatchesCaller returns a predicate that is true when `<issueAlias>.assignee`
// names the user whose id is bound at `callerIDPh` (a placeholder token such as
// "$3", already bound by the caller, or a column of the query) AND that user
// may work in the issue's project now — the delivery resolution of LeftJoin,
// for that user (INV-30): it is true exactly when LeftJoin brings him in.
// Filter "assigned to X" with it, not with `ua.id = X` over LeftJoin: it
// reads from X's handles (namesUser), so it needs no join and uses the
// index.
//
// It replaces the raw `LOWER(i.assignee) = <lowercased caller username>` shape
// that the "mine" counters used. Two properties are deliberate:
//
//   - It pivots on the caller's ID, which every one of these queries already
//     binds, so widening the counters needed no new value threaded through the
//     arg structs — and in particular no caller e-mail, which is absent from
//     every principal type and, on the MCP transport, is not plumbed at all.
//   - It never evaluates to NULL (namesUser is never NULL, the access check
//     is an EXISTS), so the negated form (NOT MatchesCaller) is a clean
//     complement. The raw `<>` shape it replaces went NULL on an unassigned
//     row, which is why every such site carries its own
//     `assignee IS NOT NULL` conjunct; those conjuncts are left in place,
//     since "assigned to somebody else" and "not assigned to me" are
//     different questions and the call sites mean the former.
//
// It is a plain boolean of the issue row, not an EXISTS over users: a
// correlated users row is pulled up into a (anti-)join, which on a small
// users table is hashed and costs an ordered read its order (sk-712, the
// same reason LeftJoin is LATERAL).
func MatchesCaller(issueAlias, callerIDPh string) string {
	return "(" + namesUser(issueAlias, callerIDPh) + " AND " +
		authzsql.WorkerEligibleExpr(callerIDPh, issueAlias+".project_id") + ")"
}

// NamesUser is MatchesCaller without the access condition: the handle names
// the user bound at userIDPh, whatever his access now. It is for the code
// that acts on a person's assignments BECAUSE he is losing or has lost the
// access — the removal of the assignee on the loss of access (INV-12,
// authz/revoke.go) and the preview of that removal, which must count what the
// removal will remove (US-7). Nothing that delivers or shows uses it.
func NamesUser(issueAlias, userIDPh string) string {
	return namesUser(issueAlias, userIDPh)
}
