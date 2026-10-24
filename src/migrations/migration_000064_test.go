package migrations

// migration_000064_test.go — sk-622.21, T026, matrix row No. 55 (the migration
// part: "migration up/down/up"). Reference — plan K16, decision R26 item 1 and
// the T009 contract (issues.IssueSummary.RowSeq: 0 = the issue row has not
// been written since the phase-2 rollout, order unknown), written before
// the migration:
//
//   - up does NOT rewrite `issues` or its indexes: the column is added with
//     a constant default, which PostgreSQL keeps in the catalogue — so the
//     relfilenode of the table and of every index (HNSW included) is the
//     same before and after up;
//   - there is no backfill: an EXISTING issue reads row_seq = 0;
//   - after up the column is NOT NULL and has no default — the order comes
//     only from the database, on every INSERT and UPDATE of the issue row,
//     positive and growing, whatever the writer supplies — including a
//     writer that does not know the column (the previous code version
//     during rollout, exercised here by its row shape);
//   - down removes the column, the trigger, its function and the sequence;
//   - up after down works again (up/down/up), and rows read 0 again.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// m64RowSeq reads row_seq of one issue.
func m64RowSeq(t *testing.T, pool *pgxpool.Pool, stage, issueID string) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(),
		`SELECT row_seq FROM issues WHERE id = $1`, issueID).Scan(&seq); err != nil {
		t.Fatalf("%s: read row_seq of %s: %v", stage, issueID, err)
	}
	return seq
}

// m64Storage returns the relfilenode of `issues` and of each of its indexes,
// keyed by relation name. A table or index rewrite gives the relation a new
// relfilenode; a catalogue-only ALTER keeps it.
func m64Storage(t *testing.T, pool *pgxpool.Pool, stage string) map[string]uint32 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT c.relname, c.relfilenode::bigint
  FROM pg_class c
 WHERE c.oid = 'issues'::regclass
    OR c.oid IN (SELECT indexrelid FROM pg_index WHERE indrelid = 'issues'::regclass)`)
	if err != nil {
		t.Fatalf("%s: read relfilenodes: %v", stage, err)
	}
	defer rows.Close()
	out := map[string]uint32{}
	for rows.Next() {
		var name string
		var node int64
		if err := rows.Scan(&name, &node); err != nil {
			t.Fatalf("%s: scan relfilenode: %v", stage, err)
		}
		out[name] = uint32(node)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: relfilenodes: %v", stage, err)
	}
	if len(out) < 2 {
		t.Fatalf("%s: read %d relations of issues, want the table and its indexes", stage, len(out))
	}
	return out
}

// m64Schema reports what of the migration's schema objects exist.
type m64Schema struct {
	column, notNull, hasDefault, trigger, function, sequence bool
}

func m64ReadSchema(t *testing.T, pool *pgxpool.Pool, stage string) m64Schema {
	t.Helper()
	ctx := context.Background()
	var s m64Schema
	err := pool.QueryRow(ctx, `
SELECT true, a.attnotnull, a.atthasdef
  FROM pg_attribute a
 WHERE a.attrelid = 'issues'::regclass AND a.attname = 'row_seq' AND NOT a.attisdropped`).
		Scan(&s.column, &s.notNull, &s.hasDefault)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("%s: read column: %v", stage, err)
	}
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_trigger
                WHERE tgrelid = 'issues'::regclass AND tgname = 'issues_stamp_row_seq' AND NOT tgisinternal),
       EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'skein_issues_stamp_row_seq'),
       to_regclass('issues_row_seq_seq') IS NOT NULL`).
		Scan(&s.trigger, &s.function, &s.sequence); err != nil {
		t.Fatalf("%s: read trigger/function/sequence: %v", stage, err)
	}
	return s
}

