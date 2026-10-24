package search

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunBackfillWorker is the embedding worker.  It runs as a single
// goroutine started by main(); shutdown is signalled by ctx.Done.
//
// On every tick it walks the four searchable entities in priority order
// (issues, comments, projects, components — the canonical AllKinds
// order) and processes the *first* one that has pending rows. This way
// a backlog on issues never starves comments, and an empty issues
// backlog moves on to the next entity within the same tick.
//
// Each batch runs in a ReadCommitted transaction:
//
//  1. SELECT ... FOR NO KEY UPDATE SKIP LOCKED — claim a batch (lock mode
//     and what it does and does not block: Store.FetchPendingBatch).  SKIP LOCKED
//     means multiple replicas can run this worker simultaneously
//     without fighting over rows; ReadCommitted is sufficient because
//     the row-level locks provide the necessary isolation against other
//     workers, and Serializable would only add cost without benefit.
//  2. Embed the batch in one Ollama round-trip (60s timeout).
//  3. UPDATE each row with its vector.
//  4. COMMIT.
//
// Failure modes:
//
//   - Ollama unreachable → log warn, increment errors_total, rollback.
//     Next tick retries from scratch (the rows are unlocked when the tx
//     aborts, so they're fair game for any replica).
//   - DB error during UPDATE → same as above.
//   - ctx cancelled mid-tx → rollback via deferred Rollback (idempotent
//     after Commit).
func RunBackfillWorker(
	ctx context.Context,
	pool *pgxpool.Pool,
	embedder *Embedder,
	interval time.Duration,
	batchSize int,
) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 32
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	store := NewStore(pool)

	slog.Info("embedding worker started",
		"interval", interval, "batchSize", batchSize)

	// Run once immediately so the first batch isn't delayed by `interval`.
	tick(ctx, pool, store, embedder, batchSize)

	for {
		select {
		case <-ctx.Done():
			slog.Info("embedding worker stopping")
			return
		case <-ticker.C:
			tick(ctx, pool, store, embedder, batchSize)
		}
	}
}

// tick executes one round of the worker. Returns silently — errors are
// logged + counted via metrics but don't propagate (the next tick
// retries cleanly).
func tick(
	ctx context.Context,
	pool *pgxpool.Pool,
	store *Store,
	embedder *Embedder,
	batchSize int,
) {
	// Publish pending gauges first so operators see a non-zero value
	// even if every tick fails to make progress. One round-trip for all
	// four tables (same values as sequential CountPending).
	//
	// On failure we fall back to the per-table queries rather than
	// skipping the update: the combined statement fails as a unit, so a
	// single bad subquery would otherwise leave ALL FOUR gauges stale and
	// an operator staring at frozen numbers with no signal why. The
	// per-table path degrades one gauge instead of four.
	specs := []entitySpec{issueSpec, commentSpec, projectSpec, componentSpec}
	if counts, err := store.CountPendingAll(ctx); err == nil {
		for _, t := range specs {
			EmbeddingsPending.WithLabelValues(t.metricName).Set(float64(counts[t.table]))
		}
	} else {
		slog.WarnContext(ctx, "count pending (combined) failed, falling back to per-table",
			"error", err.Error())
		for _, t := range specs {
			n, perErr := store.CountPending(ctx, t.table)
			if perErr != nil {
				slog.WarnContext(ctx, "count pending failed",
					"table", t.table, "error", perErr.Error())
				continue
			}
			EmbeddingsPending.WithLabelValues(t.metricName).Set(float64(n))
		}
	}

	// Process in priority order, stopping after the first table that
	// produced work — this gives issues priority during catch-up while
	// keeping per-tick cost predictable.
	for _, t := range []entitySpec{
		issueSpec, commentSpec, projectSpec, componentSpec,
	} {
		processed, err := processOne(ctx, pool, store, embedder, t, batchSize)
		if err != nil {
			EmbeddingsErrorsTotal.WithLabelValues(t.metricName, classifyErr(err)).Inc()
			slog.WarnContext(ctx, "embedding worker batch failed",
				"entity", t.metricName, "error", err.Error())
			// Don't break — try next entity.  Maybe issues are jammed
			// on a poison row but comments work fine.
			continue
		}
		if processed > 0 {
			slog.InfoContext(ctx, "embedding worker processed batch",
				"entity", t.metricName, "rows", processed)
			// Stop after first productive entity so the next tick can
			// re-evaluate priorities (e.g. new issues created during
			// this tick should be ahead of older comments next time).
			return
		}
	}
}

