package issues

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
)

// sk-243 security follow-up (F4) — the information boundary of the
// «unavailable» bucket, pinned so it is a DECISION and not an artefact of
// how the SQL happened to be written.
//
// # What the security review actually found
//
// sk-291 closed the status leak: the status of a child in a project the
// caller cannot see is not in any bucket and is not recoverable by
// subtracting `unavailable` from `total` (that is what
// TestEpicRollup_InvisibleChildIsNotBucketedByStatus pins). What remains,
// by design, is the COUNT: a member of a visible epic sees «2 unavailable»
// and, watching it become 3, learns that somebody attached a task in a
// project they have no access to. That is object-existence / topology
// metadata across a membership boundary — real, and smaller than what it
// buys.
//
// # The decision, and why the count stays
//
// The count stays. Two reasons, in order:
//
//  1. Consistency. sk-282 introduced the «unavailable» category for the
//     dependency counters AS A NUMBER, and reasoned about the number: its
//     own rationale turns on «unavailable: 2» being unable to distinguish
//     one invisible parent from two invisible children, which is an
//     argument that only exists if the value is a count. One product must
//     not answer «what is behind the wall» with a number in one place and
//     a flag in another; an agent reading both would have to learn which
//     surface lies about arity.
//
//  2. Honesty beats vagueness. «N children are unavailable» is the answer
//     that lets a client size what it cannot see. «Something is
//     unavailable» tells it only that the epic is unclosable, which it
//     already learns from EligibleForClose.
//
// # The boundary this test pins
//
// Exact count — yes. Anything that would let the count be resolved into
// WHICH child, or into what state it is in — no. Both halves are asserted
// below, so a future change that turns the count into a flag, or that
// starts leaking identity or status alongside it, has to be a deliberate
// edit to a test that says out loud what it is protecting.

// TestUnavailableIsACountEverywhere is the consistency half, and it needs
// no database: the shape of the answer is the policy.
func TestUnavailableIsACountEverywhere(t *testing.T) {
	cases := []struct {
		what  string
		field any
		key   string
		doc   string
	}{
		{"EpicRollup.UnavailableChildren", EpicRollup{UnavailableChildren: 2}, "unavailableChildren", "the epic card"},
		{"EpicProgress.Unavailable", EpicProgress{Unavailable: 2}, "unavailable", "the list/tree progress chip"},
		{"dependencies.DepStatusCounts.Unavailable", dependencies.DepStatusCounts{Unavailable: 2}, "unavailable", "the sk-282 dependency counters"},
	}
	for _, c := range cases {
		v := reflect.ValueOf(c.field)
		f := v.FieldByNameFunc(func(n string) bool {
			return n == "Unavailable" || n == "UnavailableChildren"
		})
		if f.Kind() != reflect.Int {
			t.Errorf("%s is %s, want int — the unavailable category is a COUNT on every "+
				"surface (%s); turning one of them into a bool splits the contract",
				c.what, f.Kind(), c.doc)
		}
		raw, err := json.Marshal(c.field)
		if err != nil {
			t.Fatalf("marshal %s: %v", c.what, err)
		}
		var back map[string]any
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", c.what, err)
		}
		got, ok := back[c.key]
		if !ok {
			t.Errorf("%s: key %q missing from the wire shape %s", c.what, c.key, raw)
			continue
		}
		if n, isNum := got.(float64); !isNum || n != 2 {
			t.Errorf("%s: wire value for %q is %#v, want the number 2", c.what, c.key, got)
		}
	}
}

