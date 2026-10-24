package migrations

// conjuncts_test.go — sk-303.14, repair A/8.
//
// The assertion about the NUMBER of conjuncts in a partial-index body is
// the only thing that stops a fourth condition appearing silently, past
// the applicability gate (plan on volume). So the count itself must be
// correct, not "correct for today's body".
//
// The previous formula `strings.Count(body, " AND ") + 1` is wrong for
// any body with parentheses: `(a AND b) OR c` has one conjunct, and the
// formula gives two. The error did not show while the body was flat, and
// would have shown exactly when the assertion is supposed to fire.

import "testing"

func TestCountTopLevelConjuncts(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"empty", "", 0},
		{"one condition", "accepted_at IS NULL", 1},
		{"today's index body", "(status <> ALL (ARRAY['closed'::text, 'standing'::text])) " +
			"AND (accepted_at IS NULL) AND (assignee_set_at IS NOT NULL)", 3},
		{"nested AND inside parens", "((a AND b) OR c)", 1},
		{"nested AND and top level", "((a AND b) OR c) AND d", 2},
		{"AND inside a string literal", "note <> ' AND '", 1},
		{"two nesting levels", "(a AND (b AND c)) AND (d OR e)", 2},
		// pg_get_indexdef wraps the whole body in an outer pair of parens.
		{"outer parens as in pg_get_indexdef",
			"((a IS NULL) AND (b IS NULL) AND (c IS NULL))", 3},
		{"outer parens around one condition", "((a IS NULL))", 1},
		{"not outer parens", "(a) AND (b)", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := countTopLevelConjuncts(tc.body); got != tc.want {
				t.Errorf("counted %d conjuncts, want %d in body %q — the assertion about "+
					"partial-body composition is counting the wrong thing, and an extra "+
					"condition will pass silently past the applicability gate", got, tc.want, tc.body)
			}
		})
	}
}
