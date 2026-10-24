package review

// set_validation_test.go — sk-303.14, repair A/8.
//
// Validators of forms that get spliced into SQL text: a table alias, an
// `alias.column` expression, and a user-identifier expression (placeholder
// or column). Before the repair they had NOT A SINGLE test: a mutation
// "empty body of validateColumnExpr" went green across the whole
// `go test ./...`, and the package had no test file for set.go at all.
//
// The guard itself is correct: its critique could not be bypassed (24
// variants — quotes, comments, newlines, homoglyphs, a placeholder,
// `a.b.c`), and it has no reachability from outside — every construction
// site passes Go literals. But "correct and untested" is exactly the state
// from which a guard vanishes on the next edit, showing nothing. So the
// tests here are not about "we fear injection", they are about the form
// continuing to be checked.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// mustPanic calls f and requires a panic whose message has the expected
// prefix.
//
// The TEXT is checked, not only the fact of a panic: a panic for a wholly
// different reason (nil dereference in changed code) would pass an "it
// fell over" check green and declare the guard alive when it is already
// gone.
func mustPanic(t *testing.T, wantPrefix string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("no panic — the SQL-splice form is no longer checked")
			return
		}
		msg, ok := r.(string)
		if !ok {
			t.Errorf("panic is not a string: %#v", r)
			return
		}
		if !strings.HasPrefix(msg, wantPrefix) {
			t.Errorf("panic %q, want prefix %q — fell over for another reason, not "+
				"the form check", msg, wantPrefix)
		}
	}()
	f()
}

// badExprs — forms the `alias.column` validator must reject. Each catches
// its own regular-expression breakage.
var badExprs = []string{
	"",                          // empty
	"i",                         // one part — not a column expression
	"i.",                        // tail lost
	".created_by",               // head lost
	"i.a.b",                     // three parts: end anchor does not work
	"I.created_by",              // uppercase
	"i.created_by ",             // trailing space
	" i.created_by",             // leading space
	"i.created_by, 1",           // second column via comma
	"i.created_by--",            // start of a comment
	"i.created_by/*x*/",         // block comment
	"i.created_by\n",            // newline: $ and ^ anchors must be strict
	"i.created_by\nOR 1=1",      // multiline splice
	"1",                         // literal
	"$1",                        // placeholder — not valid for THIS form
	"\"i\".\"created_by\"",      // quotes
	"i.created_by)",             // closing parenthesis
	"i.сreated_by",              // homoglyph: Cyrillic "с"
	"i.created_by OR true",      // condition tail
	"(SELECT 1)",                // subquery
	"i.created_by;DROP TABLE x", // statement separator
}

// TestValidateColumnExpr_RejectsEverythingButTwoIdentifiers.
func TestValidateColumnExpr_RejectsEverythingButTwoIdentifiers(t *testing.T) {
	for _, expr := range badExprs {
		t.Run("rejects "+expr, func(t *testing.T) {
			mustPanic(t, "review: invalid SQL column expression", func() {
				validateColumnExpr(expr)
			})
		})
	}

	// The other half: legal forms must pass. Without it "we always panic"
	// would pass the check above in full, and the fragments would stop
	// being built at all.
	for _, expr := range []string{
		"i.created_by", "ru.id", "u.id", "_x._y", "a1.b2",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("legal form %q rejected: %v — set fragments "+
						"will stop being built", expr, r)
				}
			}()
			validateColumnExpr(expr)
		}()
	}
}

// TestValidateAlias_RejectsEverythingButAnIdentifier.
func TestValidateAlias_RejectsEverythingButAnIdentifier(t *testing.T) {
	for _, alias := range []string{
		"", "I", "i.j", "i ", " i", "1i", "i;", "i--", "i\n", "\"i\"", "i)",
		"i OR 1=1", "и",
	} {
		t.Run("rejects "+alias, func(t *testing.T) {
			mustPanic(t, "review: invalid SQL alias", func() { validateAlias(alias) })
		})
	}
	for _, alias := range []string{"i", "issue", "_i", "i2", "a_b_c"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("legal alias %q rejected: %v", alias, r)
				}
			}()
			validateAlias(alias)
		}()
	}
}