// TestEpicRollup_UnavailableIsAnExactCount is the same decision against the
// database: the number tracks arity, and it is the ONLY thing that moves.
//
// The second assertion is the boundary. Two invisible children in
// DIFFERENT statuses produce the same rollup as two invisible children in
// the same status — the count says how many, and nothing says which or
// what state.
func TestEpicRollup_UnavailableIsAnExactCount(t *testing.T) {
	byCount := map[int]EpicRollup{}
	for _, n := range []int{0, 1, 2, 3} {
		pool := newTestPool(t)
		caller, epicID := hiddenChildrenFixture(t, pool, statesFor(n))
		store := NewStore(pool)
		dto, err := store.GetIssue(callerCtx(t, caller, "user"), epicID)
		if err != nil {
			t.Fatalf("GetIssue (%d hidden): %v", n, err)
		}
		if dto.Epic == nil {
			t.Fatalf("no rollup on the epic (%d hidden)", n)
		}
		if dto.Epic.UnavailableChildren != n {
			t.Fatalf("UnavailableChildren = %d, want %d — the bucket is an exact count, "+
				"not a flag (see the file comment before changing this)",
				dto.Epic.UnavailableChildren, n)
		}
		byCount[n] = *dto.Epic
	}
	// Arity is the only signal: nothing else in the rollup moved.
	base := byCount[0]
	for n := 1; n <= 3; n++ {
		got := byCount[n]
		// sk-489 — CountedChildren tracks Total by construction (Counted =
		// Total minus the VISIBLE children that dropped out), and INV-4 keeps
		// the invisible ones inside the denominator: «cannot say» is not
		// «dropped out». So it is expected to move by exactly n, and the
		// movement is ASSERTED rather than normalised away — an implementation
		// that quietly dropped the hidden children out of the denominator
		// would be the INV-4 defect itself.
		if got.CountedChildren != base.CountedChildren+n {
			t.Errorf("hiding %d children moved CountedChildren to %d, want %d: invisible "+
				"children stay in the progress denominator (INV-4)",
				n, got.CountedChildren, base.CountedChildren+n)
		}
		got.CountedChildren = base.CountedChildren
		got.UnavailableChildren = base.UnavailableChildren
		got.TotalChildren = base.TotalChildren
		got.EligibleForClose = base.EligibleForClose
		// DoneChildren / NoWorkChildren are NOT normalised, and the fixture is
		// what makes that mean something: the hidden children are CLOSED
		// WITHOUT WORK (hiddenChildrenFixture, state «closed/<outcome>»), so a
		// leg that lost the caller's horizon moves NoWorkChildren by n and
		// DoneChildren by ∓n, and this comparison catches it. With hidden
		// children in a non-closed status — the shape this guard had until the
		// A/3 review — both legs were identically zero under every possible
		// implementation, and the sentence above described a discrimination the
		// fixture could not perform.
		if got != base {
			t.Errorf("hiding %d children moved a status bucket: %+v vs %+v", n, byCount[n], base)
		}
	}

	// …and the states of the hidden children are not a signal at all —
	// including their OUTCOME (sk-489, R-6): "closed/superseded" next to "open"
	// must produce the rollup two uniform hidden children produce.
	pool := newTestPool(t)
	caller, epicID := hiddenChildrenFixture(t, pool,
		[]string{"open", "closed/" + CloseOutcomeSuperseded})
	store := NewStore(pool)
	mixed, err := store.GetIssue(callerCtx(t, caller, "user"), epicID)
	if err != nil {
		t.Fatalf("GetIssue (mixed): %v", err)
	}
	if *mixed.Epic != byCount[2] {
		t.Errorf("the hidden children's statuses changed the rollup:\n mixed  -> %+v\n uniform-> %+v",
			*mixed.Epic, byCount[2])
	}
}

// statesFor returns n hidden-child states, all the same, so that the
// count is the only variable in the first half of the test.
//
// "Closed without work", not "in progress": the legs this guard leaves
// un-normalised (DoneChildren / NoWorkChildren) are legs of the OUTCOME, and
// a hidden child that is not closed cannot move them under ANY
// implementation — which is exactly how the guard came to promise a
// discrimination it could not perform (review A/3). A hidden child closed
// WITHOUT work is the state in which the horizon on the `no_work` leg is the
// only thing keeping it out of the numbers.
func statesFor(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "closed/" + CloseOutcomeWontDo
	}
	return out
}

