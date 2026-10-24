package status

import "testing"

// TestPredicates_ByteStable pins the exact SQL text each helper emits for the
// real aliases used across the query tree. These literals are spliced into
// hand-written SQL with the contract that the assembled string is byte-identical
// to the pre-refactor literal — so any drift here is a behavior change and must
// fail loudly.
func TestPredicates_ByteStable(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		// ActiveWork — issue/child-side "counts as active work". F2g (sk-118.8)
		// widened the body to exclude `standing` so a standing issue stops
		// counting as active work. It now matches GatesReadiness text again (both
		// `NOT IN ('closed', 'standing')`) but the two helpers keep DISTINCT
		// identities so future divergence stays possible.
		{"ActiveWork i", ActiveWork("i"), "i.status NOT IN ('closed', 'standing')"},
		{"ActiveWork ch", ActiveWork("ch"), "ch.status NOT IN ('closed', 'standing')"},
		{"ActiveWork children", ActiveWork("children"), "children.status NOT IN ('closed', 'standing')"},

		// GatesReadiness — blocker-side readiness gate. F2f (sk-118.7) widened
		// the body to exclude `standing` so a standing blocker stops gating
		// readiness.
		{"GatesReadiness b", GatesReadiness("b"), "b.status NOT IN ('closed', 'standing')"},
		{"GatesReadiness i", GatesReadiness("i"), "i.status NOT IN ('closed', 'standing')"},

		// IsClosed — terminal-state test.
		{"IsClosed i", IsClosed("i"), "i.status = 'closed'"},
		{"IsClosed c", IsClosed("c"), "c.status = 'closed'"},
		{"IsClosed b", IsClosed("b"), "b.status = 'closed'"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestPredicates_ActiveWorkGatesReadinessBothWidened records the end-state after
// F2f (sk-118.7) widened GatesReadiness and F2g (sk-118.8) widened ActiveWork:
// both now emit `NOT IN ('closed', 'standing')`, so a standing issue is excluded
// from BOTH active-work counts and readiness gating. The F1-era tripwire asserted
// the two were identical (`<> 'closed'`); the F2f successor asserted they had
// diverged. This is the final-state tripwire: both must be the widened literal —
// catching either an accidental narrowing back to `<> 'closed'` or a typo in the
// widened text. The helpers keep distinct identities even though the text matches
// today.
func TestPredicates_ActiveWorkGatesReadinessBothWidened(t *testing.T) {
	for _, alias := range []string{"i", "b", "ch", "children"} {
		aw := ActiveWork(alias)
		gr := GatesReadiness(alias)
		want := alias + ".status NOT IN ('closed', 'standing')"
		if aw != want {
			t.Errorf("alias %q: ActiveWork %q, want %q (F2g-widened)", alias, aw, want)
		}
		if gr != want {
			t.Errorf("alias %q: GatesReadiness %q, want %q (F2f-widened)", alias, gr, want)
		}
	}
}

// TestPredicates_PanicOnBadAlias asserts the validation guard fires for an
// alias that violates ^[a-z_][a-z0-9_]*$. The alias is always a
// compile-time-constant programmer input, so a violation is a programming
// error — panic is the contract.
func TestPredicates_PanicOnBadAlias(t *testing.T) {
	bad := []string{
		"",                     // empty
		"1i",                   // leading digit
		"I",                    // uppercase
		"i.status",             // dot — would inject SQL
		"i status",             // space
		"i'",                   // quote — injection
		"i;DROP",               // semicolon
		"i.status <> 'closed'", // a whole predicate
		"' OR '1'='1",          // classic injection
	}
	for _, alias := range bad {
		t.Run(alias, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("ActiveWork(%q): expected panic, got none", alias)
				}
			}()
			_ = ActiveWork(alias)
		})
	}
}

// TestPredicates_AcceptsValidAliases confirms the guard does NOT reject the
// legitimate aliases used in the tree.
func TestPredicates_AcceptsValidAliases(t *testing.T) {
	for _, alias := range []string{"i", "b", "c", "ch", "children", "_x", "i2", "a_b"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("alias %q: unexpected panic %v", alias, r)
				}
			}()
			_ = ActiveWork(alias)
			_ = GatesReadiness(alias)
			_ = IsClosed(alias)
		}()
	}
}
