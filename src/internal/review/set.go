package review

// set.go — sk-303.14, task T012. The SINGLE definition of the two
// mechanic sets: unreviewed (results the author has not yet read) and
// unaccepted (issues the assignee has not yet accepted).
//
// # Why one file instead of an in-place predicate
//
// INV-3: "the unreviewed set is defined once; the badge count, the list
// filter contents, and the reminder recipients are derived from it and must
// agree". This is not a stylistic requirement. In the previous notifications
// wave, three surfaces showed the same number, each with its own copy of the
// predicate, and they drifted — a fix reached two copies out of three. There
// will be no copies here: consumers call this file's functions and splice the
// result into their own WHERE, and they do not write membership conditions of
// their own.
//
// Consumers split across three different groups of work, so both fragments
// appear HERE, before the first of them:
//
//	unreviewed — counter (T013), list filter (T012),
//	             re-check at delivery (T015), reminder selection (T017);
//	unaccepted — reminder selection (T017) and the same re-check (T015).
//
// Unaccepted has neither a public counter nor a public list filter — that is
// how their consumer sets differ, and that is why an index for unaccepted is
// NOT created here: its hot query (join with rate-limit state and a due-date
// predicate) appears only at the reminder worker, where the index shape is
// derivable. Indexes for unreviewed were created by migration 000046 together
// with this file's queries — the index shape is derived from the query shape,
// not the other way around.
//
// # API shape: a predicate text, not a finished query
//
// The functions return a READY-TO-SPLICE parenthesized boolean expression and
// register their parameters through the binder they are given. That is how the
// fragment is grafted into any of the existing queries — list by project, list
// by user, counter, future reminder selection — none of which is ready to
// hand SQL construction over to a foreign package: each has its own joins,
// its own pagination, and its own order.
//
// # Fail-closed
//
// Both fragments are self-contained: the visibility horizon, project
// archival, the mechanic switch, and assignee resolvability sit INSIDE, not
// on the caller's conscience. A consumer that forgot to apply someone else's
// extra condition is exactly how the sets drift; here there is nothing to
// forget.
//
// # Correlation of the fragment's subqueries (measured, repair A/8)
//
// ⚠ CORRECTED LIST (corrected in review). This used to say the unreviewed
// fragment carries FOUR subqueries — caller name, mechanic switch, admin
// bypass, and the membership filter — and that "in VALUE form all four
// depend only on the parameter". Both claims are wrong, and this was a
// handwritten list inside the text that was supposed to fix handwritten
// lists.
//
// Measured on the assembled text: FIVE subqueries in expression form and
// FOUR in value form. The one that was missed is the project-archival filter
// (EXISTS projects pr … archived_at IS NULL). Full list:
//
//	                        depends on                  expression / value form
//	callerNameExpr          the identifier expression   present / present
//	mechanicEnabledFor      the identifier expression   present / present
//	AdminBypassExpr         the identifier expression   present / ABSENT (bound boolean)
//	EXISTS projects         the ROW (i.project_id)      present / present
//	EXISTS project_members  the ROW and the identifier  present / present
//
// (sk-674.18 T010: the admin bypass and the project_members probe are now
// ONE horizon filter, authz.RecipientVisibleExpr — skein_project_visible(identifier,
// i.project_id), dependent on the row and the identifier in both forms. The
// measurements below were taken before and are kept as the record of why the
// remaining subqueries are shaped as they are.)
//
// Hence the correct wording. In VALUE form only the FIRST TWO of the remaining
// four depend on the parameter; the two horizon filters depend on a ROW
// COLUMN and are correlated in BOTH FORMS — their correlation has nothing to
// do with the choice of form. In EXPRESSION form all five depend on the row.
//
// Observations, EXPRESSION FORM (EXPLAIN ANALYZE on the background worker's
// selection, 20 020 issues rows, 20 in the unreviewed set):
//
//	callerNameExpr         SubPlan 2 → Index Scan users_pkey,             loops=20
//	mechanicEnabledFor     SubPlan 3 → Seq Scan user_notification_prefs,  loops=20
//	                                   (a table of a dozen rows — the planner is right)
//	EXISTS project_members SubPlan 7 → hashed,                            loops=1
//	AdminBypassExpr        see "On the Seq Scan of users" below
//	EXISTS projects        see "On the Seq Scan of projects" below
//
// Observations, VALUE FORM (EXPLAIN ANALYZE on the "switch + counter" pair
// from handler/me.go, 5 020 issues rows, 20 in the set, second repair round):
//
//	mechanicEnabledFor     InitPlan 1 and InitPlan 3 → Index Scan
//	                       user_notification_prefs_pkey,                  loops=1
//	callerNameExpr         InitPlan 2 → Seq Scan users cu,                loops=1
//	EXISTS project_members SubPlan 5 → hashed Seq Scan,                   loops=1
//	EXISTS projects        Index Scan projects_pkey,                      loops=20
//	AdminBypassExpr        no node: a bound boolean parameter
//
// The shape of a particular node is the planner's decision, not a property of
// the expression: it collapsed the membership filter into a hashed subplan
// and computed it ONCE, even though the expression is correlated on paper.
// So "correlated" here means "may be computed per row", not "is computed".
//
// # On the Seq Scan of users in the admin bypass — measured, nothing to fix
//
// A review critic observed `Seq Scan on users` under the admin bypass and
// asked whether it needed fixing. The measurement answers: this is an artifact
// of a SMALL users table, not of the expression's shape.
//
//	users ≈ 614 rows:     SubPlan 5 → hashed Seq Scan on users au, loops=1,
//	                      Buffers: shared hit=15
//	users ≈ 40 600 rows:  SubPlan 4 → Index Scan using users_pkey on users au,
//	                      loops=20, Buffers: shared hit=60
//
// That is, as the table grows the planner itself switches to the primary key
// that is already there. An index for this subquery is not created: it would
// be built against an extrapolation, not against a measurement — exactly the
// "guessed index" the wave plan forbids.
//
// # On the Seq Scan of projects — measured, nothing to fix (second round)
//
// The missed archival filter is the only REAL per-row Seq Scan in the previous
// measurement (`Seq Scan on projects pr … loops=20` at every users volume),
// so the "on Seq Scan" section, written for honesty, missed the very node it
// was written for. The answer to "is an index needed" was obtained by
// MEASUREMENT, not by expectation.
//
// EXPLAIN ANALYZE of the background worker's selection (unreviewed, 5 020
// issues rows, 3 in the set), the projects table volume varies, the rest does
// not:
//
//	projects ≈ 1 row:         Seq Scan on projects pr,  loops=3, shared hit=3
//	projects ≈ 1 001 rows:    Index Scan projects_pkey, loops=3, shared hit=9
//	projects ≈ 10 001 rows:   Index Scan projects_pkey, loops=3, shared hit=9
//	projects ≈ 50 001 rows:   Index Scan projects_pkey, loops=3, shared hit=9
//
// The conclusion is the same and for the same reason as with users: sequential
// read is an artifact of a table of a handful of rows (the fixture), not of
// the condition's shape. The condition `pr.id = <column> AND pr.archived_at
// IS NULL` hits the PRIMARY KEY, and the planner switches to it on its own
// as soon as projects stops being trivially small — already at a thousand
// rows, long before any plausible production volume. An index is not created:
// creating one would mean building against an extrapolation. A partial index
// on archived_at IS NULL is not needed either — the filter is on a single
// identifier, not on the set of non-archived projects.

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/google/uuid"

	"skein-api/internal/assignee"
	"skein-api/internal/authz"
	"skein-api/internal/status"
)