// hiddenChildrenFixture is crossProjectEpicFixture generalised to N
// cross-project children: an epic the caller owns, one visible open child,
// and len(states) children in a project the caller is not a member of.
//
// A state is a status, or «closed/<outcome>» when the child must be closed
// with a particular outcome — the same spelling crossProjectEpicFixture uses,
// and for the same reason: status and outcome are written in ONE update, so
// the 000050 trigger never seeds a `done` the fixture did not ask for.
func hiddenChildrenFixture(t *testing.T, pool *pgxpool.Pool, states []string) (caller uuid.UUID, epicID string) {
	t.Helper()
	caller = seedUser(t, pool)
	outsider := seedUser(t, pool)

	visiblePID, prefix := seedProject(t, pool, caller)
	hiddenPID, hiddenPrefix := seedProject(t, pool, outsider)

	epicID = prefix + "-1"
	seedTypedIssue(t, pool, visiblePID, caller, epicID, "the epic", TypeEpic, nil)
	seedTypedIssue(t, pool, visiblePID, caller, prefix+"-1.1", "open child", TypeTask, &epicID)

	for i, st := range states {
		hiddenID := fmt.Sprintf("%s-%d", hiddenPrefix, 90+i)
		seedTypedIssue(t, pool, hiddenPID, outsider, hiddenID, "secret child", TypeTask, nil)
		if status, outcome, ok := strings.Cut(st, "/"); ok {
			if status != "closed" {
				t.Fatalf("an outcome is allowed only on a closed issue (%q)", st)
			}
			setClosedWithOutcome(t, pool, hiddenID, outcome)
		} else {
			setIssueStatus(t, pool, hiddenID, st)
		}
		seedParentChild(t, pool, hiddenID, epicID, outsider)
	}
	return caller, epicID
}

// ─────────────────── sk-489, matrix row 2: zero ≠ absence ───────────────────
//
// The neighbouring assertion above (TestUnavailableIsACountEverywhere) holds the shape
// of "unavailable": it is a count, and the key arrives on the wire. Its limit — it marshals
// a field with a NONZERO value and asks only about the presence of the key and its type,
// so it would not notice `omitempty` hung on such a counter: the key on
// two would stay in place.
//
// This gap is what the case below closes — for the three progress numbers
// introduced by sk-489. It discriminates exactly `omitempty`: with it the key on zero
// disappears, and a new frontend, for which "no field" means "an OLD
// api answers" (US-10), would draw the old picture from the old fields on every epic without
// dropped-out children. Zero must read as "counted, and there are none".

// TestProgressNumbersSurviveTheirOwnZero — sk-489, US-10, INV-3.
func TestProgressNumbersSurviveTheirOwnZero(t *testing.T) {
	// The fixture is the most dangerous possible: all three new numbers are zero, while
	// the old ones are not. An epic with one done child gives exactly that.
	raw, err := json.Marshal(EpicProgress{Closed: 1, Total: 1})
	if err != nil {
		t.Fatalf("marshal EpicProgress: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal EpicProgress: %v", err)
	}

	for _, key := range []string{"done", "noWork", "counted"} {
		got, ok := wire[key]
		if !ok {
			t.Errorf("key %q disappeared from the wire at a zero value (%s).\n"+
				"This is `omitempty` on a progress number: the client reads the absence of the key as "+
				"\"an old api answers\" (US-10) and draws the old picture from the old fields, "+
				"i.e. the new \"no work\" leg disappears on every epic that has no dropped-out children",
				key, raw)
			continue
		}
		if n, isNum := got.(float64); !isNum || n != 0 {
			t.Errorf("%q on the wire = %#v, expected a numeric zero", key, got)
		}
	}

	// The old keys are untouched (INV-3): the same names, the same values.
	for key, want := range map[string]float64{"closed": 1, "total": 1, "unavailable": 0} {
		got, ok := wire[key]
		if !ok {
			t.Errorf("the old key %q disappeared from the wire — new fields are only added", key)
			continue
		}
		if n, isNum := got.(float64); !isNum || n != want {
			t.Errorf("the old key %q = %#v, it was and must remain %v", key, got, want)
		}
	}
}
