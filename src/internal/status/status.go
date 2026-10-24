// Package status centralizes the SQL status predicates that the rest of the
// backend embeds in hand-written queries. Before this package, the literal
// expressions `i.status <> 'closed'`, `b.status <> 'closed'` and
// `i.status = 'closed'` were copy-pasted across ~9 query files and dozens of
// call sites. That made it impossible to change the *meaning* of "active work"
// or "gates readiness" in one place — every site had to be hunted down by grep.
//
// The three helpers below intentionally have DISTINCT identities even where two
// of them emit byte-identical text today (ActiveWork and GatesReadiness both
// return `<alias>.status <> 'closed'` in this slice). They are kept separate so
// that later slices can widen each independently:
//
//   - ActiveWork      — widened by F2g (and the epic-specific legs by F2h) to
//     `NOT IN ('closed','standing')` once the `standing`
//     status exists. "Counts as active/open WORK" — board
//     pills' blocked/assigned legs, my-assigned lenses,
//     on-me/delegated badges, epic eligibility.
//   - GatesReadiness  — widened by F2f to `NOT IN ('closed','standing')`. The
//     blocker-side leg of readiness / blocked-derivation /
//     waits-for gating (the `JOIN issues b … AND
//     b.status <> 'closed'` inner EXISTS).
//   - IsClosed        — literal terminal-state test `<alias>.status = 'closed'`.
//     NEVER widens (standing is correctly NOT closed).
//
// In this F1 slice every helper is a pure, behavior-preserving extraction: the
// assembled SQL string is byte-identical to the literals it replaced.
package status

import "regexp"

// aliasRe constrains the table alias to a SQL-identifier shape. The alias is
// always a compile-time-constant programmer input (e.g. "i", "b", "ch",
// "children"), so a violation is a programming error, not runtime/user data —
// hence panic rather than an error return.
var aliasRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func validateAlias(alias string) {
	if !aliasRe.MatchString(alias) {
		panic("status: invalid SQL alias " + alias)
	}
}

// ActiveWork returns the predicate selecting issues that count as active/open
// work for the given table alias. F1 emitted `<alias>.status <> 'closed'`; F2g
// (sk-118.8) widened the body ONCE to `<alias>.status NOT IN ('closed',
// 'standing')` so that a STANDING issue stops counting as active work exactly
// like a closed one. A standing task is a perpetual obligation, not open work —
// it must drop out of every active count / lens / my-assigned site. Because
// every active-work site routes through this single helper (F1b's split), this
// one-line edit ripples to all ~23 ActiveWork sites with no per-site changes.
// The epic-eligibility / epic-rollup legs that ALSO route through this helper
// (projects/store.go epicsEligible, issues/store.go epic-rollup blocked) widen
// for free here; F2h owns the remaining epic-specific exprs that do NOT route
// through ActiveWork.
func ActiveWork(alias string) string {
	validateAlias(alias)
	return alias + ".status NOT IN ('closed', 'standing')"
}

// GatesReadiness returns the blocker-side predicate that gates an issue's
// readiness: a blocker still gates while it is neither closed NOR standing.
// F1 emitted `<alias>.status <> 'closed'`; F2f (sk-118.7) widened the body
// ONCE to `<alias>.status NOT IN ('closed', 'standing')` so that a STANDING
// blocker stops gating readiness exactly like a closed one. A standing task is
// a perpetual obligation that is never "done", so a `blocks`/`waits-for` edge
// targeting it would otherwise gate the blocked issue forever. Because every
// readiness / blocked-derivation / waits-for site routes through this single
// helper (F1b's split), this one-line edit ripples to all ~45 GatesReadiness
// sites with no per-site changes.
func GatesReadiness(alias string) string {
	validateAlias(alias)
	return alias + ".status NOT IN ('closed', 'standing')"
}

// IsClosed returns the terminal-state predicate `<alias>.status = 'closed'`.
// This helper exists for symmetry and to give the static guard a single home
// for closed-ness, but it NEVER widens — `standing` is genuinely not closed.
func IsClosed(alias string) string {
	validateAlias(alias)
	return alias + ".status = 'closed'"
}
