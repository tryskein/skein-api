package authz

import "skein-api/internal/status"

// The reader's view of «blocked» (sk-423).
//
// An issue is blocked FOR A READER when it has an outgoing blocks / waits-for
// edge whose target still gates readiness (status.GatesReadiness: neither
// closed nor standing) AND lives in a project inside that reader's horizon
// (ProjectVisibleExpr — the same rule, and the same placeholders, as every
// other horizon-scoped aggregate in this package's callers).
//
// Why the horizon is part of the predicate, not of the rendering: whether a
// blocker gates is a statement about ITS status. Counting a hidden blocker
// in a flag or a counter answers «is that invisible issue closed?» for
// anyone who can compare two answers — the same leak sk-291 closed for the
// epic rollup and sk-421.1 closed for the blocker's title and status. So a
// hidden edge never contributes to «blocked»; the reader is told only that
// the edge EXISTS (HiddenBlockingLinkExpr), which does not depend on the
// hidden side's status at all.
//
// What deliberately does NOT use these fragments: readiness
// (dependencies.Store.ListReady and the gating of waits-for). Readiness is a
// property of the work, not of the reader, and must keep counting every
// blocker.

// blockingEdgeTypes is the dependency-type leg shared by both fragments.
const blockingEdgeTypes = ".type IN ('blocks', 'waits-for')"

// ReaderBlockerExpr renders the SQL boolean «the edge dep makes its target
// blocker a blocker this reader is told about»: a blocks / waits-for edge
// whose target gates readiness and lives inside the reader's horizon. dep is
// the dependencies alias, blocker the alias of the issues row on the edge's
// to_id side; the two placeholders bind Scope.Args, as for
// ProjectVisibleExpr.
//
// This is THE rule of reader-facing blocked-ness: the issue detail's list and
// flag, the list / search / tree rows and the isBlocked filter, the epic
// rollup's blockedChildren and every project / component counter go through
// it (directly, or through ReaderBlockedExpr).
func ReaderBlockerExpr(dep, blocker, unrestrictedPlaceholder, callerPlaceholder string) string {
	return dep + blockingEdgeTypes +
		" AND " + status.GatesReadiness(blocker) +
		" AND " + ProjectVisibleExpr(blocker, unrestrictedPlaceholder, callerPlaceholder)
}

// ReaderBlockedExpr renders the SQL boolean «the issue row issue is blocked
// for this reader»: EXISTS an outgoing edge satisfying ReaderBlockerExpr.
// The subquery's own aliases (d, b) shadow nothing the callers use for the
// outer row.
//
// OFFSET 0 is an optimiser fence, and it is load-bearing (sk-674.18 T009,
// measured on the 10k stand): without it a wide visible set — an instance
// admin's, ~2500 projects — makes the planner turn this correlated EXISTS
// into a hashed subplan over the whole dependency graph, one parallel hash
// per counter of the project rows, and the query died on /dev/shm. With the
// fence each row keeps an index probe on its own edges (dependencies.from_id)
// and the visibility test runs only on those.
func ReaderBlockedExpr(issue, unrestrictedPlaceholder, callerPlaceholder string) string {
	return `EXISTS (
    SELECT 1 FROM dependencies d
      JOIN issues b ON b.id = d.to_id
     WHERE d.from_id = ` + issue + `.id
       AND ` + ReaderBlockerExpr("d", "b", unrestrictedPlaceholder, callerPlaceholder) + `
    OFFSET 0
)`
}

// HiddenBlockingLinkExpr renders the SQL boolean «the edge dep is a blocks /
// waits-for link to an issue outside this reader's horizon, in one of the
// reader's own non-personal spaces» — the fact of the link, WHATEVER the
// hidden side's status: a closed or standing target counts exactly like an
// open one, because anything else would publish that status. A link into
// another's space or another's "Personal" is not counted at all (INV-28 (4),
// HiddenCountableExpr). Disjoint from ReaderBlockerExpr by construction
// (visible vs not). The third placeholder binds Scope.BindSpaces.
func HiddenBlockingLinkExpr(dep, blocker, unrestrictedPlaceholder, visiblePlaceholder, spacesPlaceholder string) string {
	return dep + blockingEdgeTypes +
		" AND " + HiddenCountableExpr(blocker+".project_id", unrestrictedPlaceholder, visiblePlaceholder, spacesPlaceholder)
}
