package tools

// prime_rows.go — the US-2 row rule applied to session priming (sk-282
// T018, spec US-12).
//
// Priming returns three issue lists, and until now each of them shipped
// the full carrier struct. US-12 puts all three under the US-2 named field
// set, with ONE exception: the «ready to take» list keeps the two fields
// its carrier does not load out of the row entirely — the same two names
// T017 removes from the standalone ready output, from the same variable,
// so the two surfaces cannot disagree about what this carrier is silent
// on.
//
// # Why the cross-project flag is on
//
// Priming defaults to every visible project, so its rows are read in a
// cross-project context and are unreadable without the project identifier
// (SC-014 measures them against exactly that budget). The flag is set for
// all three lists rather than derived per list: a caller scanning three
// sections should not have to work out which of them happens to be
// single-project today.
//
// # The recent-activity section is not here
//
// US-12 declares it Unchanged, and it is a different shape entirely
// (events, not issue rows). It passes through untouched.

import (
	"context"

	"skein-api/internal/authz"
	"skein-api/internal/childcount"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// projectPrimeIssueRows projects one of the two issues-carrier sections.
func projectPrimeIssueRows(
	ctx context.Context,
	q childcount.Querier,
	rows []issues.IssueSummary,
	scope authz.Scope,
) ([]map[string]any, error) {
	counts, err := childcount.For(ctx, q, rowIDs(rows), scope)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		c := counts[rows[i].ID]
		projected, perr := ProjectIssueRow(rows[i], IssueRowOptions{
			CrossProject: true,
			ChildCounts:  &IssueRowChildCounts{Children: c.Total, Unclosed: c.Unclosed},
		})
		if perr != nil {
			return nil, perr
		}
		out = append(out, projected)
	}
	return out, nil
}

// projectPrimeReadyRows projects the «ready to take» section.
//
// The carrier is dependencies.IssueSummary, which loads neither labels nor
// components. Those two are declared not-provided, which is what turns
// «the row is silent about them» from a silent omission into a stated one
// (INV-1): the projection would otherwise refuse to emit a default-set
// field the carrier cannot supply. Two names, and US-12's exception is
// exactly two names long.
//
// # Why `isBlocked` is injected rather than declared not-provided
//
// The carrier has no such column, and that is a fact about the STRUCT, not
// about the answer. The ready query selects on
// `NOT EXISTS (blocks|waits-for edge whose target still gates readiness)`
// — the same edge types and the same status.GatesReadiness predicate that
// the issues store's isBlockedExprFor uses, negated, and WITHOUT its
// horizon leg (sk-423: readiness counts every blocker, the flag only the
// ones the reader can see). The flag's set of blockers is therefore a
// subset of the one readiness rejected on: every row that reaches here has
// no blocker at all, so `false` is the query's own answer being carried to
// the wire, not a claim invented at projection time. Declaring it not-provided would widen US-12's
// exception from two names to three and make an agent re-derive from the
// dependency graph what this list already knows.
func projectPrimeReadyRows(
	ctx context.Context,
	q childcount.Querier,
	rows []dependencies.IssueSummary,
	scope authz.Scope,
) ([]map[string]any, error) {
	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	counts, err := childcount.For(ctx, q, ids, scope)
	if err != nil {
		return nil, err
	}

	// Every row on this list satisfied the readiness predicate — see the
	// doc comment: unblocked is what the query selected on.
	unblocked := false
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		c := counts[rows[i].ID]
		projected, perr := ProjectIssueRow(rows[i], IssueRowOptions{
			CrossProject: true,
			NotProvided:  readyRowNotProvided,
			IsBlocked:    &unblocked,
			ChildCounts:  &IssueRowChildCounts{Children: c.Total, Unclosed: c.Unclosed},
		})
		if perr != nil {
			return nil, perr
		}
		out = append(out, projected)
	}
	return out, nil
}