// processOne runs one batch for one entity. Returns the number of rows
// actually embedded.
func processOne(
	ctx context.Context,
	pool *pgxpool.Pool,
	store *Store,
	embedder *Embedder,
	spec entitySpec,
	batchSize int,
) (int, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	rows, err := store.FetchPendingBatch(ctx, tx, spec.table, spec.idCol,
		spec.columns, spec.compose, batchSize)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		// Commit the empty tx so the deferred rollback doesn't whine.
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// Embed.  We use the worker timeout (60s) since cold-start of a big
	// batch on the GPU server can be slow; the search hot-path uses 5s
	// via embedder.SearchTimeout.
	embedCtx, cancel := context.WithTimeout(ctx, embedder.embedTimeout)
	defer cancel()

	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.Text
	}
	vecs, err := embedder.Embed(embedCtx, texts, spec.metricName)
	if err != nil {
		// Tx rollback releases the SKIP LOCKED rows so the next
		// tick (or another replica) can retry them.
		return 0, err
	}

	if err := store.WriteEmbeddings(ctx, tx, spec.table, spec.idCol, rows, vecs); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	// Bump truncation counter once per truncated row, only after Commit
	// succeeds — otherwise a persistently failing oversized row would
	// inflate the counter on every retry tick.
	for _, r := range rows {
		if r.Truncated {
			EmbeddingsTruncatedTotal.WithLabelValues(spec.metricName).Inc()
		}
	}

	EmbeddingsProcessedTotal.WithLabelValues(spec.metricName).Add(float64(len(rows)))
	return len(rows), nil
}

// classifyErr maps an error to a low-cardinality reason label for the
// errors_total metric.  Values: ollama, timeout, db, other.
func classifyErr(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, ErrEmbedderUnavailable) {
		return "ollama"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "db"
}

// entitySpec describes per-entity worker bindings: which table, which
// id column, which text columns to fetch, and how to fold them into the
// embedder input.  Defining this here keeps the worker generic; new
// entities only need a new spec without touching tick / processOne.
type entitySpec struct {
	table      string
	idCol      string
	columns    []string                        // ordered, fed into compose as []any of strings
	compose    func(cols []any) (string, bool) // returns (text, truncated)
	metricName string                          // label for prometheus
}

// composeXxx callbacks bridge the generic FetchPendingBatch SELECT into
// each Build*EmbedText helper. Columns are returned by FetchPendingBatch
// in the order declared in `columns`.

var issueSpec = entitySpec{
	table:      "issues",
	idCol:      "id",
	columns:    []string{"title", "description", "acceptance_criteria"},
	metricName: "issue",
	compose: func(cols []any) (string, bool) {
		title, _ := cols[0].(string)
		desc, _ := cols[1].(string)
		acc, _ := cols[2].(string)
		return BuildIssueEmbedText(title, desc, acc)
	},
}

var commentSpec = entitySpec{
	table:      "comments",
	idCol:      "id",
	columns:    []string{"body"},
	metricName: "comment",
	compose: func(cols []any) (string, bool) {
		body, _ := cols[0].(string)
		return BuildCommentEmbedText(body)
	},
}

var projectSpec = entitySpec{
	table:      "projects",
	idCol:      "id",
	columns:    []string{"name", "description"},
	metricName: "project",
	compose: func(cols []any) (string, bool) {
		name, _ := cols[0].(string)
		desc, _ := cols[1].(string)
		return BuildProjectEmbedText(name, desc)
	},
}

var componentSpec = entitySpec{
	table:      "components",
	idCol:      "id",
	columns:    []string{"name", "description"},
	metricName: "component",
	compose: func(cols []any) (string, bool) {
		name, _ := cols[0].(string)
		desc, _ := cols[1].(string)
		return BuildComponentEmbedText(name, desc)
	},
}
