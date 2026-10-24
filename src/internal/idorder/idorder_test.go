package idorder

import (
	"sort"
	"testing"
)

// idOrderReference — the REFERENCE ORDER from the appendix "Reference id sample"
// of the sk-421.2 plan. The list is literal and NOT derived from
// the implementation: a test that checks the implementation against itself would agree with any of its
// errors. The frontend side (hierIdCompare) is checked against THIS SAME list —
// the match of the two literals is SC-004.
var idOrderReference = []string{
	"sk-1--2",
	"sk-2",
	"sk-2-1",
	"sk-2.1",
	"sk-2.2",
	"sk-2.10",
	"sk-2.",
	"sk-002",
	"sk-10",
	"sk-42",
	"sk-00000000000000000000042",
	"sk-123456",
	"sk--1",
	"sk-42x",
	"sk-x42",
}

// permute gives deterministic permutations of the input: the order of the result
// must not depend on them.
func permute(src []string, kind int) []string {
	out := append([]string(nil), src...)
	switch kind {
	case 0: // as is
	case 1: // reversed
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	case 2: // rotation by a third
		k := len(out) / 3
		out = append(out[k:], out[:k]...)
	case 3: // even ones, then odd ones
		var ev, od []string
		for i, v := range out {
			if i%2 == 0 {
				ev = append(ev, v)
			} else {
				od = append(od, v)
			}
		}
		out = append(ev, od...)
	}
	return out
}

// TestCompare_MatchesPlanReferenceOrder — the rule gives the reference
// order, and the result does not depend on the input order (US-6, SC-004).
func TestCompare_MatchesPlanReferenceOrder(t *testing.T) {
	for kind := 0; kind < 4; kind++ {
		in := permute(idOrderReference, kind)
		got := append([]string(nil), in...)
		sort.SliceStable(got, func(i, j int) bool {
			return Compare(got[i], got[j]) < 0
		})
		for i := range got {
			if got[i] != idOrderReference[i] {
				t.Fatalf("permutation %d: position %d = %q, reference %q\ngot: %v",
					kind, i, got[i], idOrderReference[i], got)
			}
		}
	}
}

// TestCompare_IsStrictTotalOrder — antisymmetry and transitivity
// on all ordered triples of the corpus. The current frontend comparator
// is non-transitive on mixed sets, and that is exactly what US-6 forbids.
func TestCompare_IsStrictTotalOrder(t *testing.T) {
	c := idOrderReference
	for _, x := range c {
		if Compare(x, x) != 0 {
			t.Errorf("Compare(%q,%q) != 0", x, x)
		}
	}
	for _, x := range c {
		for _, y := range c {
			if sgn(Compare(x, y)) != -sgn(Compare(y, x)) {
				t.Errorf("antisymmetry violated on (%q,%q)", x, y)
			}
		}
	}
	for _, x := range c {
		for _, y := range c {
			for _, z := range c {
				if Compare(x, y) < 0 && Compare(y, z) < 0 && !(Compare(x, z) < 0) {
					t.Errorf("transitivity violated: %q < %q < %q, but not %q < %q", x, y, z, x, z)
				}
			}
		}
	}
}

func sgn(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// TestCompare_RuleCorners pins the points on which two
// independent implementations diverge most often.
func TestCompare_RuleCorners(t *testing.T) {
	cases := []struct {
		a, b, why string
	}{
		{"sk-2", "sk-002", "p.3: equal value, shorter spelling goes first"},
		{"sk-42", "sk-00000000000000000000042", "p.3: digit segment longer than a machine integer"},
		{"sk-2.2", "sk-2.10", "p.3: 2 before 10, not bytewise"},
		{"sk-123456", "sk--1", "p.4: digit segment before an empty one"},
		{"sk--1", "sk-42x", "p.5: empty segment before a non-empty one"},
		{"sk-42x", "sk-x42", "p.5: bytewise, '4' < 'x'"},
		{"sk-2", "sk-2-1", "p.6: fewer segments goes first"},
		{"sk-2-1", "sk-2.1", "p.7: only the separators differ, '-' < '.'"},
	}
	for _, c := range cases {
		if Compare(c.a, c.b) >= 0 {
			t.Errorf("%q must go before %q — %s", c.a, c.b, c.why)
		}
		if Compare(c.b, c.a) <= 0 {
			t.Errorf("reverse comparison %q/%q violates antisymmetry — %s", c.b, c.a, c.why)
		}
	}
}
