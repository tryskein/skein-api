package search

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ResetEmbedding clears the `embedding` column on a row identified by
// (table, idCol, idVal). Called from the issue / project / component
// stores whenever a text field that contributes to the embedding source
// changes — the partial pending index then makes the row eligible for
// the backfill worker on its next tick.
//
// Why a helper, not an in-line SET embedding=NULL?  Three reasons:
//
//  1. Centralises the column name. If we ever rename the embedding
//     column or move it out of the parent table, one edit covers every
//     caller.
//  2. Encodes the "which text fields imply a reset" decision in the
//     caller (PatchIssue, UpdateProject, UpdateComponent) — those sites
//     already know what changed; this helper is just the executor.
//  3. Lets us add an "embedding_pending_at" timestamp column later (for
//     lag SLO metrics) without touching every call site.
//
// `tx` is the active transaction. We deliberately do not allow callers
// to pass *pgxpool.Pool — embedding resets must happen in the same
// transaction as the text update so a rolled-back patch doesn't leave
// the row with a stale-but-cleared embedding waiting for re-index.
//
// The query uses fmt.Sprintf for table/column injection.  Table names are
// hard-coded constants in the caller (issuesTable, projectsTable, etc.) —
// they never originate from user input, so SQL injection is impossible.
func ResetEmbedding(
	ctx context.Context,
	tx pgx.Tx,
	table, idCol string,
	idVal any,
) error {
	if tx == nil {
		return fmt.Errorf("reset embedding: tx is nil")
	}
	if table == "" || idCol == "" {
		return fmt.Errorf("reset embedding: table/idCol must be set")
	}
	q := fmt.Sprintf(`UPDATE %s SET embedding = NULL WHERE %s = $1`, table, idCol)
	if _, err := tx.Exec(ctx, q, idVal); err != nil {
		return fmt.Errorf("reset embedding (%s): %w", table, err)
	}
	return nil
}
