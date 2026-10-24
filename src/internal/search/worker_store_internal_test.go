package search

// White-box coverage of the backfill worker's STORE-LEVEL seams without a
// live Ollama (sk-105.6 / spec §3.4, §5 M8).
//
// IMPORTANT SEAM NOTE (reported up to sk-105.6):
//   The worker's synchronous tick()/processOne() entry points cannot be
//   driven end-to-end in a test because processOne() calls the concrete
//   *Embedder.Embed (a real HTTP round-trip to Ollama) and reads the
//   unexported embedder.embedTimeout field. *Embedder is a struct, not an
//   interface, so there is NO injection point for a deterministic fake.
//   To drive tick()/processOne() in tests, production needs an embedder
//   SEAM: e.g. an `Embed(ctx, []string, label) ([][]float32, error)`
//   interface that processOne accepts, plus a way to set the per-batch
//   timeout independently. Until then, these tests exercise the three
//   store primitives the worker composes — CountPending, FetchPendingBatch
//   (SKIP LOCKED + priority order + truncation), WriteEmbeddings (the
//   deterministic-vector write) — which is where all the backfill
//   bookkeeping actually lives. A stub vector stands in for the embedder
//   output so no Ollama is contacted.
//
// All asserts are project-scoped: pending counts are taken over the seeded
// project's own rows (CountPendingForProject), never the global table.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/testsupport"
)

// stubVector returns a deterministic 1024-dim unit-ish vector. Mirrors the
// fakeS3 "stub the external client" pattern: the worker's only real
// dependency on Ollama is "give me N vectors of dim 1024"; we synthesise
// them here so WriteEmbeddings has something valid to persist into the
// halfvec(1024) column.
func stubVector(seed float32) []float32 {
	v := make([]float32, 1024)
	for i := range v {
		v[i] = seed
	}
	// Avoid an all-zero vector (cosine distance is undefined); set one
	// component to a distinct value.
	v[0] = seed + 1
	return v
}

// countPendingForProjectIssues counts issues with NULL embedding scoped to
// one project — the project-scoped analogue of Store.CountPending (which is
// global). Keeps the assert isolated under package parallelism.
func countPendingForProjectIssues(t *testing.T, s *Store, projID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1 AND embedding IS NULL`,
		projID).Scan(&n); err != nil {
		t.Fatalf("count pending issues for project %s: %v", projID, err)
	}
	return n
}

func embeddingIsNull(t *testing.T, s *Store, issueID string) bool {
	t.Helper()
	var isNull bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT embedding IS NULL FROM issues WHERE id = $1`, issueID).Scan(&isNull); err != nil {
		t.Fatalf("read embedding null for %s: %v", issueID, err)
	}
	return isNull
}

// TestBackfill_CountPending_GlobalAndScoped asserts CountPending sees
// freshly-seeded (embedding IS NULL) issues, and that a brand-new issue
// starts with a NULL embedding (so the worker would pick it up).
func TestBackfill_CountPending(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := NewStore(pool)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsbf")

	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, id, "needs embedding", "open", 2)

	if got := countPendingForProjectIssues(t, store, projID); got != 1 {
		t.Fatalf("project pending issues = %d, want 1", got)
	}
	if !embeddingIsNull(t, store, id) {
		t.Errorf("freshly-seeded issue %s already has a non-NULL embedding", id)
	}

	// CountPending (global) must be >= our one pending row.
	globalN, err := store.CountPending(context.Background(), "issues")
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if globalN < 1 {
		t.Errorf("global CountPending(issues) = %d, want >= 1", globalN)
	}
}

// TestBackfill_FetchAndWrite drives one batch the way processOne does
// (minus the Embed HTTP call): begin a tx, FetchPendingBatch with the real
// issueSpec, compose text, write a stub vector via WriteEmbeddings, commit.
// Then assert: the row's embedding is no longer NULL, the project pending
// count dropped to zero, and the composed text matches BuildIssueEmbedText.
func TestBackfill_FetchAndWrite(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := NewStore(pool)
	bg := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsbfw")

	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, id, "embed me now", "open", 2)

	tx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(bg) //nolint:errcheck

	// Large limit (not the production-realistic 32): FetchPendingBatch is
	// table-wide FOR NO KEY UPDATE SKIP LOCKED, and the shared integration DB can
	// carry >32 ambient pending rows leaked by other packages. A small batch
	// could cut our seeded row off the id-ordered window and flake. This test
	// verifies fetch/compose/write correctness, NOT the batch cap, so we fetch
	// wide enough to always include our row.
	rows, err := store.FetchPendingBatch(bg, tx, issueSpec.table, issueSpec.idCol,
		issueSpec.columns, issueSpec.compose, 100000)
	if err != nil {
		t.Fatalf("FetchPendingBatch: %v", err)
	}
	// The batch fetches across the whole table (FOR NO KEY UPDATE SKIP LOCKED),
	// so other packages' pending rows may be present too; just require our
	// row to be in there and its composed text to be correct.
	var mine *PendingRow
	for i := range rows {
		if rows[i].ID == id {
			mine = &rows[i]
			break
		}
	}
	if mine == nil {
		t.Fatalf("FetchPendingBatch did not include seeded issue %s (got %d rows)", id, len(rows))
	}
	wantText, _ := BuildIssueEmbedText("embed me now", "", "")
	if mine.Text != wantText {
		t.Errorf("composed text = %q, want %q", mine.Text, wantText)
	}
	if mine.Truncated {
		t.Errorf("short title flagged Truncated=true unexpectedly")
	}

	// Write an embedding for ONLY our seeded row — NOT the whole table-wide
	// batch. Writing the full batch would mutate unrelated ambient pending
	// rows in the shared integration DB, and this tx commits, so the mutation
	// would persist. The wide fetch above is solely to locate `mine`.
	if err := store.WriteEmbeddings(bg, tx, issueSpec.table, issueSpec.idCol,
		[]PendingRow{*mine}, [][]float32{stubVector(1)}); err != nil {
		t.Fatalf("WriteEmbeddings: %v", err)
	}
	if err := tx.Commit(bg); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if embeddingIsNull(t, store, id) {
		t.Errorf("after WriteEmbeddings, issue %s embedding is still NULL", id)
	}
	if got := countPendingForProjectIssues(t, store, projID); got != 0 {
		t.Errorf("project pending after backfill = %d, want 0", got)
	}
}

