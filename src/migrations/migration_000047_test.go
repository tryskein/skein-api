package migrations

// migration_000047_test.go — sk-303.14, task T017.
//
// The migration introduces a PARTIAL index under the hot unaccepted-set
// query. Checking "the index exists" by name is useless: the name says
// nothing about the WHERE body or the leading column, and those are
// exactly where a mistake can be made. So here the index DEFINITION is
// compared, as pg_get_indexdef returns it.
//
// Applicability of the index by the real select is not here: that is the
// matrix-row-86 gate, class T020, where there is a representative volume
// and query plans. Here is only that the migration created exactly what
// that class rests on, and that rollback drops it whole.
//
// # Why the NUMBER of conjuncts is checked, not only their presence
//
// For a partial index, applicability is proved by the planner: every
// conjunct of the WHERE body must follow from the query WHERE, otherwise
// the index stops being chosen — quietly, because the index itself stays
// in place, the definition looks "even stricter", and the difference is
// visible only in the plan. Checked by measurement: the conjunct
// `priority = 1`, not implied by the select, returns the plan to a
// sequential scan.
//
// Hence the split of gates. APPLICABILITY checks the plan on a
// representative volume (class T020, matrix row 86) — only it can check
// it. Here the BODY COMPOSITION is checked: exactly three conjuncts, not
// one more. Not because a fourth is necessarily harmful — `assignee IS
// NOT NULL`, for example, is harmless and merely redundant — but because
// harmfulness of a fourth cannot be established by reading: the plan
// establishes it. The number assertion does not let a conjunct appear
// silently, past that gate.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// unacceptedIndexRequired — definition fragments, each of which must be
// present. Fragments are chosen so each catches its own breakage.
var unacceptedIndexRequired = map[string][]string{
	"idx_issues_unaccepted_assignee": {
		// Index key — the only column, the join key to users. Without it
		// the select has no entry into the index at all.
		//
		// ⚠ The second column (assignee_set_at DESC) was DROPPED by repair
		// A/8: its justification ("spares the fold a heap visit") did not
		// survive measurement — the plan node is Index Scan, not Index
		// Only Scan, and a one-column index yields the same plan. Details
		// and numbers are in the migration header.
		"(lower(assignee))",
		// Partial body in full. The first two conditions repeat the
		// unaccepted fragment (internal/review/set.go) verbatim — the text
		// match is what the implication rests on.
		"status <> ALL (ARRAY['closed'::text, 'standing'::text])",
		"accepted_at IS NULL",
		// The third condition is put by the select itself: a row without
		// a schedule base produces no reminders.
		"assignee_set_at IS NOT NULL",
	},
}

// unacceptedIndexConjuncts — how many conditions must be in the PARTIAL BODY.
//
// ⚠ CORRECTED DESCRIPTION (repair A/8). It used to say: "exactly as many
// as listed in unacceptedIndexRequired". That is wrong and was wrong from
// the start: that map lists fragments of the WHOLE index definition —
// both the key and the body — and this number describes only the body.
// The literal is correct (there really are three conditions), the
// inference is not; a wrong inference is more dangerous than a wrong
// number, because the next reader will fix the number by it.
//
// The number and the fragment list describe the body from two sides —
// "these are present" and "there are no others" — and the list has no
// second half: it cannot say that an extra condition did not appear.
var unacceptedIndexConjuncts = map[string]int{
	"idx_issues_unaccepted_assignee": 3,
}

