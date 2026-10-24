package dependencies

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------
// sk-303.12: startup locale legibility check
// ---------------------------------------------------------------------
//
// sk-228 (see duplicateCtypeProbe / trigramLegibilitySQL above) discovered
// that pg_trgm's tokeniser reads letters through the DATABASE's ctype, and
// that a `C`/`POSIX` cluster — or any cluster whose LC_CTYPE is `C` even
// under a non-`C` locale provider, see DEPLOYMENT-NOTES.md §1's "half
// configured" table — silently discards every non-ASCII character before
// building trigrams. The defect was invisible for months on our own
// production cluster: the service starts, migrations apply, tests pass,
// the duplicate-check endpoint answers 200 with an empty `duplicates[]`
// on every non-ASCII title, and nothing anywhere says "this comparison
// never happened".
//
// This is the operator-facing half of that finding. A third-party
// deployment cannot be expected to have read sk-228's SQL-level writeup;
// it CAN be expected to read a startup log line. So this file asks, once,
// at boot, the exact question sk-228 already answers per-request — and
// answers it with the SAME probe and SQL, not a second, independently
// derived (and possibly wrong) one.
//
// # Decision: this state is NOT additionally surfaced in per-request API
// responses (sk-303.12 acceptance criterion 4)
//
// sk-228 already put a per-request signal on the wire: DuplicateReport
// carries Degraded/DegradedReason (internal/handler/issues.go's REST DTO
// has both; the MCP skein_create tool exposes the `degraded` bool only,
// deliberately not the prose reason — see createResponse in
// internal/mcp/tools/create.go and skein_get_instructions' "Duplicate
// detection" section). That signal is STRICTLY MORE ACCURATE than
// anything this cluster-level check could add: it is measured per TITLE,
// not per database, so a `C`-locale cluster with a purely-ASCII title
// correctly reports degraded=false — nothing was lost for that specific
// comparison (see DEPLOYMENT-NOTES.md §1: "An install with strictly ASCII
// content on a C locale works correctly"). Piping ClusterReadsUnicode-
// Letters' cluster-wide answer onto every response as an additional flag
// would either (a) duplicate what Degraded already says on the titles
// where it matters, adding a second field for callers to reconcile, or
// (b) mislabel every ASCII-only-title response as degraded on a
// `C`-locale cluster, which is simply wrong per the measurement above.
// Neither outcome is worth the API surface.
//
// The startup warning is therefore the right — and sufficient — channel
// for this fact: it is cluster CONFIGURATION (the operator's problem,
// fixed once), not a per-response DATA quality signal (the caller's
// problem, already solved precisely by sk-228). An operator who reads the
// warning and fixes the locale makes the per-title signal moot; an
// operator who doesn't still gets an accurate per-title signal from
// sk-228 on every request that actually needed one.

// ClusterReadsUnicodeLetters asks whether THIS PostgreSQL cluster's ctype
// classifies non-ASCII letters as word characters — the cluster-level
// precondition behind sk-228's per-title trigramLegibility check.
//
// Deliberately reuses duplicateCtypeProbe and trigramLegibilitySQL
// unmodified: both are already carefully derived (PostgreSQL source +
// manual citations live on duplicateCtypeProbe's doc comment above) and
// exercised by the duplicate-detection integration tests against every
// ctype flavour that matters. A second hand-rolled query here would be a
// second source of truth for the same fact, with its own chance to drift
// from what pg_trgm actually does. The query title ($1) is irrelevant to
// the fifth column this function reads — CtypeReadsUnicodeLetters is a
// property of $2 (the probe) and the CLUSTER, not of $1 — so an empty
// string is passed and the other four measurements are discarded.
func ClusterReadsUnicodeLetters(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var (
		trigrams, visible, length, bytes int
		readsUnicode                     bool
	)
	if err := pool.QueryRow(ctx, trigramLegibilitySQL, "", duplicateCtypeProbe).
		Scan(&trigrams, &visible, &length, &bytes, &readsUnicode); err != nil {
		return false, fmt.Errorf("probing cluster ctype legibility: %w", err)
	}
	return readsUnicode, nil
}

// LocaleWarning returns the operator-facing warning to log at startup, or
// "" when this cluster's ctype is fine. Startup wiring (cmd/api/main.go)
// logs the non-empty case via slog.Warn and — deliberately — never treats
// it as fatal: a `C`-locale cluster with strictly ASCII content works
// correctly today, and turning a content-dependent degradation into a
// hard startup failure is exactly the mistake sk-304 made with a required
// env var that had no business gating the whole process. The check
// exists to make the degradation LOUD, not to make it impossible to run.
//
// An error from the probe itself (e.g. pg_trgm not installed, connection
// trouble) is returned separately and is ALSO non-fatal at the call
// site — this diagnostic's own failure must not be able to take the API
// down either.
func LocaleWarning(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	ok, err := ClusterReadsUnicodeLetters(ctx, pool)
	if err != nil {
		return "", err
	}
	if ok {
		return "", nil
	}
	return "this PostgreSQL cluster's ctype does not classify non-ASCII letters as " +
		"word characters (LC_CTYPE=C/POSIX under any locale provider — see " +
		"DEPLOYMENT-NOTES.md §1 for how to check and fix it). Consequence: " +
		"trigram-based duplicate-issue detection (pg_trgm) will not compare " +
		"non-ASCII titles at all — every skein_create duplicate check on such a " +
		"title comes back with degraded:true and an empty duplicates[], not a " +
		"clean bill of health. ILIKE, lower() and upper() also stop folding case " +
		"on non-ASCII text. Purely-ASCII content is unaffected", nil
}