// TestValidateUserExpr_AcceptsPlaceholderOrColumnAndNothingElse.
//
// This form is WIDER than the previous one by exactly the placeholder — and
// by that one thing, so both sides are checked: the placeholder passes, and
// everything that looks like one is rejected.
func TestValidateUserExpr_AcceptsPlaceholderOrColumnAndNothingElse(t *testing.T) {
	for _, expr := range []string{
		"", "$", "$0a", "$1a", "$1234", "$-1", "$ 1", "$1 ", "$1;",
		"$1 OR true", "i", "i.a.b", "I.id", "i.id ", "i.id--", "i.id\n",
		"1", "true", "(SELECT id FROM users)",
	} {
		t.Run("rejects "+expr, func(t *testing.T) {
			mustPanic(t, "review: invalid SQL user expression", func() {
				ValidateUserExpr(expr)
			})
		})
	}
	for _, expr := range []string{"$1", "$12", "$123", "ru.id", "i.created_by"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("legal form %q rejected: %v", expr, r)
				}
			}()
			ValidateUserExpr(expr)
		}()
	}
}

// TestPredicates_RefuseAMalformedExpression — the validators sit ON THE PATH
// of fragment construction, not beside it.
//
// The assertion is separate because checking the function says nothing about
// whether it is called. A mutation "drop the validateColumnExpr call from
// UnreviewedArgs.caller / UnacceptedArgs.addressee" goes red right here.
func TestPredicates_RefuseAMalformedExpression(t *testing.T) {
	bind := func(any) string { return "$1" }

	mustPanic(t, "review: invalid SQL column expression", func() {
		UnreviewedPredicate(bind, UnreviewedArgs{
			Alias: "i", CallerExpr: "i.created_by OR true"})
	})
	mustPanic(t, "review: invalid SQL column expression", func() {
		UnacceptedPredicate(bind, UnacceptedArgs{
			Alias: "i", AddresseeExpr: "ru.id; DROP TABLE issues"})
	})
	mustPanic(t, "review: invalid SQL alias", func() {
		UnreviewedPredicate(bind, UnreviewedArgs{Alias: "i;x", CallerID: uuid.New()})
	})
	mustPanic(t, "review: invalid SQL alias", func() {
		UnacceptedPredicate(bind, UnacceptedArgs{Alias: "i;x", AddresseeID: uuid.New()})
	})
	mustPanic(t, "review: invalid SQL user expression", func() {
		ValidateUserExpr("u.id OR true")
	})
}

// TestCategories_AreDerivedFromTheRegistry — the category set is derived
// from the declarations, not written next to them.
//
// Before repair A/8 the set existed as THREE handwritten copies: the
// constant declarations, the Categories() list, and the Valid() predicate.
// A third category introduced as a constant would silently miss both the
// worker's sweep and the admissibility check.
func TestCategories_AreDerivedFromTheRegistry(t *testing.T) {
	got := Categories()
	if len(got) == 0 {
		t.Fatal("category set is empty — the worker's sweep would walk none")
	}
	for _, c := range got {
		if !c.Valid() {
			t.Errorf("category %q is in the set, but Valid() does not admit it — "+
				"the enumeration and the admissibility check have drifted", c)
		}
	}
	for _, c := range []Category{CategoryUnaccepted, CategoryUnreviewed} {
		if !c.Valid() {
			t.Errorf("declared category %q is not admitted", c)
		}
		found := false
		for _, x := range got {
			if x == c {
				found = true
			}
		}
		if !found {
			t.Errorf("declared category %q did not enter the set — the worker "+
				"will not sweep it, and reminders for it will never form", c)
		}
	}
	if Category("weekly").Valid() {
		t.Error("unregistered category admitted — the set is no longer closed")
	}

	// A COPY is returned: a consumer that sorted or truncated the slice in
	// place would change what the background worker sweeps.
	got[0] = Category("mutated")
	if Categories()[0] == Category("mutated") {
		t.Error("Categories() returns the set itself, not a copy — a consumer can " +
			"change what the worker sweeps")
	}
}