// unwrapOuterParens strips parentheses that wrap the body as a WHOLE.
//
// pg_get_indexdef prints the partial body as `((a) AND (b) AND (c))` —
// the outer pair of parens hugs everything. Without stripping it, a
// top-level count would give one on any body, i.e. the composition
// assertion would always go red and would be turned off. Exactly the
// pair whose closing paren is last is stripped: `(a) AND (b)` is not
// touched.
func unwrapOuterParens(body string) string {
	for len(body) >= 2 && body[0] == '(' {
		depth := 0
		inQuote := false
		closesAtEnd := false
		for i := 0; i < len(body); i++ {
			switch {
			case body[i] == '\'':
				inQuote = !inQuote
			case inQuote:
			case body[i] == '(':
				depth++
			case body[i] == ')':
				depth--
				if depth == 0 {
					closesAtEnd = i == len(body)-1
					i = len(body) // exit the loop
				}
			}
		}
		if !closesAtEnd {
			return body
		}
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	return body
}

// partialBody returns the WHERE body from an index definition.
func partialBody(t *testing.T, def string) string {
	t.Helper()
	i := strings.Index(def, " WHERE ")
	if i < 0 {
		return ""
	}
	return def[i+len(" WHERE "):]
}

// countTopLevelConjuncts counts conjuncts of the partial body — those that
// stand at the TOP parenthesis level.
//
// ⚠ CORRECTED COUNT (repair A/8). It used to be
// `strings.Count(body, " AND ") + 1` — a count that is wrong for any body
// with a nested ` AND `: `(a AND b) OR c` has one conjunct, and the
// formula gives two. Today's body is flat, so the error did not show; it
// would have shown exactly when someone added a parenthesised condition
// — i.e. when the assertion is supposed to fire.
//
// String literals are skipped: ' AND ' inside quotes is data, not an
// operator.
func countTopLevelConjuncts(body string) int {
	body = unwrapOuterParens(strings.TrimSpace(body))
	if body == "" {
		return 0
	}
	const sep = " AND "
	n := 1
	depth := 0
	inQuote := false
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\'':
			inQuote = !inQuote
		case inQuote:
			// count nothing inside a literal
		case body[i] == '(':
			depth++
		case body[i] == ')':
			depth--
		case depth == 0 && strings.HasPrefix(body[i:], sep):
			n++
			i += len(sep) - 1
		}
	}
	return n
}

// TestMigration000047_CreatesPartialUnacceptedIndex — the index is created,
// and created as partial, with this leading column and without extra
// conjuncts.
func TestMigration000047_CreatesPartialUnacceptedIndex(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	ctx := context.Background()

	for name, wantParts := range unacceptedIndexRequired {
		def := indexDef(t, ctx, name)
		if def == "" {
			t.Errorf("index %s is missing", name)
			continue
		}
		if !strings.Contains(def, "WHERE") {
			t.Errorf("index %s is not partial — WHERE body is lost, the index covers the whole table: %s",
				name, def)
		}
		for _, part := range wantParts {
			if !strings.Contains(def, part) {
				t.Errorf("index %s definition has no %q: %s", name, part, def)
			}
		}
		body := partialBody(t, def)
		if body == "" {
			t.Errorf("index %s definition has no WHERE body: %s", name, def)
			continue
		}
		if got := countTopLevelConjuncts(body); got != unacceptedIndexConjuncts[name] {
			t.Errorf("partial body of index %s has %d conditions, want %d. A condition "+
				"appearing here without a plan check can turn the index off without "+
				"showing a single sign: the index itself stays in place: %s",
				name, got, unacceptedIndexConjuncts[name], body)
		}
	}
}

// TestMigration000047_DownRemovesIndex — rollback drops the migration's
// index, and a re-apply restores it.
func TestMigration000047_DownRemovesIndex(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	ctx := context.Background()

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// Return to head is registered BEFORE the down step: a failing
	// assertion would otherwise leave the shared database at version 46
	// and break fixtures of later packages in a sequential run.
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	if err := m.Migrate(46); err != nil {
		t.Fatalf("Migrate(46) (000047 down): %v", err)
	}
	for name := range unacceptedIndexRequired {
		if def := indexDef(t, ctx, name); def != "" {
			t.Errorf("after rollback index %s remained: %s", name, def)
		}
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	for name := range unacceptedIndexRequired {
		if def := indexDef(t, ctx, name); def == "" {
			t.Errorf("after re-apply index %s did not return", name)
		}
	}
}