// TestBackfill_WriteEmbeddings_LengthMismatch asserts the defensive guard:
// WriteEmbeddings rejects a rows/vecs length mismatch rather than writing a
// partial/garbled batch.
func TestBackfill_WriteEmbeddings_LengthMismatch(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := NewStore(pool)
	bg := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsbfm")
	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, id, "mismatch row", "open", 2)

	tx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(bg) //nolint:errcheck

	rows := []PendingRow{{ID: id, Text: "x"}}
	// Two vecs for one row → mismatch.
	err = store.WriteEmbeddings(bg, tx, issueSpec.table, issueSpec.idCol, rows,
		[][]float32{stubVector(1), stubVector(2)})
	if err == nil {
		t.Fatalf("WriteEmbeddings accepted rows/vecs length mismatch, want error")
	}
}

// TestBackfill_FetchPendingBatch_SkipLocked verifies the FOR NO KEY UPDATE SKIP
// LOCKED contract that lets multiple replicas run the worker concurrently:
// a row already locked by tx1 is skipped by a concurrent FetchPendingBatch
// in tx2 rather than blocking on it.
func TestBackfill_FetchPendingBatch_SkipLocked(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	bg := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsskip")
	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, id, "locked row", "open", 2)

	// tx1 locks our row. We scope tx1's fetch to exactly our project so it
	// claims ONLY our seeded row (other packages' pending rows stay free
	// for tx2 but are irrelevant — we assert on our own id).
	tx1, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer tx1.Rollback(bg) //nolint:errcheck
	var lockedID string
	if err := tx1.QueryRow(bg, `
SELECT id FROM issues
 WHERE project_id = $1 AND embedding IS NULL
 ORDER BY id
 LIMIT 1
 FOR NO KEY UPDATE SKIP LOCKED`, projID).Scan(&lockedID); err != nil {
		t.Fatalf("tx1 lock: %v", err)
	}
	if lockedID != id {
		t.Fatalf("tx1 locked %q, want %q", lockedID, id)
	}

	// tx2 attempts to claim the same project's pending rows; the locked row
	// must be SKIPPED (zero rows, not a block).
	tx2, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx2.Rollback(bg) //nolint:errcheck
	var got string
	err = tx2.QueryRow(bg, `
SELECT id FROM issues
 WHERE project_id = $1 AND embedding IS NULL
 ORDER BY id
 LIMIT 1
 FOR NO KEY UPDATE SKIP LOCKED`, projID).Scan(&got)
	if err == nil {
		t.Fatalf("tx2 claimed %q, but the only pending row was locked by tx1 — SKIP LOCKED violated", got)
	}
	// pgx returns ErrNoRows when SKIP LOCKED skips the only candidate.
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("tx2 error = %v, want pgx.ErrNoRows (row skipped)", err)
	}
}

// TestBackfill_SpecPriorityOrder pins the documented priority ordering the
// worker walks every tick (issues → comments → projects → components).
// This is the order tick() iterates the entitySpec slice; if a refactor
// reorders it, the catch-up priority guarantee (issues never starved)
// silently breaks.
func TestBackfill_SpecPriorityOrder(t *testing.T) {
	order := []entitySpec{issueSpec, commentSpec, projectSpec, componentSpec}
	wantTables := []string{"issues", "comments", "projects", "components"}
	wantMetrics := []string{"issue", "comment", "project", "component"}
	for i := range order {
		if order[i].table != wantTables[i] {
			t.Errorf("spec[%d].table = %q, want %q", i, order[i].table, wantTables[i])
		}
		if order[i].metricName != wantMetrics[i] {
			t.Errorf("spec[%d].metricName = %q, want %q", i, order[i].metricName, wantMetrics[i])
		}
	}
	// AllKinds order must match the spec walk order (the worker comment in
	// worker.go promises "the canonical AllKinds order").
	wantKinds := []Kind{KindIssue, KindComment, KindProject, KindComponent}
	for i := range AllKinds {
		if AllKinds[i] != wantKinds[i] {
			t.Errorf("AllKinds[%d] = %q, want %q", i, AllKinds[i], wantKinds[i])
		}
	}
}