// Binder registers a value as a query parameter and returns its
// placeholder ($1, $2, …). Exactly the form the builders in internal/issues
// already use (`addParam`), so grafting the fragment asks nothing of the
// caller except passing its own closure.
type Binder func(value any) string

// aliasRe restricts a table alias to SQL-identifier form. The alias is
// always a constant from code ("i", "issue"), so a violation is a programmer
// error, not data: a panic, not a returned error. The same device and the
// same reason as in internal/status.
var aliasRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func validateAlias(alias string) {
	if !aliasRe.MatchString(alias) {
		panic("review: invalid SQL alias " + alias)
	}
}

// columnRe restricts an IDENTIFIER EXPRESSION to the form `alias.column`.
//
// Expression form exists for ONE consumer — the reminder worker (T017). Every
// other reader of the sets knows the addressee as a VALUE: it arrived from
// the request context. The background worker has no request context: it
// walks ALL addressees at once whose set is non-empty, and the addressee for
// it is a join column, not a parameter. A bound parameter would force it
// either to run one query per candidate (a full read of every addressee's
// set for each — at volume that is not a plan, it is a disaster) or to keep
// ITS OWN copy of the predicate. A copy is forbidden by INV-3 and by this
// file's header, so the fragment speaks both forms and the predicate body
// stays one.
//
// The expression is always a constant from code ("i.created_by", "u.id"), so
// a form violation is a programmer error, not data: a panic, as with the
// alias. There is no "arbitrary SQL from the caller" here and there cannot
// be — the regular expression admits exactly two identifier parts.
var columnRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*$`)

func validateColumnExpr(expr string) {
	if !columnRe.MatchString(expr) {
		panic("review: invalid SQL column expression " + expr)
	}
}

// userExprRe admits a placeholder ($12) or `alias.column`.
var userExprRe = regexp.MustCompile(`^(\$[0-9]{1,3}|[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*)$`)

// ValidateUserExpr checks the form of a user-identifier expression — a
// placeholder or `alias.column` — for neighbours that splice one.
//
// Introduced by repair A/8: nudges.pushEnabledExpr spliced the given
// expression into the query text with NO check at all, sitting in the same
// SQL line next to a checked splice. The expression form there really is
// always a constant from code — but that property was held on honour, not by
// a gate, and that is exactly how checks vanish.
func ValidateUserExpr(expr string) {
	if !userExprRe.MatchString(expr) {
		panic("review: invalid SQL user expression " + expr)
	}
}

// MechanicEnabledExpr returns a scalar expression "the review workflow is
// enabled for this user" (INV-6).
//
// The user_notification_prefs row is created LAZILY: a person who has never
// saved settings has no row at all, and that is the majority. So the
// "built-in default" is what the reader substitutes, not the column DEFAULT;
// the substitution is done here by a COALESCE from the constant
// DefaultMechanicEnabled, so SQL readers and the Go reader
// (notifications.GetReviewPrefs) answer the same.
//
// The subquery depends only on the parameter, so it is not correlated with
// the issues row.
//
// ⚠ REWORDED AS AN OBSERVATION (repair A/8, second round). This used to say
// unconditionally "the planner computes it once per query (InitPlan)" — what
// the planner will do is not a property of the expression, and neighbouring
// claims of the same kind have already been turned into measurements.
// Measured (EXPLAIN ANALYZE of the "switch + counter" pair from
// handler/me.go, 5 020 issues rows, 20 in the set): `InitPlan 1 → Index Scan
// user_notification_prefs_pkey, loops=1` — once per query, as assumed. The
// numbers and the full node breakdown are in the file header.
func MechanicEnabledExpr(bind Binder, userID uuid.UUID) string {
	return mechanicEnabledFor(bind(userID))
}

// mechanicEnabledFor is the same expression over an ALREADY READY identifier
// expression: a placeholder or a join column. The split is needed by the
// unreviewed fragment, which in expression form walks many addressees and
// has no parameter.
//
// ⚠ In column form the subquery becomes CORRELATED and is computed per row,
// not once per query. That is the price of walking many addressees and it is
// accepted deliberately: the only consumer of expression form pays it — the
// background worker, whose input rows are a handful (the unreviewed set is
// narrow), not the list paths, where the form stayed as it was.
//
// The caveat is true and confirmed by measurement (SubPlan 3 → Seq Scan on
// user_notification_prefs, loops=20), but it is NOT A PECULIARITY of this
// expression: in expression form ALL FIVE of the fragment's subqueries may
// be computed per row, and two of them (the project-archival and membership
// filters) are correlated with the row in BOTH forms. The general rule, the
// corrected list, and the numbers are in the file header.
func mechanicEnabledFor(userExpr string) string {
	return fmt.Sprintf(
		"COALESCE((SELECT p.review_mechanic_enabled FROM user_notification_prefs p WHERE p.user_id = %s), %s)",
		userExpr, strconv.FormatBool(DefaultMechanicEnabled))
}

// UnreviewedArgs is the input of the unreviewed fragment.
//
// The set is STRICTLY AUTHORIAL (INV-13): membership along the previous-
// assignees chain is not counted. The existing delegated lens is wider — it
// includes issues where the caller was a previous assignee and has not
// dismissed — and the fragment deliberately does NOT inherit that width:
// otherwise the filter would pick up issues the caller is not allowed to
// mark, and the list, the count, and the action's availability would drift
// (SC-018).
type UnreviewedArgs struct {
	// Alias — alias of the issues table in the caller's query.
	Alias string
	// CallerID — the caller: also the issue author, also the mechanic's
	// addressee.
	//
	// The caller's name is NOT passed into the fragment, and that is
	// deliberate. The assignee is stored as text, so "this is not the
	// caller" needs a name; but a name that arrived over the wire is a
	// second source of truth next to users.username, and it is sometimes
	// empty (a context with no name header). An empty name would mean
	// "nothing to compare against", and the fragment would return an empty
	// set where the neighbouring list — narrowed to strict authorship —
	// would return rows. That is exactly the drift INV-3 exists to prevent.
	// So the name is RESOLVED IN THE QUERY from this identifier — the same
	// device as in the lens counters (handler/me.go, WITH me AS …).
	CallerID uuid.UUID
	// CallerExpr — the caller's identifier as an `alias.column` EXPRESSION
	// instead of a value. A non-empty field displaces CallerID. Same reason
	// as UnacceptedArgs.AddresseeExpr: the background worker walks every
	// addressee in one query.
	CallerExpr string
}

// caller returns the caller's identifier expression.
func (a UnreviewedArgs) caller(bind Binder) string {
	if a.CallerExpr != "" {
		validateColumnExpr(a.CallerExpr)
		return a.CallerExpr
	}
	return bind(a.CallerID)
}

// UnreviewedPredicate returns the membership predicate of an issue in the
// unreviewed set for the caller.
//
// The set's contract, verbatim from the spec:
//
//	the issue is closed · it was created by the caller · an assignee is set
//	and is not the caller · the review_required flag is on · there is no
//	reviewed_at mark · the caller has not explicitly dismissed this issue ·
//	the issue's project is not archived · the review workflow is enabled for the
//	caller · the caller can see the issue's project.
//
// Line-by-line notes on the non-obvious translations:
//
//   - "has not explicitly dismissed" is dismissed_by, the array of those who
//     removed the issue from their delegated lens with the "remove from
//     delegated" action (sk-34). The person has already spoken on exactly
//     this question, and asking them a second time with a review mark is
//     disrespect for the first answer (SC-026).
//
//   - "project is not archived" is projects.archived_at IS NULL. There is no
//     separate archival state ON THE ISSUE in the system: the bulk
//     "archive" action moves the issue into ordinary closed, differing only
//     by reason. So the fragment excludes issues of ARCHIVED PROJECTS and
//     does not try to recognise archival of a single issue — an issue
//     "archived" by the bulk action does NOT drop out of the set (SC-025
//     against the edge case of matrix row 57).
//
//   - "can see the project" is the access relation (authz.RecipientVisibleExpr,
//     sk-674.18 T010) for the caller — the one rule every surface reads.
//     The horizon sits inside the fragment, not on the caller's conscience:
//     list paths apply their own matching filter, the counter does not, and
//     it is the counter that would drift.
func UnreviewedPredicate(bind Binder, args UnreviewedArgs) string {
	validateAlias(args.Alias)
	a := args.Alias

	callerPh := args.caller(bind)

	// Caller's name — a scalar subquery. No user with that identifier
	// (impossible for an authenticated caller) → the subquery yields NULL,
	// the comparison yields NULL, the row drops out. Fail-closed without a
	// separate branch.
	//
	// ⚠ CORRECTED CLAIM (repair A/8). This used to say "UNcorrelated
	// subquery: the planner computes it once per query". For VALUE form
	// (placeholder) that is true; for EXPRESSION form it is not: the
	// subquery depends on a row column and becomes correlated. Measured on
	// the background worker's selection (EXPLAIN ANALYZE, 20 020 issues
	// rows, 20 in the set): `SubPlan 2 → Index Scan using users_pkey on
	// users cu, loops=20` — one lookup PER ROW. The caveat used to sit
	// only on mechanicEnabledFor; the general rule is now in the file
	// header.
	callerNameExpr := fmt.Sprintf(
		"(SELECT LOWER(cu.username) FROM users cu WHERE cu.id = %s)", callerPh)

	return "(" +
		status.IsClosed(a) +
		fmt.Sprintf(" AND %s.created_by = %s", a, callerPh) +
		fmt.Sprintf(" AND %s.assignee IS NOT NULL", a) +
		fmt.Sprintf(" AND LOWER(%s.assignee) <> %s", a, callerNameExpr) +
		fmt.Sprintf(" AND %s.review_required", a) +
		fmt.Sprintf(" AND %s.reviewed_at IS NULL", a) +
		fmt.Sprintf(" AND NOT (%s = ANY(%s.dismissed_by))", callerPh, a) +
		fmt.Sprintf(" AND EXISTS (SELECT 1 FROM projects pr"+
			" WHERE pr.id = %s.project_id AND pr.archived_at IS NULL)", a) +
		" AND " + mechanicEnabledFor(callerPh) +
		" AND " + authz.RecipientVisibleExpr(callerPh+"::uuid", a+".project_id") +
		")"
}

// UnacceptedArgs is the input of the unaccepted fragment.
type UnacceptedArgs struct {
	// Alias — alias of the issues table in the caller's query.
	Alias string
	// AddresseeID — the person being reminded: the issue's current
	// assignee. Used when AddresseeExpr is empty.
	AddresseeID uuid.UUID
	// AddresseeExpr — the addressee's identifier as an `alias.column`
	// EXPRESSION instead of a value. A non-empty field displaces
	// AddresseeID.
	//
	// Needed exactly by the background reminder worker, which walks every
	// addressee in one query and therefore knows the addressee as a join
	// column, not a parameter (see columnRe). Other readers leave the
	// field empty.
	AddresseeExpr string
}

// addressee returns the addressee's identifier expression: the column if the
// caller supplied one, otherwise a bound parameter.
func (a UnacceptedArgs) addressee(bind Binder) string {
	if a.AddresseeExpr != "" {
		validateColumnExpr(a.AddresseeExpr)
		return a.AddresseeExpr
	}
	return bind(a.AddresseeID)
}

// UnacceptedPredicate returns the membership predicate of an issue in the
// unaccepted set for the addressee.
//
// The set's contract, verbatim: the issue is not closed · the assignee is
// the addressee · there is no acceptance. The spec's status-by-status
// breakdown is covered in full and checkable line by line:
//
//	open,      no acceptance → yes ─┐
//	deferred,  no acceptance → yes  ├─ status.ActiveWork: NOT IN ('closed','standing')
//	in_progress, no acceptance → yes─┘
//	accepted by any path     → no  ── accepted_at IS NULL
//	closed                   → no  ─┐
//	standing                 → no  ─┴─ the same ActiveWork
//
// "Took into work" is not a command name, it is an observed result: the
// appearance of an acceptance mark. So the in_progress status by itself does
// not take the issue out of the set, and accepted_at does (SC-009, SC-010).
//
// A standing task is excluded by ActiveWork, not by a separate condition,
// and that is deliberate: a standing task does not wait for acceptance, it
// never completes, and a perpetual reminder on it is pure noise (SC-024).
// ActiveWork is the very place where "standing does not count as active
// work" has already been decided once for the whole system.
//
// The assignee is STORED AS TEXT, so "the assignee is the addressee" is
// expressed by resolving that text to a living user. Two exclusions come
// free with no separate conditions: an UNRESOLVABLE assignee (typo, deleted
// account — no users row) and a DEACTIVATED addressee (deactivated_at IS
// NOT NULL) do not enter the set. There is nobody to remind in either case,
// and fail-open here would mean the worker ticking forever over rows that
// will never be delivered.
func UnacceptedPredicate(bind Binder, args UnacceptedArgs) string {
	validateAlias(args.Alias)
	a := args.Alias

	addresseePh := args.addressee(bind)

	return "(" +
		status.ActiveWork(a) +
		fmt.Sprintf(" AND %s.accepted_at IS NULL", a) +
		fmt.Sprintf(" AND EXISTS (SELECT 1 FROM users u"+
			" WHERE u.id = %s"+
			" AND u.deactivated_at IS NULL)"+
			" AND %s",
			addresseePh, assignee.MatchesCaller(a, addresseePh)) +
		")"
}