// TestMigration000064_UpDownUp — see the file header.
func TestMigration000064_UpDownUp(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")
	ctx := context.Background()

	owner := seedUser(t, pool, "m64o")
	pid, prefix := seedProject(t, pool, owner, "m64")
	a, b := prefix+"-1", prefix+"-2"

	// --- before the migration: rows written without the column ---
	if err := m.Migrate(63); err != nil {
		t.Fatalf("Migrate(63): %v", err)
	}
	if s := m64ReadSchema(t, pool, "at 63"); s != (m64Schema{}) {
		t.Fatalf("at 63: %+v, want none of the migration's objects", s)
	}
	seedIssue(t, pool, a, pid, owner, "")
	seedIssue(t, pool, b, pid, owner, "")

	for round := 1; round <= 2; round++ {
		stage := "up #" + string(rune('0'+round))
		before := m64Storage(t, pool, stage+" (before)")
		if err := m.Migrate(64); err != nil {
			t.Fatalf("%s: Migrate(64): %v", stage, err)
		}
		s := m64ReadSchema(t, pool, stage)
		if !s.column || !s.notNull || s.hasDefault || !s.trigger || !s.function || !s.sequence {
			t.Fatalf("%s: schema %+v, want column NOT NULL without a default, trigger, function, sequence", stage, s)
		}

		// No rewrite of the table or of any of its indexes.
		after := m64Storage(t, pool, stage+" (after)")
		for name, node := range before {
			if got, ok := after[name]; !ok || got != node {
				t.Errorf("%s: relfilenode of %s = %d (present %v), was %d — up rewrote it", stage, name, got, ok, node)
			}
		}

		// No backfill: rows that existed before up read 0 ("order unknown").
		existing := []string{a, b}
		if round == 2 {
			existing = append(existing, prefix+"-3") // inserted in round 1
		}
		for _, id := range existing {
			if got := m64RowSeq(t, pool, stage, id); got != 0 {
				t.Errorf("%s: existing row %s row_seq = %d, want 0 (no backfill)", stage, id, got)
			}
		}

		// A row inserted in the previous version's shape (no row_seq named)
		// gets a positive order from the database.
		c := prefix + "-" + string(rune('2'+round))
		seedIssue(t, pool, c, pid, owner, "")
		seqC := m64RowSeq(t, pool, stage, c)
		if seqC <= 0 {
			t.Errorf("%s: new row row_seq = %d, want positive", stage, seqC)
		}

		// An UPDATE that names neither row_seq nor version moves the order
		// past every value given so far, and again on the next UPDATE.
		if _, err := pool.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE id = $1`, a); err != nil {
			t.Fatalf("%s: touch %s: %v", stage, a, err)
		}
		seqA := m64RowSeq(t, pool, stage, a)
		if seqA <= seqC {
			t.Errorf("%s: after UPDATE row_seq of %s = %d, want greater than %d", stage, a, seqA, seqC)
		}
		if _, err := pool.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE id = $1`, a); err != nil {
			t.Fatalf("%s: touch %s again: %v", stage, a, err)
		}
		if got := m64RowSeq(t, pool, stage, a); got <= seqA {
			t.Errorf("%s: after second UPDATE row_seq of %s = %d, want greater than %d", stage, a, got, seqA)
		}

		// A value supplied by the writer is overwritten — including 0.
		for _, forced := range []int64{1, 0} {
			if _, err := pool.Exec(ctx, `UPDATE issues SET row_seq = $2 WHERE id = $1`, b, forced); err != nil {
				t.Fatalf("%s: write row_seq = %d directly: %v", stage, forced, err)
			}
			if got := m64RowSeq(t, pool, stage, b); got <= seqA {
				t.Errorf("%s: writer-supplied row_seq = %d left %d, want it overwritten by a fresh order above %d", stage, forced, got, seqA)
			}
		}

		if round == 2 {
			break
		}
		// --- down ---
		if err := m.Migrate(63); err != nil {
			t.Fatalf("Migrate(63) (000064 down): %v", err)
		}
		if s := m64ReadSchema(t, pool, "after down"); s != (m64Schema{}) {
			t.Fatalf("after down: %+v, want every object of the migration gone", s)
		}
	}
}
